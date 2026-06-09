use std::sync::{Arc, Mutex};

#[derive(Debug)]
pub struct DpqEntry { pub data: Vec<u8>, pub vtime: i64, pub hash: u64, pub seq: u64 }

#[derive(Debug)]
pub struct DpqQueue { entries: Vec<DpqEntry>, seq: u64 }

impl DpqQueue {
    pub fn new() -> Arc<Mutex<Self>> { Arc::new(Mutex::new(Self { entries: Vec::new(), seq: 0 })) }
    pub fn enqueue(&mut self, data: Vec<u8>, vtime: i64) {
        let hash = {
            let mut h: u64 = 0xcbf29ce484222325;
            for &b in &data { h ^= b as u64; h = h.wrapping_mul(0x100000001b3); }
            h
        };
        self.seq += 1;
        self.entries.push(DpqEntry { data, vtime, hash, seq: self.seq });
    }
    pub fn drain(&mut self) -> Vec<DpqEntry> {
        self.entries.sort_by(|a, b| a.vtime.cmp(&b.vtime).then(a.hash.cmp(&b.hash)).then(a.seq.cmp(&b.seq)));
        std::mem::take(&mut self.entries)
    }
    pub fn reset(&mut self) { self.entries.clear(); self.seq = 0; }
    pub fn len(&self) -> usize { self.entries.len() }
}
