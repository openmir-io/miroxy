# Epic 3 — Sidecar ML + Adapter Ecosystem (v3.x)

> **Rule:** every story ends in a **runnable binary**.  
> **Prerequisite:** Epic 2 must pass before Epic 3 starts.  
> Last updated: 2026-06-27

---

## Status

| Story | State |
|---|---|
| 3.1 SidecarPlugin skeleton — stub echo sidecar | 🔲 planned |
| 3.2 MLRouter — Ollama backend + KeywordBackend fallback | 🔲 planned |
| 3.3 ObservePlugin + Prometheus metrics | 🔲 planned |
| 3.4 CompressPlugin + DeepSeek / GLM translators | 🔲 planned |
| 3.5 miroxy-hub registry + install CLI | 🔲 planned |

---

## Todolist

### Story 3.1 — SidecarPlugin Skeleton

**Note:** gRPC interface is defined in `miroxy-ir/proto/service.proto` (created at Epic 2 start),
not a local `plugin.proto`. Python sidecar implementations consume `miroxy-ir/gen/python/`.
See §10.3 in `docs/design/architecture-v2-three-layer-ward.md`.

- [ ] Add `miroxy-ir` gRPC `TranslatorService` as the sidecar protocol (replaces bespoke plugin.proto)
- [ ] `internal/pipeline/loader.go` — SidecarLoader: gRPC client dialing UDS path from spec.Path
- [ ] `internal/pipeline/sidecar_plugin.go` — SidecarPlugin: projection → gRPC → diff → apply; decisionBudget timeout; fail_closed/fail_open behavior
- [ ] `sidecars/stub/stub.py` — Python gRPC echo server on UDS; returns `{action:"continue"}`; requirements.txt
- [ ] Startup health check: dial each configured sidecar UDS; log warn if unreachable; don't block server start
- [ ] Integration test: miroxy + stub sidecar → Gemini traffic flows; sidecar killed → fail_open → requests continue without error
- [ ] `config/config.yaml.example` — commented stub sidecar plugin entry (exec_model: sidecar, path: /tmp/stub.sock)
- [ ] `go test ./...` green

### Story 3.2 — MLRouter
- [ ] `sidecars/ml_router/` — Python gRPC server; Ollama client; classify first 2 messages → routing category → RouteTarget JSON
- [ ] `internal/router/ml_backend.go` — MLBackend implementing Backend; calls SidecarPlugin adapter within decisionBudget
- [ ] `config/config.go` — RouterCfg{MLBackend *SidecarSpec, DecisionBudgetMs int} in Config
- [ ] `server.go` — wire Router{primary: MLBackend, fallback: KeywordBackend} when router.ml_backend config present
- [ ] Integration test: stub ML sidecar returns fixed RouteTarget → Router uses it; sidecar down → KeywordBackend used; budget exceeded → fallback within budget+5ms; no request fails
- [ ] `config/config.yaml.example` — commented router.ml_backend section
- [ ] `go test ./...` green

### Story 3.3 — ObservePlugin + Prometheus
- [ ] `internal/plugins/observe/plugin.go` — ObservePlugin at PriorityObserve=100; wraps next; emits ObserveEvent after chain returns
- [ ] `internal/plugins/observe/observe.go` — ObserveSink interface, ObserveEvent struct (model, duration, input/output tokens, cost_usd, error bool)
- [ ] `internal/plugins/observe/json_sink.go` — JSONL append; daily rotation by date suffix; bounded channel 1K events; drop+warn on full (fail-open)
- [ ] `internal/plugins/observe/otel_sink.go` — OTLP gRPC push; same bounded channel pattern
- [ ] `config/config.go` — ObserveCfg{Sink string, Path string, OTLPEndpoint string, PriceTable map[string]TokenPrice}
- [ ] `internal/metrics/metrics.go` — MetricsRecorder interface; PrometheusRecorder implementation
- [ ] Prometheus metrics: miroxy_key_requests_total, miroxy_key_requests_in_flight, miroxy_key_failures_total{reason}, miroxy_key_latency_ms (histogram 50–5000ms), miroxy_pool_exhausted_total, miroxy_requests_total{model,status}, miroxy_stream_errors_total{model}
- [ ] Replace /metrics stub with `promhttp.Handler()` when metrics.enabled=true
- [ ] Integration test (ObservePlugin): 3 requests → 3 JSONL entries with latency + tokens; disk-full mock → request succeeds; sink-full → drop+warn, request succeeds
- [ ] Integration test (Prometheus): request + 429 retry → scrape /metrics → assert counter values correct
- [ ] `go test ./...` green

