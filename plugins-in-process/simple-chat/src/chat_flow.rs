//! The chat pipeline — port of Go `server.go`'s `handleChatCompletions` /
//! `runAttempt` / `deliverStream` / `deliverNonStream`: parse → flatten →
//! prompt cap → image extraction (pre-lease, R4) → the 2-attempt retry
//! ladder → delivery with the commit boundary.
//!
//! Commit boundary in the buffer world: `ChatOutcome::Sse` is one returned
//! buffer, so "client-visible bytes" exist exactly when the function
//! returns an `Ok` outcome. A stream that fails before its first delta
//! frame therefore has NOT committed — the ladder may retry it, which is
//! the property Go's deferred `WriteHeader` provided (M3 OQ #1, answered:
//! the buffer variant preserves the retry boundary; a chunked variant
//! would need a commit callback, deliberately deferred).

use std::sync::Arc;

use oxcost_sdk::ChatOutcome;

use crate::body::CompletionRequest;
use crate::client::WatchedStream;
use crate::errmap::WireError;
use crate::image_fetch::{extract_images, Image};
use crate::lease::Lease;
use crate::oai_response as resp;
use crate::openai::{self, Request as ParsedRequest};
use crate::pool::Pool;
use crate::sse::{Interpreter, StreamError};
use crate::wire::UpstreamError;

/// Bounds the completion retry ladder (gap-analysis R1): attempt 1, one
/// full retry. Go `maxAttempts`.
pub const MAX_ATTEMPTS: u32 = 2;

/// The conservative prompt cap (Go `DefaultMaxPromptChars`): below the
/// upstream's observed ~2.6M hard limit with headroom so a legit-but-long
/// request answers our clean 400 before the upstream's opaque rejection.
pub const DEFAULT_MAX_PROMPT_CHARS: usize = 2_000_000;

/// One chat request flowing through the ladder, already parsed and
/// validated (the Go handler's request-scoped state).
#[derive(Debug)]
pub struct ChatJob {
    pub parsed: ParsedRequest,
    pub prompt: String,
    pub images: Vec<Image>,
}

/// Parse + flatten + cap — everything that happens before the first image
/// fetch or lease acquisition (Go `handleChatCompletions` up to the retry
/// loop).
pub fn prepare(body: &serde_json::Value, max_prompt_chars: usize) -> Result<ChatJob, WireError> {
    let parsed = openai::parse_request(body).map_err(map_parse_error)?;
    if !parsed.stripped.is_empty() {
        // Go logs the report; the report itself never reaches the client.
        tracing::info!(
            fields = parsed.stripped.join(", "),
            "stripped unsupported request fields"
        );
    }
    let prompt = openai::flatten_messages(&parsed.messages);
    if max_prompt_chars > 0 && prompt.len() > max_prompt_chars {
        return Err(WireError::context_length_exceeded(
            prompt.len(),
            max_prompt_chars,
        ));
    }
    Ok(ChatJob {
        parsed,
        prompt,
        images: Vec::new(),
    })
}

/// ParseError → §4 400 row (Go: `err.Error()` under bad_request, except
/// the model row which is a 404).
fn map_parse_error(e: openai::ParseError) -> WireError {
    match &e {
        openai::ParseError::UnknownModel { model, .. } => WireError::model_not_found(model),
        _ => WireError::bad_request(&e.to_string()),
    }
}

/// The pre-lease image fetch (async half of `prepare`; Go resolves images
/// after the prompt cap, still before the retry loop). Deliberately
/// BEFORE lease acquire: an unbounded external fetch must never hold a
/// pool slot (gap-analysis R4).
pub async fn resolve_images(job: &mut ChatJob) -> Result<(), WireError> {
    match extract_images(&job.parsed.messages).await {
        Ok(imgs) => {
            job.images = imgs;
            Ok(())
        }
        Err(e) => {
            // The failure detail (URL host, reason) is a log line, never
            // client-facing (token-leak guard: first-segment only upstream).
            tracing::warn!(error = %e, "image extraction failed");
            Err(WireError::image_fetch_failed())
        }
    }
}

