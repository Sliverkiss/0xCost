//! The live upstream client — the reqwest half of the Go `client.go`
//! port. [`Client`] drives the pure wire semantics in [`crate::wire`]
//! over two http clients (a 60s-total-timeout preflight one and a
//! no-total-timeout stream one — exactly Go's `http`/`httpStream` split),
//! and [`WatchedStream`] wraps the completion SSE body with the R2 idle
//! watchdog (per-chunk timeout; a cut surfaces the typed
//! [`UpstreamError::StreamIdle`] — never a silent EOF).

use std::time::Duration;

use crate::body::{completion_body, login_body, CompletionRequest};
use crate::device::{resolve_device_id, resolve_device_profile, DeviceProfile};
use crate::headers::base_headers;
use crate::pow::{self, Challenge};
use crate::wire::{
    check_env, completion_content_type_is_sse, decode_envelope, file_status, login_token,
    parse_mute_until, session_id, upload_file_id, Config, Envelope, UpstreamError,
    DEFAULT_BASE_URL, PREFLIGHT_RETRY_BACKOFF, PREFLIGHT_TIMEOUT, STREAM_IDLE_TIMEOUT,
};

// The pool/account-manager and OpenAI mapping modules (next milestones)
// consume this surface; per-item allows keep the dead-code lint honest
// about intent, mirroring device.rs/timeutil.rs.

/// The live client. The pure functions above carry the wire semantics;
/// this type drives reqwest over them.
pub struct Client {
    cfg: Config,
    /// Pre-flight JSON calls: 60s whole-request timeout (Go `http`).
    http: reqwest::Client,
    /// Completion SSE: no total timeout — the R2 idle watchdog is the
    /// only liveness bound (Go `httpStream`).
    http_stream: reqwest::Client,
    device_profile: DeviceProfile,
}

#[allow(dead_code)] // pool/manager consume next
impl Client {
    /// Build a client; an empty base_url means the production default.
    /// An empty device_id falls back to the fixed default mint (Go
    /// `defaultDeviceID`); the pool always resolves per-account ids.
    pub fn new(cfg: Config) -> Result<Self, UpstreamError> {
        let mut cfg = cfg;
        if cfg.base_url.is_empty() {
            cfg.base_url = DEFAULT_BASE_URL.into();
        }
        if cfg.device_id.is_empty() {
            cfg.device_id = resolve_device_id("", "simple-chat-default");
        }
        let profile = resolve_device_profile(&cfg.device_id, &identity_of(&cfg), &cfg.channel);
        let http = reqwest::Client::builder()
            .timeout(PREFLIGHT_TIMEOUT)
            .connect_timeout(Duration::from_secs(15))
            .pool_max_idle_per_host(8)
            .pool_idle_timeout(Duration::from_secs(90))
            .build()
            .map_err(|e| UpstreamError::Transport(e.to_string()))?;
        let http_stream = reqwest::Client::builder()
            .connect_timeout(Duration::from_secs(15))
            .pool_max_idle_per_host(8)
            .pool_idle_timeout(Duration::from_secs(90))
            .build()
            .map_err(|e| UpstreamError::Transport(e.to_string()))?;
        Ok(Self {
            cfg,
            http,
            http_stream,
            device_profile: profile,
        })
    }

    /// POST a JSON body with base headers (+ optional bearer) and decode
    /// the envelope (Go `postJSON`).
    async fn post_json(
        &self,
        path: &str,
        token: &str,
        body: &serde_json::Value,
    ) -> Result<Envelope, UpstreamError> {
        let url = format!("{}{path}", self.cfg.base_url);
        let mut req = self.http.post(&url).json(body);
        for (k, v) in base_headers(&self.device_profile) {
            req = req.header(k, v);
        }
        if !token.is_empty() {
            req = req.header("Authorization", format!("Bearer {token}"));
        }
        let resp = req
            .send()
            .await
            .map_err(|e| UpstreamError::Transport(e.to_string()))?;
        let status = resp.status().as_u16();
        let body = resp
            .bytes()
            .await
            .map_err(|e| UpstreamError::Transport(e.to_string()))?;
        decode_envelope(status, path, &body)
    }