### Story 3.4 — CompressPlugin + DeepSeek / GLM Translators
- [ ] `internal/plugins/compress/plugin.go` — CompressPlugin at priority 390; estimateTokens heuristic (4 chars≈1 token; image size formula for vision); compress if over maxTokens
- [ ] `internal/plugins/compress/strategies.go` — sliding_window: keep system prompt + most-recent N fitting budget
- [ ] `internal/plugins/compress/strategies.go` — drop_oldest: remove oldest non-system messages until under budget
- [ ] `internal/plugins/compress/strategies.go` — summarize_sidecar: call summarizer UDS within decisionBudget; degrade to drop_oldest on failure
- [ ] `sidecars/summarizer/` — Python + Ollama gRPC summarizer; Docker image (optional)
- [ ] `internal/translator/deepseek.go` — Anthropic→DeepSeek: OpenAI-compatible wire + reasoning_content strip/forward per config; parseDeepSeekRetryAfter
- [ ] `internal/translator/deepseek.go` — streaming: accumulate reasoning_content deltas; emit 7-event Anthropic SSE sequence
- [ ] `internal/translator/glm.go` — GLM (智谱) translator; tool call format normalization; streaming framing differences; provider-pluggable parseRateLimit
- [ ] `internal/detector/` — provider auto-detection: URL pattern matching → /v1/models probe → response fingerprinting; parallel probes at startup, 5s timeout per provider; failure → openai_compat
- [ ] Unit tests: CompressPlugin each strategy; DeepSeek/GLM request-response conversion; provider detection URL patterns
- [ ] Integration test: stub DeepSeek server → tool_use round-trip correct; stub GLM server → response correct
- [ ] Integration test: CompressPlugin 50-message input → compressed to under max_tokens; Gemini still responds
- [ ] `go test ./...` green

### Story 3.5 — miroxy-hub Registry + Install CLI
- [ ] `miroxy-hub` repo — catalog.json schema: {name, version, exec_model, sha256, description, author, tags}; CDN-served static JSON
- [ ] `cmd/miroxy/hub.go` — `miroxy hub install <name>`: fetch catalog, verify SHA-256, download, write to plugins/, append disabled PluginSpec to config.yaml
- [ ] `miroxy hub list` — fetch catalog, print available plugins with description + version
- [ ] Trust model: SHA-256 pinned in catalog; PluginSpec.AllowedHosts []string gates http_fetch() per plugin; unsigned plugins install with visible warning (not blocked)
- [ ] GitHub Actions in miroxy-hub: PR → SDK test harness CI → maintainer merge → catalog entry published (MIT-licensed plugins only)
- [ ] Integration test (mock hub server): install passthrough → SHA-256 verified → plugins/passthrough.wasm written + disabled stub in config; bad SHA-256 → install rejected
- [ ] `go test ./...` green

---

## Story 3.1 — SidecarPlugin Skeleton

**Title:** SidecarPlugin adapter — gRPC-over-UDS, stub echo sidecar, traffic flows end-to-end

