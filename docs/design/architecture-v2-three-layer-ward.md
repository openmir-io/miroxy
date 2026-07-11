# miroxy v2 — AI-Era Traffic Governance Hub ("Three-Layer Ward" / 三层结界)

> Architecture Decision Record + Full Evolution Roadmap.  
> Supersedes the extensibility model in `architecture.md` §2.  
> Core component interfaces (`Translator`, `KeyPool`, `ConfigStore`) and all hard  
> constraints in `architecture.md` §6 remain in force.

Status: **Accepted** · Date: 2026-06-27 · Source: `discussions/d1-20260627.md`

---

## 0. Strategic Positioning — Platform, Not Tool

> *"Where Kong/NGINX govern HTTP traffic, miroxy governs **LLM traffic** — authentication,  
>  routing, rate management, protocol translation, observability, and security — in a  
>  **single Go process**."*

Users today install 5+ tools for 5 needs. miroxy absorbs them:

| Need | Current tool | miroxy's answer |
|---|---|---|
| Key pool / rate management | miroxy v1 | Builtin `KeyPoolPlugin` (done) |
| Observability / cost tracking | Helicone | Builtin `ObservePlugin` → async sink |
| Multi-provider routing | LiteLLM | Builtin `Translator` plugins |
| Protocol normalization | cc-switch | Builtin + WASM `RectifierPlugin` |
| Smart routing / ML dispatch | CCR | Builtin Keyword + GRPC ML backends |
| Security / PII redaction | TrueFoundry | WASM `SecurityPlugin` (fail-closed) |
| Context compression | Headroom | Builtin `CompressPlugin` |

**The competitive moat is not breadth of features — it is depth and architecture:**

1. **Depth of the core**: zero-copy pipeline, sub-ms pre-stream retry, exact G-05 429 routing.
2. **Privacy by local processing**: no request data leaves the user's machine to a
   third-party SaaS. The WASM/GRPC model makes complex logic (PII detection, ML routing)
   **local-first by architecture** — a guarantee no cloud-native competitor can match.
3. **Single binary, single process**: no sidecar cluster, no Docker Compose constellation —
   one `miroxy` binary replaces 5 running services.
4. **Open Plugin seam**: community writes WASM plugins in Rust/TS/Go without touching Go core.
   The SDK (Phase B) is the ecosystem moat.

---

## 1. Context — Why This Change

The lifecycle `architecture.md` describes (`Auth → Router → Rectifier → Translator → KeyPool`)
is only *partially real*. In `internal/server/server.go` it is **two near-duplicate hardcoded
retry loops** (`handleNonStream` lines 174–269, `handleStream` lines 275–372); the only real
middleware is `authMW` + `requestLogger`, and "routing" is a single `cfg.LookupModel(req.Model)`
call (line 158). `Router` (§2-B) and `Rectifier` (§1-C) are *planned but unwritten*.

This is the cheapest moment to introduce the plugin seam — **before** Router and Rectifier get
welded into `server.go`. The seam costs nothing at Phase A (zero behavior change, all 50 tests
pass unchanged). Deferred = unwinding three hardcoded handlers instead of one.

---

## 2. The Decision: One `Plugin` Interface, Three Execution Models

**Everything is a Plugin.** Auth, KeyPool, Rectifier, Router, Cache, Security, the upstream call
itself — all implement one interface: `Execute(c *LLMContext, next Handler) error`. The Core owns
only the *pipeline* (ordering, dispatch, the `LLMContext`) and the redline hot path. Functionality
is loaded.

A `PluginLoader` runs each plugin under one of **three execution models** — this is the
"Three-Layer Ward", defined by *how* a plugin runs:

```
┌────────────────── miroxy Core — single Go binary ──────────────────┐
│                                                                        │
│  Pipeline:  c → [Auth] → [Observe] → [Cache] → [Security] →          │
│                  [Rectifier] → [Router] → [Retry→KeyPool→Exec]        │
│             all are Plugins. Pipeline.Run(c *LLMContext) error        │
│                                                                        │
│  PluginLoader — three execution models (the three layers):            │
│    ① BUILTIN  built-in Go. Critical path. Sub-ms, holds locks.       │
│               Redline: Auth, KeyPool, 429-retry, SSE, translation     │
│    ② WASM     wazero embedded sandbox. Primary ecosystem vehicle.     │
│               Rust/TS/Go → .wasm. Zero IPC latency. Stateless logic. │
│    ③ GRPC     gRPC/HTTP to external ML/Python process.               │
│               Use: heavy semantic PII, ML router backend.             │
└────────────────────────────────────────────────────────────────────────┘
```

### 2.1 Core Redlines — Never Externalize

