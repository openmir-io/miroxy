# miroxy — Architecture Design Document

## 1. Purpose and Problem Statement

Claude Code is tightly coupled to the Anthropic API. Users who want to
experiment, cut costs, or avoid rate limits must either pay Anthropic
directly or run a translation proxy. Existing proxies make this harder
than it should be:

- **LiteLLM** routes through OpenAI format as a hub (two translation
  hops: Anthropic → OpenAI → provider). It requires Python and optional
  Redis. Its usage-based-routing tracks its own request count rather than
  parsing the provider's authoritative quota signals.
- **CCR (claude-code-router)** has structural signal routing but zero key
  pooling or rotation capability.
- **cc-switch** does deep protocol adaptation (the "Rectifier" concept)
  and multi-turn thinking-block preservation, but requires a Node.js
  runtime and lacks key pooling or quota-aware backoff.

miroxy ships as a single Go binary. It translates Anthropic ↔ provider
directly (no intermediate hub format), pools and rotates provider API
keys, and parses authoritative quota signals from provider 429 responses.
It is the option for users who want Claude Code experience against
Gemini/OpenAI/DeepSeek without managing runtime dependencies or paying
Anthropic rates.

---

## 2. Core Architectural Principles

1. **Direct translation, no hub.** Every provider gets its own
   `Translator` implementation. There is no canonical intermediate
   format. Anthropic ↔ Gemini is one hop; adding OpenAI is another
   translator file, not a pivot of the whole system.

2. **Single binary, zero runtime dependencies.** Go stdlib for HTTP,
   `go:embed` for UI assets. No Python, no Node, no Redis, no database.
   Restart resets in-memory state — this is acceptable in v1.

3. **Clean interfaces as the only extension seam.** New providers,
   strategies, and rectifiers all implement a published interface.
   Core server and router code never changes for new providers.

4. **Fail loud, not silently.** A 503 when all keys are exhausted is
   correct. A silent fallback that degrades quality is not. Panic in
   request-handling paths is forbidden; return 500 instead.

5. **Secrets never touch disk in plaintext.** All credentials live in
   an encrypted store (`miroxy.s`) or environment variables. Config
   files hold only `${ENV_VAR}` references.

6. **Streaming and non-streaming are always separate code paths.**
   Buffering a stream through a non-streaming abstraction loses latency
   and forces full-response buffering. They must stay distinct.

7. **Retry before streaming to client; never after.** A 429 that arrives
   before the first SSE byte is flushed to the client is completely
   transparent — switch key and retry within the same client connection
   in tens of milliseconds. The client never knows. A 429 after streaming
   has started cannot be silently retried; the client stream is already
   open and must be closed with an error. The retry window is narrow and
   must be exploited fully: a pool of a few dozen well-managed keys is
   sufficient — massive key counts do not compensate for a late retry.

---

## 3. Component Breakdown

### 3.1 Request Lifecycle

```
Claude Code
    │
    ▼
┌─────────────────────────────────────────────────────┐
│  miroxy proxy  (localhost:7777)                    │
│                                                     │
│  Auth ──► Router ──► Rectifier ──► Translator       │
│                            │                        │
│                         KeyPool                     │
│                            │                        │
│                      Upstream provider              │
│                            │                        │
│            Translator ◄── (response)                │
│                            │                        │
│          (key released)  ◄─┘                        │
│                            │                        │
│                     (Anthropic response)             │
└─────────────────────────────────────────────────────┘
    │
    ▼
Claude Code
```

Step by step:
1. **Auth**: validate inbound `x-api-key` against configured token.
2. **Router**: inspect request structure to select provider + model.
3. **Rectifier**: fix known cross-protocol mismatches before translation.
4. **KeyPool**: `Acquire` a live key from the selected provider pool.
5. **Translator.ToUpstream**: build the provider-native HTTP request.
6. **Upstream call**: fire the request. **On 429: if no bytes have been
   flushed to the client yet**, release the failed key (mark it cooling),
   go back to step 4 and acquire the next live key. This retry loop is
   invisible to the client. On 429 after streaming has started, the
   client stream must be closed with an error — silent retry is no longer
   possible.
