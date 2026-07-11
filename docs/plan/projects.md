# miroxy Project Plan — Incremental Delivery

> **Core rule:** every story ends in a **runnable binary**.  
> Stub what you must; never leave a story in a state where `go run ./cmd/miroxy` fails or tests regress.  
> Each story is a shippable increment — not a scaffold waiting for the next story to activate it.

---

## Phase 1 — Pipeline Seam (v1.x)

**Epic description:**  
Refactor `server.go` from two hardcoded retry loops into a `Pipeline.Run(c)` dispatch without
changing any externally-observable behavior. All 50 existing tests must pass at every story boundary.
The epic ends when Claude Code can run a 10-turn agentic session through miroxy against a real
Gemini key without a single error.

**Epic gate:** E2E 10-turn agentic session completes; `go test ./...` green throughout; retry SLA < 500 ms.

---

### Story 1.1 — Pipeline MVP: running with stub plugins

**Title:** Minimum working pipeline — UpstreamExecutor wired, all 50 tests green

**Description:**  
Cut the pipeline seam at the smallest possible surface. Define the `Plugin` / `Handler` /
`LLMContext` / `Pipeline` interfaces. Wire `handleMessages` to call `Pipeline.Run(c)` instead
of branching to `handleNonStream` / `handleStream`. The only real plugin is `UpstreamExecutor`
— it contains the existing retry loops **verbatim**, zero logic change. Auth stays as the
existing `authMW` HTTP middleware (not yet a plugin). Router is a stub: `KeywordBackend`
directly calling `cfg.LookupModel`, no plugin abstraction yet.

At the end of this story miroxy starts, proxies Gemini traffic, and all 50 tests pass.
Everything else is defined but empty — this is the skeleton the next stories will fill in.

**Deliverables:**
- `internal/pipeline/pipeline.go` — `Plugin`, `Handler`, `Pipeline.Run`, `dispatch`, priority constants
- `internal/pipeline/context.go` — `LLMContext`, `RouteTarget`; private `streamSrc` / `releaseUpstream` with public accessors
- `internal/pipeline/loader.go` — `PluginSpec`, `PluginLoader` interface, `NativeLoader` registry
- `internal/server/executor.go` — `UpstreamExecutor` (loop bodies moved verbatim from `server.go`)
- `server.go` `handleMessages` collapsed: build `LLMContext` → `pipeline.Run(c)` → deliver
- `pipeline_test.go` — priority ordering, short-circuit, zero-copy mutation visibility

**Runnable check:** `go run ./cmd/miroxy --config config/config.yaml` starts; `curl POST /v1/messages` returns a valid Anthropic response through Gemini; `go test ./...` green.

---

### Story 1.2 — AuthPlugin: auth moves into the pipeline

**Title:** AuthPlugin at priority 0 — bearer validation as a first-class pipeline plugin

**Description:**  
Move inbound bearer token validation from the `authMW` HTTP wrapper on `POST /v1/messages`
into a proper `Plugin` at `PriorityAuth=0`. On invalid/missing token: write 401 and return
error without calling `next` (chain halts). On success: set `c.Values["auth.key_hash"]`
and continue. `GET /v1/models` keeps its own `authMW` wrapper — it is outside the LLM pipeline.

At the end of this story the pipeline chain is `[Auth(0) → UpstreamExecutor(1000)]`.
Auth behavior is identical to before; integration tests for auth pass unchanged.

**Deliverables:**
- `internal/plugins/auth/plugin.go` — `AuthPlugin` wrapping `*auth.Validator`
- `server.go` — remove `authMW` from `POST /v1/messages`; prepend `AuthPlugin` to pipeline
- Auth integration tests confirmed passing

**Runnable check:** request with bad key → 401; request with valid key → Gemini response; `go test ./...` green.

---

### Story 1.3 — Router Plugin: KeywordBackend as a real plugin

**Title:** Router at priority 500 — KeywordBackend wired, fallback seam defined