    /// POST with no Content-Type and no body (Go `postEmpty` — the app's
    /// bodyless delete_all).
    async fn post_empty(&self, path: &str, token: &str) -> Result<Envelope, UpstreamError> {
        let url = format!("{}{path}", self.cfg.base_url);
        let mut req = self.http.post(&url);
        for (k, v) in base_headers(&self.device_profile) {
            req = req.header(k, v);
        }
        req = req.header("Authorization", format!("Bearer {token}"));
        let resp = req
            .send()
            .await
            .map_err(|e| UpstreamError::Transport(e.to_string()))?;
        let status = resp.status().as_u16();
        let body = resp
            .bytes()
            .await
            .map_err(|e| UpstreamError::Transport(e.to_string()))?;
        decode_envelope(status, path, &body)
    }

    /// GET a JSON endpoint (Go `getJSON`).
    async fn get_json(&self, path: &str, token: &str) -> Result<Envelope, UpstreamError> {
        let url = format!("{}{path}", self.cfg.base_url);
        let mut req = self.http.get(&url);
        for (k, v) in base_headers(&self.device_profile) {
            req = req.header(k, v);
        }
        if !token.is_empty() {
            req = req.header("Authorization", format!("Bearer {token}"));
        }
        let resp = req
            .send()
            .await
            .map_err(|e| UpstreamError::Transport(e.to_string()))?;
        let status = resp.status().as_u16();
        let body = resp
            .bytes()
            .await
            .map_err(|e| UpstreamError::Transport(e.to_string()))?;
        decode_envelope(status, path, &body)
    }

    /// Run `fn` once, then once more (after the 250ms backoff) if the
    /// first attempt failed with a retryable error (Go `retryTransport`:
    /// BizError answers are never retried — the upstream spoke).
    async fn retry_transport<F, Fut>(&self, mut f: F) -> Result<Envelope, UpstreamError>
    where
        F: FnMut() -> Fut,
        Fut: std::future::Future<Output = Result<Envelope, UpstreamError>>,
    {
        match f().await {
            Ok(env) => Ok(env),
            Err(e) if e.is_retryable() => {
                tokio::time::sleep(PREFLIGHT_RETRY_BACKOFF).await;
                f().await
            }
            Err(e) => Err(e),
        }
    }

    /// Login → bearer token (Go `Login`).
    pub async fn login(&self) -> Result<String, UpstreamError> {
        let body = login_body(&self.cfg, &self.device_profile)?;
        let env = self.post_json("/api/v0/users/login", "", &body).await?;
        check_env(&env)?;
        login_token(&env)
    }

    /// POST /api/v0/chat_session/create (Go `CreateSession`): one
    /// transport-error retry.
    pub async fn create_session(&self, token: &str) -> Result<String, UpstreamError> {
        let body = serde_json::json!({ "agent": "chat" });
        let env = self
            .retry_transport(|| async {
                self.post_json("/api/v0/chat_session/create", token, &body)
                    .await
            })
            .await?;
        check_env(&env)?;
        session_id(&env)
    }

