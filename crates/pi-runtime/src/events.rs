use anyhow::{Result, ensure};
use std::{
    fs::File,
    io::{BufRead, BufReader, Read, Seek, SeekFrom},
    path::{Path, PathBuf},
};
use vantry_protocol::ProviderEvent;

/// Incremental observer of an attempt journal. An incomplete last record stays pending
/// until a later call sees its newline. The worker owns terminal state in result.json.
pub struct IncrementalJournalReader {
    reader: BufReader<File>,
    job_dir: PathBuf,
    recovered_tail: bool,
    line: Vec<u8>,
    sequence: i64,
    offset: u64,
}
pub type JobEvents = IncrementalJournalReader;

/// Persist with the Core event transaction. Offset always follows a complete newline.
#[derive(Debug, Default, Clone, Copy, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct JournalCursor {
    pub sequence: i64,
    pub offset: u64,
}
impl IncrementalJournalReader {
    pub fn resume(job_dir: &Path, cursor: JournalCursor) -> Result<Self> {
        ensure!(
            cursor.sequence >= 0 && (cursor.sequence == 0) == (cursor.offset == 0),
            "invalid journal cursor"
        );
        let mut file = File::open(job_dir.join("events.jsonl"))?;
        if cursor.offset > 0 {
            file.seek(SeekFrom::Start(cursor.offset - 1))?;
            let mut byte = [0];
            file.read_exact(&mut byte)?;
            ensure!(byte == [b'\n'], "offset within journal record");
        }
        Ok(Self {
            reader: BufReader::new(file),
            job_dir: job_dir.into(),
            recovered_tail: false,
            line: vec![],
            sequence: cursor.sequence,
            offset: cursor.offset,
        })
    }
    pub fn checkpoint(&self) -> JournalCursor {
        JournalCursor {
            sequence: self.sequence,
            offset: self.offset,
        }
    }
    pub fn open(job_dir: &Path, after: i64) -> Result<Self> {
        ensure!(after >= 0, "negative provider cursor");
        let mut events = Self::resume(job_dir, JournalCursor::default())?;
        while events.sequence < after {
            ensure!(
                events.next_event()?.is_some(),
                "cursor beyond committed journal"
            );
        }
        Ok(events)
    }
    pub fn next_event(&mut self) -> Result<Option<ProviderEvent>> {
        let pending = !self.line.is_empty();
        self.reader.read_until(b'\n', &mut self.line)?;
        // A recovering worker saves the torn record, truncates it, then publishes
        // closing.json before appending any undelivered control results. A live
        // observer may still have that torn record in memory when this happens.
        if pending
            && !self.recovered_tail
            && self.job_dir.join("closing.json").try_exists()?
            && self.job_dir.join("events.partial").try_exists()?
        {
            self.reader.seek(SeekFrom::Start(self.offset))?;
            self.line.clear();
            self.recovered_tail = true;
            self.reader.read_until(b'\n', &mut self.line)?;
        }
        if self.line.last() != Some(&b'\n') {
            return Ok(None);
        }
        let event: ProviderEvent = serde_json::from_slice(&self.line)?;
        ensure!(
            self.sequence.checked_add(1) == Some(event.sequence),
            "provider sequence gap after {}",
            self.sequence
        );
        self.sequence = event.sequence;
        self.offset += self.line.len() as u64;
        self.line.clear();
        Ok(Some(event))
    }
    pub fn cursor(&self) -> i64 {
        self.sequence
    }
    pub(crate) fn tail(&self) -> (u64, &[u8]) {
        (self.offset, &self.line)
    }
}