7. **Translator.FromUpstream** / **StreamFromUpstream**: rebuild
   Anthropic-shaped response or SSE event stream.
8. **KeyPool.Release**: record success/failure; update circuit-breaker state.

Mid-stream client disconnect: cancel upstream context, decrement key's
`in_flight` counter via deferred `Release`.

### 3.2 Interfaces

```go
// internal/translator/translator.go
type Translator interface {
    ToUpstream(ctx context.Context, req *types.MessageRequest, key string) (*http.Request, error)
    ToUpstreamStream(ctx context.Context, req *types.MessageRequest, key string) (*http.Request, error)
    FromUpstream(resp *http.Response) (*types.MessageResponse, error)
    StreamFromUpstream(ctx context.Context, resp *http.Response, msgID, modelAlias string) (<-chan types.SSEEvent, error)
}

// internal/keypool/keypool.go
type KeyPool interface {
    Acquire(ctx context.Context) (*Key, error)
    Release(key *Key, err error)
}

// internal/config/config.go
type ConfigStore interface {
    Load() (*Config, error)
}

// internal/router/router.go  (Phase 2)
type Router interface {
    Route(req *types.MessageRequest) (RouteTarget, error)
}

// internal/rectifier/rectifier.go  (Phase 1)
type Rectifier interface {
    Apply(req *types.MessageRequest) error
}

// internal/detector/detector.go  (Phase 3)
type ProviderDetector interface {
    Detect(ctx context.Context, cfg *config.ProviderConfig) (ProviderHint, error)
}

// internal/metrics/metrics.go  (Phase 3)
type MetricsRecorder interface {
    RecordRequest(provider, keyID string)
    RecordInFlight(provider, keyID string, delta int)
    RecordFailure(provider, keyID, reason string) // reason: rate_limit|circuit_break|network
    RecordLatency(provider, keyID string, ms float64)
    RecordPoolExhausted(provider string)
}
```

### 3.3 KeyPool

Two strategies, selected per provider pool in config:

**Transparent pre-stream retry (applies to all strategies)**

The key retry rule: **a 429 received before the first byte is sent to the
client is invisible to the client**. The retry loop runs entirely inside
miroxy:

```
receive request
  → Acquire key A → send to upstream
  → upstream returns 429
  → [no byte sent to client yet]
  → Release key A (mark cooling, set retry_after)
  → Acquire key B → send to upstream
  → upstream returns 200
  → begin streaming to client   ← client connection opens here
```

The client only ever sees the successful response. As long as the retry
completes in tens of milliseconds (typically < 300 ms), the user
experience is identical to no 429 having occurred.

**Implication:** key count matters less than key freshness. A pool of
20–30 keys spread across separate Google accounts is sufficient for
continuous Claude Code sessions. The strategy is fast transparent retry,
not massive key volume.

**QuotaAwareStrategy** (free-tier / rate-limited keys)
- Tracks per-key state: `in_flight int64`, `circuit_open bool`,
  `retry_after time.Time`, `failures int`
- On 429: parse `retryDelay` from the Gemini response body (authoritative
  signal); mark key with `retry_after`; immediately select next live key
- Incremental cooldown on repeated 429s for the same key: 10 → 30 → 60
  → 120 → 300 s (backoff applies to subsequent selections, not to the
  initial transparent retry which uses the next available key instantly)
- Distinguish "rate limited" (temporary) vs "quota exhausted" (daily reset)
- Circuit open after N consecutive failures; probe after cooldown

**ThroughputOptimizedStrategy** (paid keys)
- Round-robin or least-in-flight selection
- No conservative backoff; standard retry on transient 5xx

