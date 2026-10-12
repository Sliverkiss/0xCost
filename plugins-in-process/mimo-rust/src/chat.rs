//! Chat pipeline (Go `internal/api/chat.go` run/execute/creds): one client
//! request through pool → credentials → upstream turn(s) → OpenAI shape,
//! with the §7.1 failure classification and §7.2 retry ladder.
//!
//! Retry ladder (original + 1 retry): auth-expired → 1 transparent refresh
//! on the same account (RENEW-005), failing over to disable on a second
//! 401 or a dead passToken; busy → 1 retry after 800 ms backoff; empty
//! response → 1 retry with fresh ids; other upstream/transport failures →
//! 1 retry on another account (excluded); risk-control / account-dead /
//! upstream-429 / model-rejected / content-level / gateway-deadline → no
//! retry (the first three also carry a pool action).
//!
//! Port note on the commit boundary: the host SDK's `ChatOutcome::Sse`
//! is a single buffer, so this pipeline collects the whole answer before
//! returning — the Go distinction "a committed stream cannot take a
//! status code" (ADR 0034) collapses to a simpler rule: every failure
//! here is pre-delivery and mapped through [`map_error`]; an in-band
//! error frame only appears at the host layer if it ever streams
//! partial output.

use std::time::Duration;

use crate::errmap::{map_error, MappedError, PipelineError};
use crate::login::{Client as LoginClient, Device, LoginError, Session};
use crate::models::max_query_chars;
use crate::openai::{
    add_usage, map_usage, new_chat_id, now_unix, random_hex32, shape_completion, StreamIdent,
};
use crate::pool::{LeaseHandle, Pool, PoolError, RiskEvent};
use crate::sse::InBandError;
use crate::store::{Store, StoredAccount};
use crate::turnplan::{
    composed_first_query_len, decode_chat_body_with_default_thinking, plan_full_replay, ChatInput,
    ParseError, PlanError,
};
use crate::upstream::{ChatClient, UpstreamChatError, UpstreamChatRequest};

/// Busy retry backoff (Go `busyRetryBackoff`, chat.go): live-observed to
/// clear within seconds, and the client is already waiting (SDD §7.2).
pub const BUSY_RETRY_BACKOFF: Duration = Duration::from_millis(800);

/// Replay cap (Go `replayCap`): max user turns replayed into one fresh
/// upstream conversation (PORT_DESIGN `..._REPLAY_CAP`).
pub const REPLAY_CAP: usize = 50;

/// A refresh outcome for the transparent auth retry.
enum RefreshOutcome {
    /// The renewal succeeded and was persisted; the re-execution reads the
    /// session back through the normal creds path.
    Refreshed,
    /// Refresh failed; the caller checks for `PassTokenDead` (permanent
    /// disable) vs other failures (recoverable disable).
    Failed(LoginError),
}

/// The pipeline's success answer, in the host SDK's two delivery shapes.
pub enum ChatAnswer {
    /// Non-streaming: the serialized completion JSON object.
    Completion(serde_json::Value),
    /// Streaming: the full SSE byte sequence, terminal `[DONE]` included.
    Sse(Vec<u8>),
}

/// Terminal result: an answer or a mapped client error.
pub type ChatOutcome = Result<ChatAnswer, MappedError>;

/// The chat pipeline: everything one request needs, cloned per request
/// (all fields are Arc/cheap clones).
#[derive(Clone)]
pub struct ChatPipeline {
    pub pool: Pool,
    pub store: Store,
    pub login: LoginClient,
    /// Default thinking per model when the request says nothing (ADR 0031;
    /// the config layer sets this from the model surface).
    pub thinking_default_on: bool,
    /// Default model id (upstream spelling).
    pub default_model: String,
}

