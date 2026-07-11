# miroxy

LLM API gateway with multi-provider routing.

Accepts requests from any Anthropic or OpenAI-compatible client, routes them across upstream providers (Gemini, OpenAI, DeepSeek, Anthropic, and more), manages API key pools with rotation and 429 backoff, and translates protocols automatically.

---

## Quickstart

```bash
go build -o build/miroxy ./cmd/miroxy   # Go 1.22+
cp config/config.yaml.example.min config/config.yaml
# edit config.yaml — set GEMINI_KEY and MIROXY_CLIENT_KEY
export GEMINI_KEY=AIzaSy...
export MIROXY_CLIENT_KEY=sk-my-client-key
./build/miroxy serve -c config/config.yaml
```

### Point your client at miroxy

**Claude Code** — add to `~/.claude/settings.json`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:9000",
    "ANTHROPIC_AUTH_TOKEN": "sk-my-client-key",
    "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1"
  }
}
```

**Codex** — run the setup script once, then restart Codex:

```bash
cd /path/to/miroxy
./scripts/enable_miroxy_for_codex.sh config/config.yaml
```

**OpenAI SDK / any OpenAI-compatible client:**

```python
client = openai.OpenAI(
    base_url="http://localhost:9000/v1",
    api_key="sk-my-client-key",
)
```

---

## Minimum required configuration

Every miroxy config needs these five sections.

```yaml
server:
  default_model: miroxy      # must match a model_name in model_routes

auth:
  allowed_keys:
    - ${MIROXY_CLIENT_KEY}   # what your clients send as the Bearer token

providers:
  gemini: {}                 # declare every provider used in model_routes
                             # fields left blank use built-in defaults

credpools:
  my-pool:
    keys:
      - my_key: ${GEMINI_KEY}

model_routes:
  - model_name: miroxy
    provider: gemini
    provider_model: gemini-2.5-flash
    credpool_ref: my-pool
```

See [`config/config.yaml.example.min`](config/config.yaml.example.min) for the
smallest working config, and the other `config.yaml.example.*` files for more
complex setups.

---

## Default ports

| Port | Purpose |
|------|---------|
| **9000** | Proxy — receives LLM traffic from clients |
| **9001** | Admin — management API, localhost only |

Both can be overridden in config:

```yaml
server:
  port: 9000

admin:
  addr: "127.0.0.1:9001"
```

---

## Proxy endpoints (port 9000)

| Method | Path | Protocol |
|--------|------|----------|
| `POST` | `/v1/messages` | Anthropic Messages API |
| `POST` | `/v1/responses` | OpenAI Responses API (Codex) |
| `POST` | `/v1/chat/completions` | OpenAI Chat API |
| `GET`  | `/v1/models` | Model list (gateway discovery) |

---

## Admin endpoints (port 9001)

Authentication: `Authorization: Bearer <token>` where token is any value from
`auth.allowed_keys`, OR the admin session token from `POST /admin/login`.

| Method | Path | Description |
|--------|------|-------------|
| `GET`  | `/health` | Liveness probe — no auth required |
| `GET`  | `/stat` | Runtime stats: uptime, token usage, compress perf |
| `GET`  | `/v1/config` | Full effective config (defaults resolved, keys masked) |
| `GET`  | `/v1/config/providers` | Resolved provider definitions |
| `GET`  | `/v1/config/credpools` | Credpools with masked keys |
| `GET`  | `/v1/config/routes` | Model routes (including auto-discovered) |
| `POST` | `/admin/reload` | Hot-reload config file |
| `POST` | `/admin/proxy/stop` | Stop proxy listener |
| `POST` | `/admin/proxy/start` | Start proxy listener |

Full API spec: [`internal/api/admin-openapi.yaml`](internal/api/admin-openapi.yaml)

---

## CLI reference

```
miroxy serve                    Start the proxy server
miroxy config                   Show full effective config of running instance
miroxy config providers         Show resolved provider definitions
miroxy config routes            Show model routes (including auto-discovered)
miroxy config credpools          Show credpools (keys masked)
miroxy health                   Liveness check against running instance
miroxy stat                     Runtime stats
miroxy reload                   Hot-reload config file
```

### `miroxy serve`

```
miroxy serve [flags]

Flags:
  -c, --config string     path to config file (default "config/config.yaml")
  -p, --port int          proxy listen port (overrides config)
      --admin-port int    admin listen port (overrides config)
      --dump              enable traffic capture to dump.jsonl
      --log-level string  log level: trace|debug|info|warn|error
```

### `miroxy config`

Acts as an API client against a running instance. Requires `MIROXY_AUTH_TOKEN`
env var (any value from `auth.allowed_keys`):

```bash
export MIROXY_AUTH_TOKEN=sk-my-client-key

miroxy config                             # full effective config
miroxy config providers                   # providers section
miroxy config routes                      # model routes
miroxy config credpools                    # credpools (keys masked)
miroxy config --admin-addr 10.0.0.5:9001 routes   # remote instance
```

### Admin address resolution

`health`, `stat`, `reload`, and `config` resolve the admin address in this order:

1. `--admin-addr` flag
2. `MIROXY_ADMIN_ADDR` environment variable
3. `admin.addr` read from `-c config.yaml`
4. Default `http://127.0.0.1:9001`

