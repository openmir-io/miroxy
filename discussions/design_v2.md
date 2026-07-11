You are a senior software architect and technical product manager 
with deep expertise in Go systems programming, LLM API protocols, 
reverse proxy design, and developer tooling commercialization.

Your task is to produce two documents in English for the miroxy 
project based on ALL context below. Be opinionated. Make hard 
prioritization calls. Justify sequencing decisions briefly.

════════════════════════════════════════
PROJECT OVERVIEW
════════════════════════════════════════

miroxy is a lightweight, single-binary Go proxy that translates 
between the Anthropic/Claude API protocol and upstream LLM providers 
(Gemini, OpenAI, DeepSeek, GLM, etc.), enabling Claude Code to use 
cheaper or free alternative models transparently.

Core philosophy:
- Single Go binary, zero runtime dependencies (no Node, no Python, 
  no Redis)
- Direct point-to-point protocol translation (no OpenAI hub 
  intermediary like LiteLLM)
- Embed all UI assets into binary via Go embed (offline capable)
- Lightweight but extensible: clean interfaces, pluggable strategies

════════════════════════════════════════
COMPETITIVE LANDSCAPE
════════════════════════════════════════

vs LiteLLM:
- LiteLLM routes everything through OpenAI format as hub (two 
  translation hops: Anthropic→OpenAI→Gemini)
- miroxy does direct Anthropic↔Provider translation (one hop)
- LiteLLM requires Python + optional Redis; miroxy is single binary
- LiteLLM usage-based-routing tracks its own request count, not 
  real provider quota; miroxy parses Gemini's authoritative 
  retryDelay field from 429 responses

vs CCR (claude-code-router):
- CCR has zero key pooling/rotation capability
- CCR does structural signal routing (token count, tool presence, 
  image detection) NOT true semantic routing
- miroxy should adopt and improve CCR's structural routing approach
- CCR's scenario classification: background, think, longContext, 
  webSearch, image → miroxy should extend this

vs cc-switch:
- cc-switch has deep protocol adaptation (Rectifier concept: 
  actively fixes known cross-protocol field mismatches)
- cc-switch key fixes miroxy must replicate:
  * thinking block signature preservation across multi-turn tool calls
  * tool_call continuity (reasoning_content must persist)
  * cache_control stripping for incompatible providers
  * thinking_rectifier: strip Anthropic thinking blocks before 
    sending to OpenAI-compatible endpoints
- cc-switch requires Node.js runtime; miroxy is single Go binary
- miroxy differentiator: lighter weight + key pooling + 
  quota-aware backoff that cc-switch lacks

════════════════════════════════════════
TECHNICAL ARCHITECTURE
════════════════════════════════════════

Language & Framework:
- Go, net/http + httputil.ReverseProxy
- No ORM, no Redis, no message queue in v1
- Frontend: Alpine.js (embedded via go:embed, ~15KB, offline)

Core Components:

1. KeyPool
   - Interface with pluggable strategies:
     * QuotaAwareStrategy: for free-tier keys
       - Incremental backoff: 10→30→60→120→300s
       - Parse Gemini's authoritative retryDelay from 429 body
       - Distinguish "rate limited" vs "quota exhausted"
     * ThroughputOptimizedStrategy: for paid keys
       - Round-robin / least-in-flight
       - No conservative backoff needed
   - Per-provider key pools, strategies configured independently

2. Translator interface (per provider file)
   type Translator interface {
       ToUpstream(req *AnthropicRequest) (*http.Request, error)
       FromUpstream(resp *http.Response) (*AnthropicResponse, error)
       StreamFromUpstream(resp *http.Response) 
           (<-chan AnthropicSSEEvent, error)
   }
   Files: translator/gemini.go, translator/openai.go, 
          translator/deepseek.go, translator/glm.go

3. Rectifier layer (inspired by cc-switch)
   - Sits between Translator and upstream dispatch
   - Actively fixes known cross-protocol mismatches:
     * thinking block signature preservation
     * reasoning_content injection for multi-turn tool calls
     * cache_control stripping per provider capability
     * tool_call ID format normalization
   - Per-provider rectifier rules, not global