Per-key Prometheus metrics:
- `miroxy_key_requests_total{provider,key_id}`
- `miroxy_key_requests_in_flight{provider,key_id}`
- `miroxy_key_failures_total{provider,key_id,reason}`
- `miroxy_key_latency_ms{provider,key_id}` (histogram)

When all keys in a pool are circuit-open simultaneously → return `503`;
emit `miroxy_pool_exhausted_total{provider}`.

### 3.4 Translator (per-provider files)

| File | Provider | Phase |
|------|----------|-------|
| `internal/translator/gemini.go` | Google Gemini | 1 ✅ |
| `internal/translator/openai.go` | OpenAI-compatible | 2 |
| `internal/translator/deepseek.go` | DeepSeek | 3 |
| `internal/translator/glm.go` | 智谱 GLM | 3 |

`UpstreamError` (`internal/translator/upstream_error.go`): a typed error returned
by `FromUpstream` when the Gemini response body carries an application-level error
inside an HTTP 200 response (relay channel pattern). `server.go` routes it:
`HTTPStatus 429` → `RateLimitError` (cooldown); `≥ 500` → circuit-break;
`4xx` → return to client, no retry, key not penalized.

Each translator handles:
- Message array conversion (role mapping, content block types)
- Tool/function definition translation
- Stop reason normalization (`stop_reason` / `finish_reason`)
- Streaming SSE framing (7-event Anthropic sequence)
- Token count fields

### 3.5 Rectifier Layer

Sits between Router output and Translator input. Fixes known
cross-protocol mismatches before the request hits the provider.
Per-provider rule sets; not global.

**Rules implemented for Gemini (Phase 1):**
- `thinking_block_signer`: preserve `thinking` block signature across
  multi-turn tool calls (required for agentic loops)
- `reasoning_content_injector`: inject `reasoning_content` field for
  tool call continuity so the model doesn't lose context
- `cache_control_stripper`: remove `cache_control` fields Gemini rejects
- `tool_id_normalizer`: normalize tool call ID format differences

**Rules to add for OpenAI (Phase 2):**
- `thinking_rectifier`: strip Anthropic `thinking` blocks before sending
  to OpenAI-compatible endpoints (they reject unknown content types)
- Additional `cache_control` handling differences

### 3.6 KeyPool Metrics (Phase 3)

`MetricsRecorder` interface is injected into `InMemoryPool` at construction time.
This keeps the `keypool` package free of a direct Prometheus import and allows
unit tests to use a simple counter map instead of a real registry.

`PrometheusRecorder` implements `MetricsRecorder` and registers all metrics
with the default Prometheus registry. `server.go` constructs it and injects it
into each pool. When `MetricsConfig.Enabled: false`, a no-op recorder is used.

### 3.7 Provider Auto-Detection (Phase 3)

Users frequently configure an OpenAI-compatible endpoint that is actually backed
by Gemini, DeepSeek, or an aggregator. The compat layer loses protocol fidelity.
`ProviderDetector` runs at startup for any provider configured with
`provider_type: auto` and selects the native `Translator` instead.

Three-tier detection (applied in priority order):
1. **URL pattern matching** (zero HTTP cost) — known host substrings mapped to
   providers; no network call; covers all first-party endpoints
2. **`GET /v1/models` probe** — one HTTP call; inspect model ID prefixes
3. **Test-message fingerprinting** — one probe message; inspect response body fields

All `auto` providers are detected in parallel with a 5 s timeout. On failure:
log a warning, fall back to `openai_compat`. Never blocks startup.
Results stored as `map[providerName]Translator`, immutable after startup.
Zero per-request overhead; detection cost is amortized over the process lifetime.

### 3.8 Structural Router (Phase 2)

Classifies requests by observable structural signals — no NLP, no model
inference. Signals checked in priority order:

| Signal | Route | Rationale |
|--------|-------|-----------|
| Token count > threshold | `longContext` | Some providers handle long contexts better |
| Image content blocks present | `vision` | Must route to vision-capable model |
| Tool definitions present | `agentic` | Coding / tool-use model preferred |
| Plan Mode header detected | `think` | Extended thinking model preferred |
| Short prompt, no tools | `lightweight` | Cheapest capable model |
| Default | `default` | Catch-all |

Router output is a `RouteTarget{Provider, Model, KeyPoolID}`. Config
maps route names to providers and models.

**Phase 3.5 Smart Switch extension** adds 3 more signals and a full
multi-provider routing matrix (Chinese content, math/reasoning keywords,
image blocks → specific provider preference). See `milestones.md §Phase 3.5`.
All signals remain structural — zero NLP, zero inference. NLP routing is
reserved for SaaS (Phase 4+).

### 3.9 Graceful Degradation / Fallback Chains (Phase 3.5)

When a provider pool is exhausted (quota_exhausted 429, circuit open, 502/503),
`FallbackChain` selects the next capable model from a configured list.

Before selecting a fallback candidate: filter by required capabilities
(`vision`, `tool_use`, `thinking`). A vision request skips non-vision candidates
silently. On full chain exhaustion: return 503 — never serve from an incompatible
model.

DISTINCT from Phase 1 transparent retry:
- Transparent retry = same provider, different key, pre-stream window
- Fallback chain = different provider, triggered after all provider keys exhausted

The two mechanisms are independent and compose cleanly at the `FallbackChain`
boundary: router selects which chain; chain handles provider failure.

### 3.10 NLP Semantic Routing (Phase 4+, SaaS Only)

Three progressive tiers. SaaS-only — not released in open source. The NLP
classifier is the primary commercial differentiator; releasing it removes the
incentive to upgrade to SaaS.

**Tier 1 (Phase 4):** Keyword / rule classifier. Zero ML model. ~75% coverage.
Primary purpose is data collection for Tier 2 training.

**Tier 2 (Phase 4.5):** Local ML classifier (fastText preferred) trained on
miroxy's own production traffic. Hard prerequisite: 10K labeled requests from
Tier 1. Accuracy target: > 90%.

**Tier 3 (Phase 5):** Self-improving classifier with feedback loop (implicit
quality signals + optional explicit feedback). Per-user preference profiles.
Real-time provider latency and cost signals.

Cost savings dashboard (Phase 5): displays per-user routing breakdown and
estimated savings vs all-Claude pricing. First-class deliverable, not an
afterthought — ship it from Phase 5 day one.

### 3.7 Config and Secret Storage

**`miroxy.config`** (plaintext YAML):
- Proxy port, UI port
- Provider definitions: endpoint, model list, key pool strategy
- Routing rules: signal → RouteTarget mapping
- Path reference to `miroxy.s`
- `${ENV_VAR}` substitution applied before unmarshal

**`miroxy.s`** (encrypted binary):
```
Header:
  [8 bytes] magic "MIROXY"
  [1 byte]  version
  [1 byte]  mode: 0x01=password, 0x02=autokey
  [32 bytes] argon2id salt
  [12 bytes] AES-GCM nonce

Payload: AES-GCM(argon2id(password, salt), JSON{keys})
```

Unlock priority (highest wins):
1. `MIROXY_MASTER_KEY` environment variable (32-byte hex)
2. User password entered at startup (password mode)
3. `~/.config/miroxy/.key` autokey file (autokey mode)

Key derivation: argon2id (time=1, memory=64 MB, threads=4).

### 3.8 Embedded Web UI (Phase 2)

All frontend assets embedded via `//go:embed ui/dist`. Offline capable.
Technology: Alpine.js (~15 KB), plain HTML/CSS.

```
Ports:
  7777  — proxy endpoint (Claude Code ANTHROPIC_BASE_URL)
  7778  — management UI

Both started by: ./miroxy
Proxy is inactive until user clicks Start in UI.
```

