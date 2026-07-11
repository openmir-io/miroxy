# miroxy vs claude-code-router vs cc-switch

A thorough comparison to help decide whether miroxy development is worth continuing.

---

## TL;DR

They solve different problems. miroxy is a **quota multiplier** (pool multiple API keys for higher throughput). claude-code-router is a **request router** (send different request types to different models). cc-switch is a **provider manager** (GUI to switch which backend your tools use). There is real overlap, but miroxy's core value proposition — horizontal scaling through key pooling — is not covered by either alternative.

---

## The Three Tools at a Glance

| | miroxy | claude-code-router | cc-switch |
|---|---|---|---|
| Language | Go | TypeScript / Node.js | Rust + React (Tauri desktop) |
| Interface | Headless HTTP proxy | Headless HTTP proxy | Desktop GUI + optional proxy |
| Binary / install | Single static binary | npm install -g | Platform installer (.deb/.dmg/.msi) |
| Runtime deps | None | Node.js 20 + ~40 npm packages | None (bundled) |
| Providers | Gemini (v1), extensible | 12+ (any OpenAI-compatible) | 50+ presets, 7 app integrations |
| Key pooling | Yes — the core feature | No | No |
| Request routing | No | Yes — token-count + scenario | Basic failover only |
| Config | YAML file | JSON5 file | SQLite + GUI |
| Tests | Yes (unit + integration) | None | Yes (vitest + cargo test) |

---

## Detailed Architecture

### miroxy

```
Claude Code
    │  POST /v1/messages
    ▼
[Auth validator]
    │
[Model lookup] ──► config.yaml
    │
[KeyPool.Acquire]
  round_robin | least_requests
  sliding-window RPM check
    │
[Translator.ToUpstream]
  Anthropic → Gemini request format
    │
[Gemini API] ◄── per-key HTTP call
    │
[Translator.FromUpstream]
  Gemini → Anthropic response/SSE format
    │
[KeyPool.Release]
  escalating 429 backoff (10→30→60→120→300s)
  circuit-break on 5xx threshold
    │
Claude Code ◄── Anthropic-shaped response
```

The `KeyPool` is the central abstraction. Multiple API keys share a pool per model entry. The pool tracks: in-flight concurrency per key, sliding-window request counts (proactive soft rotation before hitting the cap), per-key 429 failure tier (escalating with authoritative `retryDelay` parsing from the Gemini 429 body), and circuit-break state for 5xx failures. Streaming and non-streaming are separate code paths; streaming retries up to 3 times with a different key before committing any SSE headers.

---

### claude-code-router (CCR)

```
Claude Code
    │  POST /v1/messages
    ▼
[Auth] ── optional APIKEY check
    │
[Router] ◄── tiktoken token count
  scenario: background / think / longContext
             webSearch / image / default
    │
[Provider + Model] selected from Router config
    │
[Agent intercept?] ── image agent, tool-call intercept
    │
[Transformer chain — request]
  GeminiTransformer, DeepseekTransformer,
  MaxTokenTransformer, ReasoningTransformer …
    │
[Upstream HTTP] ──► provider base_url
    │
[Transformer chain — response]
    │
Claude Code ◄── Anthropic-shaped response
```

CCR's core idea is **semantic routing** — it counts tokens in the incoming request and classifies the task (quick background op, long-context, reasoning, web-search, image), then dispatches to a different model for each class. A `Router` config maps scenario names to `"provider,model"` strings. This is pure quality and cost optimization: cheap/fast model for background tasks, powerful model for hard reasoning. It has 20+ provider-specific transformers, a preset-sharing ecosystem with marketplace distribution, a CLI for service management, and a web UI. There is no key pooling; one key per provider, no rotation.

---

### cc-switch

```
-- Primary mode: config-file management --

GUI action (switch provider)
    │
[Provider service] ── reads from SQLite
    │
Writes live config files:
  ~/.claude/settings.json
  ~/.codex/config.toml + auth.json
  ~/.config/gemini/config.json
  ~/.config/opencode/opencode.json
  … (7 app targets)

-- Optional mode: local proxy --

Claude Code
    │  HTTP → 127.0.0.1:15721
    ▼
[Handler context] ── detect app type, auth strategy
    │
[Provider router] ── circuit breaker per provider
    │
[Format adapter]
  Anthropic ↔ OpenAI Chat ↔ OpenAI Responses ↔ Gemini native
    │
[Forwarder] ── reqwest, 3–6 retries, exponential backoff
    │
[Upstream API]
    │
[Response processor] ── parse usage, stream conversion
    │
[Usage logger] ──► SQLite (cost, tokens, latency)
    │
Claude Code ◄── response
```

cc-switch is fundamentally a **configuration manager**, not just a proxy. Its primary job is writing to `~/.claude/settings.json` and equivalent files for 7 tools simultaneously. The proxy is an optional bolt-on. It has 50+ provider presets, session history browsing, usage dashboards with cost charts, MCP server syncing, deep-link import (`ccswitch://...`), and cloud backup (WebDAV / S3). Provider management is one-at-a-time — no key pooling, just a sequential failover queue.