The critical path **MUST stay Builtin**. Pinned; `PluginLoader` refuses to load as WASM or GRPC:

- SSE stream read/write (latency-zero-tolerance, per-event Flush)
- KeyPool state locking + `in_flight` accounting (concurrency safety)
- 429-transparent-retry timing (must complete before the first SSE byte)
- Core protocol translation (Anthropic ⇄ provider)
- `miroxy.s` secret decryption (security boundary)

### 2.2 Tool Ecosystem Execution-Model Mapping

| Product | Concern | Execution model | Rationale |
|---|---|---|---|
| miroxy KeyPool | Quota-aware backoff, 429 retry | **Builtin** | Locks, sub-ms (done) |
| LiteLLM | Core Translators (OpenAI/Anthropic, streaming) | **Builtin** | Hot path, SSE |
| LiteLLM | Niche-LLM translators (non-streaming) | **WASM** | Stateless, multi-lang |
| cc-switch | Protocol Rectifier rules | Builtin / WASM (custom) | Mix |
| CCR | Routing | **Builtin** keyword + **GRPC** ML | Fallback pair (§3.4) |
| TrueFoundry | Security audit / PII redaction | **WASM** (+ GRPC for heavy semantic) | Stateless, fail-closed |
| Helicone | Observability / cost tracking | Builtin plugin → async sink | Fire-and-forget, fail-open |
| Headroom | Context compression | **Builtin** plugin | Stateful, latency-sensitive |

---

## 3. Interface Design (Plugin Contract Specification)

New package `internal/pipeline/`. WASM and GRPC plugins drop in later as **plug-and-play**
modules with **zero change to callers or to the chain seam**.

### 3.1 `Plugin` + `Handler` + `PluginLoader`

```go
// internal/pipeline/pipeline.go

// Handler is the continuation: invoking it runs the rest of the chain.
type Handler func(c *LLMContext) error

// Plugin is THE unified extension interface. Auth, KeyPool, Router, the upstream
// call — all implement it. A plugin may inspect/mutate c, short-circuit (cache hit),
// or delegate downstream via next(c).
type Plugin interface {
    Name() string
    Priority() int                            // lower runs earlier; fixes chain order
    Execute(c *LLMContext, next Handler) error
}

// ExecModel is how the loader runs a plugin.
type ExecModel int
const (
    Builtin ExecModel = iota // built-in Go (redline plugins pinned here)
    WASM                     // wazero in-process sandbox
    GRPC                     // gRPC/HTTP to an external process
)

// PluginSpec is the declarative descriptor for one plugin in config.
type PluginSpec struct {
    Name      string
    ExecModel ExecModel
    Priority  int
    Enabled   bool
    Config    map[string]any // passed to plugin at load time
    Path      string         // .wasm file path or UDS socket path
}

// PluginLoader instantiates a Plugin for a given spec.
// WASM/GRPC adapters themselves implement Plugin: their Execute() marshals the
// LLMContext projection across the boundary, invokes the guest/remote, applies diff back.
type PluginLoader interface {
    Load(spec PluginSpec) (Plugin, error)
}

// Pipeline composes loaded plugins (sorted by Priority ascending).
type Pipeline struct {
    plugins []Plugin
}

func (p *Pipeline) Run(c *LLMContext) error {
    return p.dispatch(0, c)
}

func (p *Pipeline) dispatch(i int, c *LLMContext) error {
    if i >= len(p.plugins) {
        return nil
    }
    return p.plugins[i].Execute(c, func(c *LLMContext) error {
        return p.dispatch(i+1, c)
    })
}
```

**Priority constants (reserved ranges):**

```go
const (
    PriorityAuth      = 0    // must run first; reads R.Header
    PriorityObserve   = 100  // timing / cost start
    PriorityCache     = 200  // short-circuit on hit
    PrioritySecurity  = 300  // fail-closed before Request is touched
    PriorityRectifier = 400  // mutates Request before routing
    PriorityRouter    = 500  // sets Target
    PriorityRetry     = 600  // retry boundary — everything below retries
    PriorityKeyPool   = 700  // acquires key into Target
    PriorityTerminal  = 1000 // UpstreamExecutor — last, ignores next
)
```

### 3.2 `LLMContext` — Strongly Typed, Pointer-Based, Extensible

Passed by pointer through the entire Builtin chain — **zero copy on the hot path**.
Marshaling happens **only** at a WASM/GRPC boundary, for the projection the remote plugin
declares it needs. `Values map[string]any` must contain only JSON-serializable types.

