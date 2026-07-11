# miroxy — Development Log

---

## [2026-07-06] — Architecture pivot: no embedded UI, full provider system, passthrough routing

### Completed

#### refactor: remove embedded UI and setup mode
- Deleted `internal/server/ui/` (index.html, app.js, style.css) and `internal/server/setup.go`.
- `serve.go`: missing config → fatal error with copy instruction; no longer enters setup mode.
- Architecture decision: miroxy is a single binary with YAML config; UI will be a separate `miroxy-hub` process.

#### feat: model_list → model_routes rename
- Bulk rename across all Go source, YAML, tests, and docs.
- No behaviour change; naming is now consistent with "routes" vocabulary used in proxy/gateway projects.

#### feat: model_discovery config switch (strict | auto)
- `server.model_discovery: auto` (default) — on startup, calls Anthropic or OpenAI `/v1/models` if a provider-tagged keypool exists; discovered models are injected into in-memory `ModelRoutes`.
- `server.model_discovery: strict` — only explicitly configured `model_routes` are exposed; no outbound API calls at startup.

#### feat: Claude Code gateway discovery — auto-prefix by User-Agent
- `GET /v1/models`: detects `User-Agent: claude-code/*` header; auto-prefixes model IDs with `claude-` so they appear in the Claude Code `/model` picker.
- `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1` must be set in Claude Code settings.
- Other clients (curl, SDK) receive original model names unchanged.

#### feat: LookupModel — longest prefix matching + provider passthrough
- Four-step cascade: (1) exact match, (2) strip `claude-` prefix exact match, (3) longest prefix match with clean boundary (`-` or EOL), (4) provider passthrough.
- Step 3: `haiku` config matches `claude-haiku-4-5-20251001`; `gpt-5.4` matches all `gpt-5.4-*` variants; more specific configs always win.
- Step 4 (passthrough): if model name matches `claude-*` or `gpt-*/o1*/o3*` pattern AND a provider-tagged keypool exists, miroxy creates a synthetic route and uses a pre-built `passthroughSelector` — no need to enumerate model IDs in config.

#### feat: passthrough selectors in routingState
- `routingState.passthroughSelectors map[string]selector.Selector` keyed by provider name.
- Built at startup from keypools tagged `provider: anthropic` or `provider: openai`.
- Enables zero-config routing of any Anthropic or OpenAI model without listing it in `model_routes`.

#### feat: token usage stats (`internal/stats`)
- `Registry` with atomic counters per model route and per key; zero allocation on the hot path.
- Collected in `upstreamExecutor` for both streaming (SSE event channel wrapper) and non-streaming paths.
- Exposed in `GET /stat` JSON (`usage.by_model`) and `:miroxy stats` text output.

#### feat: compression performance stats (headroom-perf style)
- `core/compress/stats.go` already had full stats; wired `*compress.Stats` pointer into `Server.compressStats`.
- `:miroxy stats` now appends a compression performance report (requests, token reduction %, latency p50/p95/max).
- `GET /stat` JSON includes `compress.enabled`, `reduction_pct`, `latency_*_ms`.

#### feat: dump file rotation with timestamp filenames
- `JSONLStore` rotates when file exceeds `max_size_mb` (default 10 MB).
- Rotated files named `dump.jsonl.20260706132950` (UTC timestamp) instead of `.1`, `.2`.
- Pruning: keeps `max_backups` most recent files, deletes oldest by lexicographic sort.
- `reopenIfDeleted()`: if dump file is deleted mid-run, next write auto-recreates it.

#### feat: named key shorthand syntax
- `keys: - my_label: ${ENV_VAR}` single-key mapping format; short and log-friendly.
- Key name appears as `key_id` in 429/circuit-break log lines.
- Duplicate name validation at startup; names must be unique within a pool.