impl ChatPipeline {
    /// The full request path: decode → caps → pool → retry ladder →
    /// upstream turn(s) → OpenAI shape (Go `Server.handleChat` + `run`).
    pub async fn handle(&self, body: &serde_json::Value) -> ChatOutcome {
        // One identity per request: a retried attempt must not change
        // what the client is told (ADR 0017 — one terminal outcome).
        let ident = StreamIdent {
            id: new_chat_id(),
            model: String::new(), // filled after decode (echo id)
            created: now_unix(),
        };

        // Decode + client-side validation. Everything here fails fast,
        // before any pool interaction — a typo must not cost an upstream
        // turn (OSTD-003/005, TDD API-016).
        let input = match decode_chat_body_with_default_thinking(
            body,
            &self.default_model,
            self.thinking_default_on,
        ) {
            Ok(i) => i,
            Err(e) => return Err(map_error(&parse_error_to_pipeline(e))),
        };
        // Turn planning: full replay, fresh conversation (ADR 0017). Cap
        // errors are client errors surfaced before the pool.
        let plan = match plan_full_replay(&input.msgs, REPLAY_CAP) {
            Ok(p) => p,
            Err(e) => return Err(map_error(&plan_error_to_pipeline(&e))),
        };

        // Model resolution (catalog floor; the live-catalog redirect path
        // is upstream-driven and out of v1 scope — see turnplan docs).
        let norm =
            crate::models::normalize_model(&input.requested_model, &self.default_model, None)
                .map_err(|e| {
                    map_error(&PipelineError::ModelNotFound {
                        requested: e.requested,
                    })
                })?;
        // Echo id: the id the client named, or the resolved model when
        // the request said "auto"/empty.
        let model_echo = if input.requested_model.trim().is_empty()
            || input.requested_model.trim().eq_ignore_ascii_case("auto")
        {
            norm.model.clone()
        } else {
            input.requested_model.trim().to_string()
        };
        let mut ident = ident;
        ident.model = model_echo.clone();

        // Client-side char caps BEFORE any pool interaction (SDD §2.8,
        // TDD API-016: zero upstream requests recorded). The planner
        // splices every system message into the first query, so a
        // client's system prompt spends the same budget (ADR 0015).
        let cap = max_query_chars(&norm.model);
        let (composed, overhead) = composed_first_query_len(&input.msgs);
        if composed > cap {
            return Err(map_error(&PipelineError::QueryTooLong {
                limit: cap,
                actual: composed,
                model: norm.model.clone(),
                overhead,
            }));
        }

        let mut excluded: Vec<String> = Vec::new();
        let mut last_err: Option<PipelineError> = None;

        // Original + 1 retry (SDD §7.2).
        for attempt in 0..2 {
            let lease = match acquire(&self.pool, &excluded).await {
                Ok(l) => l,
                Err(e) => {
                    // Pool-level failure is pre-first-byte: mapped error,
                    // no upstream call happened on any account.
                    return Err(map_error(&pool_error_to_pipeline(&e)));
                }
            };

            // Fresh conversation ids every attempt (SDD §4.2 — a retried
            // attempt must not inherit a half-built upstream thread).
            let conversation_id = random_hex32();

            match self
                .execute(
                    &input,
                    &lease,
                    &ident,
                    &norm,
                    &plan.queries,
                    &conversation_id,
                )
                .await
            {
                Ok(answer) => {
                    self.pool.report_success(&lease.id);
                    lease.release();
                    return Ok(answer);
                }
                Err(err) => {
                    let retry_available = attempt == 0;
                    match classify(&err) {
                        Class::Auth => {
                            // 1 transparent refresh-retry, same account
                            // (SDD §7.1, TDD RENEW-005). A second auth
                            // failure disables the account (RENEW-006).
                            match self.refresh_session(&lease.id).await {
                                RefreshOutcome::Refreshed => {
                                    let r = self
                                        .execute(
                                            &input,
                                            &lease,
                                            &ident,
                                            &norm,
                                            &plan.queries,
                                            &conversation_id,
                                        )
                                        .await;
                                    match r {
                                        Ok(answer) => {
                                            self.pool.report_success(&lease.id);
                                            lease.release();
                                            return Ok(answer);
                                        }
                                        Err(e2) => {
                                            if e2.is_auth_expired() {
                                                self.pool.report_disabled(
                                                    &lease.id,
                                                    "session dead after refresh",
                                                    false,
                                                );
                                            }
                                            last_err = Some(PipelineError::from(&e2));
                                        }
                                    }
                                }
                                RefreshOutcome::Failed(rerr) => {
                                    // Dead passToken → permanent; every
                                    // other login failure (captcha demand,
                                    // OTP pending, protocol break) →
                                    // recoverable disable (Go
                                    // isPermanentDead; a disabled account
                                    // needs an admin flip). The client
                                    // sees the 401 (RENEW-006).
                                    let permanent = matches!(rerr, LoginError::PassTokenDead(_));
                                    self.pool.report_disabled(
                                        &lease.id,
                                        &rerr.to_string(),
                                        permanent,
                                    );
                                    last_err = Some(PipelineError::from(&err));
                                }
                            }
                            lease.release();
                            return Err(map_error(last_err.as_ref().unwrap()));
                        }
                        Class::RiskControl => {
                            // 451/461: disabled (NOT permanent — the signal's
                            // form is unverified, REQ-10), no retry, fail
                            // closed (SDD §4.5.2).
                            let ev = risk_event(&err, &norm.model);
                            self.pool
                                .report_risk_control(&lease.id, &err.to_string(), ev);
                            lease.release();
                            return Err(map_error(&PipelineError::from(&err)));
                        }
                        Class::Upstream429 => {
                            // No retry for this request; cooldown ladder
                            // (SDD §7.1, TDD ERRMAP-006).
                            self.pool.report_429(&lease.id);
                            lease.release();
                            return Err(map_error(&PipelineError::from(&err)));
                        }
                        Class::ModelRejected => {
                            // Deterministic: retrying can only fail again
                            // (web-surface-reverse §11.1). The catalog
                            // disagrees with upstream — surfaced, not
                            // silently retried.
                            lease.release();
                            return Err(map_error(&PipelineError::from(&err)));
                        }
                        Class::Busy => {
                            lease.release();
                            if !retry_available {
                                return Err(map_error(&PipelineError::from(&err)));
                            }
                            // Transient platform load, not a dead account:
                            // retry once after the short backoff.
                            tokio::time::sleep(BUSY_RETRY_BACKOFF).await;
                            continue;
                        }
                        Class::GatewayDeadline => {
                            // Our own bound fired while the account was
                            // serving: no cooldown, no retry — the client
                            // gets a 504 naming the knob (ADR 0019/0036).
                            lease.release();
                            return Err(map_error(&PipelineError::from(&err)));
                        }
                        Class::Content => {
                            // Content-level: fail, no pool action (SDD §7.1).
                            lease.release();
                            return Err(map_error(&PipelineError::from(&err)));
                        }
                        Class::Empty => {
                            // 1 retry, fresh ids, full replay (TDD
                            // EMPTY-001).
                            lease.release();
                            if !retry_available {
                                return Err(map_error(&PipelineError::from(&err)));
                            }
                            last_err = Some(PipelineError::from(&err));
                            continue;
                        }
                        Class::Sick => {
                            // Upstream sick: cooldown on its own class
                            // ladder, retry once on another account
                            // (SDD §4.5.1). A fresh attempt mints a fresh
                            // conversation (SDD §4.2).
                            self.report_failure(&lease.id, &err);
                            excluded.push(lease.id.clone());
                            lease.release();
                            last_err = Some(PipelineError::from(&err));
                            continue;
                        }
                    }
                }
            }
        }
        // Retries exhausted: the last upstream failure, mapped.
        Err(map_error(
            last_err.as_ref().unwrap_or(&PipelineError::NoAccounts),
        ))
    }