```go
// internal/pipeline/context.go

type RouteTarget struct {
    Provider  string // e.g. "gemini", "openai"
    Model     string // upstream model alias
    KeyPoolID string // selects which pool to acquire from
}

type LLMContext struct {
    // Transport
    Ctx context.Context        // request cancellation / deadline
    W   http.ResponseWriter    // delivery only; plugins MUST NOT write directly
    R   *http.Request          // inbound request (Auth reads here)

    // Request — pointer; mutated in place by Rectifier, Compressor, etc.
    Request *types.MessageRequest
    Stream  bool

    // Routing decision — set by Router, read by UpstreamExecutor
    Target RouteTarget

    // Produced by the terminal; consumed by the server's delivery step
    Response        *types.MessageResponse               // non-stream result
    streamSrc       <-chan types.SSEEvent                // stream source (set by terminal)
    StreamHooks     []func(types.SSEEvent) types.SSEEvent // applied per event at drain
    releaseUpstream func(err error)                      // delivery calls after drain

    // Cross-cutting metadata + WASM/GRPC-serializable bag
    // Rules: JSON-serializable values only; keys namespaced by plugin
    Values    map[string]any
    StartedAt time.Time
}
```

**Metadata conventions for `Values`:**

| Key | Owner | Type | Notes |
|---|---|---|---|
| `"auth.key_hash"` | AuthPlugin | `string` | SHA-256 prefix of validated key |
| `"route.reason"` | Router | `string` | e.g. `"keyword:vision"`, `"ml:routing"` |
| `"observe.req_tokens"` | ObservePlugin | `int` | Estimated tokens at request time |
| `"observe.cost_usd"` | ObservePlugin | `float64` | Finalized on response drain |
| `"cache.hit"` | CachePlugin | `bool` | Set when short-circuiting |
| `"security.pii_detected"` | SecurityPlugin | `bool` | Set by WASM scan |

**Boundary contract:** Builtin plugins share the pointer (zero-copy). The WASM/GRPC
adapter projects `{Request, Target, selected Values}` to JSON, ships across the wazero ABI /
UDS, applies the returned diff back onto the pointer. `Values` being `map[string]any` of
JSON-able values keeps that projection cheap and is why core fields stay strongly typed.

### 3.3 The Hot Path — `Retry → KeyPool → UpstreamExecutor` (Builtin, terminal)

Retry boundary is expressed through **plugin ordering**, not a special mechanism:

- `Router`, `Rectifier`, `Cache`, `Security` sit **above** `RetryPlugin` → run **once**.
- `RetryPlugin` calls `next` ≤ `maxRetries` on 429/5xx; each attempt re-runs everything below.
- `KeyPoolPlugin` acquires a key, sets `c.Target` key field, calls `next`, releases with result.
- `UpstreamExecutor` (terminal, ignores `next`) translates, calls upstream, classifies result:
  - HTTP 429 → `RateLimitError` (cooldown, no circuit-break)
  - HTTP 5xx → generic error (increments circuit-break counter)
  - HTTP 4xx → returns to client, no retry, key not penalized (G-05)

```go
// internal/server/executor.go — terminal Builtin Plugin. Owns the redline.
type UpstreamExecutor struct {
    pools       map[string]keypool.KeyPool
    translators map[string]translator.Translator
    httpClient  *http.Client
    timeouts    map[string]time.Duration
}
func (e *UpstreamExecutor) Name() string  { return "upstream" }
func (e *UpstreamExecutor) Priority() int { return PriorityTerminal }
func (e *UpstreamExecutor) Execute(c *pipeline.LLMContext, _ pipeline.Handler) error {
    // non-stream: ToUpstream → Do → classify → set c.Response; release immediately.
    // stream:     ToUpstreamStream → Do → on 2xx set c.streamSrc + c.releaseUpstream.
    //             Pre-first-byte retry happens HERE, before any SSE header is written.
}
```

