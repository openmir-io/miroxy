# Miroxy Design Log

Captures key architectural decisions and design discussions as they happen.
Format: dated entries, each covering one decision with context and rationale.
Implementation details belong in code; roadmap belongs in `docs/plan/`.

---

## 2026-06-30 — Core/Internal split, Selector abstraction, CredPool naming

### Context

Extended design session covering product path, module structure, and the
keypool/routing abstraction hierarchy. Prompted by 9Router analysis
(see `docs/design/learnfromothers.md`) and the question of whether to
introduce SQLite for OAuth token persistence.

---

### Decision 1: No SQLite in v1; JSON file when OAuth tokens are needed

SQLite adds 5–8 MB to the binary (`modernc.org/sqlite`, CGO-free) and brings
significant complexity for a use case that doesn't yet exist in v1.

The only hard requirement for OAuth token persistence is storing
`{provider → {accessToken, refreshToken, expiresAt}}` across restarts.
A JSON file with atomic write (write `.tmp` → rename) and `sync.RWMutex`
covers this with zero new dependencies.

**Rule:** Introduce SQLite only when persistence needs concurrent writes,
cross-query joins, or usage-history aggregation. For OAuth tokens alone,
JSON file is sufficient. This fits the Phase 2 "encrypted secret storage
(`miroxy.s`)" milestone already in `docs/plan/`.

---

### Decision 2: Keep single repo; `core/` parallel to `internal/`

**Rejected:** `internal/core/` — Go compiler prevents external import of
`internal/` packages, which directly conflicts with the goal of easy future
extraction into a standalone `miroxy-core` module.

**Rejected:** `go.work` workspace from day one — unnecessary overhead for a
solo project at v1 stage.

**Adopted:** Single module, `core/` at the repo root level (parallel to
`internal/`):

```
miroxy/
├── core/          stable interfaces + pure-Go implementations (extraction candidate)
│   ├── types/
│   ├── pipeline/
│   ├── router/
│   ├── selector/
│   └── idgen/
├── internal/      IO-bound implementations (never extracted)
│   ├── server/
│   ├── config/
│   ├── auth/
│   └── selector/  (ModelGroupSelector, ProviderSelector)
└── cmd!miroxy/
```

**Future extraction path:** when a separate `miroxy-core` repo is warranted,
copy `core/` → new repo, add `require github.com/openmir!miroxy-core` to
`go.mod`, and do a mechanical `sed` replace of `miroxy/core/` →
`github.com/openmir!miroxy-core/`. No logic changes required.

The preferred import style once extracted:
`github.com/openmir!miroxy-core/<package>` (not go.work).

---

### Decision 3: `KeyPool` interface abolished; replaced by unified `Selector`

**Previous design:** `KeyPool` interface (Acquire/Release) + `Selector` interface
were two separate public abstractions. This was redundant — both address
"pick a credential/resource for this request."

**Root cause of confusion:** `KeyPool`, `ModelGroupSelector`, and
`ProviderSelector` all solve the same problem at different granularities:

| Implementation | Fixed | Variable |
|---|---|---|
| `CredPool` (was InMemoryPool) | provider + model | credential |
| `ModelGroupSelector` | provider | model + credential |
| `ProviderSelector` | — | provider + model + credential |

**Adopted:** Single `Selector` interface in `core/selector/`:

```go
type ExecutionPlan struct {
    SelectionID string
    Credential  string
    Model       string
    Translator  translator.Translator
}

type Selector interface {
    Select(ctx context.Context, req *types.MessageRequest) (*ExecutionPlan, error)
    Release(plan *ExecutionPlan, err error)
}
```

`KeyPool` as a public interface is deleted. `InMemoryPool` is renamed
`CredPool` and directly implements `Selector`.

---

### Decision 4: `InMemoryPool` → `CredPool`

`InMemory` describes storage mechanism, not business semantics.

The lowest-level scheduling unit is a **credential** — either a static API key
or an OAuth access token. The thing that manages a pool of them and selects
one is a `CredPool`.

- `Cred` is the standard abbreviation for credential (covers both API keys and
  OAuth tokens without implying either)
- `Pool` accurately describes the managed-resource semantics (acquire/release,
  usage tracking, health state)
- Does not encode implementation details (not "InMemory", not "Static")

**Package layout after rename:**

```
core/selector/
    selector.go     Selector interface + ExecutionPlan + ErrNoSelection
    credpool.go     CredPool (key rotation, 429 cooldown, circuit break)
    errors.go       RateLimitError

internal/selector/
    modelgroup.go   ModelGroupSelector (same-provider model fallback)
    provider.go     ProviderSelector (cross-provider fallback)
```

---

### Decision 5: Router and Selector are not in conflict

They are sequential stages in the request lifecycle:

```
Router.Route(modelAlias) → RouteTarget{Selector, ModelInfo, Timeout, Invisible}
    ↓  (once, stateless config lookup)

for each retry:
    Selector.Select(ctx, req) → ExecutionPlan
        ↓  (stateful: skips cooling keys, checks capabilities)
    upstream call
    Selector.Release(plan, err)
```

`Router` answers: "which configured target handles this model alias?"
`Selector` answers: "which credential + model is healthy right now?"

---

### Decision 6: `RouteTarget.Entry config.ModelEntry` removed

`pipeline/context.go`'s `RouteTarget` previously embedded `config.ModelEntry`,
creating a dependency from `core/` on the IO-bound `config/` package.

**Fix:** Replace with `ModelInfo` defined in `core/router/`:

```go
type ModelInfo struct {
    Name          string  // client-facing alias, e.g. claude-sonnet-4-6
    ProviderModel string  // upstream model, e.g. gemini-2.5-flash
    Provider      string  // gemini / openai / kiro
}

type RouteTarget struct {
    Selector  selector.Selector
    Model     ModelInfo
    Timeout   time.Duration
    Invisible bool
}
```

`Translator` is removed from `RouteTarget`; it now lives in `ExecutionPlan`
(selected dynamically per attempt, not statically wired at config load time).

---

## On documentation philosophy

**The problem with detailed implementation docs:** they describe the current
state of code that changes daily. The moment a refactor happens, the doc is
wrong, and wrong docs are worse than no docs.