**Description:**  
Introduce `Router` and `Backend` interface. Default backend is `KeywordBackend`: structural
identity routing (maps `Request.Model` → `cfg.LookupModel` → `RouteTarget`). `Router` holds
an optional `primary Backend` and a `fallback Backend`; if primary is nil or times out within
`decisionBudget`, fallback is used. Router never returns an error to the pipeline — fallback
is infallible. This is the seam that Phase 3's `MLBackend` sidecar will plug into.

Pipeline is now `[Auth(0) → Router(500) → UpstreamExecutor(1000)]`.
Identity routing produces byte-identical responses to the pre-router build.

**Deliverables:**
- `internal/router/router.go` — `Backend` interface, `Router` plugin, `decisionBudget` + fallback logic
- `internal/router/keyword_backend.go` — `KeywordBackend` (`LookupModel` identity dispatch, `ErrNoModel` → 400)
- `router_test.go` — primary errors → fallback used; budget timeout → fallback; identity routing correctness
- `server.go` `NewWithTranslators` — wire `Router{fallback: KeywordBackend}` into default pipeline chain

**Runnable check:** unknown model → 400 with hint; known model → Gemini response via Router; `go test ./...` green.

---

### Story 1.4 — RectifierPlugin: per-provider protocol fixes in the pipeline

**Title:** RectifierPlugin at priority 400 — CacheControlStripper + ToolIDNormalizer

**Description:**  
Implement the deferred §1-C Rectifier. `RectifierPlugin` holds an ordered list of `Rectifier`
rules; each mutates `c.Request` in place before the request reaches the Router. Ship two
built-in Native rules:
- `CacheControlStripper`: removes `cache_control` from all content blocks (Gemini rejects them)
- `ToolIDNormalizer`: ensures every tool-call and tool-result has a non-empty `id`; generates via `idgen` if absent

Pipeline is now `[Auth(0) → Rectifier(400) → Router(500) → UpstreamExecutor(1000)]`.
Agentic sessions with tool calls become more robust — missing `id` fields no longer silently corrupt round-trips.

**Deliverables:**
- `internal/rectifier/plugin.go` — `Rectifier` interface, `RectifierPlugin`
- `internal/rectifier/cache_control_stripper.go`
- `internal/rectifier/tool_id_normalizer.go`
- `rectifier_test.go` — each rule in isolation; ordering; rule error halts chain
- `server.go` — prepend `RectifierPlugin` to default pipeline chain

**Runnable check:** agentic tool-call request missing `id` fields → normalized; Gemini response returns; `go test ./...` green.

---

### Story 1.5 — Config pipeline: section + E2E gate

**Title:** Config-driven pipeline + E2E 10-turn session gate

**Description:**  
Add an optional `pipeline:` section to `config.yaml`. When present, the server resolves named
plugins from `NativeLoader` in priority order. When absent, the default chain `[auth, rectifier, router, upstream]`
is built automatically — no behavior change for existing configs.

Then run the Phase 1 gate: 10-turn agentic Claude Code session through miroxy against a real
Gemini free-tier key pool. This story does not close until the session completes without errors.

**Deliverables:**
- `config/config.go` — `PipelineCfg{Plugins []PluginSpec}` added to `Config`
- `config/yaml.go` — YAML deserialization for `exec_model` string tag; `${ENV_VAR}` in plugin `config` map
- `cmd/miroxy/main.go` — if `cfg.Pipeline.Plugins` non-empty: resolve + sort; else: default chain
- `config/config.yaml.example` — commented `pipeline:` block showing all four default plugins
- `config/e2e.yaml` — real Gemini key pool config for E2E run
- DEVLOG entry; `phase1_plan.md` Phase A status table updated; tag `v1.5.0`

**Runnable check:** E2E Claude Code session completes 10 turns; no 429 surfaced to client; streaming arrives in order; `go test ./...` green; retry SLA < 500 ms confirmed.