---

## Feature-by-Feature Comparison

### Key Rotation & Quota Management

| Feature | miroxy | CCR | cc-switch |
|---|---|---|---|
| Multiple keys per provider | **Yes** | No | No |
| Round-robin / least-in-flight | **Yes** | — | — |
| Sliding-window RPM cap per key | **Yes** | — | — |
| Proactive soft rotation (before hitting limit) | **Yes** | — | — |
| 429 escalating backoff | **Yes** (10→30→60→120→300s) | — | 60s flat |
| Authoritative `retryDelay` parsing | **Yes** (Gemini body) | — | No |
| Circuit breaker (5xx) | **Yes** | — | Yes (per-provider) |
| Streaming retry with key rotation | **Yes** (3 attempts pre-header) | No | No |

This is where miroxy is uniquely positioned. Neither alternative does key pooling. If your goal is squeezing maximum throughput from multiple free-tier Gemini accounts (each capped at ~15 RPM), miroxy is the only option here.

---

### Provider & Format Support

| Feature | miroxy | CCR | cc-switch |
|---|---|---|---|
| Providers supported today | Gemini only | 12+ (OpenAI-compatible) | 50+ presets |
| Adding a new provider | 1 file implementing `Translator` | Config entry + transformer | GUI or deep link |
| Gemini native format | **Yes** (hand-written, correct) | Via GeminiTransformer | Yes |
| OpenAI Chat format | No (planned) | Yes | Yes |
| Anthropic (pass-through) | — | Yes | Yes |
| VertexAI | No | Yes (transformer) | Via presets |
| DeepSeek reasoning | No | Yes | Via presets |
| Ollama / local models | No | Yes | Yes |
| Tool call translation | Partial (deferred) | Yes | Yes |

CCR and cc-switch win on breadth. miroxy's translator is narrow (Gemini only) but deep — the streaming translation, tool-call format mapping, and system-prompt handling are implemented correctly from spec. Breadth is an explicit v2 concern.

---

### Request Routing Intelligence

| Feature | miroxy | CCR | cc-switch |
|---|---|---|---|
| Per-request model selection | No (client chooses) | **Yes** — token + scenario | No |
| Background task routing | No | **Yes** | No |
| Long-context threshold routing | No | **Yes** (configurable token count) | No |
| Reasoning task routing | No | **Yes** | No |
| Image routing | No | **Yes** (image agent) | No |
| Project-level config override | No | **Yes** (`~/.claude/projects/<id>`) | Partial |
| Transformer pipeline | No | **Yes** (20+ transformers, chainable) | No |

CCR wins here by design. Its routing system is the core differentiator: transparent to the client, automatically selects cheap models for cheap tasks and powerful models for hard ones.

---

### Streaming

| Feature | miroxy | CCR | cc-switch |
|---|---|---|---|
| SSE streaming | Yes | Yes | Yes |
| Streaming retry on 429 (pre-header) | **Yes** | No | No |
| Anthropic SSE format emitted | Yes | Yes | Yes |
| Mid-stream format conversion | Yes | Yes (transformers) | Yes |
| Tool-call interception mid-stream | No | **Yes** (agent system) | No |
| First-byte latency tracking | No | No | Yes |

---

### Observability & Data

| Feature | miroxy | CCR | cc-switch |
|---|---|---|---|
| Structured request logging | Yes (slog, JSON) | Yes (pino, rotating) | Yes (SQLite rows) |
| Usage / cost tracking | No | Session LRU cache | **Full** (dashboard + charts) |
| Per-key metrics | No (Prometheus deferred) | No | No |
| Session history browser | No | No | **Yes** |
| Cloud sync (WebDAV/S3) | No | No | **Yes** |

---

### Developer & Operator Experience

| Feature | miroxy | CCR | cc-switch |
|---|---|---|---|
| Install complexity | `go build` → 1 binary | `npm install -g` | Platform installer |
| Docker | Yes (Dockerfile included) | Yes (multi-stage) | No (desktop app) |
| Config change | Edit YAML, restart | Edit JSON5, auto-reload | GUI, instant |
| Tests | **Yes** (unit + integration) | **None** | Yes |
| Web UI | No | Yes | Yes (native app) |
| Preset / config sharing | No | **Yes** (marketplace) | **Yes** (deep link, presets) |
| MCP server management | No | No | **Yes** |

---

## Overlap Map

