# Epic 1 — Pipeline Seam (v1.x)

> **Rule:** every story ends in a **runnable binary** — `go run ./cmd/miroxy` works
> and `go test ./...` stays green at every story boundary.
> **Starting state:** G-01 to G-10 done, 429 retry done, 50 tests green.
> Last updated: 2026-06-27

---

## Pre-Story: Already Shipped

- [ ]  §1-A 429 transparent pre-stream retry — 5 integration tests, SLA < 500 ms
- [ ]  §1-B G-01 sampling params — temperature, top_p, top_k, stop_sequences
- [ ]  §1-B G-02 thought parts filtered — FromUpstream + stream
- [ ]  §1-B G-03 safety filter → stop_reason: refusal
- [ ]  §1-B G-04 finishReason completeness — TOOL_CODE, SAFETY, RECITATION
- [ ]  §1-B G-05 UpstreamError + server coordination — 429 body → RateLimitError
- [ ]  §1-B G-06 allowedFunctionNames — tool_choice "tool" constrained
- [ ]  §1-B G-07 streaming input_tokens — message_delta carries PromptTokenCount
- [ ]  §1-B G-08 bearer auth style — APIBase + AuthStyle in config
- [ ]  §1-B G-09 BatchTool filtering — skipped from function declarations
- [ ]  §1-B G-10 malformed tool call arg rectification — rectifyArgs helper
- [ ]  Unit tests (35 cases) + integration tests (15 cases)

---

## Todolist

### Story 1.1 — Pipeline Shell MVP ✅

- [x]  `internal/pipeline/pipeline.go` — Plugin, Handler, Pipeline.Run, dispatch, priority constants
- [x]  `internal/pipeline/context.go` — LLMContext, RouteTarget; private streamSrc/releaseUpstream with public accessors
- [x]  `internal/pipeline/loader.go` — PluginSpec, PluginLoader interface, NativeLoader registry
- [x]  `internal/server/executor.go` — UpstreamExecutor (loop bodies moved verbatim, zero logic change)
- [x]  `server.go` — collapse handleMessages: build LLMContext → pipeline.Run(c) → post-pipeline delivery
- [x]  `internal/pipeline/pipeline_test.go` — ordering by Priority; short-circuit; zero-copy mutation
- [x]  `go test ./...` green — 81 tests pass (50 original + 6 pipeline + 9 server package + remainder unchanged)

### Story IR-1 — IR Proto Schema + InProcess Translation Foundation ✅

- [x]  `internal/ir/ir.proto` — canonical IR schema (source of truth; protoc input at Epic 2 start)
- [x]  `internal/ir/ir.go` — hand-written Go structs matching proto field-for-field
- [x]  `internal/ir/convert.go` — `FromRequest` + `ToResponse`; resolves all Anthropic format ambiguities; strips `cache_control`
- [x]  `internal/ir/convert_test.go` — 9 tests: string/block content, system prompt, tool_use, tool_result name resolution, generation config, ToResponse
- [x]  `internal/translator/gemini.go` — `buildGeminiRequest` takes `*ir.IRRequest`; old Anthropic-aware helpers removed; `buildToolConfig` takes `*ir.IRToolChoice`
- [x]  `go test ./...` green — 90 tests pass (81 original + 9 IR converter tests)

### Story IR-2 — Bidirectional Converters + InProcess Backend Seam ✅

- [x]  `internal/translator/converter.go` — `FrontendConverter` (Anthropic⇄IR) + `ProviderConverter` (provider⇄IR) interfaces
- [x]  `internal/translator/anthropic.go` — `AnthropicConverter`: `RequestToIR` / `ResponseFromIR` / `StreamFromIR` (relocated from `ir/convert.go`)
- [x]  `internal/translator/gemini.go` — `GeminiConverter`: `RequestToProvider` / `ResponseToIR` / `StreamToIR`; response + stream now flow through IR; `resolveFunctionNameIR` owns name resolution
- [x]  `internal/translator/backend.go` — `TranslatorBackend` iface + `InProcessBackend` (WASM/gRPC/HTTP future)
- [x]  `internal/ir/stream.go` — neutral 9-kind `StreamEvent` schema; `IRToolResultPart.FunctionName` removed (IR is provider-neutral)
- [x]  `internal/translator/translator.go` — `Translator` port unchanged; `GeminiTranslator` composes frontend + backend; `server.go`/`executor.go` untouched
- [x]  `go test ./...` green — 91 tests pass (0 fail); 7-event SSE sequence + 429-retry invariants preserved