**Description:**  
Define gRPC `plugin.proto`. Generate Go client stubs. Implement `SidecarLoader` and
`SidecarPlugin`: build `LLMContext` projection, call `Plugin.Execute` RPC within `decisionBudget`
timeout, apply diff. Timeout/error behavior controlled by `fail_closed` flag. Ship minimal
Python echo sidecar (`sidecars/stub/stub.py`) returning `{action:"continue"}`. Proves the
full gRPC-over-UDS machinery works in production before any real ML runs in it. Traffic flows
through the stub and reaches Gemini.

**Runnable check:** miroxy + stub sidecar → Gemini traffic flows; sidecar killed → `fail_open` → requests continue; `go test ./...` green.

---

## Story 3.2 — MLRouter

**Title:** MLRouter — Ollama sidecar backend, auto-fallback to KeywordBackend on failure

**Description:**  
Implement `MLBackend` (implementing `router.Backend`) backed by a Python gRPC sidecar that
classifies requests using a local Ollama model into routing categories (vision, code,
reasoning-heavy, lightweight, default). Wire as `Router.primary`; `KeywordBackend` remains
`Router.fallback`. The `decisionBudget` (default 20 ms) is enforced by `Router.decide()` —
sidecar non-response within budget → silent fallback. Routing never fails a request. This fills
the `primary Backend` seam defined in Epic 1 Story 1.4.

**Runnable check:** ML sidecar running → routing decision logged; sidecar stopped → KeywordBackend used automatically; Gemini responds; `go test ./...` green.

---

## Story 3.3 — ObservePlugin + Prometheus

**Title:** ObservePlugin (Helicone-parity observability) + Prometheus /metrics endpoint

**Description:**  
`ObservePlugin` at priority 100 wraps the full downstream chain. Measures latency and token
usage. Emits `ObserveEvent` to a bounded-channel sink — full buffer drops the event, never
blocks the request (fail-open). No data goes to Helicone cloud; user controls destination
(`json_file`, `otel`, or `clickhouse` sink).

`MetricsRecorder` interface injected into `InMemoryPool` at construction, keeping the keypool
package free of Prometheus import. Replaces existing `/metrics` stub with `promhttp.Handler()`.

**Prometheus metrics registered:**
```
miroxy_key_requests_total{provider, key_id}              counter
miroxy_key_requests_in_flight{provider, key_id}          gauge
miroxy_key_failures_total{provider, key_id, reason}      counter  (reason: rate_limit|circuit_break|network)
miroxy_key_latency_ms{provider, key_id}                  histogram (50,100,200,500,1000,2000,5000 ms buckets)
miroxy_pool_exhausted_total{provider}                    counter  (fires on ErrNoKeys)
miroxy_requests_total{model, status}                     counter
miroxy_stream_errors_total{model}                        counter
```

**Runnable check:** observe plugin enabled → JSONL written per request with latency + tokens; /metrics returns correct counters after 429 + retry; `go test ./...` green.

---

## Story 3.4 — CompressPlugin + DeepSeek / GLM Translators

**Title:** Context compression + DeepSeek + GLM (智谱) + provider auto-detection

**Description:**  
Three deliverables sharing the same config changes and test infrastructure:

**CompressPlugin (priority 390):** Estimates token count; if over `max_tokens`, compresses
`c.Request.Messages` in-place. Strategies: `sliding_window` (keep system prompt + most-recent N
messages), `drop_oldest`, `summarize_sidecar` (degrades to `drop_oldest` on sidecar failure).
Heuristic: 4 chars ≈ 1 token for text; image size formula for vision content blocks.

**DeepSeek translator:** OpenAI-compatible wire format with `reasoning_content` handling (strip
or forward per config). Custom `parseDeepSeekRetryAfter` for 429 bodies.

**GLM (智谱) translator:** Tool call format normalization; streaming framing differences from
OpenAI. Provider-pluggable `parseRateLimit`.