**Delivery is post-pipeline (server's job):**
- Non-stream → `writeJSON(w, 200, c.Response)`
- Stream → write SSE headers; `for ev := range c.streamSrc { apply StreamHooks; writeSSEEvent; Flush() }`; then `c.releaseUpstream(streamErr)`.

> **Pragmatic first PR:** ship `Retry + KeyPool + UpstreamExecutor` as a *single composite
> Builtin plugin* containing today's loop verbatim (zero behavior change, all 50 tests green).
> Decompose into sub-plugins later — no seam change required.

### 3.4 Router Plugin with GRPC→Keyword Fallback

```go
// internal/router/router.go

type Backend interface {
    Route(ctx context.Context, c *pipeline.LLMContext) (RouteTarget, error)
}

type Router struct {
    primary        Backend       // GRPC ML backend; nil if not configured
    fallback       Backend       // KeywordBackend — always present, always Builtin
    decisionBudget time.Duration // hard cap on primary (default: 20 ms)
}
func (r *Router) Name() string  { return "router" }
func (r *Router) Priority() int { return PriorityRouter }

func (r *Router) Execute(c *pipeline.LLMContext, next pipeline.Handler) error {
    c.Target = r.decide(c)  // never errors — keyword fallback is infallible
    return next(c)
}

func (r *Router) decide(c *pipeline.LLMContext) RouteTarget {
    if r.primary != nil {
        ctx, cancel := context.WithTimeout(c.Ctx, r.decisionBudget)
        defer cancel()
        if t, err := r.primary.Route(ctx, c); err == nil {
            return t
        }
        slog.Warn("router: primary backend failed/timed-out, degrading to keyword")
    }
    t, _ := r.fallback.Route(c.Ctx, c)
    return t
}
```

`KeywordBackend`: structural signals only (token-count, vision, agentic, plan-mode,
lightweight, default). **DEFAULT = identity**: `Request.Model` → `cfg.LookupModel` →
`RouteTarget`, reproducing today's flow exactly. No NLP, no inference — SaaS boundary holds.

---

## 4. Tool Ecosystem "Absorption" Logic

> miroxy does not rewrite competitor algorithms. It builds a **Shim layer** that translates
> competitor concepts into `Plugin` implementations — capturing the value while gaining the
> performance and **privacy of local, in-process execution**.

### 4.1 Absorption Principle

Each absorbed tool maps to one of three patterns:

| Pattern | When | How |
|---|---|---|
| **Builtin Shim** | Stateful, latency-sensitive, or SSE-path | Reimplement core logic in Go as Builtin Plugin; no IPC |
| **WASM Adapter** | Stateless, policy-like, multi-language authors | Port logic to Rust/TS compiled to `.wasm`; loaded via wazero |
| **GRPC Delegation** | Heavy ML model, external dependency preferred | External process communicates via gRPC-over-UDS; bounded timeout |

**Privacy moat:** a WASM or GRPC plugin running locally never sends request content to a
third-party cloud endpoint. This is architecturally enforced — the Guest ABI exposes only the
declared projection; `http_fetch()` host function is scoped to allowlisted local endpoints only.

### 4.2 Helicone → `ObservePlugin` (Builtin Shim, async)

Helicone's value: per-request cost accounting + latency tracing + external storage sink.

```go
// internal/plugins/observe/plugin.go

type ObserveSink interface {
    Emit(ObserveEvent)  // non-blocking; backed by a bounded channel
}

type ObservePlugin struct {
    sink ObserveSink
}

func (p *ObservePlugin) Priority() int { return PriorityObserve }
func (p *ObservePlugin) Execute(c *LLMContext, next Handler) error {
    start := time.Now()
    err := next(c)                  // let chain run; c.Response filled by terminal
    p.sink.Emit(ObserveEvent{
        Model:     c.Request.Model,
        Duration:  time.Since(start),
        InputTok:  c.Response.Usage.InputTokens,
        OutputTok: c.Response.Usage.OutputTokens,
        CostUSD:   estimateCost(c),
        Error:     err != nil,
    })
    return err
}
```

Sink implementations: local JSON file, ClickHouse, OTEL collector. **Zero data to Helicone
cloud**; the user controls telemetry destination. Sink is bounded-buffer, fail-open.

### 4.3 LiteLLM → Builtin `Translator` Plugins + WASM Shims

LiteLLM's value: provider routing table + request format normalization.

- **Core providers (streaming):** Builtin `Translator` implementations. SSE path must be
  in-process. Already: Gemini. Next: OpenAI.
- **Niche/low-traffic providers (non-streaming):** WASM plugins. Author writes Rust/TS
  translator compiled to `.wasm`; miroxy loads via `PluginLoader{ExecModel: WASM}`.
- **Model alias table:** absorbed directly into `config.yaml` `model_list` — zero Python runtime.

### 4.4 cc-switch → `RectifierPlugin` (Builtin Shim + WASM Custom)

cc-switch's value: rewrite or strip request fields before forwarding (strip `cache_control`,
normalize tool-call IDs, inject thinking budget, etc.).

```go
// internal/rectifier/plugin.go

type RectifierPlugin struct {
    rules []Rectifier  // ordered; each mutates req in place
}
type Rectifier interface {
    Rectify(req *types.MessageRequest) error
}

func (p *RectifierPlugin) Priority() int { return PriorityRectifier }
func (p *RectifierPlugin) Execute(c *LLMContext, next Handler) error {
    for _, r := range p.rules {
        if err := r.Rectify(c.Request); err != nil {
            return err
        }
    }
    return next(c)
}
```

Built-in rules: `CacheControlStripper`, `ToolIDNormalizer`, `ThinkingBudgetInjector`.
Custom rules: loaded as WASM plugins by `PluginLoader`.