4. Router layer (inspired by CCR structural routing)
   - Classifies requests by structural signals (not NLP):
     * token count → longContext route
     * tool definitions present → coding/agentic route
     * no tools + short prompt → lightweight route
     * Plan Mode signal → think route
   - Routes to appropriate provider+model combination
   - Configured via miroxy.config routing rules

5. Config & Secret Storage
   miroxy.config (plaintext):
   - port, routing rules, model mappings, provider settings
   - path reference to miroxy.s

   miroxy.s (encrypted):
   - File header: magic "MIROXY" + version + mode + salt + nonce
   - Payload: AES-GCM encrypted JSON of all provider tokens
   - Two unlock modes:
     * password mode: argon2id key derivation from user password
     * autokey mode: random 32-byte key in ~/.config/miroxy/.key
   - Unlock priority: 
     MIROXY_MASTER_KEY env → user password → autokey file

6. Embedded Web UI (miroxy-ui)
   Single binary includes all frontend assets via go:embed
   UI server: localhost:7778 (config/management)
   Proxy server: localhost:7777 (Claude Code endpoint)
   
   Both start with single command: ./miroxy
   Proxy is inactive until user clicks Start in UI
   
   UI Tabs:
   - Status: proxy Start/Stop, uptime, keys loaded count
   - API Keys: per-provider token management, 
                add/delete/show/hide/import-from-file
   - Routing: routing rule configuration
   - Models: model mapping configuration  
   - Settings:
     * Claude Code integration:
       - Auto-detect platform (macOS/Linux/Windows)
       - One-click Apply: backup + inject apiUrl/authToken 
         into ~/.claude/settings.json (or platform equivalent)
       - One-click Restore: restore from .miroxy.backup
       - Status: Not configured / miroxy active / Modified
     * Security: change password, regenerate autokey

   Backend API endpoints:
   POST /api/proxy/start|stop
   GET  /api/proxy/status
   GET|POST /api/keys, DELETE /api/keys/:id
   POST /api/unlock|lock|rekey
   GET|POST /api/config
   GET /api/claude/status
   POST /api/claude/apply|restore

════════════════════════════════════════
PHASED ROADMAP CONTEXT
════════════════════════════════════════

Phase 1 - Core proxy (reference cc-switch Gemini adaptation):
1. Gemini translator with full field translation
2. Multi-key pool with QuotaAwareStrategy
3. Rectifier layer for known Gemini mismatches
4. Basic CLI operation (config file based)

Phase 2 - OpenAI + Routing + UI:
1. OpenAI translator (reference cc-switch OpenAI adaptation)
2. Structural router inspired by CCR
3. Full embedded Web UI (Alpine.js)
4. miroxy.s encrypted secret storage
5. Usage logging and metrics

Phase 3 - More providers:
1. DeepSeek translator
2. GLM (智谱) translator  
3. Additional providers (reference cc-switch implementations)
4. ThroughputOptimizedStrategy for paid tier keys

════════════════════════════════════════
LONG-TERM VISION
════════════════════════════════════════

Open source strategy:
- Core: MIT/Apache (Translator interface + provider implementations)
- SaaS control plane: closed source

Commercial model:
- Free tier: self-hosted, manage own keys
- Hosted SaaS (miroxy.io):
  * Users buy token credits, no key management needed
  * miroxy operates Gemini paid API key pool
  * Price: significantly cheaper than Anthropic official API
  * Revenue model: markup on Gemini API cost
  * Target: developers who want Claude Code experience cheaply 
    without managing keys themselves
- NOT a key custody service (user keys stay local)
- NOT competing with 1Password (no third-party key management)

════════════════════════════════════════
DELIVERABLES REQUIRED
════════════════════════════════════════

Document 1: Architecture Design Document
- Project purpose and problem statement
- Core architectural principles
- Component breakdown with interfaces/types
- Data flow diagram (ASCII)
- Extension points for future providers
- Non-goals (explicit list)

Document 2: Milestone Document
For each milestone:
- Goal in one sentence
- Key deliverables (bullet list)
- Success criteria (measurable)
- Known risks or open questions

Sequencing rules:
- Phase 1 must deliver a working proxy Claude Code can actually use
- Each phase must be independently shippable
- UI comes in Phase 2, not Phase 1 (core proxy first)
- SaaS comes after open source traction, not before
- Be explicit about what is OUT OF SCOPE for each phase