**What ages well:**
- *Why* a decision was made (this file)
- Interface contracts (the `Translator`, `Selector`, `Router` interfaces)
- Constraints and non-goals (`CLAUDE.md` hard constraints section)
- High-level roadmap phases (not sprint-level tasks)

**What ages poorly and should not be maintained:**
- Detailed implementation walkthroughs ("function X calls function Y")
- Sequence diagrams of current code paths
- Anything that duplicates what the code already says

**Rule for this project:** `docs/design/` holds architectural rationale and
stable interface contracts. `docs/plan/` holds epic-level roadmap. This file
(`DESIGNLOG.md`) captures decisions as they happen. Code comments explain
non-obvious invariants. Nothing else is documented proactively.

---

## 2026-07-01 — Plugin directory structure: domain-first, shared pluginrt

### Context

miroxy's plugin model supports three execution modes:

1. **Native** — in-process Go, on the critical path (locks, sub-ms SSE, 429-retry).
2. **WASM** — wazero-embedded sandbox for stateless/ecosystem plugins (Rust/TS/tinygo).
3. **Sidecar** — gRPC-over-Unix-Socket (or HTTP) to an external process for heavy
   ML/semantic logic or multi-instance shared state (e.g. Credpool).

The question was how to organize the codebase so that adding a wasm or sidecar
adapter to a new domain does not require re-implementing connection/dial/health-check/
circuit-breaker machinery each time.

### Options considered

1. **Execution-model-first** (`internal/native/`, `internal/wasm/`, `internal/sidecar/`) —
   rejected. Domain logic for router/security/cred would be split across three unrelated
   folders per concern. Nobody navigating the codebase to review a routing change wants
   to visit three folders.

2. **Domain-first with full native/wasm/sidecar triad duplicated per domain** — rejected.
   Causes real duplication of dial/health-check/circuit-breaker code. Concrete failure
   mode: copy-paste a `router/` folder to create `security/`, and the sidecar transport
   in `security/` now contains stale naming leftovers like `router_grpc`. This class of
   mistake must become structurally impossible.

3. **Domain-first with shared `internal/pluginrt/` for cross-process machinery** —
   **adopted**. See decisions below.

### Industry precedent

- **HashiCorp go-plugin** (Terraform/Vault/Nomad): providers are organized by domain;
  the plugin transport (gRPC/netrpc) is a shared runtime detail, not a top-level folder.
- **Kubernetes CSI/CNI**: driver implementations are domain-organized; the sidecar
  gRPC protocol is shared (external-provisioner, node-driver-registrar).
- **containerd plugin registry**: interface defined once, adapters registered by domain
  name; execution model is a property of registration, not folder location.
- **Envoy proxy-wasm**: host API defined once for all wasm filter domains; filter
  domains are not split by execution model.

### Decisions

**Decision 1: Domain is the primary directory axis.**
A developer working on "cred" finds everything about cred in `internal/cred/`.
Native code, sidecar glue, and proto file all live there together.
Do not create `internal/wasm/cred/`, `internal/sidecar/cred/`, etc.

**Decision 2: Cross-process machinery lives once in `internal/pluginrt/`.**
Every domain's sidecar/wasm adapter calls into `pluginrt` — it must not
reimplement dialing, retries, or health-checks. Added:
- `internal/pluginrt/sidecar/transport.go` — protocol-agnostic `Transport` interface
- `internal/pluginrt/sidecar/registry.go` — `NewClient(domain, cfg)` helper (containerd-style)
- `internal/pluginrt/wasm/runtime.go` — wazero instance lifecycle stub
- `internal/pluginrt/wasm/hostfuncs.go` — host import allow-list stub

**Decision 3: `transport.go` defines the interface; concrete implementations deferred.**
`grpc_transport.go` and `http_transport.go` are added only when a real,
non-connect-go consumer needs them. The current transport.go carries only the
interface and `TransportConfig` — no concrete implementations yet.

**Decision 4: Domains using connectrpc.com/connect bypass `pluginrt/sidecar` entirely.**
The connect-generated client already handles dial/retry/health. Forcing an extra
`Transport` wrapper on top would be a leaky abstraction with no benefit.

**Decision 5: Each domain owns its own proto file.**
Follows the precedent of `internal/ir/ir.proto` next to `internal/ir/ir.go`.
No generic `PluginService{Invoke(bytes) returns(bytes)}` — that trades compile-time
type safety for false DRY-ness.
First concrete example: `internal/cred/credpool.proto`.

**Decision 6: Existing native code does not move.**
`internal/auth/auth.go`, `internal/translator/*.go`, etc. stay exactly where they
are. Native plugins that already implement `core.Plugin` get no `native/` subfolder.

**Decision 7: Credpool is the first real sidecar consumer.**
Glue code in `internal/cred/credpool.go`, proto in `internal/cred/credpool.proto`.
`CredpoolSource` implements `selector.CredentialSource` and falls back to the local
credential chain (`OAuthSource` / `StaticSource`) on any transport failure.
The fallback logic lives inside `CredpoolSource` — `pipeline/loader.go` sees exactly
one credential plugin and has no knowledge of the internal Credpool→local chain.

**Decision 8: Core redlines are native-only.**
SSE stream handling, KeyPool locking, 429-retry timing, protocol translation, and
secret decryption MUST NOT be loadable as wasm or sidecar. The wasm host-function
allow-list in `internal/pluginrt/wasm/hostfuncs.go` documents this explicitly.

**Decision 9: Third-party-language wasm SDKs do NOT belong under `internal/`.**
`internal/` is a Go-visibility boundary meaningless to Rust/TS/tinygo code. When
a real wasm plugin SDK is authored, it goes in a repo-root `plugins/` directory
(sibling to `core/` and `internal/`). Not scaffolded yet — documented here as the
intended future location.

### Target directory tree (as implemented)