### 4.5 CCR / Smart Routing → `Router` with ML GRPC Backend

CCR's value: traffic-weighted routing across multiple providers based on request semantics.

The `Router` (§3.4) is the shim:
- `KeywordBackend` (Builtin): structural signals, zero latency, always-on fallback.
- `MLBackend` (GRPC): local Llama.cpp / Ollama over UDS within `decisionBudget`.
  Degrades silently to `KeywordBackend` on any failure — routing never fails a request.

**SaaS boundary:** trained NLP classifier and per-user preference learning stay SaaS-only.
Open-source build ships seams + `KeywordBackend` only.

### 4.6 TrueFoundry → `SecurityPlugin` (WASM, fail-closed)

TrueFoundry's value: PII detection + prompt injection guard + compliance audit.

```yaml
# config.yaml
pipeline:
  plugins:
    - name: security_pii
      exec_model: wasm
      path: /plugins/security_pii.wasm
      priority: 300
      config:
        action_on_detect: block  # or "redact" | "log"
        fail_closed: true        # WASM guest panic → reject request
```

The WASM guest receives `{Request.Messages}` projection; returns `{action, redacted_messages?}`.
The `WASMPlugin` adapter applies the diff. No request content leaves the process — **stronger
privacy than TrueFoundry's cloud scan**, verifiable by integration test.

### 4.7 Headroom → `CompressPlugin` (Builtin, pre-Router)

Headroom's value: context window compression to reduce upstream token cost.

```go
// internal/plugins/compress/plugin.go

type CompressPlugin struct {
    maxTokens int
    strategy  CompressStrategy // sliding_window | drop_oldest | summarize_grpc
}
func (p *CompressPlugin) Priority() int { return PriorityRectifier - 10 } // just before Rectifier
func (p *CompressPlugin) Execute(c *LLMContext, next Handler) error {
    if estimateTokens(c.Request) > p.maxTokens {
        p.compress(c.Request) // mutates c.Request.Messages in place
    }
    return next(c)
}
```

`summarize_grpc` strategy delegates to an external summarizer over UDS within a hard
timeout; degrades to `drop_oldest` on failure.

---

## 5. `server.go` Refactor Blueprint (Behavior-Preserving)

Steps are ordered; each step's `go test ./...` must pass before the next.

### Step 1 — Scaffold `internal/pipeline/`

- `pipeline.go`: `Plugin`, `Handler`, `Pipeline.Run`, `dispatch`
- `context.go`: `LLMContext`, `RouteTarget`
- `loader.go`: `PluginSpec`, `PluginLoader` interface + `BuiltinLoader` (registry of Go factory funcs)
- `pipeline_test.go`: ordering by Priority; short-circuit (plugin that doesn't call `next`
  halts chain); zero-copy (mutation of `c.Request` visible downstream without copy)

### Step 2 — Scaffold `internal/router/`

- `router.go`: `Backend`, `Router` with `decisionBudget` + fallback (§3.4)
- `keyword_backend.go`: `KeywordBackend` (identity default = `LookupModel`)
- `router_test.go`: fallback on primary error/timeout; identity routing when no primary

### Step 3 — Extract `UpstreamExecutor` (composite Builtin plugin)

Move bodies of `handleNonStream` / `handleStream` verbatim into `internal/server/executor.go`.
No logic change; retry loop, `pool.Acquire/Release`, `Flush` per event all preserved.
This is the pragmatic staging: one composite plugin, loop verbatim, all 50 tests green.

### Step 4 — Rewire `handleMessages`

```go
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
    var req types.MessageRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil { ... }
    if err := req.Validate(); err != nil { ... }

    c := &pipeline.LLMContext{
        Ctx:       r.Context(),
        W:         w,
        R:         r,
        Request:   &req,
        Stream:    req.Stream,
        StartedAt: time.Now(),
        Values:    make(map[string]any),
    }

    if err := s.pipeline.Run(c); err != nil {
        writeError(w, http.StatusBadGateway, "api_error", err.Error())
        return
    }

    // Delivery — post-pipeline
    if req.Stream {
        flusher := w.(http.Flusher)
        w.Header().Set("Content-Type", "text/event-stream")
        w.Header().Set("Cache-Control", "no-cache")
        w.Header().Set("Connection", "keep-alive")
        w.Header().Set("X-Accel-Buffering", "no")
        w.WriteHeader(http.StatusOK)
        var streamErr error
        for ev := range c.StreamSrc() {
            for _, hook := range c.StreamHooks {
                ev = hook(ev)
            }
            if err := writeSSEEvent(w, ev.Event, ev.Data); err != nil {
                streamErr = err
                break
            }
            flusher.Flush()
        }
        c.ReleaseUpstream(streamErr)
    } else {
        c.Response.Model = req.Model
        writeJSON(w, http.StatusOK, c.Response)
    }
}
```

