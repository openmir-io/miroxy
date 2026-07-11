# miroxy — Milestone Document

Sequencing rationale: Phase 1 must produce a proxy that Claude Code can
actually use today with a real Gemini API key. Everything else — UI,
multi-provider, SaaS — builds on that foundation. Each phase is
independently shippable; no phase assumes the next one will land.

---

## Phase 1 — Working Gemini Proxy

**Goal:** Deliver a single-binary proxy that Claude Code can point at
and use with a real Gemini API key, including multi-key rotation and
correct multi-turn agentic behavior.

### Key Deliverables

- Gemini `Translator`: non-streaming + full 7-event SSE streaming path,
  including G-01 to G-10 hardening (sampling params, thought filtering,
  safety refusal, finishReason completeness, UpstreamError routing,
  allowedFunctionNames, streaming input_tokens, bearer auth, BatchTool
  filter, malformed arg rectification)
- `Rectifier` layer with Gemini-specific rules:
  - `cache_control_stripper` — strip fields Gemini rejects
  - `tool_id_normalizer` — normalize tool call ID format differences
- `KeyPool` with `QuotaAwareStrategy`:
  - **Transparent pre-stream retry**: on 429, if no byte has been sent to
    the client yet, immediately release the failed key and retry with the
    next live key — the client never sees the error
  - Parse `retryDelay` from Gemini 429 body (authoritative signal);
    mark the failed key with `retry_after` before acquiring next key
  - Incremental cooldown on repeated 429s for same key: 10 → 30 → 60
    → 120 → 300 s
  - Circuit breaker per key; 503 only when all keys are cooling
    simultaneously and no retry can succeed
- `UpstreamError` type: routes body-level error codes (e.g. relay channel
  returns `200 + {"error":{"code":429}}`) through the same retry/circuit-break
  decision tree as HTTP-level status codes
- `GET /v1/models` + `POST /v1/messages` (streaming + non-streaming)
- Config: plaintext YAML with `${ENV_VAR}` substitution; `api_base`
  and `auth_style` per model entry (bearer token support for relay channels)
- CLI operation: `./miroxy --config miroxy.config`
- Dockerfile for containerized deployment
- Integration tests against stub Gemini server (no live API in CI)

### Success Criteria

- Claude Code session completes a 10-turn agentic tool-use task against
  a real Gemini API key with zero manual intervention
- **Transparent 429 retry**: with 2+ keys configured, a simulated 429
  on key A results in the client receiving a complete, unbroken response
  from key B — no error surfaced, no stream interruption, retry latency
  < 500 ms (verified by integration test with stub that returns 429 on
  first key)
- 503 is returned to the client **only** when all keys are simultaneously
  in cooldown — not on a single key's 429
- `UpstreamError{429}` (body-level 429 from relay channels) uses
  rate-limit cooldown path, not circuit-break counter
- All unit tests (35 cases) and integration tests (15 cases) pass
- `go vet` and `golangci-lint` pass with zero warnings

### Known Risks / Open Questions

- Gemini's `retryDelay` field format is not officially documented; parse
  defensively and fall back to tier-0 cooldown (10 s) if absent or unparseable
- `thinking_block_signer` and `reasoning_content_injector` require the
  Phase 3 shadow store (G-14) — deferred to Phase 3
- The pre-stream retry window requires the server to buffer upstream
  response headers before committing to the client response; confirm this
  does not introduce meaningful latency on the success path

**OUT OF SCOPE for Phase 1:** UI, encrypted secret storage, OpenAI
translator, structural router, Prometheus metrics, DeepSeek/GLM,
thinking block signing, reasoning content injection.

---

## Phase 2 — OpenAI + Structural Routing + Web UI

**Goal:** Add OpenAI-compatible providers, intelligent structural routing,
an embedded web UI for zero-friction setup, and encrypted key storage.

### Key Deliverables

- OpenAI `Translator`: non-streaming + streaming path
- OpenAI `Rectifier` rules:
  - `thinking_rectifier` — strip Anthropic thinking blocks before send
  - `cache_control_stripper` (OpenAI variant)
