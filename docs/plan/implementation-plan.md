# miroxy — Implementation Plan Index

> Source of truth for overall progress. Each epic has its own file with story-level todolists.  
> Last updated: 2026-06-27

---

## Epics

| Epic | File | Status | Goal |
|---|---|---|---|
| Epic 1 — Pipeline Seam | [epic1.md](epic1.md) | 🔄 in progress | Plugin pipeline seam; E2E Gemini gate |
| Epic 2 — WASM + Multi-Provider | [epic2.md](epic2.md) | 🔲 planned | WASM runtime; OpenAI; encrypted storage; Web UI |
| Epic 3 — Sidecar ML + Ecosystem | [epic3.md](epic3.md) | 🔲 planned | ML routing; observability; hub registry |

---

## Overall Progress

### Done (pre-Epic 1)

- [x] Gemini translator — non-stream + 7-event SSE stream
- [x] KeyPool — round_robin, least_requests, circuit breaker, rate-limit tiers, proactive soft-limit rotation
- [x] 429 transparent pre-stream retry — non-stream + stream (§1-A)
- [x] Gemini adapter hardening G-01 to G-10 (§1-B)
- [x] Unit tests (35 cases) + integration tests (15 cases)
- [x] Dockerfile + README + DEVLOG

### Epic 1 — Pipeline Seam (in progress)

- [ ] Story 1.1 — Pipeline shell MVP (interfaces + UpstreamExecutor verbatim)
- [ ] Story 1.2 — RectifierPlugin (§1-C CacheControlStripper + ToolIDNormalizer)
- [ ] Story 1.3 — AuthPlugin (auth moves into pipeline priority 0)
- [ ] Story 1.4 — Router Plugin (KeywordBackend + structural signals)
- [ ] Story 1.5 — Config pipeline: section + E2E 10-turn gate

### Epic 2 — WASM + Multi-Provider (planned)

- [ ] Story 2.1 — WASM skeleton (passthrough.wasm, full machinery proven)
- [ ] Story 2.2 — OpenAI translator (second provider; both Gemini + OpenAI work)
- [ ] Story 2.3 — SecurityPlugin (Rust WASM, PII detection, fail-closed)
- [ ] Story 2.4 — Encrypted storage (miroxy.s) + Web UI (Alpine.js, port 7778)
- [ ] Story 2.5 — Plugin SDK (miroxy-sdk repo) + hot-reload

### Epic 3 — Sidecar ML + Ecosystem (planned)

- [ ] Story 3.1 — SidecarPlugin skeleton (gRPC-over-UDS, stub echo sidecar)
- [ ] Story 3.2 — MLRouter (Ollama backend, KeywordBackend fallback seam filled)
- [ ] Story 3.3 — ObservePlugin + Prometheus /metrics
- [ ] Story 3.4 — CompressPlugin + DeepSeek + GLM translators + provider auto-detection
- [ ] Story 3.5 — miroxy-hub catalog + install CLI

---

## Key Architectural Decisions

See `docs/design/architecture-v2-three-layer-ward.md` for full rationale.

| Decision | Value |
|---|---|
| Plugin interface | `Execute(c *LLMContext, next Handler) error` — everything is a Plugin |
| Three execution models | Native (in-process, redline), WASM (wazero, zero CGO), Sidecar (gRPC-over-UDS) |
| Retry boundary | Plugin ordering — plugins above Retry run once; below = retried per attempt |
| 429 never circuit-breaks a key | RateLimitError → separate escalating cooldown counter |
| Retry before first byte | Pre-stream 429 retry invisible to client; post-stream errors are in-band |
| No intermediate routing hop | Anthropic ↔ provider is one hop at routing level; no canonical re-encoding mid-chain |
| Protobuf IR (Epic 2+) | `miroxy-ir` repo defines `ir.proto`; `TranslatorBackend` replaces `Translator` when WASM/gRPC backends arrive. Current `*types.MessageRequest` stays until then. See §10 in architecture-v2 doc. |
| SaaS boundary | Open source ships Plugin seams + KeywordBackend only; NLP classifier stays SaaS |

---

## Epic Gate Conditions

| Epic | Gate |
|---|---|
| Epic 1 | `go test ./...` green throughout; E2E 10-turn Claude Code session completes; retry SLA < 500 ms |
| Epic 2 | SecurityPlugin blocks PII with zero outbound HTTP calls; `CGO_ENABLED=0 go build ./...` succeeds |
| Epic 3 | Any of {ObservePlugin, CompressPlugin, SecurityPlugin} toggle via config.yaml alone, zero code change |