UI tabs and backend endpoints:

| Tab | Purpose | Endpoints |
|-----|---------|-----------|
| Status | Start/Stop proxy, uptime, keys loaded | `POST /api/proxy/start\|stop`, `GET /api/proxy/status` |
| API Keys | Per-provider token management | `GET\|POST /api/keys`, `DELETE /api/keys/:id` |
| Routing | Routing rule configuration | `GET\|POST /api/config` |
| Models | Model mapping configuration | `GET\|POST /api/config` |
| Settings | Claude Code integration, security | `GET /api/claude/status`, `POST /api/claude/apply\|restore`, `POST /api/unlock\|lock\|rekey` |

Claude Code integration (Settings tab):
- Auto-detect platform (macOS: `~/Library/Application Support/Claude`,
  Linux: `~/.claude`, Windows: `%APPDATA%\Claude`)
- One-click Apply: backup current `settings.json` → inject `apiUrl` and
  `authToken` fields pointing to miroxy
- One-click Restore: restore from `.miroxy.backup`
- Status indicator: Not configured / miroxy active / Modified externally

---

## 4. Data Flow Diagrams

### 4.1 Non-Streaming Request

```
Claude Code                   miroxy                    Gemini API
    │                            │                            │
    │  POST /v1/messages         │                            │
    │ ─────────────────────────► │                            │
    │                            │                            │
    │                         Auth.Validate()                 │
    │                         Router.Route()                  │
    │                         Rectifier.Apply()               │
    │                         KeyPool.Acquire()               │
    │                         Translator.ToUpstream()         │
    │                            │                            │
    │                            │  POST /v1beta/models/...   │
    │                            │ ──────────────────────────►│
    │                            │                            │
    │                            │     200 OK {response}      │
    │                            │ ◄──────────────────────────│
    │                            │                            │
    │                         Translator.FromUpstream()       │
    │                         KeyPool.Release(nil)            │
    │                            │                            │
    │  200 OK {AnthropicResp}    │                            │
    │ ◄───────────────────────── │                            │
```

### 4.2 Streaming Request

```
Claude Code                   miroxy                    Gemini API
    │                            │                            │
    │  POST /v1/messages         │                            │
    │  stream: true              │                            │
    │ ─────────────────────────► │                            │
    │                            │                            │
    │                   [Auth / Route / Rectify / Acquire]    │
    │                         Translator.ToUpstream()         │
    │                            │                            │
    │                            │  POST (stream=true)        │
    │                            │ ──────────────────────────►│
    │                            │                            │
    │  data: message_start       │  data: {...} (chunks)      │
    │ ◄───────────────────────── │ ◄──────────────────────────│
    │  data: content_block_start │                            │
    │ ◄───────────────────────── │  data: {...}               │
    │  data: content_block_delta │ ◄──────────────────────────│
    │ ◄───────────────────────── │                            │
    │  ...                       │  ...                       │
    │  data: message_stop        │  [stream ends]             │
    │ ◄───────────────────────── │                            │
    │                            │                            │
    │                         KeyPool.Release(nil)            │
```

### 4.3 Transparent 429 Retry (Pre-Stream — Invisible to Client)

This is the critical happy path for rate-limited free-tier keys.
The client connection does not open until a successful upstream response
is confirmed. The entire retry loop is internal to miroxy.

```
Claude Code                   miroxy                    Gemini API
    │                            │                            │
    │  POST /v1/messages         │                            │
    │  stream: true              │                            │
    │ ─────────────────────────► │                            │
    │                            │                            │
    │  [waiting — no response    │  POST (key A)              │
    │   bytes yet]               │ ──────────────────────────►│
    │                            │                            │
    │                            │  429 {retryDelay: "30s"}   │
    │                            │ ◄──────────────────────────│
    │                            │                            │
    │                            │  Release key A             │
    │                            │  retry_after = now+30s     │
    │                            │                            │
    │                            │  Acquire key B             │
    │                            │  POST (key B)              │
    │                            │ ──────────────────────────►│
    │                            │                            │
    │                            │  200 OK (stream begins)    │
    │                            │ ◄──────────────────────────│
    │                            │                            │
    │  data: message_start   ◄── │ ◄── first byte to client   │
    │  data: content_block_delta │                            │
    │  ...                       │                            │
    │  data: message_stop        │                            │
    │ ◄───────────────────────── │                            │
```