- `parseOpenAIRetryAfter`: reads `Retry-After` header from OpenAI 429s;
  feeds existing `RateLimitError.RetryAfter` field — no new server logic
- Structural `Router` with signals:
  - token count > threshold → `longContext`
  - image blocks present → `vision`
  - tool definitions present → `agentic`
  - Plan Mode header → `think`
  - default → `lightweight`
- `miroxy.s` encrypted secret storage:
  - AES-GCM with argon2id key derivation (time=1, mem=64 MB, threads=4)
  - Password mode + autokey mode
  - `MIROXY_MASTER_KEY` env override
- Embedded Web UI (Alpine.js, `go:embed`):
  - Status, API Keys, Routing, Models, Settings tabs
  - Claude Code one-click Apply/Restore integration
  - Platform auto-detection (macOS / Linux / Windows)
- Dual-port startup: proxy on 7777, UI on 7778
- `ThroughputOptimizedStrategy` for paid OpenAI keys

### Success Criteria

- A fresh user can: download binary → open UI → paste API keys → click
  Apply → use Claude Code — in under 3 minutes, no CLI required
- Structural router directs image-containing requests only to
  vision-capable models; verified by integration test
- `miroxy.s` round-trip: keys written, binary restarted, keys available
  without re-entry when autokey file exists
- OpenAI tool-use round-trip: stub `tool_calls` response → client
  `tool_use` content block (unit test)
- `thinking_rectifier` strips thinking blocks before OpenAI request (unit test)

### Known Risks / Open Questions

- Alpine.js + embedded assets: verify offline behavior (no CDN calls)
  before shipping; all external JS must be vendored
- `~/.claude/settings.json` format varies by Claude Code version; must
  handle missing fields gracefully and not corrupt the file
- OpenAI `thinking_rectifier` may need to handle extended thinking content
  types added in future Anthropic API versions
- Multi-provider routing rules can produce surprising behavior; the UI
  must show the active route decision for each request (debug panel)

**OUT OF SCOPE for Phase 2:** DeepSeek, GLM, SaaS control plane,
persistent request logging, semantic routing, Prometheus metrics
(deferred to Phase 3), provider auto-detection.

---

## Phase 3 — Additional Providers + Auto-Detection + Prometheus

**Goal:** Extend provider coverage to DeepSeek and GLM, instrument all
per-key activity with Prometheus, and add startup-time provider
auto-detection so relay/aggregator endpoints use their native Translator
instead of the generic OpenAI-compat fallback.

### Key Deliverables

- DeepSeek `Translator` (non-streaming + streaming)
- DeepSeek `Rectifier` rules (normalize `reasoning_content` handling;
  exact rules TBD after protocol analysis)
- GLM (智谱) `Translator` (non-streaming + streaming)
- GLM `Rectifier` rules (TBD based on protocol analysis)
- Provider-pluggable `parseRateLimit` variants for DeepSeek and GLM
- Integration test coverage for DeepSeek and GLM (stub servers)
- **Prometheus metrics** (`internal/metrics/`, `MetricsRecorder` interface):
  - `miroxy_key_requests_total{provider,key_id}` counter
  - `miroxy_key_requests_in_flight{provider,key_id}` gauge
  - `miroxy_key_failures_total{provider,key_id,reason}` counter
    (reason: `rate_limit` | `circuit_break` | `network`)
  - `miroxy_key_latency_ms{provider,key_id}` histogram
    (buckets: 50, 100, 200, 500, 1000, 2000, 5000 ms)
  - `miroxy_pool_exhausted_total{provider}` counter (fires on ErrNoKeys)
  - `miroxy_requests_total{model,status}` counter
  - `MetricsRecorder` interface injected into `InMemoryPool` — keeps
    `keypool` package free of a direct Prometheus dependency; testable
    without a global registry