```
core/
  router/         unchanged — ports/interfaces
  selector/       unchanged — ports/interfaces

internal/
  pluginrt/       NEW — shared cross-process machinery, written once
    sidecar/
      transport.go    protocol-agnostic Transport interface + TransportConfig
      registry.go     NewClient(domain, cfg) containerd-style registration helper
    wasm/
      runtime.go      wazero instance lifecycle (stub until wazero dep added)
      hostfuncs.go    host import allow-list (stub until first WASM plugin)

  auth/           unchanged (native, implements core.Plugin)
  config/         unchanged
  cred/
    oauth.go      unchanged — local fallback chain
    credpool.go   NEW — CredpoolSource: connect-go Credpool sidecar glue + fallback
    credpool.proto NEW — Credpool RPC shape (Acquire/Release), buf-generate target
  idgen/          unchanged
  ir/             unchanged (existing proto precedent)
  pipeline/       unchanged — loader sees one credential plugin, no Credpool knowledge
  server/         unchanged
  translator/     unchanged
  types/          unchanged
```

### Migration notes

- No existing files were renamed or moved — the existing layout was already domain-first.
- Only new files were added: `internal/pluginrt/**` and `internal/cred/credpool.{go,proto}`.
- `go build ./... && go test ./...` passes clean after this change.

### Deferred / open questions

- **`grpc_transport.go` / `http_transport.go`**: deferred until a concrete non-connect-go
  sidecar consumer exists. `transport.go` interface is in place to prevent stale copies.
- **`router/`, `security/`, `compress/` sidecar/wasm adapters**: deferred until those
  domains have a named concrete need. Pattern to follow: thin `<domain>_sidecar.go` or
  `<domain>_wasm.go` inside the existing domain folder, calling `internal/pluginrt/`.
- **`plugins/wasm-sdk/`** (repo root, third-party-language wasm): deferred until we
  author a Rust/TS/tinygo plugin SDK. Not scaffolded — empty directories would imply
  false commitment.
- **`CredpoolSource.Release()`**: deferred until the executor refactor in Epic 3 can
  thread `leaseID` through `ExecutionPlan` or a context value. Currently leases expire
  by TTL on the Credpool side.
- **connect-go wiring**: `credpool.go` stubs the client behind a local `CredpoolClient`
  interface. Real wiring requires `buf generate` from `credpool.proto` and adding
  `connectrpc.com/connect` to `go.mod`. Deferred until Credpool service is available.

---

## 2026-07-03 — Typed Credential + Dispatcher abstraction

### Context

miroxy v1 used `string` throughout the credential stack. This blocked three things:
1. Providers with multi-field auth (AWS SigV4 = accessKey + secretKey + sessionToken + region + service).
2. Passthrough mode (client protocol == upstream protocol, no IR conversion needed).
3. Dispatcher extensibility (SDK-based sending vs raw HTTP).

Extended discussion also clarified the long-term architecture for credpoold and how
translate vs passthrough modes interact with credential types.

### Credential abstraction: behavior over enum

Rejected: `CredentialType = "api_key" | "bearer" | "aws_sigv4"` — encodes presentation
inside the type name, forces switch statements in routing and pipeline layers.

Adopted: `Credential` interface with `Apply(*http.Request) error`. Adding a new auth
scheme is a new struct; no changes to selector, credpool, or pipeline.

Three concrete types cover ~95% of the market:

| Type | Covers |
|---|---|
| `HeaderCredential{Header, Value}` | Anthropic x-api-key, OpenAI Bearer, Azure api-key, OAuth access tokens |
| `QueryCredential{Param, Value}` | Gemini ?key= |
| `SigV4Credential` (stub, Apply returns error) | AWS Bedrock — requires SDKDispatcher, not HTTPDispatcher |

`credentialFromConfig(keyValue, authStyle string) cred.Credential` in `server.go` is the
single point where a raw config string is wrapped into a typed Credential. All downstream
code calls `cred.Apply(req)` only.

### Dispatcher abstraction

`dispatch.Dispatcher` (`core/dispatch/`): `Do(ctx, *http.Request) (*http.Response, error)`.
`HTTPDispatcher` (`internal/server/`): default, wraps `net/http.Client`.
`RouteTarget.Dispatcher` holds one per route; executor reads it instead of owning `httpClient`.

Future `SDKDispatcher` (AWS Bedrock SDK) slots in at `RouteTarget.Dispatcher` with no
changes to executor, pipeline, selector, or routing layers.

### Passthrough mode

`PassthroughTranslator` implements `Translator` but forwards the Anthropic request body
without IR conversion. Enabled via `mode: passthrough` in `ModelEntry`. Used when the
upstream natively accepts the Anthropic Messages API format (e.g. Bedrock with Claude).

The executor is blind to which Translator is active — polymorphism handles it.

### Transparent proxy note (deferred)

If a client agent sends pre-signed SDK requests through miroxy wanting only read-only
plugins (logging, routing decisions): transparent proxy mode could forward the client's
SigV4 signature intact, but body-modifying plugins (token compression, content filtering)
break the signature. Resolution requires miroxy to hold its own Bedrock credentials and
re-sign. This reduces to the standard credential-owned mode. Deferred; architecture
accommodates it via `PassthroughTranslator` + future `SDKDispatcher`.

### credpoold repo name: `openmir-io/credpoold`

The `d` suffix follows Unix daemon naming (containerd, dockerd). The `CredpoolClient`
interface in `internal/cred/credpool.go` is the client-side handshake; it will be replaced
by the connect-go generated client after `buf generate` runs against `credpool.proto`.

### Files changed

**New:** `core/cred/credential.go`, `core/dispatch/dispatcher.go`,
`internal/server/http_dispatcher.go`, `internal/translator/passthrough.go`

**Modified:** `core/selector/credential_source.go` (return type string→Credential),
`core/selector/selector.go` (ExecutionPlan.Credential), `core/router/router.go`
(RouteTarget.Dispatcher), `internal/translator/translator.go` (key string→Credential,
authStyle removed from GeminiTranslator), `internal/cred/oauth.go` (returns HeaderCredential),
`internal/cred/credpool.go` (CredpoolClient.Acquire returns Credential),
`internal/server/executor.go` (Dispatcher.Do), `internal/server/prober.go` (Dispatcher),
`internal/server/server.go` (credentialFromConfig, HTTPDispatcher wiring, Mode support),
`internal/server/direct.go` (directRoute.credential), `internal/config/config.go` (Mode field)

### Deferred