    /// execute: run the turn plan on the leased account and deliver the
    /// answer (Go `chatPipeline.execute`). Only the final turn is
    /// delivered; earlier turns rebuild the upstream conversation with
    /// `usage` summed across all of them (SDD §5.4).
    async fn execute(
        &self,
        input: &ChatInput,
        lease: &LeaseHandle,
        ident: &StreamIdent,
        norm: &crate::models::NormalizedModel,
        queries: &[String],
        conversation_id: &str,
    ) -> Result<ChatAnswer, UpstreamChatError> {
        let (cookie, ph) = self.creds(&lease.id).await?;

        let upstream = ChatClient::new(self.login.for_egress(&lease.egress));
        let mut usage_sum = crate::openai::Usage::default();
        let mut last_dialog_id = String::new();

        for (qi, query) in queries.iter().enumerate() {
            let prev = last_dialog_id.clone();
            let req = UpstreamChatRequest {
                query: query.clone(),
                model: norm.model.clone(),
                conversation_id: conversation_id.to_string(),
                previous_dialog_id: prev,
                enable_thinking: input.thinking,
                web_search_status: input.search.clone(),
                temperature: input.temperature,
                top_p: input.top_p,
                idle_timeout: lease.ttl(),
            };
            let res = upstream.chat(&cookie, &ph, &req).await?;
            usage_sum = add_usage(usage_sum, map_usage(&res.usage));
            if !res.dialog_id.is_empty() {
                last_dialog_id = res.dialog_id.clone();
            }
            let is_final = qi == queries.len() - 1;
            if !is_final {
                continue;
            }
            // The one delivery point (SDD §5.4). Zero deltas on the final
            // turn is EmptyResponse — retryable with fresh ids upstream.
            if res.content.is_empty() && res.reasoning.is_empty() {
                return Err(UpstreamChatError::EmptyResponse);
            }
            // include_citations:false → the search still ran, the search
            // results are just not rendered (M19).
            let cits: Vec<_> = if input.citations {
                res.citations.clone()
            } else {
                Vec::new()
            };
            // Thinking off: the client asked for an answer, not the
            // model's working (TDD API-010 companion rule).
            let reasoning = if input.thinking {
                res.reasoning.clone()
            } else {
                String::new()
            };
            return if input.stream {
                let sse = render_stream(ident, &res.content, &reasoning, usage_sum, &cits);
                Ok(ChatAnswer::Sse(sse))
            } else {
                let comp = shape_completion(ident, &res.content, &reasoning, usage_sum, &cits);
                serde_json::to_value(&comp)
                    .map(ChatAnswer::Completion)
                    .map_err(|e| UpstreamChatError::Transport(format!("completion encode: {e}")))
            };
        }
        // Unreachable: plan_full_replay always yields ≥1 query.
        Err(UpstreamChatError::EmptyResponse)
    }