/// One attempt's carry-over state (Go `chatRetry`): the prior lease
/// identity and whether a same-account fallback is permitted. Local to
/// one request; identities never reach logs (redaction is upstream of
/// any tracing call).
#[derive(Default)]
struct ChatRetry {
    identity: String,
    err: Option<String>,
    allow_same_account: bool,
}

/// The outcome of one ladder run.
enum RunResult {
    /// Terminal answer for the client.
    Done(Result<ChatOutcome, WireError>),
    /// Retry allowed (pre-commit failure under the bounded policy).
    Retry(ChatRetry),
}

/// The full chat flow over the pool (Go's handler loop). Returns the
/// client-facing outcome; `WireError` (not bare `PluginError`) so the
/// Retry-After side-channel survives to the host boundary.
pub async fn run_chat(pool: &Arc<Pool>, job: ChatJob) -> Result<ChatOutcome, WireError> {
    let mut previous = ChatRetry::default();
    for attempt in 1..=MAX_ATTEMPTS {
        match run_attempt(pool, &job, attempt, previous).await {
            RunResult::Done(out) => return out,
            RunResult::Retry(next) => previous = next,
        }
    }
    // Unreachable in Go too (the last attempt never returns retry); kept
    // as a fail-safe generic row rather than a panic on a live path.
    Err(WireError::from_stream(&StreamError::Other(
        "ladder exhausted".into(),
    )))
}

/// Go `noReadyAccounts`: the pool's health snapshot, not a transiently
/// full slot — a ready-but-busy account is NOT "no accounts".
fn no_ready_accounts(pool: &Arc<Pool>) -> bool {
    pool.snapshot().iter().all(|s| s.state != "ready")
}

/// One full attempt: lease → token → uploads → session → completion →
/// delivery (Go `runAttempt`).
async fn run_attempt(
    pool: &Arc<Pool>,
    job: &ChatJob,
    attempt: u32,
    previous: ChatRetry,
) -> RunResult {
    // ---- lease (attempt 1 plain; attempt 2 excludes the prior identity) ----
    let lease = match acquire(pool, attempt, &previous).await {
        Ok(l) => l,
        Err(out) => return RunResult::Done(Err(out)),
    };

    // ---- token ----
    let tok = match lease.token().await {
        Ok(t) => t,
        Err(e) => {
            lease.note_error(&e);
            return RunResult::Done(Err(WireError::from_upstream(&e, lease.park_until())));
        }
    };

    // ---- image uploads ----
    let mut ref_file_ids: Vec<String> = Vec::with_capacity(job.images.len());
    for img in &job.images {
        match lease
            .client()
            .upload_image_and_wait(&tok, img.data.clone(), &format!("image.{}", img.ext))
            .await
        {
            Ok(id) => ref_file_ids.push(id),
            Err(e) => {
                lease.note_error(&e);
                tracing::warn!(error = %e, "image upload failed");
                return RunResult::Done(Err(WireError::upload_failed()));
            }
        }
    }

    // ---- session ----
    let session_id = match lease.create_session().await {
        Ok(s) => s,
        Err(e) => {
            lease.note_error(&e);
            if attempt < MAX_ATTEMPTS && e.is_retryable() {
                return RunResult::Retry(ChatRetry {
                    identity: lease.identity().to_string(),
                    err: Some(e.to_string()),
                    allow_same_account: true,
                });
            }
            return RunResult::Done(Err(WireError::from_upstream(&e, lease.park_until())));
        }
    };

    // ---- completion ----
    let up_req = CompletionRequest {
        session_id,
        prompt: job.prompt.clone(),
        ref_file_ids,
        thinking_disabled: !job.parsed.thinking_enabled,
        search_enabled: job.parsed.search_enabled,
        temperature: job.parsed.temperature,
        top_p: job.parsed.top_p,
        max_tokens: job.parsed.max_tokens,
    };
    let stream = match lease.completion(&up_req).await {
        Ok(s) => s,
        Err(e) => {
            lease.note_error(&e);
            tracing::warn!(attempt, error = %e, "completion call failed");
            if attempt < MAX_ATTEMPTS && e.is_retryable() {
                return RunResult::Retry(ChatRetry {
                    identity: lease.identity().to_string(),
                    err: Some(e.to_string()),
                    allow_same_account: true,
                });
            }
            return RunResult::Done(Err(WireError::from_upstream(&e, lease.park_until())));
        }
    };

    // ---- delivery ----
    if job.parsed.stream {
        deliver_stream(stream, job, attempt, lease).await
    } else {
        deliver_non_stream(stream, job, attempt, lease).await
    }
}