The client receives a seamless stream. The 429 on key A is never
visible. Typical retry latency: < 300 ms, imperceptible to the user.

### 4.4 Post-Stream 429 (Cannot Be Hidden)

If a 429 or upstream error arrives after streaming has already started
(after the first byte was sent to the client), silent retry is impossible
— the client's HTTP response is already open. miroxy must close the
connection with an error, and Claude Code will show a broken stream.

```
Claude Code                   miroxy                    Gemini API
    │                            │                            │
    │  data: message_start   ◄── │                            │
    │  data: content_block_delta │  [mid-stream error]        │
    │  ...                       │ ◄──────────────────────────│
    │                            │                            │
    │  [connection closed]   ◄── │  Release key (mark error)  │
    │                            │  Cancel upstream context   │
```

This case is rare because providers typically complete a stream they
started. It is documented here to make the asymmetry explicit: the retry
opportunity exists only in the pre-stream window.

---

## 5. Extension Points

### Adding a new provider

1. Create `internal/translator/<provider>.go` implementing `Translator`.
2. Create `internal/rectifier/rules_<provider>.go` with provider-specific
   fix rules.
3. Add provider section to `miroxy.config` schema.
4. No changes to server, router, or keypool code.

### Adding a new key pool strategy

1. Implement `KeyPool` interface in `internal/keypool/<strategy>.go`.
2. Register in `internal/keypool/factory.go`.
3. Reference by name in `miroxy.config`.

### Adding a new routing signal

1. Add detection logic to `internal/router/signals.go`.
2. Add a new route name to config schema.
3. Map the route in `miroxy.config` routing rules.

---

## 6. Non-Goals (Explicit)

The following are explicitly out of scope and will not be designed for:

- **No intermediate canonical format.** miroxy is not a hub. There is
  no "miroxy native format" that all providers translate to/from.
- **No persistent state.** No database, no SQLite, no file-based request
  log in v1. KeyPool state resets on restart. Metrics are Prometheus
  scrape targets, not stored time-series.
- **No management API that survives restart.** Runtime config changes via
  the UI are ephemeral unless written back to `miroxy.config`.
- **No multi-tenancy.** miroxy is a single-user local proxy. Each user
  runs their own instance.
- **No semantic routing in open source.** The open-source router classifies
  by structural signals only (token count, content block types, header presence,
  unicode range checks, keyword heuristics). It does not call a model to
  classify intent. NLP routing is SaaS-only (Phase 4+).
- **No key custody for third parties.** User keys stay local. The hosted
  SaaS (Phase 4+) uses miroxy's own provider keys, not user-supplied keys.
- **No OpenAI hub routing.** The OpenAI translator is for direct
  OpenAI-compatible endpoints, not an intermediary for other providers.
- **No cross-user learning in open source.** Per-user preference profiles
  and aggregate routing intelligence are SaaS-only features.
- **No real-time provider latency/cost signals in open source.** These
  require cloud-side telemetry collection infrastructure.
- **No pre-optimized multi-instance coordination.** Cross-instance key pool
  contention (multiple miroxy processes) is NOT addressed until SaaS traffic
  monitoring confirms 429 retry rate > 5%. Redis or equivalent shared state
  is explicitly deferred. YAGNI.
- **No `init()` in business logic packages.**
- **No `panic` in request-handling paths.**
- **No third-party HTTP frameworks** (Gin, Fiber, Echo). `net/http` only.
