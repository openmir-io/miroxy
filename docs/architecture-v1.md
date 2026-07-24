# Miroxy Architecture v1

Status: current as of 2026-07-24 (§3, §5, §6 revised for the AWS Bedrock
native-dispatch implementation and passthrough header forwarding). Decision
history for everything below lives in `docs/dev/DESIGNLOG.md`, dated.

## 1. Extension model: builtin + sidecar, no shared transport abstraction

miroxy has exactly two ways a capability gets implemented:

- **builtin** — direct Go call, in-process, zero overhead. The default and
  fallback for everything.
- **sidecar** — an external service, called over whichever transport fits
  that service (HTTP/JSON for credstone today; gRPC would fit a
  high-throughput compression or security-scanning service if/when one is
  built).

There used to be a planned third tier (WASM, via a wazero-embedded sandbox)
and a shared cross-process abstraction (`internal/pluginrt/`: a
`Transport`/`Register`/`NewClient` factory pattern, plus
`pipeline.PluginSpec`/`BuiltinLoader` for config-driven exec-model dispatch).
All of it was deleted 2026-07-11 after an audit found **zero callers** for
any of it — every real plugin (compression, commands, the upstream executor)
is wired via direct Go constructors in `internal/server/server.go`, and the
one real external integration (credstone) never went through the shared
abstraction either; it's a self-contained HTTP client. See the 2026-07-11
DESIGNLOG entry for the full audit.

**The pattern to follow, proven by `core/cred.CredentialSource`:**

```go
// core/cred — the capability, no mention of transport
type CredentialSource interface {
    Credential(ctx context.Context) (cred.Credential, error)
}

// builtin implementations
type StaticSource struct{ ... }   // core/selector
type OAuthSource struct{ ... }    // internal/cred/oauth.go

// sidecar implementation — HTTP/JSON internally, invisible to callers
type CredSource struct{ ... }     // internal/cred/credsource.go
```

Define the interface around **what capability you need**, never the
protocol. Give it a builtin implementation and, when a real external service
exists, a sidecar implementation that picks HTTP or gRPC internally — nothing
above that implementation needs to know which. Do not build a shared
`Transport` interface, a generic plugin loader, or gRPC/HTTP abstraction
ahead of a second real sidecar that needs it. When compression or a security
scanner gets a real sidecar implementation, follow this exact shape:
`core/compress.Compressor` (already exists, builtin-only today) gets a
sidecar-backed implementation the same way `CredentialSource` got `CredSource`.

## 2. Sidecar configuration

Every optional external-service integration lives under one `sidecar:` YAML
namespace, one field per service:

```yaml
sidecar:
  credsource:      # credstone — the only real sidecar today
    enabled: false
    base_url: "http://credstone:8000"
    auth_token: ${CREDSTONE_AUTH_TOKEN}
    sync_interval: 300
  # compressor: {...}    — future, once real
  # securitygate: {...}  — future, once real
```

`internal/config/config.go`'s `SidecarConfig` struct is the extension point —
add a new `<Domain>Config` field there only when that sidecar has a real
implementation, not before. Zero sidecars are configured by default; miroxy
is fully standalone and in-memory out of the box.

### Principles credstone's integration established (apply to any future sidecar)

Audited and enforced 2026-07-11 (see that DESIGNLOG entry for the full
principle-by-principle writeup):

1. **Local in-memory state is always written synchronously; a sidecar is
   strictly additive, async, optional.** `newCredPool` always builds local
   `CredSpec`s first; a credstone-backed entry is appended, never a
   replacement (`internal/server/server.go`).
2. **Outcome reporting fans out through one existing optional-interface
   hook**, not a builtin/sidecar runtime branch. `CredSource` implements
   `outcomeReporter`, checked via type assertion in `CredPool.Release`
   (`core/selector/credpool.go`) — no separate dispatch mechanism.
3. **No synchronous "ask the sidecar" call in the request-serving retry
   path.** `CredSource.ReportOutcome`'s HTTP call to credstone fires in a
   goroutine — a slow/down sidecar never adds latency before the next
   `Select()`.
4. **`/metrics`/`/stat` are pull-only, local-state-only, zero dependency on
   any sidecar's reachability** (`internal/server/admin.go`,
   `registerRoutes`'s `/metrics` handler).
5. **A sidecar is the authority for whatever data it owns — a local disk
   cache must yield to it, not duplicate it.** See §4.

## 3. Credential subsystem

- `core/cred.Credential` — behavior interface (`Apply`, `Type`, `Redacted`).
  Implementations: `HeaderCredential`, `QueryCredential`, `SigV4Credential`
  (signs directly with stdlib `crypto/hmac`/`crypto/sha256` — no AWS SDK
  dependency, no separate dispatcher; `core/dispatch.HTTPDispatcher` covers
  Bedrock like every other upstream since auth is fully resolved before the
  request reaches it — see the 2026-07-23 DESIGNLOG entry, which reverses the
  earlier SDKDispatcher plan).