/// The acquire step with the Go ladder's NoAlternative handling: a retry
/// prefers a DISTINCT identity; the same-account fallback is allowed only
/// for bounded, non-account-specific failures, and only while a ready
/// account still exists.
async fn acquire(pool: &Arc<Pool>, attempt: u32, previous: &ChatRetry) -> Result<Lease, WireError> {
    let got = if attempt == 1 {
        pool.acquire_with_wait().await
    } else {
        let r = pool.acquire_with_wait_excluding(&previous.identity).await;
        match r {
            Err(crate::pool::PoolError::NoAlternative) if previous.allow_same_account => {
                if no_ready_accounts(pool) {
                    return Err(WireError::no_eligible_for_retry());
                }
                pool.acquire_with_wait().await
            }
            Err(crate::pool::PoolError::NoAlternative) => {
                // Go re-renders the ORIGINAL pre-response error, not the
                // pool's no-alternative signal.
                return Err(WireError::from_stream(&StreamError::Other(
                    previous
                        .err
                        .clone()
                        .unwrap_or_else(|| "no alternative account".into()),
                )));
            }
            other => other,
        }
    };
    match got {
        Ok((lease, _waited)) => Ok(lease),
        Err(e) => {
            // A safe same-account retry cannot proceed if its account was
            // removed or parked in the meantime — distinct from a
            // capacity-bound ready account (which stays pool_busy/429).
            if attempt > 1 && previous.allow_same_account && no_ready_accounts(pool) {
                return Err(WireError::no_eligible_for_retry());
            }
            Err(WireError::from_pool(&e))
        }
    }
}

