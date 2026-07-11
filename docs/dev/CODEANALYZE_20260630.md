# miroxy Code Structure Analysis — 2026-06-30

Scope: full call chain from client (Claude Code) through miroxy to Gemini 2.5 Flash backend
with 3-key round-robin rotation.

---

## Source File Inventory

```
cmd/miroxy/main.go
core/router/router.go
core/selector/credpool.go
core/selector/errors.go
core/selector/selector.go
internal/auth/auth.go
internal/config/config.go
internal/config/yaml.go
internal/idgen/idgen.go
internal/ir/ir.go
internal/ir/stream.go
internal/pipeline/context.go
internal/pipeline/loader.go
internal/pipeline/pipeline.go
internal/server/executor.go
internal/server/prober.go
internal/server/server.go
internal/translator/anthropic.go
internal/translator/backend.go
internal/translator/converter.go
internal/translator/gemini.go
internal/translator/translator.go
internal/translator/upstream_error.go
internal/types/anthropic.go
internal/types/gemini.go
internal/types/sse.go
```

---

## Startup Initialization (once at process start)

```
main()
 │
 ├─▶ config.NewYAMLStore(path).Load()       read config.yaml, expand ${ENV_VAR}
 │      └─▶ returns *Config
 │             └─ ModelList[gemini-2.5-flash]
 │                  ├─ ProviderModel: "gemini-2.5-flash"
 │                  ├─ APIBase, AuthStyle
 │                  └─ KeyPool.Keys: [key1, key2, key3]
 │
 ├─▶ configureLogger(level, file)
 │
 └─▶ server.New(cfg)
        │
        ├─▶ for each model in cfg.ModelList:
        │      │
        │      ├─▶ translator.NewGeminiWithConfig(providerModel, apiBase, authStyle)
        │      │      └─▶ returns *GeminiTranslator     implements Translator interface
        │      │
        │      └─▶ selector.NewCredPool(CredPoolConfig{
        │               Keys:           [key1, key2, key3]   3 credEntry instances
        │               Translator:     geminiTranslator
        │               Strategy:       "round_robin"         default
        │               Threshold:      5                     circuit-break threshold
        │               Cooldown:       60s                   circuit-break cooldown
        │               RateLimitTiers: [10s,30s,60s,120s,300s]
        │             })
        │             └─▶ returns *CredPool               implements Selector interface
        │
        ├─▶ newKeyProber(modelName, credPool, httpClient)
        │      └─▶ returns *keyProber   background probe; detects when rate-limited keys recover
        │
        ├─▶ newUpstreamExecutor(httpClient, probers)
        │      └─▶ returns *UpstreamExecutor              implements Plugin interface
        │
        ├─▶ pipeline.New([]Plugin{executor})
        │      └─▶ sort by Priority, returns *Pipeline
        │
        ├─▶ auth.NewValidator(cfg.Auth.AllowedKeys)
        │      └─▶ returns *Validator
        │
        └─▶ register routes on http.ServeMux:
               GET  /v1/models   → authMW → handleModels
               POST /v1/messages → authMW → handleMessages
               GET  /health      → 200 OK
               GET  /metrics     → stub (metrics phase pending)

 └─▶ http.ListenAndServe(":8080", requestLogger(mux))
```

---

## Per-Request Call Chain (POST /v1/messages)

```
Claude Code
   │  POST /v1/messages
   │  Authorization: Bearer <your-key>
   ▼
requestLogger (middleware)          log DEBUG on entry, INFO with latency on exit
   │
   ▼
auth.Validator.Middleware           check Bearer token against allowedKeys allowlist
   │ pass
   ▼
server.handleMessages()
   ├─ json.Decode → *MessageRequest
   ├─ req.Validate()
   ├─ cfg.LookupModel(req.Model)    resolve gemini-2.5-flash config entry
   ├─ pipeline.NewContext(...)      build LLMContext
   │     └─ RouteTarget {
   │           Selector: credPool   the 3-key CredPool
   │           Timeout:  from config
   │           Model:    ModelInfo
   │        }
   │
   └─▶ pipeline.Run(c)
          │
          └─▶ UpstreamExecutor.Execute(c, next)    Priority=1000, only plugin currently
                 │
                 ├─[non-streaming] executeNonStream(c)
                 │    │
                 │    └─ for attempt in 0..maxRetries(3):   ◀──────────────┐
                 │            │                                             │
                 │            ├─▶ CredPool.Select(ctx, req)               │
                 │            │      ├─ recover keys whose coolEnd has passed
                 │            │      ├─ round-robin pick eligible credEntry │
                 │            │      │   key1 → key2 → key3 → key1 ...    │
                 │            │      └─▶ returns ExecutionPlan{            │
                 │            │              SelectionID: "key1"           │
                 │            │              Credential:  "<api-key>"      │
                 │            │              Translator:  geminiTranslator  │
                 │            │          }                                 │
                 │            │                                            │
                 │            ├─▶ plan.Translator.ToUpstream(...)         │
                 │            │      convert Anthropic → Gemini request format
                 │            │      build *http.Request                  │
                 │            │                                            │
                 │            ├─▶ httpClient.Do(upstreamReq)              │
                 │            │      ──── HTTPS ────▶ Gemini API          │
                 │            │                                            │
                 │            ├─ resp.StatusCode == 429?                   │
                 │            │      ├─ parseRetryDelay(body)              │
                 │            │      ├─▶ CredPool.Release(plan,           │
                 │            │      │       &RateLimitError{RetryAfter}) │
                 │            │      │      key1 enters stateRateLimited   │
                 │            │      │      (cooldown escalates: 10s→30s→60s→...)
                 │            │      └─▶ continue ───────────────────────▶┘
                 │            │            next Select skips key1, picks key2
                 │            │
                 │            ├─ resp.StatusCode >= 500?
                 │            │      ├─▶ CredPool.Release(plan, err)
                 │            │      │      failures++; threshold hit → circuit-break
                 │            │      └─▶ continue ──────────────────────▶┘
                 │            │
                 │            └─ success:
                 │                   ├─▶ plan.Translator.FromUpstream(resp)
                 │                   │      convert Gemini → Anthropic response format
                 │                   ├─▶ CredPool.Release(plan, nil)
                 │                   │      reset failures=0, rateLimitFailures=0
                 │                   └─ c.Response = anthropicResp
                 │
                 └─[streaming] executeStream(c)
                      │  same retry loop until first byte; after success:
                      ├─▶ plan.Translator.ToUpstreamStream(...)
                      ├─▶ httpClient.Do(upstreamReq)
                      ├─▶ plan.Translator.StreamFromUpstream(ctx, resp, msgID, model)
                      │      returns events chan (one SSE event per token)
                      └─▶ c.SetStream(events, releaseFunc)
                             releaseFunc holds cancel + CredPool.Release
                             called only when the stream drains

   ▼
server.handleMessages() delivers response
   ├─[non-streaming] writeJSON(w, 200, c.Response)
   └─[streaming]     write SSE loop: "event: ...\ndata: ...\n\n"
                     flusher.Flush() after every event
                     c.ReleaseUpstream(streamErr) on loop exit

   ▼
Claude Code receives response
```