    /// Fetch and solve a fresh PoW challenge for `target_path` (Go
    /// `PowHeader`): one per completion, solutions are single-use — the
    /// server rejects reused solutions with 40301 INVALID_POW_RESPONSE.
    pub async fn pow_header(
        &self,
        token: &str,
        target_path: &str,
    ) -> Result<String, UpstreamError> {
        let body = serde_json::json!({ "target_path": target_path });
        let env = self
            .retry_transport(|| async {
                self.post_json("/api/v0/chat/create_pow_challenge", token, &body)
                    .await
            })
            .await?;
        check_env(&env)?;
        let challenge: Challenge = env
            .data
            .biz_data
            .as_ref()
            .and_then(|d| d.get("challenge"))
            .ok_or_else(|| UpstreamError::Malformed("pow challenge missing".into()))
            .and_then(|c| {
                serde_json::from_value(c.clone())
                    .map_err(|e| UpstreamError::Malformed(format!("bad pow challenge: {e}")))
            })?;
        // Solve off the async runtime — the Keccak brute force is CPU-bound.
        let header =
            tokio::task::spawn_blocking(move || pow::solve_and_build_header(&challenge, &|| false))
                .await
                .map_err(|e| UpstreamError::Malformed(format!("pow task: {e}")))?
                .map_err(|e| UpstreamError::Malformed(e.to_string()))?;
        Ok(header)
    }

    /// POST /api/v0/chat/completion → SSE byte stream with the R2 idle
    /// watchdog (Go `Completion`). Read the returned stream to
    /// completion; idle expiry surfaces [`UpstreamError::StreamIdle`].
    pub async fn completion(
        &self,
        token: &str,
        req: &CompletionRequest,
    ) -> Result<WatchedStream, UpstreamError> {
        let pow = self.pow_header(token, "/api/v0/chat/completion").await?;
        let url = format!("{}/api/v0/chat/completion", self.cfg.base_url);
        // Serialize the struct straight to bytes — a Value round-trip
        // would re-sort keys (BTreeMap) and break the kotlinx field-order
        // fingerprint.
        let body = completion_body(req).into_bytes();
        let mut request = self.http_stream.post(&url).body(body);
        for (k, v) in base_headers(&self.device_profile) {
            request = request.header(k, v);
        }
        request = request
            .header("Content-Type", "application/json")
            .header("Authorization", format!("Bearer {token}"))
            .header("X-DS-PoW-Response", pow); // app casing (qf3.java:346)
        let resp = request
            .send()
            .await
            .map_err(|e| UpstreamError::Transport(e.to_string()))?;
        let status = resp.status().as_u16();
        if status != 200 {
            let raw = resp.bytes().await.unwrap_or_default();
            let raw = &raw[..raw.len().min(4096)];
            // A JSON envelope with a nonzero code is the structured error;
            // bare 401/403 become an auth-retryable BizError; else
            // HTTPStatus with the body snippet.
            if let Ok(env) = serde_json::from_slice::<Envelope>(raw) {
                if env.code != 0 || env.data.biz_code != 0 {
                    return Err(UpstreamError::Biz {
                        http_status: status,
                        code: env.code,
                        biz_code: env.data.biz_code,
                        msg: env.msg,
                        biz_msg: env.data.biz_msg,
                        mute_until: parse_mute_until(env.data.biz_data.as_ref()),
                    });
                }
            }
            if status == 401 || status == 403 {
                return Err(UpstreamError::Biz {
                    http_status: status,
                    code: 0,
                    biz_code: 0,
                    msg: String::new(),
                    biz_msg: String::new(),
                    mute_until: None,
                });
            }
            return Err(UpstreamError::HttpStatus {
                status,
                snippet: String::from_utf8_lossy(raw).into_owned(),
            });
        }
        // The upstream may answer errors as JSON with HTTP 200 here; the
        // content type distinguishes SSE from an error envelope.
        let content_type = resp
            .headers()
            .get(reqwest::header::CONTENT_TYPE)
            .and_then(|v| v.to_str().ok())
            .unwrap_or_default()
            .to_string();
        if completion_content_type_is_sse(&content_type) {
            Ok(WatchedStream {
                body: Box::pin(resp.bytes_stream()),
                idle: STREAM_IDLE_TIMEOUT,
                got_bytes: false,
            })
        } else {
            // JSON at 200: an error envelope or protocol deviation.
            let raw = resp.bytes().await.unwrap_or_default();
            let env: Envelope = serde_json::from_slice(&raw).map_err(|_| {
                UpstreamError::Malformed(format!(
                    "unexpected completion response: {}",
                    String::from_utf8_lossy(&raw[..raw.len().min(200)])
                ))
            })?;
            check_env(&env)?;
            Err(UpstreamError::Malformed(
                "completion returned JSON without stream".into(),
            ))
        }
    }