- `SigV4Credential.Apply()` — needs SDKDispatcher (AWS SDK)
- `SDKDispatcher` implementation — deferred until AWS Bedrock support
- Transparent proxy mode (client-owned credentials, read-only pipeline)
- `credpoold` server — proto shape exists; server is a separate repo

---

## 2026-07-03 — Platform / Protocol / Model three-layer separation

### Context

Adding OpenAI, DeepSeek, and GLM providers raised the question of how
"provider" relates to "protocol". AWS Bedrock, for example, is a platform
that hosts many models via different wire formats. Groq, Together AI, and
Azure all speak OpenAI Chat Completions but are distinct platforms with
different auth and endpoint URLs.

The existing `provider` field conflated platform identity with converter
selection. This was fine for Gemini (only one platform speaks that protocol)
but breaks down when many platforms share one protocol (OpenAI-compat) or
one platform hosts multiple protocols (Bedrock Converse vs InvokeModel).

### Decision: three-layer config + optional `protocol` field

The three layers and their config mapping:

```
Platform  →  provider + auth_style + api_base + key_pool
              Who to talk to, how to authenticate, where to send bytes

Protocol  →  protocol (new field; falls back to provider when empty)
              Which ProviderConverter to use — the wire format

Model     →  provider_model
              String passed verbatim into the request body or URL
```

`protocol` is optional. When absent, `provider` is used for both roles
(backward compatibility — all existing configs work unchanged).

This enables three patterns that were previously impossible without code changes:

**Pattern 1: multiple platforms, same protocol**
```yaml
# Groq running Llama — provider ≠ protocol
provider: groq
protocol: openai          # ← reuses OpenAIConverter
api_base: https://api.groq.com/openai/v1
provider_model: llama-3.3-70b-versatile

# Together AI running DeepSeek-R1 — same converter, different platform
provider: together
protocol: openai
api_base: https://api.together.xyz/v1
provider_model: deepseek-ai/DeepSeek-R1

# OpenRouter — aggregator proxy, hundreds of models via one endpoint
provider: openrouter
protocol: openai
api_base: https://openrouter.ai/api/v1
provider_model: anthropic/claude-opus-4
```

**Pattern 2: same platform, different protocol (future — Bedrock)**
```yaml
# Bedrock Converse API (unified interface)
provider: bedrock
protocol: bedrock-converse    # ← future BedrockConverter
api_base: https://bedrock-runtime.us-east-1.amazonaws.com
auth_style: sigv4
provider_model: anthropic.claude-3-5-sonnet-20241022-v2:0

# Bedrock passthrough (Claude natively speaks Anthropic)
provider: bedrock
mode: passthrough
auth_style: sigv4
provider_model: anthropic.claude-opus-4-5-20251101-v1:0
```

**Pattern 3: third-party proxies are transparent**
Any LiteLLM proxy, Portkey, Helicone, or self-hosted relay that exposes
an OpenAI-compat endpoint works with `protocol: openai` + its base URL.
miroxy does not need to know or care what the proxy routes to internally.

### Implementation

`internal/config/config.go`: `Protocol string \`yaml:"protocol"\`` added to
`ModelEntry`. `server.go` reads `m.Protocol`, falls back to `m.Provider`
when empty — a one-liner change, zero impact on existing configs.

The `credentialFromConfig` function already handles `auth_style` independently
of protocol, so platform-layer auth (bearer, api_key, query_key, future sigv4)
is already decoupled from converter selection.

### What this unblocks (deferred, not implemented)

- `BedrockConverter` — Bedrock Converse API wire format + `SigV4Credential`
- `OpenAIResponsesConverter` — OpenAI Responses API (needed for GitHub Copilot)
- `VertexConverter` — Vertex AI with GCP service account auth
- Any future protocol without a dedicated platform

### Files changed

**Modified:** `internal/config/config.go` (Protocol field + three-layer doc comment),
`internal/server/server.go` (dispatch on Protocol with Provider fallback)

**New:** `internal/irc/openai_irc.go`, `internal/irc/deepseek_irc.go`,
`internal/irc/glm_irc.go`, `internal/translator/openai.go`
(OpenAI-compat ProviderConverter + Translator for openai/deepseek/glm protocols)

---

## 2026-07-04 — OpenAI-compat providers, downstream client support, IRC cleanup, terminology

### Context

Extended session covering: adding OpenAI/DeepSeek/GLM/Grok providers, supporting
OpenAI-compatible downstream clients (Codex, openai-python), completing the
Platform/Protocol/Model three-layer separation, and cleaning up IRC naming
inconsistencies accumulated over previous sessions.

---

### Decision 1: OpenAI Chat Completions as UpstreamConverter protocol

Added `openai_irc.go` implementing `UpstreamConverter` for any OpenAI-compatible
upstream. Three providers use it:

- `openai` — standard OpenAI, `NewOpenAIConverter`
- `deepseek` — wire-identical to OpenAI at IR level, `NewOpenAICompatConverter(model, "deepseek")`
- `glm` — overrides temperature clamp [0,1] and finish_reason normalization (`glm_irc.go`)
- `grok` — explicit extension point even though wire-identical today (`grok_irc.go`)

**Rule:** providers wire-identical to OpenAI at the IR abstraction level share
`OpenAIConverter` via `NewOpenAICompatConverter(model, providerLabel)`. A dedicated
`_irc.go` file is only created when there are actual wire-format differences, OR as
an explicit extension point for providers likely to diverge (Grok).

**Key implementation detail:** `DeepSeekConverter` file was deleted (was a 30-line
empty stub). DeepSeek's differences (no seed/logit_bias/seed) are naturally absent
because `IRGenerationConfig` never carries them. Content-as-string constraint is
also naturally satisfied — we never build content arrays.

---

### Decision 2: `/v1/chat/completions` — OpenAI downstream client support

Any OpenAI-compatible client (openai-python, Codex, LiteLLM, curl) can now use
miroxy as a drop-in proxy via `POST /v1/chat/completions`.

**Conversion path:** OpenAI request → Anthropic `MessageRequest` (in-package bridge
using shared `oai*` types) → existing pipeline → Anthropic response → OpenAI
response. No IR changes required; the bridge lives in `openai_irc.go`.