### Step 5 — Auth as a Plugin

- `internal/plugins/auth/plugin.go`: wraps existing `auth.Validator`; priority 0
- `GET /v1/models` retains its own `authMW` http wrapper (outside the LLM pipeline)
- `requestLogger` stays as the outer `http.Handler` (wraps all routes incl. `/health`)

### Step 6 — Config Extension

```yaml
# config.yaml (new optional section — absent → default chain)
pipeline:
  plugins:
    - name: auth
      exec_model: native
      priority: 0
      enabled: true
    - name: router
      exec_model: native
      priority: 500
      enabled: true
    - name: upstream
      exec_model: native
      priority: 1000
      enabled: true
```

Absent `pipeline:` section → default chain `[auth(0), router(500), upstream(1000)]`,
preserving current behavior exactly.

### Step 7 — Invariant Checks

```
go test ./...                                    # all 50 tests pass
go test ./tests/integration/... -run TestRetry   # 5 retry invariants, SLA < 500 ms
```

Stub Gemini responses must be byte-identical to pre-refactor output.

---

## 6. Guardrails & Invariants

- **Redline plugins are Builtin, pinned.** Loader rejects WASM/GRPC for SSE I/O, KeyPool
  locking, 429-retry timing, core translation, `miroxy.s` decryption.
- **Retry boundary = ordering.** Plugins above `RetryPlugin` run once; retry rotates keys below.
- **Streaming = tee, never buffer.** `StreamHooks` transform events inline at drain;
  `flusher.Flush()` per event preserved; pre-first-byte retry only. Stream and non-stream
  remain separate code paths.
- **Zero-copy native path.** `*LLMContext` shared by pointer; marshaling only at WASM/GRPC
  boundary, only for the declared projection.
- **GRPC plugins on request path have a hard timeout + Builtin fallback.** No request blocks on a slow
  external process. Routing failure never fails the request.
- **Degradation contracts:** observability = fire-and-forget, bounded buffer, fail-**open**.
  Security/PII = fail-**closed**. Each plugin declares its contract; `PluginSpec.Config` carries
  the `fail_closed` flag.
- **SaaS boundary holds.** Open source ships `Backend`/`Plugin` seams + Builtin `KeywordBackend`
  only. Trained NLP classifier, per-user preference learning, cross-user intelligence stay
  SaaS-only — never cross into the open-source build.
- **No panic in request paths; no third-party HTTP framework; single binary; in-memory state.**

---

## 7. Evolution Roadmap

### Phase A — Pipeline Seam (v1.5 internal)

**Goal:** Introduce the seam without changing any behavior. All 50 tests pass unchanged.

| Task | Package | Status |
|---|---|---|
| `internal/pipeline/` scaffolding | pipeline | 🔲 |
| `internal/router/` + `KeywordBackend` | router | 🔲 |
| `UpstreamExecutor` composite (loop verbatim) | server/executor | 🔲 |
| Rewire `handleMessages` → `Pipeline.Run` + delivery | server | 🔲 |
| `AuthPlugin` | plugins/auth | 🔲 |
| Config `pipeline:` section | config | 🔲 |
| All 50 tests green; retry SLA < 500 ms verified | tests | 🔲 |
| §1-C Rectifier: `CacheControlStripper`, `ToolIDNormalizer` | rectifier | 🔲 |
| E2E: 10-turn agentic Claude Code session → real Gemini | e2e | 🔲 |

**Gate:** E2E session completes without errors; no behavior regression in any test.

### Phase B — WASM Runtime + Plugin SDK (v2.0 open beta)

**Goal:** Developers write miroxy plugins in Rust/TS/Go compiled to `.wasm`.
First targets validate real use cases and demonstrate the ecosystem moat.

| Task | Notes |
|---|---|
| Embed `wazero` runtime in `BuiltinLoader` | Zero CGO, zero IPC latency |
| `WASMPlugin` adapter: marshal `LLMContext` projection → ABI, apply diff | JSON projection only |
| Host Functions exposed to WASM guests | `log()`, `http_fetch()` (allowlisted local only) |
| `SecurityPlugin` as first WASM plugin | PII detection, fail-closed |
| Plugin SDK: Go/Rust/TS scaffold + ABI spec (`miroxy-sdk` repo) | Ecosystem moat |
| WASM plugin hot-reload (config `reload: true`) | No process restart |

**Phase B gate:** integration test proves `SecurityPlugin` scans a request and blocks/redacts
with zero HTTP calls leaving the binary — the architectural privacy guarantee no cloud
competitor can replicate.

### Phase C — GRPC ML + Adapter Ecosystem (v2.5)