- **Provider Auto-Detection** (`internal/detector/`, `ProviderDetector` interface):
  - Triggered by `provider_type: auto` in config (explicit values bypass)
  - Three-tier detection at startup — not per-request:
    1. URL pattern matching (zero HTTP cost; covers known hosts:
       `generativelanguage.googleapis.com` → Gemini, `api.deepseek.com` →
       DeepSeek, `open.bigmodel.cn` → GLM, `openrouter.ai` → OpenRouter compat)
    2. `GET /v1/models` probe (one HTTP call per auto provider; inspect
       model ID prefixes: `gemini-`, `deepseek-`, `glm-`, `gpt-`)
    3. Minimal test-message fingerprint as final fallback (inspect response
       body fields: `usageMetadata.promptTokenCount` → Gemini,
       `reasoning_content` → DeepSeek R1, `system_fingerprint` → OpenAI)
  - Results cached immutably for the process lifetime; zero per-request overhead
  - Detection timeout: 5 s per provider; all auto providers probed in parallel
    at startup
  - On timeout or no match: log a startup warning, fall back to `openai_compat`
    — detection failure never blocks startup or returns an error to users
  - Interface:
    ```go
    type ProviderDetector interface {
        Detect(ctx context.Context, cfg *ProviderConfig) (ProviderHint, error)
    }
    ```
  - Web UI "Test Connection" button triggers detection on demand; displays
    result inline: "✅ Detected: Gemini 2.5 Pro" or "⚠️ Unknown — OpenAI compat"
- Extended thinking (G-11 to G-14, Gemini-specific):
  - G-11: `ThinkingOptimizer` pre-request hook (adaptive vs legacy, Haiku skip)
  - G-12: Thinking Budget Rectifier (error-driven retry, same key, skip adaptive)
  - G-13: Thinking Signature Rectifier (7 error patterns, strip + retry same key)
  - G-14: `internal/shadow/` shadow store (per-session Gemini-native turn history,
    in-memory only; unblocks `thinking_block_signer` and
    `reasoning_content_injector` Rectifier rules)

### Success Criteria

- Full Claude Code agentic session completes against DeepSeek and GLM
  with the same test script used for Gemini validation in Phase 1
- All 4 providers active simultaneously; router correctly dispatches
- Prometheus `/metrics` scrape returns correct per-key counters after a
  request sequence including a 429 retry (verified by integration test)
- `provider_type: auto` correctly identifies Gemini behind Google AI Studio's
  OpenAI-compat endpoint via URL pattern match (no probe call needed)
- Detection failure (unknown endpoint) falls back to `openai_compat` with
  a logged warning; startup completes in < 10 s with all probes in parallel
- `golangci-lint` passes; no new linter suppressions added

### Known Risks / Open Questions

- GLM API documentation quality is lower than Gemini/OpenAI; expect
  undocumented edge cases in tool call format and streaming framing
- DeepSeek and GLM may have their own thinking block conventions that
  differ from both Anthropic and OpenAI formats; Rectifier rules must be
  per-provider, not forced into existing rule shapes
- Rate limit signals for DeepSeek/GLM may use different field names or
  be absent; define a per-provider fallback strategy
- Auto-detection Step 3 (test-message probe) sends a real API call at
  startup; users on strict quota should be able to disable with
  `provider_type: openai_compat` — document this explicitly

**OUT OF SCOPE for Phase 3:** SaaS control plane, semantic routing,
persistent storage, multi-tenant support, fallback chains (Phase 3.5),
smart switch routing matrix (Phase 3.5).

---

## Open Source vs SaaS Feature Boundary

This boundary is strategic, not technical. Any proposal to move a SaaS
feature to open source must be reviewed against the commercial model,
not just technical merit. Document it here so it does not drift.

