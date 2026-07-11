# miroxy Architecture v3

Status: current as of 2026-07-11. Supersedes the extension-model sections of
`architecture.md` and `architecture-v2-three-layer-ward.md` (their
native/WASM/sidecar three-tier plan) and the credential-handling sections of
`Introduction.md`. Those documents also cover early routing/translator
exploration that's mostly still accurate as history, but read this file
first for how things actually work today. Decision history for everything
below lives in `docs/dev/DESIGNLOG.md`, dated.

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
  (config/object-model complete; `Apply()` intentionally unimplemented
  pending an SDKDispatcher for real AWS Bedrock request signing — a separate,
  larger epic).
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
everything) → `CompressPlugin` (when enabled) → `UpstreamExecutor`
(`PriorityTerminal`, 1000). All wired directly in `internal/server/server.go`
via plain constructor calls — no config-driven loader.

`UpstreamExecutor` (`internal/server/upstream.go`) owns the retry loop:
`Select` → dispatch → on 429/5xx `Release(err)` and retry the next candidate
→ on success `Release(nil)`, record stats/usage/TPM. Streaming and
non-streaming are separate code paths (never buffer a stream through the
non-streaming path). `keyProber` (`internal/server/prober.go`) probes
rate-limited credentials on an escalating schedule when a whole pool goes
dark, so recovery doesn't depend on live traffic alone.

## 6. What's explicitly not built

- **WASM.** Dropped from the roadmap, not deferred — zero progress, no
  concrete plugin driving it, `wazero` never added as a dependency.
- **Compression/security sidecars.** `core/compress.Compressor` is builtin
  only today. No `SecurityScanner`/redaction capability exists yet in any
  form.
- **credstone-side rpd/tpd enforcement + `ReportUsage` endpoint.** miroxy's
  `UsageAccumulator`/`CredstoneClient.ReportUsage` define the wire shape;
  credstone doesn't yet have a receiving endpoint or limit enforcement.
  Companion work, out of scope for miroxy.
- **AWS Bedrock request dispatch.** `SigV4Credential` is fully wired at the
  config/object-model level; actually signing and sending a request needs an
  SDKDispatcher that doesn't exist yet.
- **Usage-stats-with-session-history system.** See §4 — evaluated and
  deferred, not started.