- `internal/upstream.BedrockAdapter` (`protocol: bedrock`) — a dedicated
  `UpstreamAdapter` for Claude-on-Bedrock, not a variant of `protocol:
  anthropic`: Bedrock's InvokeModel body rejects a `"model"` field (it's in
  the URL path instead) and needs `"anthropic_version"` as a body field
  where real Anthropic uses a header, so neither `AnthropicUpstream` nor
  `PassthroughAdapter` can serve it unmodified. It reuses
  `wireformat.AnthropicConverter` for the request/response shape and adds
  its own URL, body transform, and (for streaming)
  `bedrock_eventstream.go` — an AWS EventStream binary-frame decoder that
  re-emits plain SSE so the existing Anthropic SSE parser needs no
  Bedrock-specific changes. Deferred: a Bedrock Converse API adapter for
  non-Anthropic Bedrock models (Titan, Llama, …), and a Bedrock-native
  *downstream* adapter (a client speaking Bedrock's own wire shape to miroxy).
- `core/selector.CredPool` — the runtime selection pool. Per-credential
  state: circuit-breaker (`stateHealthy`/`stateCoolingDown`/`stateRateLimited`),
  escalating 429 cooldown tiers, RPM sliding window (`recentRequests`), TPM
  sliding window (`recentTokens`, fed via `RecordTokens` — token counts are
  only known after the response, well after `Select`, so this is an optional
  method callers type-assert for, same pattern as `outcomeReporter`).
  `Select()` filters cheapest-check-first and falls back to "best available"
  when every credential is over its soft RPM/TPM limit.
- `internal/config` — YAML schema. `credpools:` (renamed from `keypools:`
  2026-07-11 — "key" stopped describing what a pool holds once OAuth
  refresh tokens and SigV4 material were in scope). A pool's `auth_style`
  (`bearer`/`api_key`/`query_key`/`none`/`sigv4`) is the credential *kind*;
  `type: oauth_refresh` is the orthogonal *lifecycle* — same axis-separation
  credstone's own credential-material design uses. `CredEntry` supports
  plain-string keys (the common case) and, for `sigv4`, a structured form
  (`access_key_id`/`secret_access_key`/`session_token` per key,
  `region`/`service` shared at the pool level).
- `internal/cred` — `CredstoneClient` (plain HTTP/JSON), `CredSource`
  (sidecar `CredentialSource`, health-polled with fast-fail), `OAuthSource`
  (local refresh_token exchange — credstone has no OAuth support yet, so
  this is the only way OAuth credentials work today; warns on likely
  multi-replica deployments since there's no cross-replica coordination),
  `UsageAccumulator` (per-pool rpd/tpd delta accumulator, flushed to
  credstone on an interval, accumulate-until-success on failure — credstone
  has no receiving endpoint for this yet, tracked as a companion gap).

## 4. Local state

Two independent layers, deliberately not conflated:

- **In-memory, always on, never optional.** Every `CredPool`'s
  circuit-breaker/RPM/TPM state lives in process memory regardless of any
  sidecar config — this is what `Select()`/`Release()` actually read on
  every request. Resets on restart; this has always been acceptable (see
  `CLAUDE.md`'s "No DB in v1").
- **Optional on-disk cache, standalone mode only.** `internal/localstate`
  (buntdb-backed, pure Go, no CGO) mirrors each credential's health
  (state/cooldown/failure counts — not the RPM/TPM windows, which are
  short-lived enough that restart-reset is fine) to disk every 30s, restored
  at startup. Gated by `local_state.enabled`; **structurally disabled
  whenever `sidecar.credsource.enabled` is true** — credstone already owns
  cross-restart credential health in that mode, so a local disk copy would
  just be a second, potentially-stale source of truth with no upside. The
  store is a pure cache: any corruption or read error causes it to delete
  and recreate the file, falling back to an in-memory-only instance if that
  still fails — never a startup blocker.

  ```yaml
  local_state:
    enabled: false                     # standalone mode only
    path: ./miroxy-local-state.db
  ```

  Changing `local_state.*` or `sidecar.credsource.enabled` requires a
  restart — `Reload()` rejects the config diff, same as `server.port`.

  **Deliberately not built**: a broader multi-dimensional usage-stats system
  (per model/provider/pool/key, session-scoped and all-time, historical
  session browsing) was discussed and explicitly deferred — that's a
  relational shape (aggregation, historical queries) that doesn't fit a KV
  cache well. If it becomes a real need, evaluate `modernc.org/sqlite`
  (pure Go, no CGO, but meaningfully heavier than buntdb) on its own merits
  rather than folding it into this cache.

## 5. Request pipeline

`internal/pipeline.Plugin` — `Name()`, `Priority()`, `Execute(c, next)`.
Priority order (`internal/pipeline/pipeline.go`): `CommandPlugin` (5, before
everything) → `WardenPlugin` (`PriorityWarden`, 300) → `CompressPlugin`
(350, when enabled) → `UpstreamExecutor` (`PriorityTerminal`, 1000). All
wired directly in `internal/server/server.go` via plain constructor calls —
no config-driven loader.

`UpstreamExecutor` (`internal/server/upstream.go`) owns the retry loop:
`Select` → `dispatchFor` → dispatch → on 429/5xx `Release(err)` and retry the
next candidate → on success `Release(nil)`, record stats/usage/TPM.
Streaming and non-streaming are separate code paths (never buffer a stream
through the non-streaming path). `keyProber` (`internal/server/prober.go`)
probes rate-limited credentials on an escalating schedule when a whole pool
goes dark, so recovery doesn't depend on live traffic alone.

### Real-transform vs. raw-passthrough dispatch

Decided per retry attempt, not per route (2026-07-12, refined 2026-07-18 in
DESIGNLOG). `dispatchFor` compares the request's dynamically-detected
`ClientProtocol` against that attempt's `ExecutionPlan.Protocol` and returns
a `DispatchMode` (`DispatchRaw`/`DispatchIR`) alongside the chosen adapter —
an explicit, observable value rather than an anonymous bool, leaving room
for a future third mode (protocol matches, but a provider-dialect shim still
needs to run). `Select()` runs fresh every attempt, so a single
fallback/round-robin route spanning providers on different protocols
dispatches each attempt independently.

Passthrough forwards `RawRequestBody` — the client's original bytes,
captured before decode — verbatim. Warden's redactions patch it in place
(exact substring replace); Compress's structural rewrites can't be expressed
that way, so `CompressPlugin` sets `LLMContext.RequestRewritten` and
`RefreshRawBodyIfRewritten()` re-marshals the current `Request` into
`RawRequestBody` once — after `MaxTokens` defaulting, before the retry loop
starts — so a passthrough-eligible attempt ships what the pipeline actually
produced, not the client's untouched original bytes.

As of 2026-07-24, `LLMContext.RawRequestHeaders` (captured alongside
`RawRequestBody` in `server.go`, carried via `coreup.WithRawHeaders`)
forwards the client's original headers the same way, minus a small
auth/framing blocklist `PassthroughAdapter` owns itself (`Authorization`,
`Host`, `Content-Length`, `Content-Type`, `Connection`, `Transfer-Encoding`,
`Accept-Encoding`). Byte-for-byte body forwarding alone couldn't carry
protocol-specific headers the IR has no slot for — e.g. Anthropic's required
`anthropic-version` — so a real Anthropic client passed through raw
previously had that header silently dropped.

## 6. What's explicitly not built

- **WASM.** Dropped from the roadmap, not deferred — zero progress, no
  concrete plugin driving it, `wazero` never added as a dependency.
- **Compression/security sidecars.** `core/compress.Compressor` and Warden's
  content-defense engine (`core/warden`/`internal/warden`, secrets/PII/
  injection/jailbreak detection + redaction/tokenization — see 2026-07-12
  DESIGNLOG) are both builtin only today; no external sidecar backend for
  either exists yet.
- **Provider-agnostic superset IR.** `core/ir` and the internal canonical
  `types.MessageRequest` are Anthropic-shaped, not a true union of every
  supported wire protocol's fields (no `seed`/`logprobs`/`n`/penalty/
  reasoning-effort superset, no bidirectional per-provider extension bag).
  Evaluated and deferred 2026-07-18 (see that DESIGNLOG entry) — a real,
  current gap for OpenAI-protocol clients (fields silently dropped at
  decode today), not a hypothetical concern tied to any specific future provider.
- **credstone-side rpd/tpd enforcement + `ReportUsage` endpoint.** miroxy's
  `UsageAccumulator`/`CredstoneClient.ReportUsage` define the wire shape;
  credstone doesn't yet have a receiving endpoint or limit enforcement.
  Companion work, out of scope for miroxy.
- **Bedrock Converse API.** `BedrockAdapter` (see §3) only speaks
  Anthropic-on-Bedrock via `wireformat.AnthropicConverter`; a
  `protocol: bedrock-converse` adapter for non-Anthropic Bedrock models
  (Titan, Llama, …) would need its own wire converter. Not started.
- **Bedrock-native downstream adapter.** No client can speak Bedrock's own
  `/model/{id}/invoke` wire shape *to* miroxy — §3's Bedrock work is
  upstream-only. Not started.
- **Usage-stats-with-session-history system.** See §4 — evaluated and
  deferred, not started.