/// Go `deliverStream`: bridge the upstream SSE into OpenAI chunk frames
/// in a buffer. Commit = first delta frame emitted into the buffer; a
/// pre-commit failure retries, a post-commit one terminates with an error
/// frame + honest final chunk.
async fn deliver_stream(
    mut stream: WatchedStream,
    job: &ChatJob,
    attempt: u32,
    lease: Lease,
) -> RunResult {
    let mut interp = Interpreter::new();
    let id = random_completion_id();
    let mut out: Vec<u8> = Vec::new();
    let mut committed = false;
    let mut first = true;
    let thinking = job.parsed.thinking_enabled;

    // Read to completion, feeding the interpreter; deltas are emitted
    // from per-cycle snapshots (the Rust port walks state instead of
    // installing Go's SetOnDelta closures).
    let mut read_err: Option<UpstreamError> = None;
    let mut seen_text = 0usize;
    let mut seen_reasoning = 0usize;
    loop {
        let chunk = stream.next().await;
        match chunk {
            Some(Ok(bytes)) => {
                interp.feed(&String::from_utf8_lossy(&bytes));
            }
            Some(Err(e)) => {
                read_err = Some(e);
                break;
            }
            None => break,
        }
        // Emit deltas that arrived since the last cycle, then check the
        // terminal state BEFORE the EOF break (Go checks state between
        // read and break so a final-chunk-plus-EOF error still surfaces).
        let st = interp.snapshot();
        emit_new_deltas(
            &mut out,
            &id,
            &st.text,
            &mut seen_text,
            &st.reasoning,
            &mut seen_reasoning,
            thinking,
            &mut first,
            &mut committed,
        );
        if st.err.is_some() || st.finished {
            break;
        }
    }
    // A last-cycle snapshot after EOF: deltas carried by the final chunk.
    {
        let st = interp.snapshot();
        emit_new_deltas(
            &mut out,
            &id,
            &st.text,
            &mut seen_text,
            &st.reasoning,
            &mut seen_reasoning,
            thinking,
            &mut first,
            &mut committed,
        );
    }
    let st = interp.snapshot().clone();

    // ---- terminal state (Go's switch) ----
    if let Some(se) = &st.err {
        lease.note_stream_error(se);
        if !committed {
            // Pre-commit failure: the ladder may retry.
            if attempt < MAX_ATTEMPTS && is_client_retryable_stream_error(se) {
                return RunResult::Retry(ChatRetry {
                    identity: lease.identity().to_string(),
                    err: Some(se.to_string()),
                    allow_same_account: false,
                });
            }
            // Terminal pre-commit: answer with a real error, not an empty
            // 200 (the buffer form keeps the connection clean here).
            return RunResult::Done(Err(WireError::from_stream(se)));
        }
        // Post-commit: error frame + final chunk carrying the upstream's
        // own finish_reason (never "stop"), then [DONE].
        if se.is_content_filter() {
            out.extend_from_slice(&resp::sse_frame(&resp::error_body(
                "content filtered by upstream",
                "invalid_request_error",
                "content_filter",
            )));
        } else {
            out.extend_from_slice(&resp::sse_frame(&resp::error_body(
                "upstream stream error",
                "upstream_error",
                "stream_error",
            )));
        }
        if let StreamError::Hint { finish_reason, .. } = se {
            if !finish_reason.is_empty() {
                out.extend_from_slice(&resp::sse_frame(&resp::final_chunk(
                    &id,
                    openai::MODEL_NAME,
                    st.usage.total_tokens,
                    finish_reason,
                    &st.search_results,
                )));
            }
        }
        out.extend_from_slice(&resp::sse_done_frame());
        return RunResult::Done(Ok(ChatOutcome::Sse(out)));
    }

    if let Some(rerr) = read_err {
        lease.note_error(&rerr);
        // Transport cut: honest stream_error either way (never lie with
        // finish_reason "stop").
        if !committed {
            if attempt < MAX_ATTEMPTS && rerr.is_retryable() {
                return RunResult::Retry(ChatRetry {
                    identity: lease.identity().to_string(),
                    err: Some(rerr.to_string()),
                    allow_same_account: true,
                });
            }
            return RunResult::Done(Err(WireError::stream_transport()));
        }
        out.extend_from_slice(&resp::sse_frame(&resp::error_body(
            "upstream stream terminated early",
            "upstream_error",
            "stream_error",
        )));
        out.extend_from_slice(&resp::sse_done_frame());
        return RunResult::Done(Ok(ChatOutcome::Sse(out)));
    }

    // Empty-but-clean stream before any client byte: one re-run (R1).
    // THINK-only output is NOT empty — reasoning counts. Once committed
    // (including a suppressed THINK delta), an empty finish is delivered
    // honestly, never retried.
    if st.text.is_empty() && st.reasoning.is_empty() && !committed && attempt < MAX_ATTEMPTS {
        return RunResult::Retry(ChatRetry {
            identity: lease.identity().to_string(),
            err: None,
            allow_same_account: true,
        });
    }

    // Stream ended without deltas (state accumulated without emission —
    // defensive parity with Go's `first && st.Text != ""` arm).
    if first && !st.text.is_empty() {
        out.extend_from_slice(&resp::sse_frame(&resp::stream_chunk(
            &id,
            openai::MODEL_NAME,
            &st.text,
            "assistant",
        )));
    }
    out.extend_from_slice(&resp::sse_frame(&resp::final_chunk(
        &id,
        openai::MODEL_NAME,
        st.usage.total_tokens,
        "stop",
        &st.search_results,
    )));
    out.extend_from_slice(&resp::sse_done_frame());
    RunResult::Done(Ok(ChatOutcome::Sse(out)))
}

