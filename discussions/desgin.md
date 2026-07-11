# miroxy design doc — convert arbitrary upstream protocols to an Anthropic/Claude-compatible API (v1: Gemini, Go implementation)

## Purpose

Build a lightweight, high-performance proxy service named **miroxy** that converts arbitrary upstream provider protocols (Gemini, OpenAI, Bedrock, etc.) into an Anthropic/Claude-compatible API, so existing Claude clients (Claude Code, Claude SDKs, any Anthropic Messages API consumer) can work against non-Anthropic backends without modification.

The proxy performs:
- Protocol translation (Anthropic Messages API ⇄ upstream provider format)
- Model-name mapping
- Tool/function-call translation
- Streaming (SSE) and non-streaming handling
- API-key pool management (round_robin / least_requests / weighted strategies)

**Phase 1 (v1)** implements Gemini as the only upstream provider. The architecture is deliberately designed so that adding a new provider (OpenAI, Bedrock, etc.) later requires implementing one interface, not restructuring the core.

**Audience**: this document is handed to engineers (or automation agents) implementing the project in **Go**. It documents API contracts, conversion rules, the KeyPool design, ops/testing guidance, and the technology decision rationale.

---

## Design principles (high level)

- **Keep it small and pluggable**: the core proxy focuses on protocol conversion and routing only. Anything resembling "management" (persisted model registration, multi-tenant config, audit dashboards) is explicitly out of scope for v1.
- **Least surprise**: present genuinely Anthropic-shaped behavior to clients — correct endpoint (`/v1/messages`), correct request/response schema, correct streaming event sequence — so that an unmodified Claude client cannot tell it isn't talking to Anthropic.
- **Operable from day one**: API-key/master-key auth, health checks, and per-key metrics are not optional extras — they ship in v1.
- **Secure by default**: no secrets baked into the binary or image. All credentials are injected at runtime via environment variables, referenced from config using an `${ENV_VAR}`-style placeholder syntax.
- **No premature abstraction, but no dead ends either**: v1 is file/memory-backed and has no database — but every component that *would* need to grow a persistence layer later (config store, KeyPool state) sits behind an interface, so that swapping in a DB-backed implementation in v2 does not touch the core request-handling path.

---

## v1 scope — single binary, no database, single provider

This is a hard scope boundary for v1, not a soft suggestion:

- **No DB.** No Postgres, no SQLite, no ORM. All model mappings, keys, and runtime state are config-file- or memory-backed.
- **File-first configuration.** A single YAML file (`config.yaml`) is the source of truth: `model_list`, provider mappings, and `${ENV_VAR}` references for secrets.
- **Single binary, single container.** miroxy ships as one statically-linked Go binary. Deployment is `docker run --env-file ... -v ./config.yaml:/app/config.yaml:ro miroxy`. KeyPool state and metrics live in process memory — restart loses in-flight counters and circuit-breaker state by design (acceptable for v1).
- **No management API.** Endpoints that would require persistence (e.g., `POST /model/new` to register a model at runtime and have it survive a restart) are **not implemented** in v1. If a runtime config-mutation endpoint is added for convenience, it must be explicitly documented as ephemeral/in-memory-only and reset on restart.
- **Single upstream provider (Gemini).** OpenAI, Bedrock, etc. are out of scope for v1 code — but see "Extensibility" below for how the architecture accommodates them without a rewrite.

### Extensibility (designed-in, not built-in)

v1 deliberately ships less than it could, but the seams for growth are part of the v1 design, not an afterthought:

| Future need | Extension point already in v1 |
|---|---|
| Add OpenAI/Bedrock as upstream | Implement the `Translator` interface (see below); zero changes to router/server core |
| Persist model_list across restarts | `ConfigStore` is read through a small interface (`Load() (*Config, error)`); v1's implementation reads YAML from disk, v2's implementation can read from Postgres — callers never know the difference |
| Persist KeyPool state / multi-instance KeyPool | `KeyPool` is defined as an interface; v1 ships an in-memory implementation, v2 can ship a Redis-backed implementation for horizontal scaling, without touching the HTTP handlers |
| Management endpoints (`/model/new`, etc.) | Routes are registered in one place (`cmd/miroxy/main.go`); v2 adds new handlers backed by the DB-backed `ConfigStore`, the rest of the router is untouched |

