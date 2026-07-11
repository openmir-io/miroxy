# miroxy — Design Summary & Architecture Decisions

> Last updated: 2026-07
> Status: Architecture finalized, Phase 1 in progress

---

## 1. What is miroxy

miroxy is a lightweight, single-binary LLM proxy written in Go.

It sits transparently between an AI coding client (Claude Code, OpenCode, Cline, etc.)
and upstream LLM providers (Gemini, DeepSeek, Anthropic, GLM, etc.).

```
Claude Code / OpenCode / Cline
        │
   Anthropic protocol (always)
        │
      miroxy
   ┌────┴────────────────────┐
   │  KeyPool + Routing      │
   │  Rectifier + Translator │
   └────┬──────┬─────────────┘
        │      │
      Gemini  DeepSeek  Anthropic  GLM  ...
```

**Name origin:**
- mir = mirror / MIR (Intermediate Representation) / мир (world, peace)
- oxy = proxy
- Pronunciation: meer-oxy

---

## 2. Core Design Principles

```
1. Single Go binary, zero runtime dependencies
   No Python, no Node, no Redis, no database in v1.

2. Anthropic Messages API is the canonical/internal format
   All inbound traffic is Anthropic protocol (Claude Code sends only this).
   Direct Anthropic ↔ Provider translation — no OpenAI hub intermediary.

3. Secrets never in config, always from environment variables
   miroxy.config.yaml contains only ${ENV_VAR} references.
   Files are safe to store in git, ConfigMap, or Docker images.
   Secret management is the user's responsibility (k8s Secret, Vault, .env).

4. Stateless transparent proxy
   miroxy holds no persistent state. Restart resets in-memory KeyPool state.
   This is acceptable — KeyPool state rebuilds in seconds from provider signals.

5. Fail loud, not silently
   503 when all keys exhausted is correct.
   Silent fallback that degrades quality without telling the client is not.

6. Retry before streaming, never after
   429 received before the first SSE byte → transparent retry, client unaware.
   429 after streaming started → cannot retry, must close with error.
   A pool of 20–30 well-managed free-tier keys is sufficient.
   Key count matters less than key freshness and retry speed.

7. Plugin pipeline architecture (Caddy-inspired)
   Every processing step is a Plugin with a priority number.
   Config-driven enable/disable. Community can contribute new plugins.

8. Provider interface (K8s-inspired)
   Every provider implements the Translator interface.
   New provider = new file. Core server never changes.
```

---

## 3. Finalized Architecture

### 3.1 Component Layers

```
┌──────────────────────────────────────────────────────┐
│  Plugin Pipeline (ordered by priority)               │
│                                                      │
│  Auth(10) → Cache(40) → Router(50) →                │
│  KeyPool(60) → Rectifier(70) → Translator(80) →     │
│  Observability(90) → CostTracker(100)               │
└──────────────────────────────────────────────────────┘
         ↓ shared interface
┌──────────────────────────────────────────────────────┐
│  Provider Layer (Translator interface)               │
│  gemini.go / openai.go / deepseek.go / glm.go       │
└──────────────────────────────────────────────────────┘
         ↓ config-driven
┌──────────────────────────────────────────────────────┐
│  miroxy.config.yaml (no secrets, only ${ENV_VAR})   │
└──────────────────────────────────────────────────────┘
```

### 3.2 KeyPool — Two-Layer Strategy

```
Outer layer (across providers):
  fallback       — try targets in priority order
  round_robin    — distribute evenly across providers
  least_requests — send to provider with fewest in-flight requests

Inner layer (within one provider, across keys):
  least_requests — pick key with fewest active in-flight requests
  round_robin    — rotate keys in order

The two layers are completely independent and freely composable.
Example: outer fallback + inner least_requests (most common)
```

### 3.3 Rectifier Layer

Sits between Router and Translator. Fixes known cross-protocol field
mismatches before the request reaches the provider.