---

## Phase 2 — WASM Runtime + Plugin SDK (v2.x)

**Epic description:**  
Embed a `wazero` WASM runtime inside the miroxy process. The ecosystem moat: developers write
miroxy plugins in Rust, TypeScript, or TinyGo compiled to `.wasm` — no CGO, no IPC latency,
no external process. The first real WASM plugin is `SecurityPlugin` (PII detection + prompt
injection guard), demonstrating the privacy guarantee: no request data leaves the process.

**Epic gate:** `SecurityPlugin.Execute()` blocks a PII-containing request with zero outbound HTTP calls; any `enabled: true` WASM plugin can be toggled in `config.yaml` with zero code change.

---

### Story 2.1 — WASM skeleton: passthrough plugin runs in production

**Title:** wazero embedded — hello-world WASM plugin proxies traffic end-to-end

**Description:**  
Add `github.com/tetratelabs/wazero` (`CGO_ENABLED=0` confirmed). Implement `WASMLoader`:
reads a `.wasm` file, compiles it with `wazero.NewRuntime`, wraps in a `WASMPlugin` adapter
that marshals a minimal `LLMContext` projection to JSON, calls the guest `execute()` export,
and reads back the diff. Ship a trivial `passthrough.wasm` guest (Rust, compiled in CI):
receives projection, returns `{action: "continue"}` unchanged.

Wire `passthrough.wasm` as an optional plugin in `config.yaml.example`. When enabled, the
plugin passes all traffic through unchanged — behavior identical to disabled. This proves the
full WASM machinery (loader, ABI, host<>guest round-trip) works in production before any
real logic runs inside it.

**Deliverables:**
- `go.mod` — `wazero` added; `CGO_ENABLED=0 go build` clean
- `internal/pipeline/loader.go` — `WASMLoader` alongside `NativeLoader`
- `internal/pipeline/wasm_plugin.go` — `WASMPlugin` adapter: projection → JSON → ABI → diff → apply
- `docs/design/wasm-abi.md` — ABI v1 spec (exports: `execute/alloc/free`; projection schema; diff schema)
- `plugins/passthrough/` — Rust guest; `make plugins` builds `plugins/passthrough.wasm`
- `config/config.yaml.example` — commented `passthrough` plugin entry

**Runnable check:** `config.yaml` with `passthrough.wasm` enabled → Gemini traffic flows unchanged; `go test ./...` green; `CGO_ENABLED=0 go build ./...` succeeds.

---

### Story 2.2 — Host Functions: `log()` + `http_fetch()` (allowlisted)

**Title:** Host function ABI — controlled capabilities for WASM guests, local-only HTTP

**Description:**  
Expose two host functions to WASM guests:
- `log(level, ptr, len)` — routes to `slog` with the guest plugin name as source field
- `http_fetch(url_ptr, url_len, body_ptr, body_len) (resp_ptr, resp_len)` — HTTP POST to local endpoints only; validated against `wasm.http_allowlist` in `config.yaml`; any non-local URL is rejected with a log error and nil response

`http_fetch` allowlist enforcement is the architectural privacy guarantee — guests cannot
exfiltrate request data to external cloud SaaS. An integration test confirms this: the mock
HTTP client intercepts all outbound calls; `execute()` is called; assert zero calls to
non-allowlisted hosts.

**Deliverables:**
- `internal/pipeline/wasm_host.go` — host function implementations registered with wazero
- `config/config.go` — `WASMConfig{HTTPAllowlist []string}` in `Config`
- `internal/pipeline/wasm_plugin.go` — updated to register host module before instantiation
- Integration test: `passthrough.wasm` with `http_fetch` call to `localhost` succeeds; call to `api.external.com` rejected; traffic still flows

**Runnable check:** `passthrough.wasm` using `log()` → entries in slog; `http_fetch` to localhost returns data; external URL rejected; `go test ./...` green.

---