/// Emit frames for the newly-accumulated text/reasoning since the last
/// cycle (the delta walk): one chunk per grow event, first chunk carries
/// the assistant role.
#[allow(clippy::too_many_arguments)]
fn emit_new_deltas(
    out: &mut Vec<u8>,
    id: &str,
    text: &str,
    seen_text: &mut usize,
    reasoning: &str,
    seen_reasoning: &mut usize,
    thinking: bool,
    first: &mut bool,
    committed: &mut bool,
) {
    if *seen_reasoning < reasoning.len() {
        let delta = &reasoning[*seen_reasoning..];
        *seen_reasoning = reasoning.len();
        if thinking && !delta.is_empty() {
            let role = if *first { "assistant" } else { "" };
            *first = false;
            *committed = true;
            out.extend_from_slice(&resp::sse_frame(&resp::reasoning_stream_chunk(
                id,
                openai::MODEL_NAME,
                delta,
                role,
            )));
        }
    }
    if *seen_text < text.len() {
        let delta = &text[*seen_text..];
        *seen_text = text.len();
        if !delta.is_empty() {
            let role = if *first { "assistant" } else { "" };
            *first = false;
            *committed = true;
            out.extend_from_slice(&resp::sse_frame(&resp::stream_chunk(
                id,
                openai::MODEL_NAME,
                delta,
                role,
            )));
        }
    }
}

/// Go `deliverNonStream`: consume the whole stream, then decide —
/// retryable failure or empty output runs once more; other failures map
/// to §6 rows. No client bytes exist before the decision (inherent in the
/// buffer form).
async fn deliver_non_stream(
    mut stream: WatchedStream,
    job: &ChatJob,
    attempt: u32,
    lease: Lease,
) -> RunResult {
    let mut interp = Interpreter::new();
    let mut read_err: Option<UpstreamError> = None;
    loop {
        let chunk = stream.next().await;
        match chunk {
            Some(Ok(bytes)) => {
                interp.feed(&String::from_utf8_lossy(&bytes));
            }
            Some(Err(e)) => {
                read_err = Some(e);
                break;
            }
            None => break,
        }
        let st = interp.snapshot();
        if st.err.is_some() || st.finished {
            break;
        }
    }
    let st = interp.snapshot().clone();

    if let Some(se) = &st.err {
        lease.note_stream_error(se);
        if attempt < MAX_ATTEMPTS && is_client_retryable_stream_error(se) {
            return RunResult::Retry(ChatRetry {
                identity: lease.identity().to_string(),
                err: Some(se.to_string()),
                allow_same_account: false,
            });
        }
        return RunResult::Done(Err(WireError::from_stream(se)));
    }

    if let Some(rerr) = read_err {
        lease.note_error(&rerr);
        // Transport cut mid-stream: partial output would look like a
        // successful truncated completion — retry instead.
        if attempt < MAX_ATTEMPTS && rerr.is_retryable() {
            return RunResult::Retry(ChatRetry {
                identity: lease.identity().to_string(),
                err: Some(rerr.to_string()),
                allow_same_account: true,
            });
        }
        return RunResult::Done(Err(WireError::stream_transport()));
    }

    // Empty output with no error: one re-run before returning empty. A
    // reasoning-only completion is a valid answer, not a blip.
    if st.text.is_empty() && st.reasoning.is_empty() && attempt < MAX_ATTEMPTS {
        return RunResult::Retry(ChatRetry {
            identity: lease.identity().to_string(),
            err: None,
            allow_same_account: true,
        });
    }

    let reasoning = if job.parsed.thinking_enabled {
        st.reasoning.as_str()
    } else {
        ""
    };
    RunResult::Done(Ok(ChatOutcome::Json(resp::completion_response(
        &random_completion_id(),
        openai::MODEL_NAME,
        reasoning,
        &st.text,
        st.usage.total_tokens,
        "stop",
        &st.search_results,
    ))))
}

/// Go `isClientRetryableStreamError`: the parallel-generation limit only.
fn is_client_retryable_stream_error(err: &StreamError) -> bool {
    err.is_parallel_limit()
}

/// `chatcmpl-` + 24 hex chars (Go `randomID`).
fn random_completion_id() -> String {
    use rand::RngCore;
    let mut b = [0u8; 12];
    rand::thread_rng().fill_bytes(&mut b);
    format!("chatcmpl-{}", hex::encode(b))
}

#[cfg(test)]
mod tests;