```
Implemented (Phase 1, Gemini):
  cache_control_stripper       Gemini rejects cache_control fields
  tool_id_normalizer           Normalize tool call ID format
  batch_tool_filter            Remove Claude Code internal BatchTool
  thought_parts_filter         Strip Gemini thinking parts from response
  finish_reason_mapper         STOP/SAFETY/TOOL_CODE → Anthropic reasons

Phase 3 (thinking-aware):
  thinking_block_signer        Preserve thinking signature across turns
  reasoning_content_injector   Inject reasoning_content for continuity
  thinking_rectifier           Strip thinking blocks for OpenAI endpoints
```

### 3.4 Translation Architecture (LLM-Rosetta inspired)

Internally each Translator is built as an **Ops chain**, not a monolithic
function. Each Op is a small, independently testable transformation:

```go
func (t *GeminiTranslator) ToUpstream(req *AnthropicRequest) (*http.Request, error) {
    return NewPipeline(req).
        Apply(MapSystemPrompt()).
        Apply(MapMessages()).
        Apply(MapTools()).
        Apply(MapGenerationConfig()).
        Apply(FilterBatchTool()).
        Apply(StripCacheControl()).
        Apply(MapToolChoice()).
        Build()
}
```

Shared Ops library: `internal/ops/common/` — reused across all providers.
Provider-specific Ops: `internal/ops/gemini/`, `internal/ops/openai/`, etc.

### 3.5 Three Translator Backend Forms (future extensibility)

```
Form 1 (now):     In-process Go — function call, zero overhead
Form 2 (Phase 5): WASM plugin   — any language, sandboxed, embedded
Form 3 (Phase 4): gRPC sidecar  — separate process, independent lifecycle

Interface is identical for all three forms.
Switching backends requires only config change, not code change.

anthroxy-ir (separate repo):
  Defines ir.proto and AgentService.proto using protobuf.
  Generated Go/Python/TypeScript stubs for all forms.
```

### 3.6 Provider Auto-Detection

When `provider_type: auto` is configured, miroxy probes at startup:

```
Step 1: URL pattern matching (zero-cost, covers known hosts)
Step 2: GET /v1/models endpoint probe (one HTTP call, result cached)
Step 3: Response field fingerprinting (send one probe request)
Fallback: openai_compat mode

Detection runs once at startup. Zero per-request overhead.
```

### 3.7 Ports and Interfaces

```
Port 8080 — Proxy endpoint
  Receives LLM traffic from Claude Code / agents.
  Exposes: POST /v1/messages, GET /v1/models

Port 8090 — Admin endpoint (optional, localhost only)
  Management and observability. Never expose to public internet.
  Exposes:
    GET  /health          liveness probe
    GET  /ready           readiness probe (200 only if routes exist)
    GET  /metrics         Prometheus scrape target
    GET  /stats           lightweight stats snapshot
    GET  /admin/config    view current effective config
    POST /admin/reload    hot-reload local config file
    GET  /admin/routes    view routing table
    GET  /admin/keypools  view keypool health (keys masked)
```

### 3.8 Startup Modes

```
Full mode (with config file):
  miroxy --config /etc/miroxy/config.yaml

Minimal mode (no config, wait for Hub):
  miroxy --admin-port 9090
  /ready returns 503 until routes are available

Hybrid:
  miroxy --config config.yaml --admin-port 9090
```

---

## 4. Config Design

### 4.1 Principles

```
- All secrets are ${ENV_VAR} references, never literal values
- Config file is safe to commit to git
- Three named routing styles (mix freely):
    Style 1: miroxy-* named routes (recommended)
    Style 2: Agent model name mapping (intercept hard-coded model names)
    Style 3: Native model passthrough (add key management on top)
- keypools defined once, referenced by name (keypool_ref)
  Multiple model_list entries sharing a keypool_ref share ONE pool instance
- providers defined once, referenced by name
```

### 4.2 Two-Layer Routing Config