**Goal:** ML-based routing and the full adapter ecosystem. miroxy becomes a platform.

| Task | Notes |
|---|---|
| `SidecarPlugin` adapter (gRPC-over-UDS) | `MLBackend` first target |
| `MLRouter`: local Llama.cpp / Ollama → routing decisions within `decisionBudget` | Degrades to `KeywordBackend` on failure |
| `ObservePlugin` → OTEL sink + local ClickHouse option | Helicone parity |
| `CompressPlugin` (`sliding_window`, `drop_oldest`, `summarize_grpc`) | Headroom parity |
| WASM shims for niche LLM translators | LiteLLM long-tail parity |
| Adapter registry: `miroxy-hub` (curated `.wasm` + grpc images) | Ecosystem marketplace |

**Phase C gate:** any of {`ObservePlugin`, `CompressPlugin`, `SecurityPlugin`} can be
enabled via config change alone, with zero code change.

### Phase D — SaaS Platform (v3.0+)

| Feature | License | Reason |
|---|---|---|
| Core pipeline + all Builtin plugins | MIT open source | Drives adoption |
| Plugin SDK + WASM runtime | MIT open source | Ecosystem moat |
| `KeywordBackend` (structural routing) | MIT open source | Useful without SaaS |
| Trained NLP routing classifier | **SaaS only** | Core commercial differentiator |
| Per-user preference learning | **SaaS only** | Privacy-critical; requires user isolation |
| Cross-user routing intelligence | **SaaS only** | Requires ≥ 10 K labeled requests to train |
| Cost dashboard + team analytics | **SaaS only** | Monetization surface |
| SSO / RBAC / compliance audit trail | Enterprise add-on | Org-level feature |

**Hard prerequisite for per-user ML:** minimum 10 K labeled requests from Tier 1 production
traffic confirmed before Tier 2 classifier training begins. A bug that leaks one user's
signals into another's profile is a **privacy violation**, not a quality bug.

---

## 8. Files

**Create**
- `internal/pipeline/pipeline.go`, `context.go`, `loader.go`
- `internal/router/router.go`, `keyword_backend.go`
- `internal/server/executor.go` (`UpstreamExecutor`)
- `internal/plugins/auth/plugin.go`
- `internal/pipeline/pipeline_test.go`, `internal/router/router_test.go`

**Modify**
- `internal/server/server.go` — `handleMessages` → context build + `pipeline.Run` + delivery
- `internal/config/config.go` — add `Pipeline []PluginSpec`, `RouterCfg`
- `cmd/miroxy/main.go` — wire `BuiltinLoader` + default chain into `Server`
- `DEVLOG.md`, `CLAUDE.md` (layout + interfaces), `docs/plan/implementation-plan.md`

---

## 9. Verification

1. **Regression gate:** `go test ./...` — all 35 unit + 15 integration tests pass **unchanged**.
   The 5 429-retry invariants are critical: they prove the retry boundary survived the move
   into `UpstreamExecutor`.
2. **New unit tests:** plugin ordering by `Priority`; short-circuit (plugin that doesn't call
   `next` halts chain); `Router` fallback (primary errors/times-out → `KeywordBackend`);
   zero-copy (mutation of `c.Request` visible downstream without a copy).
3. **Behavior-preservation:** with default config, non-streaming and streaming requests against
   the stub Gemini server produce byte-identical responses to pre-refactor.
4. **Latency:** re-run the §1-A pre-stream 429-retry test; confirm SLA < 500 ms holds.
5. **Phase B privacy test:** integration test confirms `SecurityPlugin.Execute()` produces a
   block/redact decision with zero outbound HTTP calls from the miroxy process.

---

## 10. Translation IR Layer + `miroxy-ir` Repo (Phase B Prerequisite)

> **Source:** design discussion 2026-06-28.  
> Captures the decision before Epic 2 begins so WASM and gRPC backends are built toward the same data contract.

### 10.1 Why Protobuf IR

The three execution models (Builtin, WASM, GRPC) all implement `TranslatorBackend`, but they
cross different serialization boundaries:

| Backend | Boundary | Required format |
|---|---|---|
| Builtin | None — Go struct pointer | Go struct (zero-copy) |
| WASM | wazero ABI | Must be serializable |
| GRPC | Unix socket | Must be serializable |

The lowest-common-denominator contract is **Protobuf**: the generated Go struct is usable
natively (zero-copy pointer pass), serializes efficiently at WASM/gRPC boundaries, and provides
schema guarantees raw JSON cannot. One `.proto` file → three backend forms, same contract.

> **This does NOT add an extra translation hop.** The IR is the *internal data contract* for
> the translator layer — not a routing intermediary. Anthropic ↔ provider remains one hop.