| Feature | Open Source | SaaS |
|---|---|---|
| All provider Translators (Gemini, OpenAI, DeepSeek, GLM, ...) | ✓ | ✓ |
| Provider auto-detection (startup probe, Phase 3) | ✓ | ✓ |
| KeyPool: QuotaAwareStrategy + ThroughputOptimizedStrategy | ✓ | ✓ |
| Transparent 429 retry + incremental backoff | ✓ | ✓ |
| Rectifier layer (all providers) | ✓ | ✓ |
| Prometheus metrics | ✓ | ✓ |
| Structural signal router (Phase 2+) | ✓ | ✓ |
| Smart Switch routing matrix — structural only (Phase 3.5) | ✓ | ✓ |
| Graceful degradation / fallback chains (Phase 3.5) | ✓ | ✓ |
| Encrypted secret storage (miroxy.s) | ✓ | ✓ |
| Web UI: config, key management, Claude Code Apply/Restore | ✓ | ✓ |
| NLP semantic routing (any tier) | ✗ | ✓ |
| Cross-user learning / aggregate routing intelligence | ✗ | ✓ |
| Real-time provider latency / cost awareness | ✗ | ✓ |
| Cost savings dashboard | ✗ | ✓ |
| Multi-tenant account management or billing | ✗ | ✓ |
| miroxy-operated key pool (user buys credits, no key management) | ✗ | ✓ |

---

## Phase 3.5 — Fallback Chains + Smart Switch Routing

**Goal:** Enable capability-filtered cross-provider degradation and a
full multi-provider structural routing matrix — the two features that
make "multiple providers" mean more than availability.

**Hard prerequisite:** Phase 3 must ship DeepSeek and GLM. Fallback
chains and cross-provider routing are meaningless with a single provider.

### Key Deliverables

- **Per-model capability registry** (declared in config, trusted at runtime):
  - `vision: bool`, `tool_use: bool`, `thinking: bool`
  - `max_context_window: int`, `long_context_threshold: int`
- **Graceful Degradation / Fallback Chains** (`FallbackChain` type):
  - Triggers (priority order):
    1. `quota_exhausted` 429 — daily limit, distinguished from temporary
       rate-limiting by provider error body; temp 429 is handled by KeyPool
       cooldown and does NOT trigger fallback
    2. Circuit breaker open across all keys in a provider pool
    3. Provider returns 502 or 503
  - Before selecting fallback: filter by required capabilities; a vision
    request skips non-vision candidates silently and tries next in chain
  - On successful fallback: emit structured log event
    `{original_provider, fallback_provider, model, reason, latency_ms}`;
    client receives a normal response with zero error signal
  - On full chain exhaustion (all candidates failed or capability-incompatible):
    return 503; never serve a result from an incompatible model
  - DISTINCT from Phase 1 transparent retry:
    retry = same provider, different key;
    fallback = different provider, after all keys exhausted
- **Smart Switch Routing Matrix** (extends Phase 2 structural router):

  | Signal | Preferred model | Detection method |
  |---|---|---|
  | image blocks | gemini-2.5-pro | content block type inspection |
  | token count > 100K | gemini-2.5-pro | tiktoken estimate, in-process |
  | tool definitions | claude-sonnet | `len(req.Tools) > 0` |
  | Plan Mode | deepseek-r1 / claude | header or equivalent signal |
  | Chinese content | deepseek-chat / glm | unicode range U+4E00–U+9FFF |
  | math / reasoning keywords | deepseek-r1 | keyword heuristics |
  | lightweight (short, no tools) | gemini-flash | structural checks |
  | default | gemini-flash | catch-all |

  All signals are structural — zero NLP, zero inference, zero extra API
  calls. Structural signals cover ~80% of real Claude Code usage patterns.
  NLP routing is reserved for SaaS Phase 4+.

- **Composition:** `route(request) → FallbackChain → try primary → degrade`
  Router selects which chain; chain handles provider failure.
  Independent concerns with a clean interface boundary.
- **Multi-agent concurrency hardening:**
  - `sync.Mutex` on `KeyPool.Acquire` (microsecond hold; key selection only)
  - Atomic `InFlight` counter per key with `MaxConcurrent` cap
  - Existing 429 transparent retry as final safety net

### Success Criteria

- Structural router directs Chinese-dominant content to DeepSeek/GLM when
  configured; verified by integration test with 4 providers active
- When Gemini's daily quota is exhausted, a vision request falls back to
  the next vision-capable model in the chain; a text-only candidate is
  skipped; full chain exhaustion returns 503
- Multi-agent burst test: 10 simultaneous requests, 3 keys, no deadlocks;
  all complete within 2× single-request latency

### Known Risks / Open Questions