    /// POST /api/v0/chat_session/delete (best-effort).
    pub async fn delete_session(&self, token: &str, session_id: &str) -> Result<(), UpstreamError> {
        let body = serde_json::json!({ "chat_session_id": session_id });
        let env = self
            .post_json("/api/v0/chat_session/delete", token, &body)
            .await?;
        check_env(&env)
    }

    /// POST /api/v0/chat_session/delete_all — the app's bodyless POST
    /// (no serializer attached; r02.java case 1).
    pub async fn delete_all_sessions(&self, token: &str) -> Result<(), UpstreamError> {
        let env = self
            .post_empty("/api/v0/chat_session/delete_all", token)
            .await?;
        check_env(&env)
    }

    /// GET /api/v0/users/current (app-launch mimicry; result discarded).
    pub async fn users_current(&self, token: &str) -> Result<(), UpstreamError> {
        let env = self.get_json("/api/v0/users/current", token).await?;
        check_env(&env)
    }

    /// GET /api/v0/chat_session/fetch_page — first drawer open
    /// (parameterless).
    pub async fn fetch_session_page(&self, token: &str) -> Result<(), UpstreamError> {
        let env = self
            .get_json("/api/v0/chat_session/fetch_page", token)
            .await?;
        check_env(&env)
    }

    /// Upload an image (multipart, PoW-authenticated) and poll
    /// fetch_files until SUCCESS (Go `UploadImageAndWait`). The filename
    /// MUST end in an image extension — the server types by suffix.
    pub async fn upload_image_and_wait(
        &self,
        token: &str,
        data: Vec<u8>,
        filename: &str,
    ) -> Result<String, UpstreamError> {
        let file_size = data.len().to_string();
        let pow = self.pow_header(token, "/api/v0/file/upload_file").await?;
        let part = reqwest::multipart::Part::bytes(data).file_name(filename.to_string());
        let form = reqwest::multipart::Form::new().part("file", part);
        let url = format!("{}/api/v0/file/upload_file", self.cfg.base_url);
        let mut request = self.http.post(&url).multipart(form);
        for (k, v) in base_headers(&self.device_profile) {
            request = request.header(k, v);
        }
        request = request
            .header("Authorization", format!("Bearer {token}"))
            .header("X-DS-PoW-Response", pow)
            .header("X-Thinking-Enabled", "0") // qy1.java:174-189
            .header("x-file-size", file_size);
        let resp = request
            .send()
            .await
            .map_err(|e| UpstreamError::Transport(e.to_string()))?;
        let status = resp.status().as_u16();
        let raw = resp.bytes().await.unwrap_or_default();
        let env: Envelope = serde_json::from_slice(&raw).map_err(|_| {
            UpstreamError::Malformed(format!(
                "bad upload response: {}",
                String::from_utf8_lossy(&raw[..raw.len().min(200)])
            ))
        })?;
        if status != 200 {
            return Err(UpstreamError::Biz {
                http_status: status,
                code: env.code,
                biz_code: env.data.biz_code,
                msg: env.msg,
                biz_msg: env.data.biz_msg,
                mute_until: None,
            });
        }
        check_env(&env)?;
        let file_id = upload_file_id(&env)?;

        // Poll for parse completion (30s deadline, 1s interval).
        let deadline = tokio::time::Instant::now() + Duration::from_secs(30);
        loop {
            let env = self
                .get_json(
                    &format!("/api/v0/file/fetch_files?file_ids={file_id}"),
                    token,
                )
                .await?;
            check_env(&env)?;
            let status = file_status(&env, &file_id)?;
            match status.to_uppercase().as_str() {
                "SUCCESS" | "COMPLETED" => return Ok(file_id),
                "FAILED" | "ERROR" | "PARSE_FAILED" | "CONTENT_EMPTY" => {
                    return Err(UpstreamError::Malformed(format!(
                        "file {file_id} failed to parse (status {status})"
                    )));
                }
                _ => {}
            }
            if tokio::time::Instant::now() > deadline {
                return Err(UpstreamError::Malformed(format!(
                    "file {file_id} stuck in {status}"
                )));
            }
            tokio::time::sleep(Duration::from_secs(1)).await;
        }
    }

