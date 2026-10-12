# TASK M3 — host-sdk: in-process plugin layer (the SDK)

> Executor: Claude Code CLI, dispatched by the maintainer's orchestrator (Hermes).
> Working directory: `/root/oxcost`, branch `m3-sdk-layer` (create from main).
> Model: `GLM-5.3-Flash` via `--model`.
>
> **Language**: think/write in English (code, comments, commits, docs).
> Maintainer conversation is Chinese — not your concern here.

## 0. Mission

Implement the **host-sdk plugin form** declared in contract §10: Rust plugins
compiled INTO the host binary as first-class citizens. This is the foundation
three upcoming SDK plugins build on (TokenHarbor, mimo-rust, simple-chat).
Also apply two small contract revisions coming out of M2's alignment report.

## 1. Design requirements

### 1.1 New crate: `crates/oxcost-sdk`

- Public trait `OxcostPlugin` (name final up to you) — the ENTIRE surface a
  plugin author touches. Sketch:
  ```rust
  pub trait OxcostPlugin: Send + Sync {
      fn manifest(&self) -> &SdkManifest;         // id, name, version, capabilities
      fn models(&self) -> Vec<ModelDescriptor>;   // what this plugin serves
      fn chat(&self, req: ChatRequest, ctx: RequestContext)
          -> BoxFuture<'_, ChatOutcome>;          // stream or non-stream
      fn health(&self) -> HealthStatus;
      // web_search/audio/...: capability-gated default methods (unsupported
      // by default). Do NOT implement them now — reserved hooks only.
  }
  ```
- `SdkManifest`: same semantics as plugin.toml's id/name/capabilities/model
  prefix rules (contract §3) — but declared in code, not TOML. The
  `插件名/模型名` prefix rule and capability whitelist apply IDENTICALLY to
  SDK plugins (single contract, four forms — that's the whole point).
- Registration: explicit registry function, e.g. `oxcost_sdk::Registry::
  register(plugin: Arc<dyn OxcostPlugin>)` called from host main. NO
  inventory/linkme magic — explicit is debuggable and sufficient.

### 1.2 Host integration (`oxcost-core` + `oxcost` bin)

- Internal `PluginHandle` enum: `External(ExternalPluginClient)` (existing
  M2 path) | `Sdk(Arc<dyn OxcostPlugin>)`. Routing/auth/aggregation/
  error-mapping layers stay UNCHANGED — both forms unify behind this handle.
- Model catalog aggregation must include SDK plugins (merged view, same
  `plugin/model` namespace).
- A tiny built-in **sample SDK plugin** (e.g. `sample-echo`: deterministic
  canned chat responses) for tests — NOT shipped as a real feature, marked
  clearly, used by host tests to exercise the SDK path offline.
- Host-side tests: SDK plugin visible in /v1/models, chat round-trip through
  the axum server, health reflected in /healthz, unknown-model 404, auth 401.

### 1.3 Contract revisions (docs/PLUGIN_CONTRACT.md)

1. §4 error type spelling: `invalid_request` → `invalid_request_error`
   (match OpenAI's actual strings — M2 alignment finding); pin exact type
   strings (`invalid_request_error`, `api_error`, `rate_limit_error`,
   `model_not_found`... align with what mimo2api+OpenAI really emit). Add
   the `param` field to the §4 body sketch.
2. §10: replace the "roadmap" wording with the ACTUAL spec of the SDK form
   implemented here (trait surface summary, registration, capability gating,
   manifest-in-code vs TOML-in-sidecar, single namespace rule).

### 1.4 Explicitly OUT of scope (do not build)

- Dynamic model-catalog TTL refresh machinery (M4).
- Load balancing across candidates, circuit breaker (M4/M5) — keep the
  single-candidate M2 routing as-is, but don't paint into a corner: the
  catalog data structure should make "multiple candidates per model"
  representable (a Vec, not an Option).
- web_search/audio/etc. trait methods beyond default unsupported stubs.
- Any new external protocol, any Anthropic endpoints.
- Touching /root/mimo2api/**, /root/th-recon/**, /root/sc-recon/**.

## 2. Quality gates (same as M1/M2, all mandatory)

`cargo build --workspace` / `cargo clippy --workspace --all-targets -- -D
warnings` (zero warnings) / `cargo fmt --check` / `cargo test --workspace`
(all green). Files < 800 lines. Offline tests only (the sample plugin IS the
offline fixture; do not call any network upstream in tests).

## 3. Commits & process

- Branch `m3-sdk-layer` off main. Conventional commits, atomic.
- Suggested sequence: contract revisions → oxcost-sdk crate + sample plugin
  (with tests) → host integration (handle enum + catalog merge + server
  tests) → gates → docs touch-up if behavior drifted.
- Do NOT merge into main; leave that to the maintainer.
- Do NOT create any files with secrets. No network calls to real upstreams.

## 4. Final summary (English)

Crate layout, trait signature as implemented, how host unifies forms, test
counts per crate, any deviations from this brief (with justification),
open questions for the three plugin authors.
