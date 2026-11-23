use anyhow::{Result, ensure};
use std::{
    fs::File,
    io::{BufRead, BufReader},
    path::Path,
};
use vantry_protocol::ProviderEvent;

/// Incremental observer of an attempt journal. An incomplete last record stays pending
/// until a later call sees its newline. The worker owns terminal state in result.json.
pub struct JobEvents {
    reader: BufReader<File>,
    line: Vec<u8>,
    sequence: i64,
    offset: u64,
}
impl JobEvents {
    pub fn open(job_dir: &Path, after: i64) -> Result<Self> {
        ensure!(after >= 0, "negative provider cursor");
        let mut events = Self {
            reader: BufReader::new(File::open(job_dir.join("events.jsonl"))?),
            line: vec![],
            sequence: 0,
            offset: 0,
        };
        while events.sequence < after {
            ensure!(
                events.next_event()?.is_some(),
                "cursor beyond committed journal"
            );
        }
        Ok(events)
    }
    pub fn next_event(&mut self) -> Result<Option<ProviderEvent>> {
        self.reader.read_until(b'\n', &mut self.line)?;
        if self.line.last() != Some(&b'\n') {
            return Ok(None);
        }
        let event: ProviderEvent = serde_json::from_slice(&self.line)?;
        ensure!(
            event.sequence == self.sequence + 1,
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