---

## Configuration

Full annotated example: [`config/config.yaml.example`](config/config.yaml.example)

### Providers — must be declared

Every provider referenced in `model_routes` must appear in the `providers` block.
Built-in defaults (base_url, protocol, auth_style) are applied automatically for
known providers — just declare the key, leave fields blank:

```yaml
providers:
  gemini: {}       # base_url=https://generativelanguage.googleapis.com, protocol=gemini, auth_style=query_key
  openai: {}       # base_url=https://api.openai.com/v1, protocol=openai, auth_style=bearer
  anthropic: {}    # base_url=https://api.anthropic.com, protocol=anthropic, auth_style=api_key
  deepseek: {}     # base_url=https://api.deepseek.com/v1, protocol=deepseek, auth_style=bearer
  glm: {}          # base_url=https://api.z.ai/api/paas/v4, protocol=glm, auth_style=bearer
  grok: {}         # base_url=https://api.x.ai/v1, protocol=grok, auth_style=bearer
```

Override only what you need:

```yaml
providers:
  gemini:
    base_url: https://gemini-relay.internal   # point at a relay
```

### Key pools

```yaml
credpools:
  gemini-flash:
    strategy: least_requests        # or round_robin
    circuit_break_threshold: 5      # consecutive failures before marking key unhealthy
    cooldown_seconds: 60
    rate_limit_rpm: 20              # Gemini free tier: 20 req/min per key
    rate_soft_limit: 18             # rotate proactively at 18 to stay under limit
    keys:
      - key_alice:   ${GEMINI_KEY_1}
      - key_bob:     ${GEMINI_KEY_2}
      - key_charlie: ${GEMINI_KEY_3}
```

Key name (e.g. `key_alice`) appears in `key_id` log fields on 429 and circuit-break events — makes it easy to identify which key is causing problems.

**Passthrough auto-routing**: tag a credpool with `provider: anthropic` or `provider: openai` to enable zero-config passthrough routing for `claude-*` or `gpt-*` models respectively:

```yaml
credpools:
  anthropic-pool:
    provider: anthropic        # enables passthrough for any claude-* model
    keys:
      - main: ${ANTHROPIC_KEY}

  openai-pool:
    provider: openai           # enables passthrough for gpt-*, o1*, o3*
    keys:
      - main: ${OPENAI_KEY}
```

With these pools configured, clients can select `claude-opus-4-8` or `gpt-5.4` and miroxy routes them automatically — no explicit `model_routes` entry needed.

### Model routes

```yaml
model_routes:
  # Simple: one provider, one model
  - model_name: miroxy
    provider: gemini
    provider_model: gemini-2.5-flash
    credpool_ref: gemini-flash
    timeout_seconds: 30

  # Routing: fallback across multiple providers
  - model_name: miroxy-smart
    routing:
      strategy: fallback
      targets:
        - provider: anthropic
          provider_model: claude-sonnet-4-6
          credpool_ref: anthropic-pool
          timeout_seconds: 60
        - provider: gemini
          provider_model: gemini-2.5-pro
          credpool_ref: gemini-pro
          timeout_seconds: 60
```

### Model name matching (LookupModel)

When a request arrives with `model: "claude-opus-4-8"`, miroxy finds the route in this order:

1. **Exact match** — `model_name: "claude-opus-4-8"` in config
2. **Strip prefix** — strip `claude-`, look up `opus-4-8`
3. **Longest prefix** — `opus` matches `opus-4-8` (next char must be `-` or end); `gpt-5.4` matches `gpt-5.4-mini` and `gpt-5.4-turbo`
4. **Provider passthrough** — `claude-*` → anthropic credpool; `gpt-*/o1*/o3*` → openai credpool
5. **Default model** — `server.default_model`

### Routing strategies

| Strategy | Behaviour |
|----------|-----------|
| `fallback` | Try targets in order; advance only when a pool is fully exhausted |
| `round_robin` | Distribute requests evenly across targets |
| `least_requests` | Send to the target with fewest active in-flight requests |

---

## Model discovery (Claude Code / Codex)

### Claude Code model picker

miroxy auto-detects Claude Code requests via `User-Agent: claude-code/*` and
adds a `claude-` prefix to model IDs in `GET /v1/models` so they appear in the
`/model` picker. No naming convention required in config — `model_name: gemini-2.5`
shows up as `claude-gemini-2.5` in the picker.

Requirements:
1. Set `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1` in `~/.claude/settings.json`
2. Point `ANTHROPIC_BASE_URL` at miroxy

### Codex model picker

Generate a Codex-compatible model catalog from your miroxy config:

```bash
# Linux / macOS
./scripts/enable_miroxy_for_codex.sh config/config.yaml

# Disable and restore original config
./scripts/disable_miroxy_for_codex.sh
```

### Auto-discovery of upstream models

With `model_discovery: auto` (default) and a provider-tagged credpool, miroxy
calls the provider's `/v1/models` at startup and injects discovered models:

```yaml
credpools:
  anthropic-pool:
    provider: anthropic    # triggers GET api.anthropic.com/v1/models on startup
    keys:
      - main: ${ANTHROPIC_KEY}
```

Discovered models appear in `GET /v1/models` responses and are immediately routable.
Disable with `server.model_discovery: strict`.

---

## In-band proxy commands (`:miroxy`)

Prefix a message with `:miroxy` to execute it without consuming LLM tokens.
Works in Claude Code, Codex, and any other client.

```
:miroxy ?              List all commands
:miroxy stats          Uptime, token usage, credpool health, compression stats
:miroxy health         Quick health check
:miroxy dump on|off    Toggle traffic capture to dump.jsonl
```

**Inject into context:**

```
:miroxy stats is credpool utilization high?
# → injects stats output, then asks the LLM the question
```

The `:miroxy dump` command requires `server.commands.allow_dump: true` in config.

---

## Dump mode

Captures all request/response traffic to JSONL for protocol debugging.

```yaml
dump:
  enabled: true
  path: ./log/dump.jsonl    # default path
  include_sse: true         # write each SSE event as a separate record
  max_size_mb: 10           # rotate at 10 MB; 0 = unlimited
  max_backups: 2            # keep 2 rotated files (timestamped)
```

Rotated files are named `dump.jsonl.20260706132950` (UTC timestamp). Deleted files
are automatically recreated on the next write.

Filter by trace ID:

```bash
grep '"trace_id":"abc123"' log/dump.jsonl | jq .
```

---

## Running locally

```bash
make build                  # outputs build/miroxy

cp config/config.yaml.example.min config/config.yaml
cp config/secrets.env.example config/secrets.env
# edit both files

make run                    # loads secrets.env automatically
```

Verify:

```bash
curl http://localhost:9000/health
curl http://localhost:9001/health

export MIROXY_AUTH_TOKEN=sk-my-client-key
./build/miroxy config       # show effective config
./build/miroxy stat         # show runtime stats
```

---

## Docker

```bash
docker run -d \
  --name miroxy \
  --env-file config/secrets.env \
  -v "$(pwd)/config/config.yaml:/app/config/config.yaml:ro" \
  -p 9000:9000 \
  -p 9001:9001 \
  forrestisagoodman/miroxy:latest
```

```bash
make docker-build VERSION=1.0.0
make docker-push  VERSION=1.0.0
make docker-run   VERSION=1.0.0   # stop old + start new
make docker-logs                   # tail logs
make docker-stop                   # stop and remove
```

---

## Hot reload

```bash
miroxy reload -c config/config.yaml
# or: kill -HUP <pid>
```

What can be changed without restart:
- Model routes, providers, credpools
- Rate limits, circuit-break settings
- `default_model`, `auth.allowed_keys`

Requires restart: `server.port`, `admin.addr`

---

## Running tests

```bash
go test ./...                   # full suite
go test ./tests/unit/...        # unit tests — no network
go test ./tests/integration/... # integration tests — stub server
```

---

## Project layout

```
miroxy/
├── cmd/miroxy/
│   ├── main.go           entry point
│   ├── root.go           root Cobra command
│   ├── serve.go          miroxy serve
│   └── admin.go          miroxy health / stat / reload / config
├── core/
│   ├── cred/             Credential interface (Header, Query, SigV4)
│   ├── selector/         CredPool, TargetSelector, RoutingSelector
│   ├── router/           Router interface, RouteTarget
│   └── dispatch/         Dispatcher interface
├── internal/
│   ├── config/           ConfigStore, YAML loader, validation, provider defaults
│   ├── server/           HTTP handlers, retry loops, config API, passthrough selectors
│   ├── translator/       Protocol translators (Gemini, OpenAI, DeepSeek, Grok, GLM)
│   ├── stats/            Token usage counters (atomic, per-model, per-key)
│   ├── pipeline/         Plugin chain, LLMContext, CommandPlugin
│   ├── compress/         Context compression plugin
│   └── dump/             JSONL traffic capture with rotation
├── docs/
│   ├── api/              admin-openapi.yaml
│   └── dev/              DEVLOG.md, DESIGNLOG.md
├── scripts/
│   ├── enable_miroxy_for_codex.sh
│   ├── disable_miroxy_for_codex.sh
│   └── data/codex-default-models.json
├── tests/
│   ├── unit/
│   └── integration/
└── config/
    ├── config.yaml.example                              — full annotated
    ├── config.yaml.example.min                          — minimal 1-key setup
    ├── config.yaml.example.n-keys.1-pool                — multiple keys, one pool
    ├── config.yaml.example.n-keys.n-pools.1-provider    — multiple pools, one provider
    ├── config.yaml.example.n-keys.n-pools.n-providers   — multi-provider fallback
    ├── config.yaml.example.n-keys.n-pools.n-providers.n-routermodels — full routing
    ├── config.yaml.example.compatible.protocol          — DeepSeek/Grok/GLM/OpenRouter/relay
    └── config.yaml.example.local.protocol               — Ollama/LMStudio/vLLM (untested)
```