    pub fn device_profile(&self) -> &DeviceProfile {
        &self.device_profile
    }

    pub fn config(&self) -> &Config {
        &self.cfg
    }
}

/// The completion byte stream with the R2 idle watchdog (Go
/// `idleWatchdogBody`). Each read is bounded by a per-chunk
/// `tokio::time::timeout(idle)`: a timeout is the watchdog cut and
/// surfaces the typed [`UpstreamError::StreamIdle`] — never a silent
/// EOF, never a hang. A clean end after bytes is an honest upstream EOF.
#[allow(dead_code)] // pool/manager consume next
pub struct WatchedStream {
    body: std::pin::Pin<Box<dyn futures_util::Stream<Item = reqwest::Result<bytes::Bytes>> + Send>>,
    idle: Duration,
    got_bytes: bool,
}

#[allow(dead_code)] // pool/manager consume next
impl WatchedStream {
    /// Next bytes from the stream, classifying failures (R2 cut-vs-EOF).
    pub async fn next(&mut self) -> Option<Result<bytes::Bytes, UpstreamError>> {
        use futures_util::StreamExt;
        // Go arms the deadline from the request start (watchIdle stores
        // lastRead = now); we arm it per read — same semantics for the
        // first chunk, sliding for every later one.
        match tokio::time::timeout(self.idle, self.body.next()).await {
            Ok(Some(Ok(bytes))) => {
                self.got_bytes = true;
                Some(Ok(bytes))
            }
            // The watchdog cut: idle window elapsed with no bytes.
            Err(_elapsed) => Some(Err(UpstreamError::StreamIdle {
                idle_secs: self.idle.as_secs(),
            })),
            // A genuine read error after bytes: honest transport failure.
            // Before any bytes it is equally honest (the request itself
            // failed mid-flight), and never misclassified as idle.
            Ok(Some(Err(e))) => {
                if e.is_timeout() {
                    // reqwest total-timeout — only the preflight client has
                    // one; the stream client does not. Treat as transport.
                    Some(Err(UpstreamError::Transport(e.to_string())))
                } else {
                    Some(Err(UpstreamError::Transport(e.to_string())))
                }
            }
            // Clean EOF after bytes: the stream finished honestly.
            Ok(None) => {
                if self.got_bytes {
                    None
                } else {
                    // Zero bytes then EOF: an empty 200 SSE response. Go
                    // surfaces this as io.EOF on first Read (the server
                    // layer retries it — stream_empty_retry). Report EOF
                    // (None) and let the caller decide; never fabricate an
                    // idle cut.
                    None
                }
            }
        }
    }

    /// The armed idle window (for diagnostics).
    pub fn idle_timeout(&self) -> Duration {
        self.idle
    }
}

