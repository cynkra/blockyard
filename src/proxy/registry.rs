use std::net::SocketAddr;

use dashmap::DashMap;

/// Caches resolved worker addresses. The backend resolves the address
/// on spawn; the registry caches it so the proxy doesn't call
/// backend.addr() on every request.
pub struct WorkerRegistry {
    addrs: DashMap<String, SocketAddr>,
}

impl Default for WorkerRegistry {
    fn default() -> Self {
        Self::new()
    }
}

impl WorkerRegistry {
    pub fn new() -> Self {
        Self {
            addrs: DashMap::new(),
        }
    }

    pub fn get(&self, worker_id: &str) -> Option<SocketAddr> {
        self.addrs.get(worker_id).map(|v| *v)
    }

    pub fn insert(&self, worker_id: String, addr: SocketAddr) {
        self.addrs.insert(worker_id, addr);
    }

    pub fn remove(&self, worker_id: &str) -> Option<SocketAddr> {
        self.addrs.remove(worker_id).map(|(_, v)| v)
    }
}
