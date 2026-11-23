use flate2::{Decompress, FlushDecompress, Status};
use std::io::{self, BufRead, BufReader, Read};

/// flate2's convenience reader accepts EOF before the DEFLATE end marker.
/// Go's flate.Reader reports truncated release streams as unexpected EOF.
pub(crate) struct Inflater<R> {
    source: BufReader<R>,
    decoder: Decompress,
    done: bool,
}
impl<R: Read> Inflater<R> {
    pub fn new(source: R) -> Self {
        Self {
            source: BufReader::new(source),
            decoder: Decompress::new(false),
            done: false,
        }
    }
}
impl<R: Read> Read for Inflater<R> {
    fn read(&mut self, out: &mut [u8]) -> io::Result<usize> {
        while !self.done && !out.is_empty() {
            let input = self.source.fill_buf()?;
            let before_in = self.decoder.total_in();
            let before_out = self.decoder.total_out();
            let status = self
                .decoder
                .decompress(input, out, FlushDecompress::None)
                .map_err(|_| {
                    io::Error::new(io::ErrorKind::InvalidData, "invalid DEFLATE stream")
                })?;
            let consumed = (self.decoder.total_in() - before_in) as usize;
            let count = (self.decoder.total_out() - before_out) as usize;
            self.source.consume(consumed);
            self.done = status == Status::StreamEnd;
            if count > 0 {
                return Ok(count);
            }
            if !self.done && consumed == 0 {
                return Err(io::Error::new(
                    io::ErrorKind::UnexpectedEof,
                    "incomplete DEFLATE stream",
                ));
            }
        }
        Ok(0)
    }
}