```yaml
# Outer layer: across providers
- model_name: miroxy-code
  routing:
    strategy: fallback          # outer strategy
    targets:
      - provider: anthropic
        provider_model: claude-sonnet-4-6
        keypool_ref: anthropic  # inner strategy defined in keypools block
        timeout_seconds: 60
      - provider: gemini
        provider_model: gemini-2.5-pro
        keypool_ref: gemini-pro
        timeout_seconds: 60

# Inner layer: within provider (defined in keypools)
keypools:
  anthropic:
    strategy: least_requests    # inner strategy, independent of outer
    keys:
      - ${ANTHROPIC_KEY_1}
      - ${ANTHROPIC_KEY_2}
```

### 4.3 Named Routes (miroxy-*)

```
miroxy           Default, balanced cost and quality
miroxy-thinking  Reasoning tasks, DeepSeek R1 first
miroxy-code      Coding/agentic, Claude Sonnet first (best tool_use)
miroxy-docs      Long-context documents, cheap, round-robin
miroxy-fast      Background tasks, cheapest, highest throughput
miroxy-max       Maximum quality, cost no object
```

User selects route via:
```bash
export ANTHROPIC_MODEL=miroxy-code   # in shell
# or in ~/.claude/settings.json:
{"env": {"ANTHROPIC_MODEL": "miroxy-code"}}
```

---

## 5. Open Source Version (miroxy + miroxy-hub OSS)

### 5.1 miroxy (open source core, MIT/Apache)

```
✅ All provider Translators (Gemini, OpenAI, DeepSeek, GLM, ...)
✅ Provider auto-detection (startup-time probe)
✅ KeyPool: QuotaAwareStrategy + ThroughputOptimizedStrategy
✅ Transparent 429 retry + incremental backoff (10→30→60→120→300s)
✅ Parse Gemini's authoritative retryDelay field from 429 body
✅ Rectifier layer (all providers)
✅ Structural signal router (8 signals)
✅ Named routes (miroxy-* model names)
✅ Graceful degradation / fallback chains (Phase 3.5)
✅ Plugin pipeline (Caddy-inspired, config-driven)
✅ Encrypted secret storage — REMOVED
     Replaced by: ${ENV_VAR} references + user-managed secrets
✅ Admin port with /health /ready /metrics /stats
✅ Single binary, zero runtime dependencies
✅ Docker / Kubernetes / Helm ready

❌ NLP semantic routing (SaaS only)
❌ Cross-user learning
❌ Real-time provider cost signals
❌ Cost savings dashboard
❌ Multi-tenant management
```

### 5.2 miroxy-hub OSS (open source, 2-instance limit)

**Phase 1 scope (now):**

```
Core purpose: Monitoring + Statistics + Audit + License enforcement

miroxy → Hub communication (ConnectRPC / AgentService):
  Register()       miroxy announces itself on startup
  Heartbeat()      periodic liveness + lightweight stats
  UploadEvents()   usage / audit / error event stream
  Watch()          proto defined, server returns empty stream
                   (config push not implemented yet — YAGNI)

Hub exposes REST API (for humans / Terraform / CLI):
  GET  /v1/instances              registered miroxy instances + health
  GET  /v1/instances/:id/stats    per-instance stats
  GET  /v1/instances/:id/events   per-instance audit log
  GET  /v1/usage                  aggregated usage (tokens, cost estimate)
  GET  /v1/license                license status
  GET  /health
  GET  /metrics

License enforcement:
  Register() checks instance count against license
  OSS limit: 2 miroxy instances
  Rejection message: "upgrade to miroxy-hub Pro for unlimited instances"
  miroxy continues operating in standalone mode if rejected
  (Hub connection is enhancement, not dependency)
```

**Hub UI — two pages:**

```
Page 1: Setup Wizard (config generator)
  Step 1: Select providers (Gemini / DeepSeek / Anthropic / GLM / Ollama)
  Step 2: Configure routes (which miroxy-* routes to enable)
  Step 3: Select deploy target (local / Docker Compose / Kubernetes / Helm)
  Step 4: Download generated files:
    - miroxy.config.yaml
    - .env.example         (variable names only, no values)
    - docker-compose.yaml
    - k8s/configmap.yaml + k8s/secret-template.yaml

Page 2: Dashboard (monitoring)
  - Instance list + health status
  - Usage stats (requests / tokens / by model)
  - KeyPool health (429 rates, cooldown status, keys masked)
  - Audit log viewer
  - License status
```