### Story 1.2 — RectifierPlugin

- [ ]  `internal/rectifier/plugin.go` — Rectifier interface, RectifierPlugin (ordered rule list)
- [ ]  `internal/rectifier/cache_control_stripper.go` — strip cache_control from all content blocks
- [ ]  `internal/rectifier/tool_id_normalizer.go` — ensure non-empty id on all tool calls/results via idgen
- [ ]  `internal/rectifier/rectifier_test.go` — each rule in isolation; rule error halts chain
- [ ]  `server.go` — prepend RectifierPlugin to default pipeline chain
- [ ]  `go test ./...` green

### Story 1.3 — AuthPlugin

- [ ]  `internal/plugins/auth/plugin.go` — AuthPlugin wrapping *auth.Validator; Priority()=0; 401 on failure without calling next
- [ ]  `server.go` — remove authMW from POST /v1/messages; prepend AuthPlugin to pipeline
- [ ]  Existing auth integration tests pass unchanged
- [ ]  `go test ./...` green

### Story 1.4 — Router Plugin

- [ ]  `internal/router/router.go` — Backend interface, Router plugin, decisionBudget + fallback logic
- [ ]  `internal/router/keyword_backend.go` — structural signals (vision/agentic/long_context/lightweight/default) → RouteTarget
- [ ]  `internal/router/router_test.go` — primary error → fallback; budget timeout → fallback; signal detection
- [ ]  `server.go` `NewWithTranslators` — wire Router{fallback: KeywordBackend} into default chain
- [ ]  `go test ./...` green

### Story 1.5 — Config + E2E Gate

- [ ]  `config/config.go` — PipelineCfg{Plugins []PluginSpec} added to Config
- [ ]  `config/yaml.go` — exec_model string tag deserialization; ${ENV_VAR} in plugin config values
- [ ]  `cmd/miroxy/main.go` — if cfg.Pipeline.Plugins non-empty: resolve + sort; else: default chain
- [ ]  `config/config.yaml.example` — commented pipeline: block with all four default plugins
- [ ]  `config/e2e.yaml` — real Gemini key pool config for E2E run
- [ ]  Run E2E: 10-turn Claude Code agentic session via ANTHROPIC_BASE_URL=http://localhost:7777
- [ ]  Confirm: no 429 to client; SSE events in order; tool results round-trip correctly; no goroutine leak
- [ ]  DEVLOG entry; tag v1.5.0
- [ ]  `go test ./...` green; retry SLA < 500 ms confirmed

---

## Story 1.1 — Pipeline Shell MVP

**Title:** Minimum working pipeline — UpstreamExecutor wired, all 50 tests green

**Description:**
Cut the pipeline seam at the smallest possible surface. Define core interfaces in
`internal/pipeline/`. Wire `handleMessages` to `Pipeline.Run(c)`. The only real plugin is
`UpstreamExecutor` — existing retry loops moved **verbatim**, zero logic change. Auth stays
as `authMW` HTTP middleware. Router is a stub calling `cfg.LookupModel` directly.

At story end miroxy starts and proxies Gemini traffic identically to before the refactor.
All 50 tests pass unchanged. Pipeline interfaces exist; everything else is stubs the next
stories will fill.

**Runnable check:** `go run ./cmd/miroxy`; `curl POST /v1/messages` → valid Gemini response; `go test ./...` green.

---

## Story 1.2 — RectifierPlugin

**Title:** §1-C Rectifier — CacheControlStripper + ToolIDNormalizer as a pipeline plugin

**Description:**
Implement the deferred §1-C Rectifier. `RectifierPlugin` at priority 400 holds an ordered
list of `Rectifier` rules that mutate `c.Request` before it reaches routing. Two built-in
rules: strip `cache_control` fields (Gemini rejects them) and normalize tool `id` fields
(generate via idgen if absent).

Pipeline becomes `[Rectifier(400) → Upstream(1000)]`.
Agentic sessions with tool calls become more robust.

**Runnable check:** agentic request with missing tool `id` → normalized before Gemini; `cache_control` stripped; `go test ./...` green.

---

## Story 1.3 — AuthPlugin

**Title:** AuthPlugin at priority 0 — bearer validation as a first-class pipeline plugin