---

## CredPool — Per-Key State Machine

```
  stateHealthy
      │
      ├─ 429 response → stateRateLimited
      │     cooldown escalates by rateLimitFailures index:
      │       hit 1: 10s  |  hit 2: 30s  |  hit 3: 60s  |  hit 4: 120s  |  hit 5+: 300s
      │     if Gemini returns RetryAfter in body, that value is used instead
      │              │
      │              └─ coolEnd expires → stateHealthy (rateLimitFailures NOT cleared)
      │                 rateLimitFailures resets only on a successful request
      │
      ├─ 5xx / network error → failures++
      │    └─ failures >= threshold (default 5) → stateCoolingDown
      │              fixed cooldown: 60s (config: cooldown_seconds)
      │              └─ expires → stateHealthy (failures = 0)
      │
      └─ success → failures = 0, rateLimitFailures = 0  (both counters cleared)
```

---

## Module Responsibility Summary

| Module | File | Responsibility |
|--------|------|----------------|
| `config` | `internal/config/` | YAML load, `${ENV_VAR}` expansion, `ConfigStore` interface |
| `auth` | `internal/auth/auth.go` | Bearer token middleware; empty allowedKeys = open mode |
| `router` | `core/router/router.go` | `RouteTarget`, `ModelInfo` structs (routing decision carrier) |
| `selector/CredPool` | `core/selector/credpool.go` | 3-key round-robin, 429 cooldown, circuit-break; implements `Selector` |
| `pipeline` | `internal/pipeline/pipeline.go` | Plugin chain sorted by Priority |
| `UpstreamExecutor` | `internal/server/executor.go` | Terminal plugin (Priority=1000); owns full retry loop |
| `GeminiTranslator` | `internal/translator/gemini.go` | Anthropic ↔ Gemini format conversion; implements `Translator` |
| `keyProber` | `internal/server/prober.go` | Background probe; tests rate-limited keys for recovery |
| `idgen` | `internal/idgen/idgen.go` | Generates message IDs and tool-call IDs |
| `ir` | `internal/ir/` | SSE stream intermediate representation; streaming conversion layer |
| `types` | `internal/types/` | Anthropic + Gemini wire type definitions |

---

## Pipeline Priority Constants

```go
PriorityAuth      = 0     // reserved: inbound auth plugin
PriorityObserve   = 100   // reserved: observability plugin
PrioritySecurity  = 300   // reserved: security plugin
PriorityRectifier = 400   // reserved: request rectification plugin
PriorityRouter    = 500   // reserved: routing plugin
PriorityTerminal  = 1000  // UpstreamExecutor (currently the only plugin)
```

Only `UpstreamExecutor` is wired into the pipeline today. All other priority slots are reserved
for the Plugin ecosystem planned in the v2 Three-Layer Ward architecture.

---

## Key Design Constraints (from CLAUDE.md)

- Streaming and non-streaming are **completely separate code paths** — no buffering across.
- **429 never triggers circuit-break** — it only increments `rateLimitFailures` and enters rate-limit cooldown.
- **Pre-stream retry only** — key rotation happens before the first SSE byte is flushed; once streaming starts, no mid-stream interruption.
- `UpstreamError` (`internal/translator/upstream_error.go`) is the sole coordination point between the Translator and the retry loop.
- No DB, no `init()` in business logic, no third-party HTTP framework.
- All files written to disk must be in English.

---

*Generated: 2026-06-30 | Tool: Claude Code + manual source reading*