**Streaming:** Anthropic SSE events are re-rendered as OpenAI SSE chunks with state
tracking (content block index → tool_calls[] index, finish_reason normalization).

**Architecture note:** The bridge converts OpenAI ↔ Anthropic types rather than
OpenAI ↔ IR directly. This is intentional for now — it avoids changing the
`Translator` interface (which takes `*types.MessageRequest`). When the pipeline
becomes IR-native (both sides operate on `*ir.IRRequest`), the bridge can be
simplified to OpenAI ↔ IR. This is deferred to the Epic 3 `TranslatorService` work.

---

### Decision 3: Platform / Protocol / Model — `protocol` and `client_protocol` config fields

Added `Protocol string` and `ClientProtocol string` to `ModelEntry`:

```yaml
# Before: provider drove both platform selection AND converter selection
provider: gemini

# After: explicit separation
provider: groq           # platform label (auth + endpoint)
protocol: openai         # wire format → selects UpstreamConverter
client_protocol: openai  # client wire format → future DownstreamConverter selection
provider_model: llama-3.3-70b
```

`client_protocol == protocol` → passthrough (raw body forwarded, no IR conversion).
`mode: passthrough` retained as backward-compatible alias.

DeepSeek and GLM now expose both protocols:
```yaml
# DeepSeek via OpenAI protocol
protocol: openai
api_base: https://api.deepseek.com/v1

# DeepSeek via Anthropic protocol (their native endpoint)
protocol: anthropic          # → passthrough
api_base: https://api.deepseek.com/anthropic
```

---

### Decision 4: IRC naming — `_irc.go` convention, terminology unification

**File naming:** all IRC converter files use `<protocol>_irc.go` suffix. The prefix
is the wire protocol name, not the provider name:
- `anthropic_irc.go` — Anthropic Messages API (implements `DownstreamConverter`)
- `openai_irc.go` — OpenAI Chat Completions (implements `UpstreamConverter`)
- `gemini_irc.go` — Gemini generateContent (implements `UpstreamConverter`)
- `glm_irc.go` — GLM variant of OpenAI (implements `UpstreamConverter`)
- `grok_irc.go` — Grok (OpenAI-compat today, explicit extension point)

**Merges:**
- `upstream_error.go` merged into `irc.go` (single struct, no standalone file needed)
- `openai_client.go` merged into `openai_irc.go` (shares all `oai*` private types)
- `deepseek_irc.go` deleted (was empty stub; use `NewOpenAICompatConverter`)

**Interface renames** (unified downstream/upstream terminology throughout):

| Old name | New name | Rationale |
|---|---|---|
| `FrontendConverter` | `DownstreamConverter` | "frontend" implies UI, not network topology |
| `ProviderConverter` | `UpstreamConverter` | "provider" conflated with platform label |
| `TranslatorBackend` | `UpstreamBackend` | execution backend for upstream converter |
| `front` field | `downstream` field | in Translator structs |
| `backend` field | `upstream` field | in Translator structs |

---

### Files changed (2026-07-04)

**New:** `internal/irc/openai_irc.go`, `internal/irc/glm_irc.go`,
`internal/irc/grok_irc.go`, `internal/translator/openai.go`,
`internal/server/downstream_openai.go`

**Renamed:** `anthropic_irc.go`, `gemini_irc.go`, `glm_irc.go` (all kept `_irc.go`
suffix; `provider_`/`frontend_` prefixes removed)

**Merged:** `upstream_error.go` → `irc.go`; `openai_client.go` → `openai_irc.go`

**Deleted:** `deepseek_irc.go` (replaced by `NewOpenAICompatConverter`)

**Modified:** `internal/config/config.go` (Protocol, ClientProtocol fields),
`internal/server/server.go` (protocol dispatch, /v1/chat/completions endpoint,
grok case), `internal/irc/irc.go` (interface renames + UpstreamError merged),
all translator files (field renames downstream/upstream)

### Deferred

- `OpenAIFrontendConverter` as a proper `DownstreamConverter` implementation —
  currently the OpenAI client bridge converts to Anthropic types rather than IR
  directly. Blocked on making the pipeline IR-native (Epic 3).
- `extra_params` passthrough in config — provider-specific fields not in IR
  (e.g. `grok_search_enabled`) currently require a converter override.

---

## [2026-07-05] CommandPlugin — In-band proxy commands (zero LLM token)

### Problem

Users interact with miroxy through AI clients (Claude Code, Codex). Querying
proxy state (stats, model list, keypool health) required either:

1. A Claude Code skill → always consumed LLM tokens, required client installation
2. CLI (`miroxy stat`) → separate terminal, breaks the in-chat workflow

### Decision

Add a `CommandPlugin` (priority=5, before auth+compress+upstream) that
intercepts messages starting with `!miroxy` at the pipeline level.

The plugin short-circuits the pipeline (does **not** call `next(c)`) and sets
`c.Response` to a synthetic `types.MessageResponse` — zero upstream calls,
zero LLM tokens. Works transparently across all client protocols (Anthropic,
OpenAI Chat Completions, OpenAI Responses API) because interception happens
inside the canonical pipeline, before any protocol translation.

### Syntax

```
!miroxy ?                         — top-level help
!miroxy stats                     — uptime + routing table + keypool health
!miroxy stats <question>          — inject stats into context, ask LLM <question>
!miroxy model show                — list model_name → provider/provider_model mappings
!miroxy model <name>              — override model for this single request
!miroxy model <name> <question>   — override model + forward <question> to LLM
!miroxy reload                    — hot-reload config
!miroxy dump on|off               — toggle dump mode
!miroxy health                    — quick health check
!miroxy <cmd> ?                   — per-command help
```

### Interception rules (explicit-only, never auto-fires)

1. Only checks the **last** message in the array
2. `role` must be **`"user"`** (never assistant / tool_result)
3. Trimmed text must start at **position 0** with `!miroxy`
4. **Case-sensitive** — `/Miroxy stats` does not trigger

### LLM inject path

When extra text follows the command (`!miroxy stats <question>`), the plugin
executes the command, replaces the last user message with:

```
\```
<command output>
\```

<question>
```

Then calls `next(c)` — the LLM receives the enriched context and processes
`<question>`. This path DOES consume LLM tokens.