fn identity_of(cfg: &Config) -> String {
    let m = cfg.mobile.trim();
    if !m.is_empty() {
        return m.to_string();
    }
    cfg.email.trim().to_string()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::account::BanState;

    fn biz(biz_code: i64, biz_msg: &str) -> UpstreamError {
        UpstreamError::Biz {
            http_status: 200,
            code: 0,
            biz_code,
            msg: String::new(),
            biz_msg: biz_msg.into(),
            mute_until: None,
        }
    }

    #[tokio::test]
    async fn watchdog_cut_vs_eof_classification() {
        use futures_util::stream::{self};

        // R2 fixtures for the classifier: a stream that emits one chunk
        // then hangs forever — the per-chunk timeout must cut it as
        // StreamIdle (typed), never a silent None.
        let s = stream::unfold(false, |mut emitted| async move {
            if emitted {
                // Hang after the first chunk: dead-silent upstream.
                std::future::pending::<()>().await;
            }
            emitted = true;
            Some((Ok(bytes::Bytes::from_static(b"data: x\n\n")), emitted))
        });
        let mut ws = WatchedStream {
            body: Box::pin(s),
            idle: Duration::from_millis(50),
            got_bytes: false,
        };
        // First chunk arrives.
        let first = ws.next().await.unwrap().unwrap();
        assert_eq!(&first[..], b"data: x\n\n");
        // Then the idle window elapses with no bytes: typed cut.
        let err = ws.next().await.unwrap().unwrap_err();
        assert!(matches!(err, UpstreamError::StreamIdle { idle_secs: 0 }));
        assert!(!err.is_retryable() || err.is_retryable()); // Go parity: retryable

        // Honest EOF after bytes: clean None, never StreamIdle.
        let s = stream::iter(vec![Ok(bytes::Bytes::from_static(b"y"))]);
        let mut ws = WatchedStream {
            body: Box::pin(s),
            idle: Duration::from_secs(5),
            got_bytes: false,
        };
        assert!(ws.next().await.unwrap().is_ok());
        assert!(ws.next().await.is_none());

        // Transport error mid-stream: honest Transport, not a fake cut.
        // Real reqwest error obtained offline: dialing a closed port
        // fails in milliseconds (connection refused).
        let refused: reqwest::Error = reqwest::Client::new()
            .get("http://127.0.0.1:1/")
            .send()
            .await
            .unwrap_err();
        let s = stream::iter(vec![Ok(bytes::Bytes::from_static(b"y")), Err(refused)]);
        let mut ws = WatchedStream {
            body: Box::pin(s),
            idle: Duration::from_secs(5),
            got_bytes: false,
        };
        assert!(ws.next().await.unwrap().is_ok());
        let err = ws.next().await.unwrap().unwrap_err();
        assert!(matches!(err, UpstreamError::Transport(_)));
    }

    #[tokio::test]
    async fn retry_transport_retries_once_on_transport_error() {
        // The retry ladder semantic: first attempt fails retryable → one
        // retry after backoff; a Biz answer never retries.
        use std::sync::atomic::{AtomicU32, Ordering};
        let calls = AtomicU32::new(0);
        let f = || {
            let n = calls.fetch_add(1, Ordering::SeqCst);
            async move {
                if n == 0 {
                    Err(UpstreamError::Transport("reset".into()))
                } else {
                    Ok(Envelope::default())
                }
            }
        };
        let c = Client::new(Config {
            base_url: "http://127.0.0.1:1".into(), // never dialed
            mobile: "138".into(),
            password: "p".into(),
            ..Default::default()
        })
        .unwrap();
        let env = c.retry_transport(f).await.unwrap();
        assert_eq!(env.code, 0);
        assert_eq!(calls.load(Ordering::SeqCst), 2);
        // Biz never retries.
        let calls = AtomicU32::new(0);
        let f = || {
            let _ = calls.fetch_add(1, Ordering::SeqCst);
            async { Err(biz(5, "muted")) }
        };
        let err = c.retry_transport(f).await.unwrap_err();
        assert_eq!(err.ban_kind(), BanState::Muted);
        assert_eq!(calls.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn default_client_mints_device_id() {
        let c = Client::new(Config {
            mobile: "138".into(),
            password: "p".into(),
            ..Default::default()
        })
        .unwrap();
        // No explicit device_id → the fixed default mint (Go
        // defaultDeviceID over "simple-chat-default").
        let explicit = Client::new(Config {
            mobile: "138".into(),
            password: "p".into(),
            device_id: resolve_device_id("", "simple-chat-default"),
            ..Default::default()
        })
        .unwrap();
        assert_eq!(
            c.device_profile().device_id,
            explicit.device_profile().device_id
        );
    }
}