    /// creds: resolve the cookie + ph an account chats with — stored
    /// session first, STS renewal from a stored passToken second, password
    /// login last — persisting whatever was fetched (SDD §4.7, Go
    /// `chatPipeline.creds`).
    async fn creds(&self, account_id: &str) -> Result<(String, String), UpstreamChatError> {
        let acct = self.load_account(account_id).ok_or_else(|| {
            UpstreamChatError::Transport(format!("account {account_id} not found in store"))
        })?;
        if let (Some(cookie), Some(ph)) = (acct.cookie.as_deref(), acct.ph.as_deref()) {
            if !cookie.is_empty() && !ph.is_empty() {
                return Ok((cookie.to_string(), ph.to_string()));
            }
        }
        let client = self.client_for(&acct);
        // Renewal first when a passToken exists (no human interaction);
        // a failed renewal falls back to password login when creds exist.
        let sess =
            if let (Some(uid), Some(pt)) = (acct.user_id.as_deref(), acct.pass_token.as_deref()) {
                match client.renew(uid, pt).await {
                    Ok(sess) => sess,
                    Err(_) => self.fallback_login(&client, &acct).await?,
                }
            } else {
                self.fallback_login(&client, &acct).await?
            };
        self.persist_session(account_id, &sess);
        Ok((sess.cookie, sess.ph))
    }

    async fn fallback_login(
        &self,
        client: &LoginClient,
        acct: &StoredAccount,
    ) -> Result<Session, UpstreamChatError> {
        let (Some(phone), Some(password)) = (acct.phone.as_deref(), acct.password.as_deref())
        else {
            return Err(UpstreamChatError::Transport(format!(
                "account {} has no stored session and no login credentials",
                acct.id
            )));
        };
        client
            .login(phone, password)
            .await
            .map_err(|e| UpstreamChatError::Transport(format!("on-demand login: {e}")))
    }

    /// refreshSession: one transparent STS renewal for the auth-expired
    /// retry (SDD §2.3/§7.1, Go `Server.refreshSession`).
    async fn refresh_session(&self, account_id: &str) -> RefreshOutcome {
        let Some(acct) = self.load_account(account_id) else {
            return RefreshOutcome::Failed(LoginError::Transport(format!(
                "account {account_id} not found in store"
            )));
        };
        let (Some(uid), Some(pt)) = (acct.user_id.as_deref(), acct.pass_token.as_deref()) else {
            return RefreshOutcome::Failed(LoginError::Transport(format!(
                "account {account_id} has no stored passToken to renew"
            )));
        };
        let client = self.client_for(&acct);
        match client.renew(uid, pt).await {
            Ok(sess) => {
                self.persist_session(account_id, &sess);
                RefreshOutcome::Refreshed
            }
            Err(e) => RefreshOutcome::Failed(e),
        }
    }