### Story 2.3 — SecurityPlugin: first real WASM plugin (PII detection, fail-closed)

**Title:** SecurityPlugin — Rust WASM plugin, PII + prompt injection guard, fail-closed

**Description:**  
The first production WASM plugin with real logic. Written in Rust, compiled by `make plugins`.
Scans `Messages[*].content` for:
- PII patterns: email, phone, SSN (XXX-XX-XXXX), credit card (16-digit groups)
- Prompt injection markers: `ignore previous instructions`, `disregard your system prompt`, etc.

Returns `{action: "block"|"redact"|"continue", redacted_messages?}`. If guest panics or
returns malformed JSON, `WASMPlugin` adapter treats it as `block` (fail-closed) — the process
never crashes. No request data leaves the process. This is the claim that differentiates
miroxy from TrueFoundry cloud scan; it is provable by the integration test from Story 2.2.

`config.yaml` `action_on_detect: block` blocks the request with 400 `security_violation`.
`action_on_detect: redact` replaces PII with `[REDACTED]` and continues.

**Deliverables:**
- `plugins/security_pii/` — Rust crate, `#[no_std]` WASM; regex-based PII scan; injection keyword list
- `Makefile` `make plugins` — builds all `.wasm` targets including `security_pii.wasm`
- `WASMPlugin` — fail-closed: WASM panic or invalid diff → return block error, no crash
- Integration tests: fake SSN in message → 400; clean request → passes; `action_on_detect: redact` → PII replaced in forwarded request; WASM panic → 400 (not 500, not crash)
- CI: `cargo build --target wasm32-unknown-unknown --release` in GitHub Actions

**Runnable check:** `security_pii.wasm` enabled in `config.yaml`; PII-laden request → 400; normal Gemini session continues to work; `go test ./...` green.

---

### Story 2.4 — Plugin SDK + hot-reload

**Title:** miroxy-sdk repo + hot-reload for WASM plugins

**Description:**  
Two independent but related deliverables shipped together:

**SDK (`miroxy-sdk` repo):** Everything a plugin author needs — ABI spec, Rust scaffold with
`#[miroxy_plugin]` proc-macro generating `execute/alloc/free` + JSON boilerplate, TinyGo scaffold,
and a local test harness binary (`miroxy-sdk/harness/`) that loads any `.wasm` with a synthetic
`LLMContext` and prints the diff. No miroxy binary needed to test a plugin.

**Hot-reload:** When `PluginSpec.Reload: true`, a `fsnotify` watcher monitors the `.wasm` path.
On file change: compile new module; atomic swap via write-lock on the module slot. In-flight
requests complete against the old `sync.Pool` instance; new requests use the updated module.
Zero downtime for plugin iteration.

**Deliverables:**
- `miroxy-sdk/` repo: `rust/` crate, `go/` TinyGo SDK, `harness/` binary, `docs/wasm-abi.md` (from Story 2.1)
- `internal/pipeline/loader.go` — `WASMLoader` `fsnotify` watcher + atomic slot swap
- `PluginSpec.Reload bool` in config
- Hot-reload integration test: replace `.wasm` mid-load-test; in-flight requests complete; new behavior visible on next request

**Runnable check:** swap `passthrough.wasm` with updated binary while miroxy serves requests; no errors; updated behavior takes effect; `go test ./...` green.

---

## Phase 3 — Sidecar ML + Adapter Ecosystem (v3.x)

**Epic description:**  
Add gRPC-over-Unix-Socket sidecar support for heavy ML workloads that cannot live inside a
Go binary (Python inference, large model loading). Deliver the full adapter set that makes
miroxy replace 5 separate tools with one config file. Launch `miroxy-hub` as the
ecosystem distribution channel.

**Epic gate:** `ObservePlugin`, `CompressPlugin`, and `SecurityPlugin` all toggle on/off via `config.yaml` alone; `MLRouter` routes a request through a local Ollama sidecar with automatic fallback to `KeywordBackend` when the sidecar is down.