**Description:**
Move bearer token validation from `authMW` HTTP wrapper on `POST /v1/messages` into a
`Plugin` at `PriorityAuth=0`. On invalid/missing token: write 401 and return error without
calling `next` — chain halts before any upstream call. On success: set
`c.Values["auth.key_hash"]` (SHA-256 prefix) and continue. `GET /v1/models` keeps its own
`authMW` wrapper — it is outside the LLM pipeline.

Pipeline becomes `[Auth(0) → Rectifier(400) → Upstream(1000)]`.

**Runnable check:** missing/invalid key → 401, no upstream call; valid key → Gemini response; `go test ./...` green.

---

## Story 1.4 — Router Plugin

**Title:** Router at priority 500 — KeywordBackend structural routing + MLBackend seam

**Description:**
Introduce `Router` plugin and `Backend` interface. `KeywordBackend` evaluates structural
signals (first-match wins) to select a route target:


| Priority | Signal                   | Route key             |
| -------- | ------------------------ | --------------------- |
| 1        | Image blocks present     | `vision`              |
| 2        | Tool definitions present | `agentic`             |
| 3        | Token count > threshold  | `long_context`        |
| 4        | Short prompt, no tools   | `lightweight`         |
| 5        | Default                  | identity`LookupModel` |

`Router.primary` is nil (seam for Phase 3 `MLBackend`). `Router.fallback` = `KeywordBackend`
— infallible, never errors. Router never fails a request.

Pipeline becomes `[Auth(0) → Rectifier(400) → Router(500) → Upstream(1000)]`.

**Runnable check:** unknown model → 400 with hint; vision request routes to `vision` alias if configured; Gemini still responds; `go test ./...` green.

---

## Story 1.5 — Config `pipeline:` Section + E2E Gate

**Title:** Config-driven pipeline + 10-turn agentic E2E session

**Description:**
Add optional `pipeline:` section to `config.yaml`. When absent: server builds default chain
`[auth, rectifier, router, upstream]` automatically. When present: `NativeLoader` resolves
each named plugin in priority order; fail fast on unknown name at startup.

Then run the Epic 1 gate: 10-turn Claude Code agentic session via miroxy against a real
Gemini free-tier key pool. Story does not close until session completes without errors.

**Runnable check:** E2E 10-turn session completes; no 429 to client; tools correct; `go test ./...` green.

---

## Epic 1 Success Criteria

- [ ]  `go test ./...` green at every story boundary (zero regressions)
- [ ]  `pipeline.Run` is the sole entry point for POST /v1/messages
- [ ]  Plugin priority ordering verified by unit test (sorted ascending)
- [ ]  Short-circuit: plugin that doesn't call `next` halts chain
- [ ]  `cache_control` stripped from all Gemini requests
- [ ]  Tool `id` normalized for all agentic requests
- [ ]  Auth 401 halts chain before any upstream call
- [ ]  Router fallback: primary timeout → KeywordBackend, no request failure
- [ ]  E2E: 10-turn Claude Code session; no 429 surfaced; tools round-trip correctly
- [ ]  Retry SLA < 500 ms confirmed post-refactor

---

## Appendix: Pipeline Architecture

```
POST /v1/messages
    │
    [Auth(0)]         verify bearer → 401 or continue
    [Rectifier(400)]  cache_control_stripper, tool_id_normalizer
    [Router(500)]     structural signals → c.Target (RouteTarget)
    [Upstream(1000)]  ← retry loop (verbatim)
         │
         ├─ KeyPool.Acquire()
         ├─ Translator.ToUpstream()
         ├─ HTTP call
         │    ├─ 429  ──►  pool.Release(RateLimitError)  key switch, RL cooldown
         │    ├─ 5xx  ──►  pool.Release(rawErr)           key switch, circuit-break
         │    └─ 200  ──►  Translator.FromUpstream()
         │                   ├─ UpstreamError{429}  ──►  RateLimitError, key switch
         │                   ├─ UpstreamError{≥500} ──►  circuit-break, key switch
         │                   ├─ UpstreamError{<500} ──►  error to client, no retry
         │                   └─ ok  ──►  c.Response / c.streamSrc set
         └─ KeyPool.Release()

Delivery (after pipeline.Run returns):
    non-stream → writeJSON(w, 200, c.Response)
    stream     → SSE headers; drain c.StreamSrc(); Flush per event; c.ReleaseUpstream(err)
```