### Model override

`!miroxy model <name>` sets `c.Request.Model` for the current request only.
The routing layer then looks up `<name>` in `model_list` as usual. No session
state is maintained — the next request reverts to the original model.

---

## [2026-07-11] Credential Status Reporting Architecture Audit

### Context

credstone (a separate repo) is the real, working credential/status service —
plain HTTP/JSON, not the hypothetical gRPC "Credpool" sidecar miroxy had
scaffolding for. This session audited miroxy's credential-handling code
against the decided principles for how local state and credstone should
relate, then retired the dead gRPC path and implemented the follow-up design
(delta-based usage reporting, local TPM tracking).

### Principles audited (condensed)

1. Local in-memory state is always written synchronously; credstone is an
   additive, optional, asynchronous side effect — never a replacement path.
2. Outcome reporting fans out through one existing optional-interface hook,
   not a builtin/credstone runtime branch or a second dispatch mechanism.
3. `CredPool.Select()` checks-and-reserves in one locked step — no two-phase
   `IsUsable()`/`Reserve()` split (that's only worth it for a networked
   limiter, which this isn't).
4. Filters run cheapest-first: health/cooldown state before rate math.
5. Rate-limit machinery no-ops entirely for credentials with no configured
   limit — no wasted lookups.
6. OAuth refresh is meant to be delegated to credstone; miroxy should never
   call a provider's refresh endpoint itself, and should warn when running
   unsafely (multi-replica, no cross-instance coordination).
7. Request-failure handling stays fully local in the hot path; credstone
   never gets a synchronous "what do I do" call — only async outcome pushes.
8. A local, non-persisted, short-lived cooldown marker is allowed as an
   optimization but must never be confused with credstone's authority.
9. `/metrics` and `/stat` are pull-only and reflect local state — zero
   dependency on credstone reachability.

### Findings

| # | Principle | Verdict | Notes |
|---|-----------|---------|-------|
| 1 | Local state always written, credstone additive | OK | `newCredPool` always builds local `CredSpec`s first; a credstone entry is appended, never substituted (`internal/server/server.go`). |
| 2 | Fan-out via existing hook, no new mechanism | OK | The only reporting path is the existing `outcomeReporter` type-assertion hook (`core/selector/credpool.go`), implemented by `CredSource.ReportOutcome`. No `AfterUpstreamCall`/`UsageObserver` exists or was needed. |
| 3 | Single check-and-reserve step | OK | `CredPool.Select()` checks and reserves inside one critical section. No `WouldAllow`/`IsUsable` abstraction exists anywhere in the repo. |
| 4 | Cheapest-first filter ordering | OK | `available(now)` (state/time compare) short-circuits before the RPM length compare, which short-circuits before the new TPM sum-over-window. |
| 5 | Rate machinery no-ops when unlimited | OK for RPM; **MISSING** for TPM/rpd/tpd (now built) | RPM pruning/append is gated on `rateLimit > 0`. TPM, rpd, tpd didn't exist before this session. |
| 6 | OAuth refresh delegated; warn if unsafe | **VIOLATION** (accepted, unavoidable) + **MISSING** (fixed) | `OAuthSource` self-refreshes via the provider's token endpoint — but credstone has no OAuth/refresh support yet (reserved, not implemented on its side), so this is the only way OAuth credentials work today; kept as-is. The missing multi-replica warning was added (`internal/cred/oauth.go: WarnIfMultiReplicaUnsafe`). |
| 7 | No synchronous credstone call in the hot path | **VIOLATION** (fixed) | `CredSource.ReportOutcome` called `client.Release` inline on the executor's retry-loop goroutine — a slow/down credstone added latency before the next `Select()`. Now fired in a goroutine. |
| 8 | Optional local cooldown marker | NOT-YET-APPLICABLE / already satisfied | The existing per-entry `coolEnd` cooldown and `CredSource.lastHealthy` poll cache already serve this purpose; no new marker needed. |
| 9 | `/metrics`/`/stat` independent of credstone | OK | Both read only in-memory config/state (`stats.Registry`, `cfg`); confirmed with a new test pointing credsource at an unreachable address. |

### Dead code retired

- Deleted `internal/cred/credpool.go`, `internal/cred/credpool.proto`, and
  `core/rpc/grpc.go` — the never-built gRPC "Credpool" sidecar path.
  Reconfirmed zero real callers of `NewCredpoolSource`/`CredpoolSource`/
  `CredpoolClient` (the one remaining hit was a comment example in
  `core/rpc/grpc.go`, itself dead TODO scaffolding for the same abandoned
  design). credstone (plain HTTP/JSON via `CredstoneClient`) is now the sole
  third-party credential integration.

### Implemented this session

- **Async outcome reporting** (`internal/cred/credsource.go`): `ReportOutcome`
  now fires its credstone `Release` call in a goroutine instead of blocking
  the retry loop.
- **Multi-replica OAuth warning** (`internal/cred/oauth.go`,
  `internal/server/server.go`): `buildCredSpecsFromPool` now warns once per
  `oauth_refresh` pool when running in what looks like (or can't be ruled
  out as) a multi-replica deployment.
- **Delta-based usage reporting** (`internal/cred/usage_accumulator.go`,
  `internal/cred/credstone_client.go`): new `UsageAccumulator` holds
  per-pool `delta_requests`/`delta_input_tokens`/`delta_output_tokens` since
  the last confirmed-successful send. `delta_requests` is wired through the
  existing `outcomeReporter` hook (`CredSource.SetUsageAccumulator`);
  `delta_input_tokens`/`delta_output_tokens` are fed from
  `UpstreamExecutor` at the same point token counts already flow to
  `stats.Registry` (both non-stream and streaming paths) — no change to the
  `Selector` interface. A periodic flusher reuses the `sync_interval`/
  `StartPoller` pattern rather than inventing a second ticker. Failed sends
  leave deltas accumulated (subtract-what-was-sent, not reset-to-zero) so
  concurrent increments during a flush aren't lost. New
  `CredstoneClient.ReportUsage` defines the wire shape credstone will
  receive — **credstone has no matching endpoint yet** (its `/Release` only
  accepts `rateLimited`/`serverOverload`/`callError`/`retryAfterSeconds`);
  this is a companion gap on the credstone side, tracked separately, not
  blocking this change.
- **Local TPM tracking** (`core/selector/credpool.go`): a token-weighted
  sliding window (`credEntry.recentTokens`) mirrors the existing RPM window
  structure. Checked in `Select()`'s first pass (cheapest-first, after the
  RPM check); the existing "all over the soft limit → best available"
  fallback now also triggers when everyone is over the TPM cap, matching
  RPM's reactive-backstop behavior. Fed via `CredPool.RecordTokens`, an
  optional method callers type-assert for (same pattern as `probeCapable`/
  `outcomeReporter`), since token counts are only known after the response —
  well after `Select`/`Release` already ran. New `KeyPoolCfg.RateLimitTPM`
  config field (0 = disabled, the default).