This is the core anti-LiteLLM-bloat principle: **build small interfaces now, so growth later is additive, not a rewrite.**

---

## Technology decision: Go (not Rust, not Python)

This was an explicit design discussion, not a default — recorded here so the rationale survives the next person who asks "why not Rust?".

**Conclusion: Go.** Rust is the right call only under specific conditions (see below); none of them apply to miroxy v1.

### Why not "Rust because it benchmarks faster"

This workload is **I/O-bound, not CPU-bound**. Protocol translation is, at its core: parse JSON → map fields → re-serialize JSON. In that workload:

- Go's goroutine + epoll networking model already saturates I/O concurrency while CPU sits mostly idle. Rust's zero-cost abstractions buy little here — there's no tight numeric loop, no vector math, no image processing where Rust's optimizations would actually move the needle.
- P99 latency is dominated by the **upstream API's own round-trip time** (Gemini responses measured in tens to hundreds of milliseconds). Whether miroxy itself spends 1ms or 0.1ms translating the payload is noise in that budget.

### Concrete advantages of Go for this specific project

1. **Developer velocity.** Protocol translation is mostly conditional field-mapping business logic across many short-lived JSON structures. Rust's borrow checker and lifetime annotations add friction exactly where this project needs the least of it. Go lets you pass and mutate structs/maps freely, keeping the focus on translation correctness rather than ownership puzzles.
2. **JSON ergonomics.** `encoding/json` plus `json.RawMessage` makes "pass through fields I don't understand yet" (unavoidable when translating `tool_use` ⇄ Gemini's `functionCall`, or any provider-specific field) cheap and idiomatic. `serde_json` in Rust can do this too, but typically needs more boilerplate (`#[serde(flatten)]`, custom deserializers) to reach the same ergonomics.
3. **Deployment simplicity.** Go produces a single static binary with trivial cross-compilation (`GOOS=linux GOARCH=arm64 go build`). For a small, single-container service this matters more than it sounds — no runtime, no dynamic linking surprises, no musl toolchain dance.
4. **Streaming/SSE handling.** LLM proxies live and die by streaming correctness. Go's `io.Reader`, `bufio.Scanner`, and goroutines make stream-decode-transform-re-encode pipelines small and readable — typically a few hundred lines, not a subsystem.

### When Rust would actually be the right call (not miroxy v1)

- Extreme-throughput gateways (tens of thousands of req/s) where GC pause behavior and per-request memory overhead genuinely matter at the margin.
- Resource-constrained deployment targets (edge devices, WASM).
- A team with deep existing Rust expertise willing to pay the upfront productivity cost for long-term memory-safety guarantees they specifically need.

None of these apply to a personal/small-scale Anthropic-compatibility proxy. Go is the right default.

---

## Framework and library choices (Go)

| Layer | Choice | Rationale |
|---|---|---|
| HTTP routing | `net/http` standard library, optionally `chi` for route params | The core logic is HTTP handlers + field mapping; a full web framework (Gin, Fiber) brings unused middleware ecosystems and, in Fiber's case, a `fasthttp`-based API incompatible with much of the standard streaming/SSE tooling |
| Reverse-proxy plumbing | `net/http/httputil.ReverseProxy` | Connection pooling and keep-alive reuse come for free; translation logic lives in `Rewrite`/`ModifyResponse` hooks, or in a custom `http.Handler` for finer control over streaming |
| JSON | `encoding/json` + `json.RawMessage` for "don't care, pass through" fields | Standard library is sufficient; no need for a third-party JSON library at this scale |
| Streaming | `io.Pipe` + goroutines, manual SSE framing | A few hundred lines; no framework abstracts this well enough to be worth the dependency |
| Metrics | `github.com/prometheus/client_golang` | De facto standard, minimal-overhead instrumentation, plugs into any existing Prometheus/Grafana stack |
| Config | `gopkg.in/yaml.v3` | Standard YAML parsing; `${ENV_VAR}` substitution implemented as a small pre-processing pass before unmarshal |

### Illustrative skeleton (not final code)

```go
// cmd/miroxy/main.go — wiring, not logic
proxy := &httputil.ReverseProxy{
    Rewrite: func(r *httputil.ProxyRequest) {
        anthropicReq := parseAnthropicRequest(r.In.Body)   // /v1/messages body
        upstreamReq  := translator.ToUpstream(anthropicReq) // Translator interface
        r.Out.Body = upstreamReq
        r.SetURL(upstreamURL)
    },
    ModifyResponse: func(resp *http.Response) error {
        return translator.FromUpstream(resp) // rewrites resp.Body in place
    },
}
```

Streaming responses bypass `ReverseProxy.ModifyResponse` (which is not suited to incremental rewriting) and instead go through a dedicated handler that reads the upstream SSE stream, decodes provider-specific chunks, re-encodes them as Anthropic SSE events, and writes them to the client via a piped `io.Writer` — cancel-aware, so a client disconnect propagates to an upstream request cancellation.

### Translator interface (the core extensibility seam)

```go
type Translator interface {
    ToUpstream(req *AnthropicRequest) (*http.Request, error)
    FromUpstream(resp *http.Response) (*AnthropicResponse, error)
    StreamFromUpstream(resp *http.Response) (<-chan AnthropicSSEEvent, error)
}
```

Each provider gets one file implementing this interface: `translator/gemini.go` in v1, `translator/openai.go` / `translator/bedrock.go` later. **No intermediate canonical format** (deliberately the opposite of LiteLLM's OpenAI-as-lingua-franca design): since the target client-facing format is fixed (Anthropic), point-to-point translation (Anthropic ⇄ Gemini) avoids an unnecessary translation hop and keeps the conversion code legible.

---

## Architecture overview

### Frontend (Anthropic-facing surface)

- Expose the real Anthropic Messages API surface:
  - `POST /v1/messages` (the actual Anthropic endpoint — not `/v1/chat/completions`, which is OpenAI's naming and would defeat the "drop-in Claude client compatibility" goal)
  - `GET /v1/models` (convenience endpoint, not part of Anthropic's public API but useful for clients/tools that probe available models)
- Support both `stream: true` (SSE) and non-streaming requests.
- Authentication via `Authorization: Bearer <api-key>`, validated against the in-memory KeyPool/allowlist. A separate master key gates any future management operations.

### Conversion layer (core)

- Translate Anthropic `messages` request bodies into upstream-specific request payloads (Gemini in v1).
- Reconcile message-shape differences (Anthropic's `role`/`content` blocks vs. Gemini's `contents`/`parts` structure).
- Resolve model aliases: config maps `claude-*`-style names (e.g. `claude-haiku`) to a concrete upstream model id (e.g. `gemini-2.5-flash`).
- Translate tool/function-call semantics: preserve `name` and `arguments` while adapting envelope shape (Anthropic's `tool_use` content block ⇄ Gemini's `functionCall` part).

### Upstream routing and KeyPool

- A `ProviderClient` interface encapsulates calls to the upstream provider and integrates with the `KeyPool`.
- `KeyPool` supports `round_robin`, `least_requests`, `random`, and `weighted` strategies; tracks `in_flight` count per key for `least_requests`.
- Failing keys are circuit-broken for a cooldown period and periodically health-probed for re-enablement.
- Per-key metrics (requests, failures, latency) are exported via Prometheus.

### Configuration

- A single `config.yaml` defines `model_list`. Each entry has: `model_name` (the Claude-facing alias), `provider_model` (the real upstream model id), `api_key_env` (an `${ENV_VAR}` reference, never a literal secret), and optional `timeout`/`rpm` settings.
- The `ConfigStore` is read through a small interface so a future DB-backed implementation is a drop-in replacement (see Extensibility table above).

---

## Design pattern references (conceptual, not code reuse)

This design borrows several proven patterns observed in LiteLLM's architecture — **conceptually**, not as code to port, since LiteLLM is a Python/FastAPI codebase and miroxy is Go:

- **Config-driven model routing**: a YAML `model_list` as the single source of truth for model aliasing and provider mapping, with `${ENV_VAR}`-style secret references rather than literal keys in config.
- **Layered request lifecycle**: a clear separation between (a) inbound auth/validation, (b) request translation, (c) upstream dispatch with retry/key-selection, (d) response translation — mirrored in miroxy as discrete Go packages (`auth`, `translator`, `keypool`, `provider_clients`) rather than one monolithic handler.
- **Streaming as a first-class path, not an afterthought**: streaming and non-streaming requests are handled by structurally different code paths rather than forcing streaming through a non-streaming abstraction with a buffering shim.
- **Per-key operational state**: tracking in-flight counts, failure counts, and circuit-breaker timers per API key, independent of the model/provider routing logic.

---

## API details and conversion rules

### `GET /v1/models`

Returns the set of models visible to the caller, sourced from `config.yaml`.

```json
{
  "data": [
    { "model_name": "claude-haiku", "provider": "gemini", "provider_model": "gemini-2.5-flash" }
  ]
}
```

### `POST /v1/messages` (Anthropic) → Gemini (upstream)

Sample Anthropic request (simplified):

```json
{
  "model": "claude-haiku",
  "messages": [{ "role": "user", "content": "Write a haiku about coffee." }],
  "max_tokens": 100,
  "stream": false
}
```

Conversion steps:

1. **Model lookup** — resolve `model_name` → `provider_model` + the env-referenced API key, via the `ConfigStore`.
2. **Message mapping** — convert Anthropic's `messages[].content` blocks into Gemini's `contents[].parts` shape (and back on the response path).
3. **Tool/function-call adaptation** — preserve `name`/`arguments` semantics across Anthropic's `tool_use` ⇄ Gemini's `functionCall` envelope.
4. **Streaming** — if `stream: true`, translate Gemini's streaming format into Anthropic's SSE event sequence (`message_start`, `content_block_delta`, `message_delta`, `message_stop`, etc.), preserving event ordering and supporting client-initiated cancellation.
5. **Headers/auth** — replace inbound `Authorization` with the key selected by `KeyPool`; attach a tracing header (`X-Request-Id`) for log correlation.

**Response**: map the upstream payload back into Anthropic's response shape (`content`, `stop_reason`, `usage`), with provider-specific error detail preserved under a `provider_specific_fields` extension when upstream returns an error. Quota/5xx errors increment the offending key's failure counter in `KeyPool` and may trigger a retry on a different key, per configured policy.

---

## KeyPool strategy details

- **Data model** (per key): `{ key_id, env_ref, in_flight_count, success_count, failure_count, last_error_ts, state }`
- **Strategies**:
  - `round_robin` — simple rotation over available keys.
  - `least_requests` — pick the key with the lowest `in_flight_count`.
  - `weighted_random` — sample proportional to configured weight.
  - **Circuit-break fallback** — a key exceeding a failure threshold is cooled down (e.g. 60s) and excluded from selection until a health probe succeeds.
- **Health checks**: periodic lightweight probes (e.g. a minimal `/v1/models`-equivalent call upstream) against cooled-down keys, re-enabling on success.
- **Monitoring**: per-key Prometheus metrics — `requests_total`, `requests_in_flight`, `failures_total`, `avg_latency_ms`.

---

## Errors and retries

- **Client errors (400)** — clear, human-readable message plus a hint (e.g. unknown model name → "see `GET /v1/models`").
- **Upstream 5xx / network errors** — `KeyPool` retries the same logical request against a different key (or, in a future multi-provider config, a different backend), with exponential backoff between attempts.
- **Partial streaming interruption** — emit a well-formed Anthropic-shaped termination event to the client; log details for diagnosis.

---

## Ops and deployment guidance

- **Containerization**: a `Dockerfile` building a static Go binary (`CGO_ENABLED=0`), copied into a minimal base image (`scratch` or `gcr.io/distroless/static`). No secrets baked in — config and credentials are mounted/injected at runtime.
- **Configuration injection**: `docker run --env-file ./secrets.env -v ./config.yaml:/app/config.yaml:ro miroxy`.
- **Logging & monitoring**: structured request/response logging (latency, status, model, key used — never the key value itself), KeyPool state, and Prometheus metrics scraped by an existing Prometheus/Grafana stack.

---

## Test plan (minimum)

- **Unit tests**: message conversion (Anthropic ⇄ Gemini), model-alias lookup, KeyPool strategy correctness (`round_robin`, `least_requests`), tool-call argument conversion.
- **Integration tests**: a local stub upstream HTTP server to validate streaming and non-streaming behavior, client-cancellation propagation, and key-fallback behavior under simulated failure.
- **E2E**: run an actual Claude client (or the Anthropic SDK) against a running miroxy instance and confirm semantic parity — including tool-call round-trips and model-name compatibility.

### Required test cases (must)

1. Send a non-streaming request with `model: claude-haiku` to `POST /v1/messages`. Mock upstream returns fixed text; assert the final response body contains that text in Anthropic's response shape.
2. Simulate `keyA` returning repeated 5xx errors; assert `KeyPool` fails over to `keyB` and the request still completes.
3. Streaming test: mock upstream emits chunks; verify the client receives correctly-shaped Anthropic SSE events, can cancel mid-stream, and the upstream request is observably cancelled on disconnect.

---

## Edge cases and failure modes

- Malformed messages or nested tool-call arguments → `400` with a specific validation message.
- All keys circuit-broken simultaneously → `503` with an explanatory body, plus an ops alert (metric threshold, not just a log line).
- Client disconnects mid-stream while upstream is still generating → cancel the upstream request context; decrement `in_flight` for the key that was serving it.
- Upstream streaming format doesn't map cleanly onto an Anthropic SSE event → buffer, attempt best-effort remapping, and increment a `stream_remap_anomalies_total` metric for visibility (do not silently drop data).

---

## Deliverables & repo layout

```
miroxy/
  cmd/
    miroxy/
      main.go              # wiring: config load, router setup, server start
  internal/
    auth/                  # API-key / master-key validation
    config/                # ConfigStore interface + YAML-backed implementation
    translator/
      translator.go        # Translator interface definition
      gemini.go             # Gemini implementation (v1)
    keypool/
      keypool.go            # KeyPool interface + in-memory implementation
    provider_clients/
      gemini_client.go      # low-level Gemini HTTP client
    metrics/
      metrics.go             # Prometheus instrumentation
  config.yaml.example
  Dockerfile
  README.md
  tests/
    unit/
    integration/
```

---

## Acceptance criteria

- **Core function**: an Anthropic client POSTs a non-streaming request to `/v1/messages` and receives a valid, semantically-correct response sourced from Gemini.
- **Model aliasing**: a `claude-haiku` config entry maps to `gemini-2.5-flash` and is visible via `GET /v1/models`.
- **KeyPool**: `round_robin` and `least_requests` both implemented; key switch-over on repeated failure is observable in tests.
- **Streaming**: streaming-in → streaming-out when upstream supports it, with correct client-cancellation propagation.
- **Tests**: unit + integration coverage for conversion functions, KeyPool strategy behavior, and error fallback.

---

## Implementation prompt (for automation or human implementer)

Implement a lightweight Go proxy named **miroxy** that converts Anthropic Messages API requests into calls to Gemini. Mandatory features:

- `GET /v1/models` — return visible models loaded from `config.yaml` (support `${ENV_VAR}` references for secrets).
- `POST /v1/messages` — translate Anthropic requests to Gemini and return Anthropic-shaped results (support both streaming and non-streaming).
- Model-name mapping via YAML `model_list`, with a clear `400` on lookup failure that hints at `GET /v1/models`.
- `KeyPool` with `round_robin` and `least_requests`, plus circuit-breaking and health-probe logic.
- Per-key Prometheus metrics (or JSON metrics endpoint acceptable for test environments).

Non-functional requirements:

- No secrets baked into the binary or container image; runtime env injection only (`--env-file`).
- Provide a `Dockerfile` and `README.md`; the service must run via `docker run --env-file`.
- Provide unit and integration tests using a local stub upstream — no live Gemini calls in CI.

### Deliverable checklist

- [ ] `config.yaml` supports `model_list` and `${ENV_VAR}` references
- [ ] `GET /v1/models` implemented, returns configured model list
- [ ] Message conversion module (Anthropic ⇄ Gemini) with unit tests
- [ ] `KeyPool` implemented (`round_robin`, `least_requests`) with integration tests
- [ ] `Dockerfile` & `README.md`; service starts via `docker run --env-file`
- [ ] Prometheus metrics (or JSON metrics endpoint for test environments)

---

## Closing notes

Go gives the fastest path to a maintainable, deployable miroxy v1 for this workload's actual bottleneck (I/O, not CPU). The architecture stays deliberately small for v1 — no DB, single provider — but every seam that future growth needs (new provider, persisted config, distributed KeyPool, management API) is already an interface boundary rather than a refactor waiting to happen.

(EOF)