**Provider auto-detection (3-tier, parallel startup, 5s timeout per provider):**
1. URL pattern matching (zero HTTP cost)
2. `GET /v1/models` probe — inspect model ID prefixes
3. Response fingerprinting — `usageMetadata.promptTokenCount` → Gemini; `reasoning_content` → DeepSeek-R1

Failure always falls back to `openai_compat` — never blocks startup.

**Runnable check:** 50-message history over max_tokens → compressed; DeepSeek tool-use correct; GLM response correct; provider auto-detection logs detected type; `go test ./...` green.

---

## Story 3.5 — miroxy-hub Registry + Install CLI

**Title:** miroxy-hub catalog + `miroxy hub install` + SHA-256 trust model

**Description:**  
CDN-served `catalog.json` of vetted `.wasm` and sidecar Docker images. `miroxy hub install`
downloads, SHA-256 verifies, writes to local `plugins/`, and appends a **disabled** `PluginSpec`
to `config.yaml` — operator enables explicitly. Each plugin's `AllowedHosts` gates its
`http_fetch()` calls. GitHub Actions CI in `miroxy-hub`: PR → SDK test harness → maintainer
merge → catalog entry published (MIT-licensed plugins only).

**Runnable check:** `miroxy hub install passthrough` → downloaded + verified + written; bad SHA-256 → rejected; `miroxy hub list` → catalog printed; `go test ./...` green.

---

## Epic 3 Success Criteria

- [ ] ObservePlugin, CompressPlugin, SecurityPlugin all toggle via config.yaml alone — zero code change (Epic 3 gate)
- [ ] MLRouter: Ollama sidecar → routing logged; sidecar down → auto-fallback; no request fails
- [ ] ObservePlugin JSONL entries contain correct model + latency + input/output tokens + cost_usd
- [ ] /metrics returns correct per-key counters after request + 429 + retry cycle
- [ ] CompressPlugin: 50-message input compressed to under max_tokens; Gemini still responds
- [ ] DeepSeek + GLM: 10-turn tool-use agentic session correct (same test as Epic 1 E2E gate)
- [ ] Provider auto-detection: Gemini behind AI Studio compat URL detected correctly; failure → openai_compat; startup < 10s
- [ ] miroxy hub install: SHA-256 verified; bad hash rejected; plugin disabled by default in config
- [ ] `go test ./...` green throughout all stories; `golangci-lint` zero new warnings

---

## Appendix: Extended Thinking G-11 to G-14 — Deferred Post Epic 3

Cannot be validated in CI without live API access. Require shadow store.

| Item | Depends on | Notes |
|---|---|---|
| G-11 ThinkingOptimizer pre-request hook | — | adaptive vs legacy model detection; Haiku skip |
| G-12 Thinking Budget Rectifier | — | error-driven retry, same key, skip adaptive |
| G-13 Thinking Signature Rectifier | — | 7 error patterns, strip + retry same key; key counters unaffected |
| G-14 Shadow Store `internal/shadow/` | — | per-session Gemini-native turn history; in-memory only; resets on restart |
| Rectifier: thinking_block_signer | G-14 | requires shadow store |
| Rectifier: reasoning_content_injector | G-14 | requires shadow store |

---

## Provider Auto-Detection Reference

**URL pattern matching (Step 1, zero HTTP cost):**

| Host substring | Detected as |
|---|---|
| `generativelanguage.googleapis.com` | gemini |
| `aistudio.google.com` | gemini |
| `api.deepseek.com` | deepseek |
| `open.bigmodel.cn` | glm |
| `openrouter.ai` | openai_compat |

**`/v1/models` probe (Step 2):** model ID prefix: `gemini-` → gemini, `deepseek-` → deepseek, `glm-` → glm, `gpt-` → openai.

**Response fingerprinting (Step 3):** `usageMetadata.promptTokenCount` → gemini; `reasoning_content` → deepseek-r1; `system_fingerprint` → openai. No match → `openai_compat` (log startup warning).