### Deferred

- Credstone-side `ReportUsage` endpoint and `rpd_limit`/`tpd_limit`
  enforcement — explicitly out of scope for miroxy; do not consider rpd/tpd
  delegation "done" until both sides exist.
- `OAuthSource` remaining a local self-refresh path is accepted debt, not
  fixed — removing it would drop OAuth support entirely until credstone
  ships refresh_token handling.

---

## [2026-07-11] keypools → credpools rename + SigV4 (AWS Bedrock) schema support

### Context

The pool config (`keypools:`, `KeyPoolCfg`/`KeyEntry`) was designed around a
single credential shape: one string per key. That's fine for header/query
auth, but AWS Bedrock's SigV4 credentials need multiple fields per key
(`access_key_id`, `secret_access_key`, `session_token`) plus pool-wide
signing parameters (`region`, `service`) — and "key" no longer described what
a pool actually holds once OAuth refresh tokens and SigV4 material are in
scope too. This renames the config-facing layer to match the runtime layer,
which was already correctly named `core/selector.CredPool` from the start,
and extends the schema to be shape-agnostic.

### Rename

`keypools` → `credpools` across the config YAML, Go types (`KeyPoolCfg` →
`CredPoolCfg`, `KeyEntry` → `CredEntry`, `KeypoolRef` → `CredpoolRef`), the
admin REST API (`/v1/config/keypools` → `/v1/config/credpools`, JSON fields
`keypools`/`keypool_ref` → `credpools`/`credpool_ref`), the OpenAPI doc, CLI
help text, and all example configs + README. `CredPoolCfg` deliberately keeps
the `Cfg` suffix rather than `CredPoolConfig` — `core/selector` already has a
`CredPoolConfig` (the runtime constructor input), and giving the config-layer
type the identical name in a different package would be a real readability
trap in grep results and logs even though Go allows it.

### SigV4 (AWS Bedrock) schema

`kind` (attach shape) and lifecycle (refresh strategy) stay orthogonal, same
principle credstone's own credential-material design already established:
- `CredPoolCfg` gains `AuthStyle` (settable directly on the pool — needed
  because sigv4 validation happens at pool-definition time, before any
  `model_routes` reference is resolved; `namedPoolAuthStyle` prefers it over
  the existing model-route-scanning inference, unchanged for pools that
  don't set it), plus `Region`/`Service` (shared AWS signing parameters for
  every key in the pool).
- `CredEntry` gains `AccessKeyID`/`SecretAccessKey`/`SessionToken`.
  `CredEntry.UnmarshalYAML` (`internal/config/config.go`) grew a branch: the
  existing shorthand (`- label: ${VALUE}`) now also accepts a nested mapping
  as the value (`- label: {access_key_id: ..., ...}`) instead of only a
  scalar; the verbose form grew the same three fields as siblings of
  `name`/`key`. All existing forms (anonymous, shorthand, verbose) are
  unchanged.
- `buildCredSpecsFromPool` (`internal/server/server.go`) gets a third branch
  alongside `oauth_refresh`/default: `authStyle == "sigv4"` builds a
  `*cred.SigV4Credential` from the entry + pool fields, wrapped in the same
  `selector.NewStaticSource` used for plain API keys (no local refresh —
  matches credstone's own note that STS temporary credentials would use a
  `refresh_strategy` mechanism reserved for later, not implemented now).
- `validateKeys` (`internal/config/yaml.go`) takes the resolved `authStyle`
  and requires `access_key_id`+`secret_access_key` for sigv4 entries instead
  of the plain `key` field (`session_token` stays optional).

**Scope note**: this is config-schema + object-model compatibility only.
`cred.SigV4Credential.Apply()` (`core/cred/credential.go`) is still
intentionally unimplemented pending an SDKDispatcher — a bedrock credpool
validates and builds the right typed credential, but actually dispatching a
signed request to Bedrock is a separate, larger epic, unchanged by this
session.

### Verification

`go build ./...`, `go vet ./...`, `go test ./...` all pass. All 8
`config/config.yaml.example*` files load successfully under the renamed
schema (checked with a throwaway `LoadFromBytesWithEnv` run, not shipped). A
`bedrock-claude`-style credpool (structured shorthand and verbose forms, both
with and without `session_token`) parses and validates correctly, and
`buildCredSpecsFromPool` produces `*cred.SigV4Credential` entries with the
right per-key and pool-level fields — covered by new tests in
`tests/unit/config_test.go` and `internal/server/server_test.go`.

---

## [2026-07-11] Two-layer extension model (builtin/sidecar); sidecar config namespace; buntdb local_state cache

### Context

The 2026-07-01 design (`## 2026-07-01 Plugin directory structure`, above)
planned a three-tier execution model — native/builtin, WASM (wazero-embedded
guests), and sidecar (external process, gRPC/HTTP) — with shared cross-process
machinery in `internal/pluginrt/`. This session audited what actually got
built against that plan, prompted by a question about whether the sidecar
layer should support both HTTP and gRPC transports.

### What the audit found

- `internal/pluginrt/sidecar` was itself renamed to `internal/pluginrt/ext`
  on 2026-07-03. Neither name mattered: `ext.Register`/`ext.NewClient` had
  **zero callers** anywhere in the codebase.
- `internal/pluginrt/wasm` (wazero runtime stub, host-function allow-list) —
  also **zero callers**, 0% implemented, `wazero` never added to go.mod.