**Hub data plane (what miroxy uploads):**

```
UploadEvents payload types:
  USAGE    model, provider, tokens_in, tokens_out, latency_ms
  AUDIT    timestamp, model, provider, truncated_prompt_hash
  ERROR    error_type, provider, status_code
  METRIC   keypool health snapshot
  HEALTH   instance uptime, version, active_routes
```

**Config management (OSS phase):**

```
NOT implemented in Phase 1:
  ❌ Hub pushes config to miroxy
  ❌ Centralized route management
  ❌ Config version history

Rationale:
  - User tokens don't change frequently
  - YAML file is sufficient for self-hosted users
  - No real customer demand yet — YAGNI

Architecture is ready for it:
  - Watch() stream proto is defined
  - miroxy receives events but logs "not implemented" and ignores
  - When a real customer asks, filling in the handler takes 1-2 days
```

### 5.3 Agent Protocol (miroxy ↔ Hub)

```
Protocol: ConnectRPC (not REST)
Rationale: miroxy is an Agent, not a managed resource

Agent behavior:
  - miroxy initiates connection to Hub (not the other way)
  - Hub never needs to know miroxy's IP address
  - Long-lived streaming connection
  - Auto-reconnect with exponential backoff

Why ConnectRPC over REST:
  - Register/Heartbeat/Watch are RPC semantics, not CRUD
  - Streaming UploadEvents needs backpressure (REST polling doesn't)
  - Same protocol reused: miroxy→Hub and Hub→Cloud (future)

Proto location: anthroxy-ir repo (separate, shared)
  proto/miroxy/v1/agent.proto    AgentService definition
  proto/miroxy/v1/admin.proto    Admin REST-ish endpoints (ConnectRPC)
  proto/miroxy/v1/ir.proto       Intermediate representation types
```

---

## 6. Commercial Version (miroxy-hub Pro / miroxy.io)

### 6.1 miroxy-hub Pro (on-premise enterprise)

```
Everything in OSS, plus:

Instance management:
  ✅ Unlimited miroxy instances
  ✅ Instance groups / environments (dev / staging / prod)
  ✅ Per-instance and per-group config management

Config management (Phase 2 Hub):
  ✅ Hub pushes route config to miroxy instances via Watch()
  ✅ Config version history + diff view
  ✅ Config change approval workflow
  ✅ Canary push (push to 1 instance first, validate, then rollout)
  ✅ One-click rollback
  ✅ Git webhook integration (CI/CD pushes config → Hub → miroxy)

Advanced monitoring:
  ✅ Multi-instance aggregated dashboards
  ✅ Cost tracking with real provider pricing
  ✅ Budget alerts (email / Slack / webhook)
  ✅ SLA reporting

Enterprise features:
  ✅ SSO / SAML integration
  ✅ RBAC (who can change routes, who can only view)
  ✅ Audit log export (SIEM integration)
  ✅ Multi-tenant (different teams, different key pools)

License model: per-seat or per-instance subscription
```

### 6.2 miroxy.io (hosted SaaS)

```
Target user: developer who wants Claude Code experience
             cheaply without managing keys or infrastructure

Value proposition:
  - User registers at miroxy.io, buys token credits
  - Sets ANTHROPIC_BASE_URL=https://api.miroxy.io
  - Gets one auth token, no provider keys needed
  - miroxy.io operates its own Gemini paid API key pool
  - Pricing: significantly below Anthropic official rates
  - No key management overhead

Revenue model:
  - Markup on Gemini API cost (provider: Gemini paid tier)
  - Not reselling user keys (legal and safe)
  - Not custody of user keys (anthroxy.s design abandoned — users manage own keys)

SaaS-only features:
  ✅ NLP semantic routing (Tier 1 → Tier 2 → Tier 3)
  ✅ Per-user routing preference learning
  ✅ Real-time provider latency + cost signals
  ✅ Cost savings dashboard ("saved $12.30 vs all-Sonnet today")
  ✅ miroxy.io-operated key pool (no user key management)
  ✅ Multi-tenant accounts + usage-based billing

NLP routing tiers:
  Tier 1 (Phase 4, SaaS launch):
    Keyword/rule classifier, ~75% accuracy, zero ML cost
    Purpose: collect labeled data for Tier 2

  Tier 2 (Phase 4.5, after 10K labeled requests):
    fastText or ONNX distilBERT, <5ms inference, in-process
    Domain-specific model trained on miroxy traffic
    Target accuracy: >90%

  Tier 3 (Phase 5, mature SaaS):
    Self-improving classifier with implicit feedback loop
    Per-user preference learning
    Real-time provider health and cost signals
```