    /// The login client for an account: its egress, presenting its stored
    /// device identity (Go `WithDevice(lease/…, dev)`).
    fn client_for(&self, acct: &StoredAccount) -> LoginClient {
        let base = self.login.for_egress(&acct.egress);
        match acct
            .device_json
            .as_deref()
            .and_then(|dj| serde_json::from_str::<Device>(dj).ok())
        {
            Some(dev) if !dev.is_empty() => base.with_device(&dev),
            _ => base,
        }
    }

    fn persist_session(&self, account_id: &str, sess: &Session) {
        let device_json = serde_json::to_string(&sess.device).ok();
        let _ = self.store.apply_session(
            account_id,
            Some(&sess.user_id),
            Some(&sess.pass_token),
            Some(&sess.cookie),
            Some(&sess.ph),
            device_json.as_deref(),
        );
    }

    fn load_account(&self, id: &str) -> Option<StoredAccount> {
        self.store
            .load_accounts()
            .ok()?
            .into_iter()
            .find(|a| a.id == id)
    }

    /// reportFailure under the right class (Go failure_class.go
    /// `classifyPoolFailure`): an upstream answer takes the 5xx ladder
    /// with its status; a transport-shaped failure (no HTTP response)
    /// takes the transport ladder — the fix that stopped transport
    /// sharing the 5xx escalation (TDD POOLATTR-001).
    fn report_failure(&self, account_id: &str, err: &UpstreamChatError) {
        match err {
            UpstreamChatError::Upstream(ue) => {
                if ue.status == 429 || (500..=599).contains(&ue.status) {
                    self.pool.report_failure(
                        account_id,
                        crate::pool::FailureClass::Upstream5xx,
                        ue.status,
                    );
                }
            }
            UpstreamChatError::Transport(_) | UpstreamChatError::IdleStall => {
                self.pool.report_transport(account_id);
            }
            _ => {}
        }
    }
}

/// Failure classification (Go chat.go switch helpers + failure_class.go):
/// which pool action a failure earns, and whether the ladder retries.
enum Class {
    /// 451/461: risk control — disabled (recoverable), no retry.
    RiskControl,
    /// Upstream 429: cooldown ladder, no retry.
    Upstream429,
    /// The channel refuses this model id: deterministic, no retry.
    ModelRejected,
    /// Busy (transient platform load): retry after backoff.
    Busy,
    /// Our own budget/lease-TTL fired: no pool action, no retry (ADR
    /// 0019/0036).
    GatewayDeadline,
    /// Content-level (filter/stream-shape): no pool action, no retry.
    Content,
    /// Empty response: retry with fresh ids (TDD EMPTY-001).
    Empty,
    /// Auth-expired: transparent refresh retry.
    Auth,
    /// Transport/upstream-sick: cooldown + retry on another account.
    Sick,
}

/// classify (Go chat.go `isAuthErr`/`isRiskControl`/`isAccountDead`/
/// `isUpstream429`/`isContentErr`/`isGatewayDeadline` ordering). Depends
/// on the error alone.
fn classify(err: &UpstreamChatError) -> Class {
    use UpstreamChatError as E;
    match err {
        E::Upstream(ue) => match ue.status {
            451 | 461 => Class::RiskControl,
            429 => Class::Upstream429,
            401 => Class::Auth,
            _ => Class::Sick,
        },
        E::AuthExpired(_) => Class::Auth,
        E::InBand(InBandError::Busy(_)) => Class::Busy,
        E::InBand(InBandError::ModelRejected(_)) => Class::ModelRejected,
        E::InBand(InBandError::ContentFilter { .. }) | E::InBand(InBandError::Stream(_)) => {
            Class::Content
        }
        E::EmptyResponse => Class::Empty,
        // Silence before any byte: our patience, not the account's (ADR
        // 0036) — mapped upstream to the 504 chat_timeout, no pool action,
        // no retry.
        E::SilentUpstream { .. } => Class::GatewayDeadline,
        E::Transport(_) | E::IdleStall => Class::Sick,
    }
}

/// channelName (chat.go): names the upstream chat channel for the risk
/// event / logs.
fn channel_name(model: &str) -> &'static str {
    if crate::models::is_fastchat_model(model) {
        "fastchat/ultraspeed"
    } else {
        "normal"
    }
}