```
          ┌─────────────────────────────────────────────────────┐
          │           Sit between Claude Code and upstream       │
          │      Expose Anthropic-compatible HTTP endpoint       │
          │               Handle SSE streaming                   │
          │        Let you use non-Anthropic providers           │
          └───────────────────┬─────────────────────────────────┘
                              │
          ┌───────────────────┼───────────────────┐
          │                   │                   │
     ┌────▼────┐        ┌─────▼─────┐       ┌────▼────┐
     │miroxy  │        │    CCR    │       │cc-switch│
     │         │        │           │       │         │
     │Key pool │        │Routing by │       │Desktop  │
     │Quota    │        │token count│       │GUI      │
     │multi-   │        │Transformer│       │7 apps   │
     │plication│        │pipeline   │       │Session  │
     │         │        │Preset mkt │       │history  │
     │         │        │Agent sys  │       │Usage    │
     │         │        │           │       │dashboard│
     └─────────┘        └─────────┘        └─────────┘

  Headless, server-  ◄────────────────► Desktop / GUI-first
  deployable proxies              Headless proxy optional
```

The functional overlap is real but narrower than it looks. All three sit in front of Claude Code. Beyond that the goals diverge sharply.

---

## Honest Assessment

### The bet miroxy is making

Gemini free tier gives ~15 RPM per API key. Claude Code generates bursts of 5–10 requests per minute during a heavy session. One key means constant throttling. Three keys with smart rotation gives ~45 RPM effective throughput and near-zero throttling in practice. **This is the entire reason miroxy exists.** The 429 errors in the logs that prompted several of our recent sessions are direct evidence this problem is real.

Neither CCR nor cc-switch addresses this. CCR assumes one sufficient key. cc-switch's failover queue is sequential — it switches to the next provider when one fails, not distributes load across keys of the same provider simultaneously.

### When to use miroxy

- You have 2+ Gemini API keys (free-tier or otherwise) and want to use them concurrently
- You hit 429s regularly with Claude Code on a single key
- You want a single static binary deployable anywhere with no runtime
- You want correct, tested Anthropic↔Gemini protocol translation
- You run headless (VPS, container, CI)

### When to use CCR instead

- You have one provider key with sufficient quota
- You want different models for different task types without changing Claude Code config
- You want OpenRouter, DeepSeek, Ollama, or VertexAI support today
- You want a preset marketplace and a web UI for managing config
- You already run Node.js in your stack

### When to use cc-switch instead

- You use more than one tool (Codex, Gemini CLI, OpenCode, etc.) and want to manage all from one place
- You want a GUI rather than editing config files
- You care about session history browsing, usage dashboards, and cost charts
- You want MCP server syncing across tools
- You prefer a native desktop app to a background HTTP daemon

---

## What miroxy Should Not Become

Looking at CCR and cc-switch, the temptation is to add: a web UI, 20-provider support, intelligent routing, and session history. Resist all of it. Both alternatives exist and do those things well. Adding them to miroxy would produce an inferior third clone with a smaller user base and more maintenance surface.

miroxy's defensible territory is key pooling with quota-aware rotation. The hard constraints in `CLAUDE.md` (no management API, no DB, no third-party HTTP framework) are correct precisely because they prevent drift into what CCR and cc-switch already do.

---

## Gaps in miroxy That Are Worth Filling

These are things neither CCR nor cc-switch have properly, that fit within miroxy's scope:

### 1. Tool call translation (blocking for agentic use)
The Anthropic tool-call format (`tool_use` content block → `tool_result`) needs full round-trip translation to Gemini's function-calling format. Without this, Claude Code's agentic workflows (file editing, bash execution) don't work reliably. This is marked deferred but is the primary functional gap.

### 2. Prometheus metrics
Per-key counters: `requests_total`, `requests_in_flight`, `failures_total`, `rate_limit_cooldowns_total`, `cooldown_duration_seconds`. This is the operational view needed for anyone running miroxy in production. Neither CCR nor cc-switch expose Prometheus-compatible metrics.

### 3. `/v1/keys/status` read-only endpoint
A debug endpoint showing each key's current state: `{ id, state, cooldown_until, rl_failures, in_flight, requests_in_window }`. Lets operators see what the pool is doing without digging through structured logs.

### 4. Long-cooldown quota detection
Currently the escalating backoff caps at 300s. If Gemini returns a `retryDelay` of `156h14m36s` (a daily quota exhaustion), the key should be circuit-broken for that model until the window resets — not retried every 5 minutes. The fix: if parsed `retryDelay > 1h`, treat as quota exhaustion and apply a full `coolEnd = now + retryDelay` instead of the escalating schedule.

---

## Summary Table

| Question | Answer |
|---|---|
| Is miroxy a duplicate of CCR? | No — CCR has no key pooling; different core problem |
| Is miroxy a duplicate of cc-switch? | No — cc-switch is a GUI config manager; different delivery model |
| Is there overlap? | Yes — all three proxy Claude Code to non-Anthropic backends |
| Is the overlap a reason to stop? | No — the overlap is the baseline; miroxy's unique value is above it |
| Should miroxy add routing like CCR? | No — out of scope, CCR already does it |
| Should miroxy add a GUI like cc-switch? | No — out of scope, cc-switch already does it |
| What should miroxy finish first? | Tool calls, Prometheus metrics, quota-exhaustion detection |