#### feat: provider declaration required + centralized defaults
- `provider_defaults.go`: all built-in provider defaults (base_url, protocol, auth_style) as named Go constants — single source of truth; no more magic strings.
- `validateConfig`: if a model_route references a provider not declared in the `providers` block AND has no explicit `api_base`+`protocol`, startup fails with a clear error.
- `applyConfigDefaults()`: fills `server.port` → 9000, `admin.addr` → 127.0.0.1:9001, `log.file` → `./log/miroxy.log`, `dump.path` → `./log/dump.jsonl`.
- Protocol and auth_style whitelist validation added.

#### feat: `/v1/config` runtime config inspection API
- Four endpoints on admin port (all accept `auth.allowed_keys` Bearer token OR admin session token):
  - `GET /v1/config` — full effective config (defaults resolved, keys masked last-4)
  - `GET /v1/config/providers` — providers block
  - `GET /v1/config/keypools` — keypools (keys masked)
  - `GET /v1/config/routes` — model_routes + providers (includes injected routes)
- `proxyAuthGuard`: middleware that accepts either admin token or proxy auth key.

#### feat: `miroxy config` CLI command (API client mode)
- `miroxy config` — prints full effective config JSON from running instance.
- `miroxy config providers|routes|keypools` — scoped sections.
- `--admin-addr` flag; reads `MIROXY_AUTH_TOKEN` env var as Bearer token.
- `adminGet()` + `resolveAuthToken()` added to CLI HTTP helpers.

#### refactor: remove `:miroxy model` commands
- `:miroxy model show`, `:miroxy model <name>`, `:miroxy model <name> <q>` removed from command plugin.
- Native `/model` picker in Claude Code and Codex (via gateway discovery) replaces this functionality.
- `ModelInfoText()`, `ModelNames()`, `IsValidModel()` removed from `ServerRef` interface and `Server`.

#### docs: multiple config.yaml.example files
- `config.yaml.example.min` — minimal 1-key 1-pool 1-route
- `config.yaml.example.n-keys.1-pool` — multiple keys, one pool
- `config.yaml.example.n-keys.n-pools.1-provider` — multiple pools, one provider
- `config.yaml.example.n-keys.n-pools.n-providers` — multiple providers with fallback
- `config.yaml.example.n-keys.n-pools.n-providers.n-routermodels` — full routing strategies
- `config.yaml.example.compatible.protocol` — DeepSeek, Grok, GLM, OpenRouter, relay patterns
- `config.yaml.example.local.protocol` — Ollama, LM Studio, vLLM, LocalAI (untested)

#### docs: Codex setup scripts (`scripts/`)
- `enable_miroxy_for_codex.sh` / `disable_miroxy_for_codex.sh` (POSIX sh, no python3 dependency)
- Reads `model_routes` from config.yaml via awk; merges with `scripts/data/codex-default-models.json`; writes `~/.codex/miroxy-models.json`; patches `config.toml`.
- Backs up existing config before any modification; restore on disable.

#### feat: OpenAPI spec + go generate
- `internal/api/admin-openapi.yaml` — OpenAPI 3.1 spec for all admin endpoints (co-located with the Go package it describes).
- `internal/api/oapi-codegen.yaml` — codegen config (package, output file, generators).
- `internal/api/generate.go` — `//go:generate oapi-codegen` directive + `AdminAPIVersion` constant; `admin_api.gen.go` committed after running `make gen`.

#### chore: Makefile — ports + gen targets
- `PROXY_PORT` 3333 → 9000, `ADMIN_PORT` 3334 → 9001, Docker port mappings updated.
- `make gen` — runs `go generate ./...` (requires oapi-codegen).
- `make gen-check` — runs gen + `git diff --exit-code` (for CI sync verification).

Changelog format: `[YYYY-MM-DD] <type>: <summary>`
Types: `feat` | `fix` | `test` | `refactor` | `docs` | `chore`

---

## [2026-07-05] — Built-in Web UI (setup mode + running mode)

### Completed

#### feat: setup mode — no-config first-run experience
- `cmd/miroxy/serve.go`: detects missing config file at startup → enters setup mode instead of exiting with error.
- `runSetupMode()`: starts admin server only (proxy port not bound); blocks until user completes setup via UI.
- On completion, shuts down setup server and calls `runProxy()` — seamless hand-off, no process restart.