---

### Story 3.1 — SidecarPlugin skeleton: stub sidecar in the pipeline

**Title:** SidecarPlugin adapter — gRPC-over-UDS, stub echo sidecar, traffic flows end-to-end

**Description:**  
Define the gRPC `plugin.proto` (`ExecuteRequest{projection_json}` / `ExecuteResponse{diff_json, action}`).
Generate Go client stubs. Implement `SidecarLoader` and `SidecarPlugin` adapter: build projection,
`context.WithTimeout(c.Ctx, decisionBudget)`, call `Plugin.Execute` RPC, apply diff. On timeout
or error: `fail_closed=true` → block request; `fail_closed=false` → warn + call `next(c)`.

Ship a minimal Python echo sidecar (`sidecars/stub/stub.py`): receives `ExecuteRequest`, returns
`{action: "continue"}` unchanged. Wire it in `config.yaml.example`. Traffic flows through
the stub sidecar and reaches Gemini. This proves the full gRPC-over-UDS machinery before
any real ML runs.

**Deliverables:**
- `internal/sidecar/proto/plugin.proto` + generated Go stubs
- `internal/pipeline/loader.go` — `SidecarLoader` (gRPC dial on UDS)
- `internal/pipeline/sidecar_plugin.go` — `SidecarPlugin` adapter with `decisionBudget` + fail behavior
- `sidecars/stub/stub.py` — Python gRPC echo server on UDS; `sidecars/stub/requirements.txt`
- Sidecar health check on startup: log warn if UDS path unreachable; don't block server start
- `config/config.yaml.example` — commented stub sidecar plugin entry

**Runnable check:** miroxy + stub sidecar running → Gemini traffic flows; sidecar killed mid-session → `fail_closed=false` → requests continue via `next`; `go test ./...` green.

---

### Story 3.2 — MLRouter: local Ollama backend with KeywordBackend fallback

**Title:** MLRouter Sidecar backend — Ollama routing, auto-fallback on failure

**Description:**  
Implement `MLBackend` (implementing `router.Backend`) backed by a Python gRPC sidecar that
uses a local Ollama model to classify requests into routing categories (vision, code,
reasoning-heavy, lightweight, default). Wire as `Router.primary`; `KeywordBackend` remains
`Router.fallback`.

The `decisionBudget` (default 20 ms) is enforced by `Router.decide()` — if the ML sidecar
does not respond within budget, the Router silently falls back to `KeywordBackend`. Routing
never fails a request. This is the seam defined in Phase 1 Story 1.3 now filled.

**Deliverables:**
- `sidecars/ml_router/` — Python gRPC sidecar; Ollama client; category → `RouteTarget` mapping
- `internal/router/ml_backend.go` — `MLBackend` implementing `Backend` via `SidecarPlugin`
- `config/config.go` — `RouterCfg{MLBackend *SidecarSpec, DecisionBudgetMs int}` in `Config`
- `server.go` — wire `Router{primary: MLBackend, fallback: KeywordBackend}` when config present
- Integration tests: stub ML sidecar → returned target used; sidecar down → KeywordBackend; budget exceeded → KeywordBackend within budget+5 ms

**Runnable check:** miroxy + ML sidecar running → routing decision logged; sidecar stopped → KeywordBackend used automatically; Gemini still responds; `go test ./...` green.

---

### Story 3.3 — ObservePlugin: cost tracking + configurable sink

**Title:** ObservePlugin — Helicone-parity: latency + tokens + cost, local sink only

**Description:**  
`ObservePlugin` at `PriorityObserve=100` wraps the full downstream pipeline, measuring
latency and token usage. Emits an `ObserveEvent` to a pluggable sink. No data goes to
Helicone or any external cloud — user controls the telemetry destination.

Ship two sinks: `JSONFileSink` (JSONL per day, local disk) and `OTELSink` (OTLP gRPC).
Both use a bounded channel (default 1 K events); full buffer drops the event and logs a
warning — observability loss never blocks the request (fail-open).