### 6.3 Cloud topology (future)

```
Two supported topologies:

Topology A: miroxy → miroxy.io (simplest)
  Small teams. miroxy talks directly to cloud.
  No hub needed.

Topology B: miroxy → Hub → miroxy.io (enterprise)
  Hub aggregates audit data before upload.
  Only Hub needs internet access.
  Supports data residency requirements:
    Hub can filter what goes to cloud (compliance).
    Audit logs stay on-premise, only usage stats go to cloud.

Agent protocol is topology-agnostic:
  miroxy uses AgentService to connect to Hub.
  Hub uses same AgentService to connect to Cloud.
  Same proto, same SDK, different endpoint.

Future Cloud proto:
  service AgentService {
    rpc Register(...)
    rpc Heartbeat(...)
    rpc UploadEvents(...)    USAGE / AUDIT / METRIC / HEALTH / LICENSE
    rpc Watch(...)           LICENSE / CONFIG / UPGRADE notifications
  }
```

---

## 7. Open Source Strategy

```
Open source (MIT/Apache):
  - miroxy core binary (all Translator + Rectifier + KeyPool + Plugin code)
  - miroxy-hub OSS (2-instance limit)
  - anthroxy-ir proto definitions
  - Official Helm chart
  - Docker images

Closed source:
  - miroxy-hub Pro (enterprise features)
  - miroxy.io SaaS control plane
  - NLP routing classifiers

Why this boundary:
  - Core is open → community contributes new providers/plugins
  - Hub OSS → lowers adoption barrier, drives organic growth
  - Enterprise features stay closed → commercial moat
  - NLP routing stays closed → primary SaaS differentiator

Reference models:
  - LiteLLM: open proxy + closed Enterprise (license key + SLA)
  - OpenRouter: open API standard + hosted SaaS (5% markup)
  - miroxy: open proxy + open Hub OSS + closed Hub Pro + hosted SaaS
```

---

## 8. Competitive Position

```
vs LiteLLM:
  ✅ Single Go binary (LiteLLM needs Python + optional Redis)
  ✅ Direct Anthropic↔Provider translation (LiteLLM routes via OpenAI hub)
  ✅ Quota-aware backoff using provider's authoritative retryDelay
  ✅ Rectifier layer for Claude Code specific protocol fixes
  ❌ LiteLLM has 100+ providers (miroxy has 6 in Phase 3)

vs CCR (claude-code-router):
  ✅ Real key pooling and rotation (CCR has zero key pooling)
  ✅ Transparent 429 retry (CCR has none)
  ✅ Graceful degradation / fallback chains
  ❌ CCR has structural routing (miroxy adds this in Phase 2)

vs cc-switch:
  ✅ Single binary, zero runtime dependencies (cc-switch needs Node.js)
  ✅ Key pool + quota-aware backoff (cc-switch has none)
  ✅ Equal or better Rectifier depth
  ❌ cc-switch supports more client types (Codex, Gemini CLI)

vs Helicone:
  Different market. Helicone = observability (pay to see your LLM calls).
  miroxy = proxy (pay to use cheaper models via your own keys).

vs Bifrost:
  Both Go, both high-performance.
  miroxy differentiator: Claude Code deep adaptation (Rectifier layer).
  Watch this space — closest technical competitor.

Unique combination no competitor has:
  ✅ Claude Code deep protocol adaptation (Rectifier)
  ✅ Quota-aware key pool with authoritative retryDelay parsing
  ✅ Single Go binary, offline capable
  ✅ Named semantic routes (miroxy-code, miroxy-thinking, etc.)
  ✅ Hub for monitoring + future enterprise config management
```