#### feat: SetupServer (`internal/server/setup.go`)
- `POST /admin/setup/import` — multipart upload of `config.yaml` + `secrets.env` (or `.bat`). Parses secrets file (supports `export K=V`, `SET K=V`, plain `K=V`), substitutes `${VAR}` placeholders, validates config.
- `POST /admin/setup/quickstart` — JSON body: provider + provider_model + api_key + model_name + auth_key → generates minimal config YAML in memory, stores for export.
- `POST /admin/setup/start` — loads pending config into channel; main loop picks up and starts proxy.
- `GET /admin/setup/export?fmt=yaml|env|bat` — download generated files: `config.yaml` (placeholders intact) + `secrets.env` / `secrets.bat`.
- `GET /admin/setup/preview` — returns config YAML and masked secrets for UI display.

#### feat: config package — `LoadFromBytesWithEnv`
- `internal/config/yaml.go`: new `LoadFromBytesWithEnv(data []byte, extraEnv map[string]string)` — substitutes from supplied map first, then `os.LookupEnv`. Used by import flow so real keys never need to touch environment.

#### feat: AdminHandler additions (`internal/server/admin.go`)
- `//go:embed ui` — all UI assets compiled into binary, zero external files needed.
- `GET /admin/mode` → `{"mode":"running"}` (setup server returns `{"mode":"setup"}`).
- `GET /admin/config` → sanitized config JSON (key values masked to last-4 chars).
- `POST /admin/reload` — convenience alias alongside ConnectRPC endpoint.
- `GET /` — serves embedded UI (catch-all after all API routes).

#### feat: `sanitizeConfig` helper
- Returns structured JSON view of current config for UI pages: keypools with `masked` key values, model_list with routing targets, server/compress/dump settings. No raw credential values exposed.

#### feat: Web UI (`internal/server/ui/`)
Three files, zero external dependencies, ~20 KB total.

**Setup mode pages:**
- `#setup` — landing page with two-column layout: Import (drag-drop config.yaml + secrets.env) and Quick Start (provider/key/model form → POST /admin/setup/quickstart).
- `#preview` — shows generated config.yaml and masked secrets side by side; export buttons for all three formats; green "Start Proxy" button.

**Running mode pages:**
- `#dashboard` — four stat cards (uptime, in-flight, models, key pools) + keypool summary cards + model table; auto-refreshes every 5 s.
- `#keypools` — per-pool cards showing strategy badge, RPM limit, per-key name + masked value.
- `#routing` — card-per-model layout showing the full resolution chain: client model name → strategy → numbered targets (provider, provider model, key pool ref). Fallback targets are numbered 1/2/3 to make priority explicit.
- `#clients` — Client Setup guide for Claude Code, Codex, OpenCode, Cursor/Windsurf. Two snippets per client: minimal (keep model name, swap base URL + key) and full-switch. Warning callout about model names in CLAUDE.md / agent configs needing routing entries.
- `#settings` — toggle cards for commands, compression, dump; reload button.

#### feat: Docker zero-mount mode
Running `docker run -p 3333:3333 -p 8090:8090 miroxy` with no `-v` mounts now starts in setup mode. User opens `http://localhost:8090`, imports or configures via UI, clicks Start Proxy. Config lives in process memory; optional `/app/data` volume for persistence.

### Test results

```
go build ./...    ← clean
go test ./...     ← all pass (server, integration, unit)
```

---

## [2026-06-28] — Story IR-2: bidirectional converters + InProcess backend seam

### Completed this session