- `pipeline.PluginSpec`/`PluginLoader`/`BuiltinLoader` (`internal/pipeline/loader.go`)
  — the config-driven `ExecModel: "builtin"|"wasm"|"grpc"` dispatch mechanism
  meant to let a plugin's execution model be chosen at runtime — **zero
  callers**. Every real plugin (`CompressPlugin`, `CommandPlugin`,
  `UpstreamExecutor`) is wired via direct Go constructor calls in
  `internal/server/server.go`, not through this mechanism.
- credstone — meant to be "the first real sidecar consumer" (Decision 7,
  2026-06-30 entry) — never actually went through `pluginrt/ext` either.
  `internal/cred/credstone_client.go` is a self-contained HTTP/JSON client
  with its own `post()` helper; it doesn't call `ext.Register`/`NewClient` or
  implement `ext.Transport`.

This is the same pattern as the `CredpoolSource`/`credpool.proto` deletion
earlier in this session (also zero callers, also a gRPC abstraction built
ahead of a real consumer) — now observed **four times** in one codebase:
every generic extension-point abstraction built before a second real need
existed went unused, while the actual implementation (credstone) took the
simpler, direct route and worked fine without it.

### Decision: two layers, no shared transport abstraction

Collapsed the model to **builtin** and **sidecar**, dropping WASM from the
roadmap entirely (not deferred — actively decided against, given zero
progress and no concrete plugin driving it). Dropped `pluginrt` as a
concept: there is no generic `Sidecar`/`Transport` interface, and none is
planned.

The reasoning: HTTP and gRPC don't need to be unified at a transport-interface
level, because the parts that are actually hard to share (retry policy,
timeout semantics, error-code mapping) are protocol-specific regardless — see
`pluginrt/ext`'s own doc comment, which already conceded "the actual RPC
surface is defined per domain." What's left to share (dial, health-check) is
already well served by `net/http.Client`/`grpc.ClientConn` directly. The
pattern to follow instead — already proven by
`core/cred.CredentialSource` — is: define the extension point as a Go
interface expressing the **capability** (e.g. "give me a Credential"), never
mentioning transport; give it a builtin implementation; give it a
sidecar-backed implementation that picks HTTP or gRPC internally, entirely
hidden from callers. `core/compress.Compressor` (builtin today) or a future
`SecurityScanner` would follow the same shape when a real sidecar for either
exists — no shared plumbing needed ahead of that.

### Removed

- `internal/pluginrt/` (all of it — `ext/` and `wasm/`).
- `internal/pipeline/loader.go` (`PluginSpec`, `PluginLoader`, `BuiltinLoader`).
- Stale comment references (`internal/irc/irc.go`'s `UpstreamBackend` doc
  comment no longer mentions WASM or `pluginrt/ext`).

### Sidecar config namespace

Every optional external-service integration now lives under one `sidecar:`
YAML block, one field per service: `Config.CredSource` → `Config.Sidecar.CredSource`
(`sidecar.credsource.*`), via a new `SidecarConfig` struct
(`internal/config/config.go`). Zero sidecars configured by default. Future
sidecars (compressor, securitygate, ...) get their own field on
`SidecarConfig` when they have a real implementation — not before.

### buntdb-backed local_state cache (standalone mode only)

New optional on-disk cache of per-credential health (state/cooldown/failure
counters), gated by `local_state.enabled` (`internal/config/config.go`'s
`LocalStateConfig`), backed by `github.com/tidwall/buntdb` (pure Go, no CGO).

Scope, deliberately narrow:
- **Health state only** — `state`/`coolEnd`/`rateLimitFailures`/`failures`
  per credential. The RPM/TPM sliding windows are NOT persisted — they're
  1-minute windows; restart-reset is correct behavior, not a gap.
- **Standalone mode only.** `openLocalStateStore` (`internal/server/server.go`)
  refuses to open the store when `sidecar.credsource.enabled` is true, logging
  a warning if `local_state.enabled` was also set. Rationale: credstone is
  already the authoritative, cross-restart (and cross-replica) source of
  credential health in that mode; a local disk cache would just be a second,
  potentially-stale copy with no correctness benefit — same principle as
  keeping `/metrics`/`/stat` independent of credstone, applied in the other
  direction (don't let a local optimization pretend to be authoritative when
  a real authority already exists).
- **Pure cache, self-healing.** `internal/localstate.Open` treats any error
  (missing file, corruption) by deleting and recreating the file; if that
  still fails, it falls back to an in-memory-only buntdb instance so a
  disk problem never blocks startup.
- **Deliberately deferred, not built**: the broader idea discussed this
  session — multi-dimensional usage stats (per model/provider/pool/key,
  session-scoped and all-time, historical session browsing, more stat types
  later) — is NOT implemented. That's a genuinely relational shape
  (aggregation, historical queries) that doesn't fit a KV store well; if it
  becomes a real need, it should be evaluated on its own against something
  like `modernc.org/sqlite` (pure Go, no CGO — but meaningfully heavier in
  binary size and build time than buntdb), not folded into this cache.

`core/selector/credpool.go` stays IO-free per the repo's core/internal split:
it exposes `CredHealthSnapshot`, `CredPool.HealthSnapshot()`, and
`CredPool.RestoreHealth()` as plain serializable data; all actual disk I/O
lives in `internal/localstate` + `internal/server`. Restore happens once at
pool construction; a periodic flush (30s, not currently configurable) mirrors
current state to disk, tied to the same per-generation `pollerCtx` already
used for `CredSource` polling and `UsageAccumulator` flushing — so it starts
and stops correctly across `Reload`/`Close` for free. `Reload` rejects a
config change to `local_state.*` or `sidecar.credsource.enabled` (requires a
restart), consistent with how `server.port`/`admin.addr` already behave.

### Note on CLAUDE.md's "No DB in v1"

This stays within the spirit of that constraint — buntdb here is an
optional, self-healing local **cache**, never a system of record, off by
default, and structurally disabled whenever a sidecar already owns the data
it would cache. It's a materially smaller commitment than a real embedded
SQL database. Flagging this explicitly rather than letting it drift in
silently; see the added clarifying note under that constraint in `CLAUDE.md`.