**Deliverables:**
- `internal/plugins/observe/plugin.go` — `ObservePlugin`, `ObserveSink` interface, `ObserveEvent` struct
- `internal/plugins/observe/json_sink.go` — JSONL file sink, daily rotation
- `internal/plugins/observe/otel_sink.go` — OTLP gRPC sink
- `config/config.go` — `ObserveCfg{Sink string, Path string, OTLPEndpoint string}` in `Config`
- Integration tests: 3 requests → 3 JSONL entries with correct tokens + latency; disk-full mock → request succeeds; sink full → drop + warn, request succeeds

**Runnable check:** `observe` plugin enabled → JSONL file written; `curl POST /v1/messages` returns Anthropic response; JSONL contains latency + token fields; `go test ./...` green.

---

### Story 3.4 — CompressPlugin: context compression before routing

**Title:** CompressPlugin — Headroom-parity: sliding_window + drop_oldest + summarize_sidecar

**Description:**  
`CompressPlugin` at `PriorityRectifier-10=390` checks estimated token count against `max_tokens`.
If exceeded, it compresses `c.Request.Messages` in-place before the request reaches the Router.
Three strategies: `sliding_window` (keep system prompt + most-recent N messages fitting budget),
`drop_oldest` (remove oldest non-system messages until under budget), `summarize_sidecar`
(delegate to Python/Ollama sidecar; degrades to `drop_oldest` on timeout).

Heuristic token estimator: 4 chars ≈ 1 token for text; image size formula for vision content blocks.
Configurable multiplier per provider. Zero data sent to any external cloud.

**Deliverables:**
- `internal/plugins/compress/plugin.go` — `CompressPlugin`, `CompressStrategy` enum, `estimateTokens`
- `internal/plugins/compress/strategies.go` — `sliding_window`, `drop_oldest`, `summarize_sidecar`
- `sidecars/summarizer/` — Python + Ollama gRPC summarizer; optional Docker image
- `compress_test.go` — each strategy; oversized message → compressed to under `max_tokens`; sidecar failure → drop_oldest fallback
- Integration test: 50-message history beyond limit → compressed; Gemini still responds

**Runnable check:** `compress` plugin with `max_tokens: 4000` + 50-message input → compressed to budget; Gemini responds; `go test ./...` green.

---

### Story 3.5 — miroxy-hub: adapter registry + install CLI

**Title:** miroxy-hub catalog + `miroxy hub install` CLI command

**Description:**  
Launch the `miroxy-hub` ecosystem marketplace: a CDN-served `catalog.json` of vetted `.wasm`
and sidecar Docker images. `miroxy hub install <plugin>` downloads, SHA-256 verifies, writes
to local `plugins/`, and appends a stub `PluginSpec` (with `enabled: false`) to `config.yaml`.
Operator enables explicitly — no plugin activates automatically.

Trust model: hub-hosted plugins are SHA-256 pinned. Unsigned plugins install with a visible
warning. Each plugin's `PluginSpec.AllowedHosts` gates what URLs its `http_fetch()` calls can reach.
GitHub Actions CI in `miroxy-hub` repo: PR → SDK test harness → merge → catalog entry published.

**Deliverables:**
- `miroxy-hub` repo — `catalog.json` schema, GitHub Actions publish workflow
- `cmd/miroxy/hub.go` — `miroxy hub install <name>` subcommand: fetch catalog, verify SHA-256, download, patch `config.yaml`
- `miroxy hub list` — print available plugins from catalog
- Install integration test: mock hub server → install `passthrough` plugin → verify SHA-256 → `plugins/passthrough.wasm` written; bad SHA-256 → install rejected
- `README.md` hub section: submit a plugin workflow

**Runnable check:** `miroxy hub install passthrough` → downloads + verifies + writes stub to config; `miroxy hub list` → prints catalog; `go test ./...` green.