#### refactor: translator layer is now bidirectional through the IR
- New `internal/translator/converter.go`: `FrontendConverter` (Anthropic⇄IR) and `ProviderConverter` (provider⇄IR) interfaces — the Ops-style seam.
- New `internal/translator/anthropic.go`: `AnthropicConverter` implementing the frontend (`RequestToIR` / `ResponseFromIR` / `StreamFromIR`), relocated from the deleted `internal/ir/convert.go`.
- `internal/translator/gemini.go`: now `GeminiConverter` (`ProviderConverter`) — `RequestToProvider` / `ResponseToIR` / `StreamToIR`. The **response and stream paths now flow through the IR** (previously Gemini→Anthropic directly).
- `internal/translator/translator.go`: `Translator` port unchanged; `GeminiTranslator` owns transport only and composes frontend + backend. `server.go` / `executor.go` / `pipeline` untouched.

#### feat: pluggable backend seam (InProcess; WASM/gRPC/HTTP future)
- New `internal/translator/backend.go`: `TranslatorBackend` interface + `InProcessBackend` (only wired backend; marks where WASM/gRPC/HTTP would cross a serialization boundary).

#### feat: neutral IR stream events
- New `internal/ir/stream.go`: 9-kind `StreamEvent` union (Go analogue of LLM-Rosetta's 10-event schema). Gemini SSE → IR events → Anthropic SSE; exact 7-event sequence preserved.

#### refactor: IR is now provider-neutral
- Removed `IRToolResultPart.FunctionName` (a Gemini-specific lookup that had leaked into the hub). Name resolution moved to `gemini.go:resolveFunctionNameIR`. `ir.proto` reserves field 2.

#### test
- Migrated `internal/ir/convert_test.go` → `internal/translator/anthropic_test.go` (10 tests, retargeted to `AnthropicConverter`; tool_result test now asserts IR neutrality). `go test ./...` green — **91 tests, 0 fail**.

#### docs
- Prior-art analysis written to `oss/llm-rosetta/docs/ANALYSIS.md` (LLM-Rosetta paper + repo).

---

## [2026-06-23] — Phase 1 hardening: Gemini adapter + 429 retry tests

### In Progress
- [ ] Prometheus metrics (`internal/metrics/`)
- [ ] Rectifier interface + Gemini rules (`internal/rectifier/`)

### Completed this session

#### feat: Gemini adapter hardening (G-01 to G-10)

**G-01 — GenerationConfig sampling params**
- `types.MessageRequest`: added `Temperature *float64`, `TopP *float64`, `TopK *int`, `StopSequences []string`
- `types.GenerationConfig`: added matching Gemini fields
- `buildGeminiRequest`: populates all fields when non-nil/non-empty

**G-02 — Extended thinking (thought) parts filtered**
- `types.GeminiPart`: added `Thought bool`
- `FromUpstream` and `streamGeminiToAnthropic`: skip parts where `Thought == true`

**G-03 — Safety filter refusal response**
- `types.GeminiResponse`: added `PromptFeedback *GeminiPromptFeedback`
- `FromUpstream`: checks `promptFeedback.blockReason` before candidates; returns `stop_reason: "refusal"` response instead of an error

**G-04 — finishReason completeness**
- `mapFinishReason`: added `TOOL_CODE`/`FUNCTION_CALL` → `"tool_use"` and safety variants → `"end_turn"`

**G-05 — Upstream HTTP status passthrough**
- New file `internal/translator/upstream_error.go`: `UpstreamError{HTTPStatus, Code, Message}`
- `FromUpstream`: returns `*UpstreamError` for Gemini application-level errors
- `server.go handleNonStream`: routes `*UpstreamError{429}` to `RateLimitError` (cooldown, not circuit-break), `≥500` to circuit-break, `4xx` to client error without retry or key penalty

**G-06 — tool_choice "tool" → allowedFunctionNames**
- `types.GeminiFunctionCallingConfig`: added `AllowedFunctionNames []string`
- `toolChoiceMode` replaced by `buildToolConfig` returning the full config struct

**G-07 — Streaming input_tokens**
- `types.DeltaUsage`: added `InputTokens int`
- `streamGeminiToAnthropic`: emits `usage.PromptTokenCount` in `message_delta`

**G-08 — Bearer token auth style**
- `types.ModelEntry`: added `APIBase string`, `AuthStyle string`
- `GeminiTranslator`: added `authStyle` field; `"bearer"` mode sends `Authorization: Bearer <key>`
- New constructor `NewGeminiWithConfig(providerModel, baseURL, authStyle)`
- `server.go New`: uses `NewGeminiWithConfig(m.ProviderModel, m.APIBase, m.AuthStyle)`

**G-09 — BatchTool filtering**
- `buildGeminiRequest`: skips `Name == "BatchTool"` from function declarations

**G-10 — Malformed tool call arg rectification**
- New helper `rectifyArgs(json.RawMessage)`: unwraps string-encoded JSON objects
- Applied in `FromUpstream` and `streamGeminiToAnthropic` for all `FunctionCall` parts

#### feat: §1-A — 429 transparent pre-stream retry tests

The retry loop was already implemented. This batch proves it with end-to-end tests.

**Stub helpers added** (`tests/integration/harness_test.go`):
- `gemini429Handler` — HTTP 429, no retryDelay
- `gemini429LongCooldownHandler` — HTTP 429, `retryDelay: "60s"`

**Integration tests added** (`tests/integration/retry_test.go`):
- `TestRetry_NonStream_429OnFirstKey_RetriesToSecondKey` — transparent retry < 500 ms
- `TestRetry_Stream_429OnFirstKey_RetriesToSecondKey` — streaming path
- `TestRetry_AllKeys429_Returns503` — all keys cooling → 503
- `TestRetry_AllKeysStream429_Returns503` — streaming variant
- `TestRetry_RateLimitCooldown_KeyNotReusedDuringWindow` — cooldown isolation verified

**Unit tests added** (`tests/unit/translator_test.go`, 15 new cases):
G-01 to G-10 coverage: temperature forwarding, thought filtering, safety refusal, finishReason mapping, UpstreamError type, allowedFunctionNames, BatchTool filter, string-wrapped arg rectification.

#### Test results

```
ok  miroxy/internal/server       0.003s
ok  miroxy/tests/integration     0.023s
ok  miroxy/tests/unit            1.421s
```

`go build ./...` clean. All packages pass.

---

## [2026-06-23] — Project scaffolding (initial)

### Completed

- `cmd/miroxy/main.go` — binary entry point
- `internal/config/` — `ConfigStore` interface + YAML implementation with `${ENV_VAR}` substitution
- `internal/translator/translator.go` — `Translator` interface
- `internal/translator/gemini.go` — Gemini non-streaming + 7-event SSE streaming
- `internal/keypool/` — `KeyPool` interface + `InMemoryPool`:
  - Strategies: `round_robin`, `least_requests`
  - Circuit breaker (per-key failure counter + cooldown)
  - Rate-limit cooldown (separate from circuit-break; escalating tiers 10→30→60→120→300 s)
  - Proactive soft-limit rotation (avoid 429 before it happens)
  - `parseRetryDelay`: extracts `retryDelay` from Gemini 429 body
- `internal/server/server.go` — HTTP server, auth middleware, transparent 429 pre-stream retry
- `internal/auth/` — inbound API key validation
- `internal/idgen/` — message and tool ID generation
- `internal/types/` — Anthropic and Gemini type definitions
- `Dockerfile`, `config/*.example`, `README.md`
- `tests/unit/` — 20 unit test cases
- `tests/integration/` — 10 integration test cases (stub Gemini server)

---

## Planned — not yet implemented

| Item | Phase | Status |
|---|---|---|
| Prometheus metrics (`internal/metrics/`) | 1 | planned |
| Rectifier interface + Gemini rules | 1 | planned |
| E2E test with real Gemini API key | 1 | planned |
| OpenAI translator | 2 | planned |
| Structural router | 2 | planned |
| Encrypted secret storage `miroxy.s` | 2 | planned |
| Embedded web UI (Alpine.js) | 2 | planned |
| DeepSeek translator | 3 | planned |
| GLM (智谱) translator | 3 | planned |
| Extended thinking: Optimizer, Budget Rectifier, Signature Rectifier, Shadow Store (G-11–G-14) | 3 | planned |
