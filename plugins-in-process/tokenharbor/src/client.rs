//! HTTP client: browser-shaped headers, pacing, per-endpoint timeouts
//! (dossier §1/§2 header requirements; task brief "1-2s pacing between
//! upstream calls; no retries that could burn quotas").
//!
//! Two hosts:
//! - `https://tokenharbor.ai` — app APIs (cookie auth, `origin` required).
//! - `https://auth.tokenharbor.ai` — Supabase (apikey auth; see auth.rs).
//!
//! Pacing: every request passes through a global 1.5s min-interval gate
//! before dispatch — the site is a web chat, not an API; hammering it is
//! both rude and the fastest way to a suspended account.
//!
//! Retries: NONE on the stream endpoint (a retry re-sends the prompt and
//! burns quota); auth-layer refresh-retry happens in chat.rs's 401 loop,
//! bounded to one attempt.

use std::sync::Arc;
use std::time::Duration;

use serde_json::Value;
use tokio::sync::Mutex;

use crate::auth::AuthError;

/// Live-verified UA (dossier VERIFICATION.md, used for every probe).
pub const DESKTOP_UA: &str = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36";

/// Pacing interval between upstream calls (1-2s per task brief).
const PACING: Duration = Duration::from_millis(1500);

/// Internal transport errors (mapped onto §4 by chat.rs).
#[derive(Debug, thiserror::Error)]
pub enum TransportError {
    #[error("upstream http: {0}")]
    Http(#[from] reqwest::Error),
    #[error("auth: {0}")]
    Auth(#[from] AuthError),
    /// Non-2xx whose body was not a parseable SSE error frame.
    #[error("upstream status {status}: {detail}")]
    Status { status: u16, detail: String },
}

/// One upstream call's raw result: status + body bytes (SSE or JSON).
#[derive(Debug, Clone)]
pub struct RawResponse {
    pub status: u16,
    pub body: Vec<u8>,
    pub content_type: String,
}

impl RawResponse {
    /// Body as lossy UTF-8.
    pub fn text(&self) -> String {
        String::from_utf8_lossy(&self.body).into_owned()
    }
    /// Is the body an SSE stream (the error frames ride SSE even on 4xx —
    /// dossier §6: parse before mapping)?
    pub fn is_sse(&self) -> bool {
        self.content_type.starts_with("text/event-stream")
    }
}

/// Paced, header-shaped client for one TokenHarbor host.
pub struct Client {
    http: reqwest::Client,
    base_url: String,
    /// Global pacing gate: the last dispatch time. Calls wait until
    /// PACING has elapsed since the previous one.
    last_dispatch: Mutex<Option<std::time::Instant>>,
}

impl Client {
    /// Production constructor: the live site.
    pub fn new_live() -> Self {
        Self::with_base("https://tokenharbor.ai".into(), stream_timeout())
    }

    /// Test constructor: a mock host with the same pacing machinery.
    pub fn with_base(base_url: String, timeout: Duration) -> Self {
        Self {
            http: reqwest::Client::builder()
                .user_agent(DESKTOP_UA)
                .timeout(timeout)
                // No cookie jar: cookies are managed per account in the
                // Cookie header (the account pool rotates credentials).
                .build()
                .expect("reqwest client builds"),
            base_url,
            last_dispatch: Mutex::new(None),
        }
    }

    /// GET with browser headers + pacing. `path` starts with `/`.
    pub async fn get(&self, path: &str, cookie: &str) -> Result<RawResponse, TransportError> {
        self.pace().await;
        let resp = self
            .http
            .get(format!("{}{}", self.base_url, path))
            .header("cookie", cookie)
            .header("origin", "https://tokenharbor.ai")
            .header("accept", "*/*")
            .send()
            .await?;
        raw(resp).await
    }

    /// POST JSON with browser headers + pacing.
    pub async fn post_json(
        &self,
        path: &str,
        body: &Value,
        cookie: &str,
    ) -> Result<RawResponse, TransportError> {
        self.pace().await;
        let resp = self
            .http
            .post(format!("{}{}", self.base_url, path))
            .header("cookie", cookie)
            .header("origin", "https://tokenharbor.ai")
            .header("content-type", "application/json")
            .header("accept", "*/*")
            .json(body)
            .send()
            .await?;
        raw(resp).await
    }

    /// POST JSON accepting SSE — the stream endpoint. Body is read to
    /// completion here (buffered SSE; see PROTOCOL.md for the M3
    /// open-question answer).
    pub async fn post_stream(
        &self,
        path: &str,
        body: &Value,
        cookie: &str,
    ) -> Result<RawResponse, TransportError> {
        self.post_json(path, body, cookie).await
    }

    /// DELETE (session cleanup; best-effort by design — callers ignore
    /// errors).
    pub async fn delete(&self, path: &str, cookie: &str) -> Result<RawResponse, TransportError> {
        self.pace().await;
        let resp = self
            .http
            .delete(format!("{}{}", self.base_url, path))
            .header("cookie", cookie)
            .header("origin", "https://tokenharbor.ai")
            .send()
            .await?;
        raw(resp).await
    }

    /// Wait for the pacing interval since the last dispatch.
    async fn pace(&self) {
        let mut last = self.last_dispatch.lock().await;
        if let Some(prev) = *last {
            let elapsed = prev.elapsed();
            if elapsed < PACING {
                tokio::time::sleep(PACING - elapsed).await;
            }
        }
        *last = Some(std::time::Instant::now());
    }
}

/// Stream timeout: chat completions run long (dossier reference: 300s).
fn stream_timeout() -> Duration {
    Duration::from_secs(300)
}

async fn raw(resp: reqwest::Response) -> Result<RawResponse, TransportError> {
    let status = resp.status().as_u16();
    let content_type = resp
        .headers()
        .get(reqwest::header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .unwrap_or_default()
        .to_string();
    let body = resp.bytes().await?.to_vec();
    Ok(RawResponse {
        status,
        body,
        content_type,
    })
}

/// Arc alias used by the plugin assembly.
pub type SharedClient = Arc<Client>;