/// The risk event a 451/461 records: model + channel + status + a body
/// digest — never the body itself (INV-4, Go RiskEvent fields).
fn risk_event(err: &UpstreamChatError, model: &str) -> RiskEvent {
    let status = match err {
        UpstreamChatError::Upstream(ue) => ue.status,
        _ => 0,
    };
    RiskEvent {
        model: Some(model.to_string()),
        channel: Some(channel_name(model).to_string()),
        status,
        ..Default::default()
    }
}

async fn acquire(pool: &Pool, excluded: &[String]) -> Result<LeaseHandle, PoolError> {
    let refs: Vec<&str> = excluded.iter().map(String::as_str).collect();
    pool.acquire(&refs).await
}

fn parse_error_to_pipeline(e: ParseError) -> PipelineError {
    match e {
        ParseError::MalformedRequest | ParseError::MalformedContent => {
            PipelineError::MalformedRequest
        }
        ParseError::UnsupportedReasoningEffort => PipelineError::UnsupportedReasoningEffort,
        ParseError::UnsupportedSearchContextSize => PipelineError::UnsupportedSearchContextSize,
        ParseError::UnsupportedSearchStatus => PipelineError::UnsupportedSearchStatus,
        ParseError::ImageUrl(reason) => PipelineError::ImageUrl { reason },
    }
}

fn plan_error_to_pipeline(e: &PlanError) -> PipelineError {
    match e {
        PlanError::NoUserMessage => PipelineError::NoUserMessage,
        PlanError::TooManyTurns { cap, .. } => PipelineError::TooManyTurns { max: *cap },
    }
}

/// Pool-level failure → PipelineError (retry-after from the pool error's
/// own deadline math).
fn pool_error_to_pipeline(e: &PoolError) -> PipelineError {
    match e {
        PoolError::NoAccounts => PipelineError::NoAccounts,
        PoolError::Parked { .. } => PipelineError::PoolParked {
            retry_after_seconds: e.retry_after_seconds().unwrap_or(1),
        },
        PoolError::QueueTimeout { .. } => PipelineError::PoolBusy {
            retry_after_seconds: e.retry_after_seconds().unwrap_or(1),
        },
        PoolError::Cancelled => PipelineError::UpstreamFailure {
            detail: "request cancelled".into(),
        },
    }
}

/// The SSE sequence for one finished turn (Go `streamWriter.finish`
/// order: role chunk → deltas → finish chunk → citations chunk → usage
/// chunk → `[DONE]`; the usage chunk is always emitted — Go does not
/// honor stream_options.include_usage). Reasoning suppression is applied
/// before this call (thinking off), matching the JSON path.
pub fn render_stream(
    ident: &StreamIdent,
    content: &str,
    reasoning: &str,
    usage: crate::openai::Usage,
    cits: &[crate::sse::Citation],
) -> Vec<u8> {
    use crate::openai::{
        render_chunk_frame, render_done_frame, shape_citations_chunk, shape_finish_chunk,
        shape_role_chunk, shape_text_chunk, shape_usage_chunk,
    };
    use crate::thinking::{Piece, PieceKind};
    let mut out = String::new();
    out.push_str(&render_chunk_frame(&shape_role_chunk(ident)));
    // Reasoning pieces first (the splitter's order), then content — the
    // same order the live stream would deliver them.
    if !reasoning.is_empty() {
        let p = Piece {
            kind: PieceKind::Reasoning,
            text: reasoning.to_string(),
        };
        out.push_str(&render_chunk_frame(&shape_text_chunk(ident, &p)));
    }
    if !content.is_empty() {
        let p = Piece {
            kind: PieceKind::Content,
            text: content.to_string(),
        };
        out.push_str(&render_chunk_frame(&shape_text_chunk(ident, &p)));
    }
    out.push_str(&render_chunk_frame(&shape_finish_chunk(ident)));
    if !cits.is_empty() {
        out.push_str(&render_chunk_frame(&shape_citations_chunk(ident, cits)));
    }
    out.push_str(&render_chunk_frame(&shape_usage_chunk(ident, usage)));
    out.push_str(render_done_frame());
    out.into_bytes()
}

#[cfg(test)]
#[path = "chat_tests.rs"]
mod tests;