- Distinguishing "temporary 429" from "quota_exhausted 429" requires
  provider-specific body parsing; treat ambiguous 429 as temporary
- Capability registry is config-declared and trusted; no runtime validation
- Smart switch keyword heuristics will produce false positives on
  math/reasoning; acceptable at Phase 3.5 — NLP routing fixes this in SaaS

**OUT OF SCOPE for Phase 3.5:** NLP routing, per-user preference
learning, real-time provider latency/cost awareness, SaaS billing.

---

## Phase 4 — SaaS Launch + Tier 1 Classifier

**Goal:** Launch miroxy.io as a hosted service with credit-based
billing; add the Tier 1 keyword classifier to begin collecting labeled
routing data as the prerequisite for ML training.

### Key Deliverables

- Closed-source SaaS control plane (separate from open-source core)
- miroxy-operated Gemini paid key pool (not user-supplied keys)
- Credit purchase and usage tracking system
- Authentication: miroxy.io account token replaces local provider keys
- Billing: markup on provider cost; pricing below Anthropic API rates
- miroxy binary update: detect miroxy.io token → route through SaaS
  pool instead of local key pool
- SaaS-specific Rectifier configuration (centrally managed, not per-user)
- **Tier 1 Keyword / Rule Classifier** (SaaS only):
  - Zero ML model — pure deterministic rule-based classification
  - Coverage target: ~75% of Claude Code request patterns
  - Rules: `containsCodePatterns`, `containsMathSymbols`,
    `isChineseDominant`, `isShortAndSimple`, `containsCreativeMarkers`
  - Unclassified (~25%) routes via structural signals
  - Cost: zero (no API calls); latency added: < 1 ms
  - Primary purpose: label routing decisions + collect implicit quality
    signals (retry rate, session continuation) for Tier 2 training data
  - Every routing decision logged: task type, model, outcome signals;
    this dataset is the hard prerequisite for Phase 4.5

### Success Criteria

- User can sign up, purchase credits, paste single token, use Claude Code
  with zero provider key management required
- Usage-based billing accurate to within 1% of actual provider cost
- SaaS backend handles 100 concurrent proxy sessions without degradation
- Tier 1 classifier labels ≥ 75% of production requests; remainder
  routes via structural signals with no quality regression
- 10K labeled requests accumulated; treat as a hard gate before Phase 4.5

### Known Risks / Open Questions

- Revenue model viability depends on Gemini paid tier pricing remaining
  below Anthropic API pricing; needs ongoing monitoring
- Regulatory: verify provider terms of service regarding resale and
  sublicensing before launch
- Open-source core must remain fully functional without the SaaS control
  plane; no feature flag entanglement between tiers
- User trust: communicate that the SaaS does not custody or re-use prompts
- **Multi-instance key pool contention (SEVERE if ignored):** Multiple
  miroxy processes on the SaaS backend compete for keys. Current
  mitigation: transparent 429 retry handles incidental contention at
  < 500 ms latency. If monitoring shows 429 retry rate > 5%, evaluate
  key sharding per instance before introducing any shared coordination
  layer (Redis or equivalent). Do NOT pre-optimize. YAGNI.

**OUT OF SCOPE for Phase 4:** Multi-provider SaaS pools (Gemini first),
enterprise SSO, Tier 2 ML classifier (hard prerequisite: 10K labeled
requests), multi-tenancy beyond credit accounts.

---

## Phase 4.5 — Tier 2 Local ML Classifier

**Goal:** Replace the Tier 1 rule classifier with a lightweight local
ML model trained on miroxy's own production traffic, improving routing
coverage from ~75% to > 90% on nuanced task types.

**HARD PREREQUISITE:** Minimum 10K labeled requests from Tier 1
production traffic. This is a data dependency, not a guideline. Training
on synthetic or general NLP data will produce a classifier that does not
match Claude Code traffic distribution and will perform worse than the
rule classifier. Do not begin Phase 4.5 before this volume is confirmed.

### Key Deliverables