### 10.2 `TranslatorBackend` Interface

Replaces the current `Translator` interface when Epic 2 arrives. Current `Translator` takes
`*types.MessageRequest`; `TranslatorBackend` takes `*ir.Request` (Protobuf-generated Go struct):

```go
// Defined in miroxy-ir/gen/go/
type TranslatorBackend interface {
    Translate(ctx context.Context, req *ir.Request, dir Direction) (*ir.Response, error)
    TranslateStream(ctx context.Context, req *ir.Request, dir Direction) (<-chan *ir.StreamEvent, error)
    Provider()      string
    Capabilities() *ir.Capabilities
    HealthCheck(ctx context.Context) error
}

type Direction int
const (
    ToUpstream   Direction = iota // Anthropic → provider
    FromUpstream                  // provider → Anthropic
)
```

**Backend forms — all implement `TranslatorBackend`:**

- `BuiltinBackend` — Go struct ops chain; `*ir.Request` passed by pointer, zero serialization
- `WASMBackend` — `proto.Marshal(req)` → Extism plugin call → `proto.Unmarshal(resp)` back
- `GRPCBackend` — calls `miroxy-ir` gRPC `TranslatorService`; auto-degrades to `BuiltinBackend` on failure

### 10.3 `miroxy-ir` Repo

Separate repository. Contains:

```
miroxy-ir/
├── proto/
│   ├── ir.proto          # IRRequest, IRResponse, IRMessage, IRContentPart, …
│   └── service.proto     # TranslatorService gRPC definition
├── gen/
│   ├── go/               # pb.go (consumed by miroxy core)
│   ├── python/           # pb2.py (consumed by Python sidecar implementations)
│   └── ts/               # pb.d.ts (consumed by TypeScript/Extism plugin authors)
└── docs/
    └── ir-schema.md      # field semantics, extension conventions
```

Core IR sketch:

```protobuf
syntax = "proto3";
package miroxy.ir.v1;

message IRRequest {
    string              system      = 1;
    repeated IRMessage  messages    = 2;
    repeated IRTool     tools       = 3;
    IRGenerationConfig  gen         = 4;
    bool                stream      = 5;
    map<string, bytes>  extensions  = 99; // provider-specific pass-through
}

message IRGenerationConfig {
    optional float  temperature = 1;
    optional float  top_p       = 2;
    optional int32  max_tokens  = 3;
    repeated string stop_seqs   = 4;
}

service TranslatorService {
    rpc Translate(TranslateRequest)       returns (TranslateResponse);
    rpc TranslateStream(TranslateRequest) returns (stream IRStreamEvent);
    rpc HealthCheck(HealthRequest)        returns (HealthResponse);
    rpc GetCapabilities(CapRequest)       returns (Capabilities);
}
```

### 10.4 Config-Driven Backend Selection

```yaml
providers:
  - name: gemini
    backend:
      type: builtin       # default; zero external dependencies

  - name: openai_ir
    backend:
      type: grpc
      endpoint: localhost:50051
      fallback: builtin   # critical — auto-degrade if gRPC service unreachable
      timeout: 100ms

  - name: gemini_wasm
    backend:
      type: wasm
      path: ./plugins/gemini_translator.wasm
```

### 10.5 Implementation Timing

| Phase | Action |
|---|---|
| Epic 1 (now) | Keep current `Translator` interface (`*types.MessageRequest`). No code change. |
| **Epic 2 start** | **Create `miroxy-ir` repo; define `ir.proto` + `service.proto`; generate Go.** |
| Epic 2 WASM | WASM ABI uses `proto.Marshal` over the wazero boundary (replaces JSON projection in Story 2.1 spec) |
| Epic 3 GRPC | `GRPCBackend` calls `miroxy-ir` `TranslatorService`; Python sidecar uses `gen/python/` |

**`miroxy-ir` is a prerequisite for Epic 2 Story 2.1 (WASM skeleton).** Define the `.proto`
files first; the WASM ABI v1 spec references `ir.proto` types directly.

### 10.6 Multi-Repo Timing

| Moment | Action | Rationale |
|---|---|---|
| Now → Epic 1 | Stay monorepo (`miroxy`) | One dev; no cross-repo coordination cost |
| Epic 2 start | Extract `miroxy-ir` | Stable contract needed before WASM + gRPC both reference it |
| Epic 2 Story 2.5 | Extract `miroxy-sdk` | Plugin SDK has its own release cadence and external audience |
| Epic 3 Story 3.5 | Extract `miroxy-hub` | Registry is a web service; separate deploy lifecycle |

**Rule: never split before the contract is stable.** A premature split turns one feature commit
into three coordinated PRs. Split only when the boundary is well-defined and the audience diverges.