---

## 9. Phased Roadmap Summary

```
Phase 1 (DONE / IN PROGRESS):
  Gemini proxy with full protocol correctness
  G-01 to G-10 Gemini adapter fixes
  Transparent 429 retry (5 integration tests)
  Rectifier interface + cache_control_stripper
  Prometheus metrics

Phase 2:
  OpenAI translator
  OpenAI Rectifier rules
  Structural router (8 signals)
  Embedded Web UI (Alpine.js, go:embed)
  Admin port (/health /ready /metrics /stats)
  Claude Code one-click Apply/Restore settings.json

Phase 3:
  DeepSeek translator (OpenAI + Anthropic endpoints)
  GLM translator
  Provider auto-detection (startup probe)
  reasoning_content dual-mode handling (R1)

Phase 3.5:
  Graceful degradation / fallback chains (cross-provider)
  Smart Switch routing matrix (structural signals → provider)
  Multi-agent concurrency (atomic InFlight + MaxConcurrent)

Phase 4 (SaaS launch):
  miroxy-hub OSS (2-instance, AgentService, Dashboard, Setup Wizard)
  miroxy.io SaaS (Gemini key pool, credit billing)
  NLP routing Tier 1 (keyword classifier, data collection)

Phase 4.5:
  NLP routing Tier 2 (fastText/ONNX, domain-specific model)
  Prerequisite: 10K labeled requests from Tier 1 production

Phase 5:
  NLP routing Tier 3 (self-improving, feedback loop)
  Cost savings dashboard
  Hub Pro enterprise features
  Hub → Cloud topology
  WASM plugin backend (if community demand)
```

---

## 10. Key Decisions Log

```
Decision: Anthropic protocol as canonical format (not OpenAI)
Rationale: Claude Code is the only inbound client. Anthropic protocol
           has the richest expressiveness (thinking blocks, cache_control).
           Direct translation means one hop not two.

Decision: Drop anthroxy.s encrypted storage
Rationale: Secret management belongs to the user's infrastructure.
           ${ENV_VAR} references make config files safe for git/ConfigMap.
           k8s Secrets, Vault, .env files handle this better than we can.

Decision: Hub does NOT push config in Phase 1
Rationale: User tokens don't change frequently. YAML file is sufficient.
           No real customer demand. Watch() proto defined, handler deferred.
           When needed: 1-2 days to implement, backward compatible.

Decision: miroxy continues operating if Hub rejects registration
Rationale: Hub is an enhancement (monitoring/stats), not a dependency.
           Data plane must not depend on control plane availability.

Decision: NLP routing is SaaS-only
Rationale: Open source users can configure structural rules manually.
           NLP routing is the primary commercial differentiator.
           Releasing it open source removes upgrade incentive.

Decision: Three Translator backend forms (in-process / WASM / gRPC)
Rationale: Interface is stable now. In-process Go for Phase 1-4.
           WASM and gRPC sidecar available when community needs them.
           Adding new backend form = new file, no interface change.

Decision: Admin port separate from proxy port
Rationale: Different security boundary. Proxy port is public-facing.
           Admin port is localhost-only internal management.
           Never expose admin port to the internet.

Decision: Multi-instance key pool contention handled by transparent 429 retry
Rationale: Current scale doesn't justify cross-instance coordination.
           If SaaS monitoring shows retry rate > 5%, evaluate key sharding.
           Do not pre-optimize (YAGNI).

Decision: keypools block in config (Terraform-style named resources)
Rationale: Multiple model_list entries sharing keypool_ref share ONE pool
           instance — in-flight counts and 429 cooldowns are accurate
           across all models. Inline pools create separate instances,
           breaking cross-model quota awareness.
```