- Evaluate and select model (priority order):
  1. **fastText (Meta):** ~5 MB, < 2 ms inference, runs in-process,
     zero network hop, no new runtime dependency. First choice.
  2. **ONNX distilBERT:** ~50 MB, ~5 ms inference, higher accuracy on
     nuanced tasks. Use only if fastText accuracy is insufficient on
     miroxy traffic.
  3. Custom classifier on miroxy-only data. Preferred long-term target.
- Training pipeline: Tier 1 labeled logs → feature extraction → training
  → offline evaluation → A/B shadow mode (2 weeks) → cutover
- Accuracy target: > 90% on held-out miroxy traffic sample
- A/B shadow: run Tier 2 in parallel with Tier 1 for 2 weeks before
  full cutover; divergence should show consistent Tier 2 improvement

### Success Criteria

- Tier 2 achieves > 90% accuracy on held-out miroxy traffic sample
- Classification latency < 5 ms P99 (negligible effect on proxy P99)
- A/B shadow: Tier 2 agrees with Tier 1 on ≥ 85% of requests
- No new Python or Node dependency; model runs in-process

### Known Risks / Open Questions

- fastText may not capture domain-specific vocabulary without fine-tuning
  on miroxy traffic; evaluate on real data, not generic NLP benchmarks
- SEVERE: the 10K labeled request gate must be enforced; a classifier
  trained on insufficient data will underperform Tier 1
- Model distribution shift requires planned periodic retraining cadence

**OUT OF SCOPE for Phase 4.5:** Per-user preference learning, real-time
provider latency/cost signals, feedback loop (Phase 5), open-source
release of the classifier.

---

## Phase 5 — Self-Improving Classifier + Mature SaaS

**Goal:** Close the feedback loop so routing quality improves
continuously from production signals; deliver the cost savings dashboard
as the primary SaaS retention and conversion tool.

### Key Deliverables

- **Feedback loop:**
  - Implicit signals: retry rate, session continuation, error rate per
    routing decision — collected from existing telemetry
  - Explicit signals: thumbs up/down in SaaS UI (optional, low friction)
  - Periodic retraining pipeline: new labeled data → retrain → offline
    evaluation → staged rollout; fully automated
- **Per-user preference learning:**
  - Routing profile built per user from their own session history
  - Preference factored into routing without explicit configuration
  - Privacy: profile is per-user, never shared without consent
- **Real-time provider signals** (SaaS only):
  - Provider latency: continuously measured, factored into routing
  - Provider cost: dynamic pricing awareness
  - Cross-user aggregate routing intelligence (anonymized)
- **Cost savings dashboard** (first-class deliverable):
  Ship before the classifier is fully mature. Users who see concrete
  dollar savings retain and refer. Display from Phase 5 day one.
  Calculation accurate to within 2% of actual provider billing.

  ```
  Today's request distribution:
    47% → Gemini Flash     (simple tasks)
    31% → Claude Sonnet    (agentic / coding)
    15% → DeepSeek R1      (reasoning)
     7% → Gemini Pro       (long context)
  Estimated savings vs all-Sonnet: $12.30 today
  ```

- Multi-provider SaaS pools: extend miroxy-operated key pool to
  DeepSeek and GLM

### Success Criteria

- Routing quality shows measurable improvement over 30-day window:
  429 retry rate < 5%, error rate < 2%, trending downward
- Per-user preference profiles demonstrably improve task match for users
  with > 50 sessions of history (A/B: profile-on vs profile-off)
- Cost savings dashboard live from Phase 5 launch; accurate to within 2%
- SaaS handles 1000 concurrent proxy sessions; P99 proxy latency < 500 ms
  exclusive of upstream provider latency

### Known Risks / Open Questions

- Feedback loop bias: routing errors clustering around specific task types
  will be amplified by retraining; requires a held-out evaluation set
  not influenced by the current model's routing decisions
- SEVERE: per-user preference learning requires robust user isolation;
  leaking one user's signals into another's profile is a privacy violation,
  not a quality bug — treat it accordingly
- Dynamic pricing awareness requires reliable provider pricing signals;
  providers change pricing without notice

**OUT OF SCOPE for Phase 5:** Enterprise SSO, team/org accounts,
on-premise SaaS deployment, open-source release of classifier or
feedback loop.
