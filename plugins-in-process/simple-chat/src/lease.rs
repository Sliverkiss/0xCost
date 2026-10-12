//! Lease — the public in-flight handle (Go `pool.go` `Lease` + the
//! `AccountStatus` snapshot type), split from `pool.rs` to keep both
//! under the 800-line limit.

use std::sync::Arc;
use std::time::{Duration, SystemTime};

use crate::client::Client;
use crate::manager::AccountManager;
use crate::pool::{Pool, SharedAccountState};
use crate::sse::StreamError;
use crate::wire::UpstreamError;

/// One account's live runtime state for the health surface.
#[derive(Debug, Clone, PartialEq)]
pub struct AccountStatus {
    pub identity: String,
    /// ready/banned/muted/risk/cooling.
    pub state: String,
    pub inflight: usize,
    pub max_inflight: usize,
    pub token_warm: bool,
}

/// One reserved in-flight slot on one account (Go `Lease`). Dropping the
/// lease releases the slot.
pub struct Lease {
    pub(crate) pool: Arc<Pool>,
    pub(crate) shared: Arc<SharedAccountState>,
}

impl std::fmt::Debug for Lease {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Lease")
            .field("identity", &self.shared.identity)
            .finish()
    }
}

impl Lease {
    pub fn identity(&self) -> &str {
        &self.shared.identity
    }

    pub fn client(&self) -> &Arc<Client> {
        &self.shared.client
    }

    pub fn manager(&self) -> &Arc<AccountManager> {
        &self.shared.am
    }

    /// The current bearer token (lazy login) — Go `Lease.Token`.
    pub async fn token(&self) -> Result<String, UpstreamError> {
        self.shared.am.token().await
    }

    /// Create a chat session on this account (lazy relogin on auth
    /// failure, once) — Go `Lease.CreateSession`.
    pub async fn create_session(&self) -> Result<String, UpstreamError> {
        self.shared.am.create_session().await
    }

    /// Run the completion stream on this account (PoW included, lazy
    /// relogin on auth failure) — Go `Lease.Completion`.
    pub async fn completion(
        &self,
        req: &crate::body::CompletionRequest,
    ) -> Result<crate::client::WatchedStream, UpstreamError> {
        self.shared.am.completion(req).await
    }

    /// Feed an upstream error back into pool health (Go
    /// `Lease.NoteError`).
    pub fn note_error(&self, err: &UpstreamError) {
        self.pool.note_error(&self.shared, err);
    }

    /// Feed a mid-stream SSE error back into pool health (parallel-limit
    /// suppression).
    pub fn note_stream_error(&self, err: &StreamError) {
        self.pool.note_stream_error(&self.shared, err);
    }

    /// Record one completed request latency (Go `Pool.Observe`).
    pub fn observe(&self, elapsed: Duration) {
        self.pool.observe(&self.shared, elapsed);
    }

    /// The effective local park deadline (introspection).
    pub fn park_until(&self) -> Option<SystemTime> {
        self.shared.am.lock_state().park_until
    }
}

impl Drop for Lease {
    fn drop(&mut self) {
        self.shared.slots.add_permits(1);
    }
}
