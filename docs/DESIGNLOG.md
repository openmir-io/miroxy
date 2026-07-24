# Miroxy Design Log

Captures key architectural decisions and design discussions as they happen.
Format: dated entries, each covering one decision with context and rationale.
Implementation details belong in code; roadmap belongs in `docs/plan/`.
---

## 2026-07-23 — AWS Bedrock: native SigV4 + EventStream, no AWS SDK; reverses the SDKDispatcher plan

### Context

The 2026-07-03 (`Typed Credential + Dispatcher Abstraction`) and 2026-07-11
(`keypools → credpools rename + SigV4 schema support`) entries both assumed
Bedrock support would arrive via a future `SDKDispatcher` — a second
`dispatch.Dispatcher` implementation wrapping the official AWS SDK, which
would handle SigV4 signing internally. `SigV4Credential.Apply` was left as a
stub error pending that work. This session evaluated that assumption
directly against the alternative — hand-rolling SigV4 signing and AWS
EventStream decoding as plain Go, no AWS SDK — by reading `goai`'s
(`~/oss/goai`, a multi-provider Go LLM library) actual Bedrock implementation
(`provider/bedrock/{bedrock,eventstream,anthropic}.go`). It turned out to be
~200 lines of stdlib `crypto/hmac`/`crypto/sha256` plus a small binary frame
parser — no AWS SDK dependency anywhere in that codebase, for any provider.

### Decision: no `SDKDispatcher`; `SigV4Credential.Apply` signs directly

`SigV4Credential.Apply` (`core/cred/credential.go`) now implements SigV4
signing directly: reads the request body via `req.GetBody()` (already
populated for every caller in this codebase, which all build requests from
`bytes.NewReader`), computes the payload hash, canonical request, and 4-level
HMAC-derived signing key, and sets `Authorization` — no dispatch-layer
involvement. `core/dispatch.Dispatcher` needed no new implementation:
`HTTPDispatcher` covers Bedrock like every other upstream, since auth is
fully resolved before the request reaches the Dispatcher. This retires the
`SDKDispatcher` plan from both 2026-07-03 and 2026-07-11 — those entries are
left as historical record, not rewritten.

### Decision: dedicated `protocol: bedrock`, not `anthropic` or `mode: passthrough`

The 2026-07-03 entry's Pattern 2 sketch showed two options for "Claude
natively speaks Anthropic" on Bedrock: reuse `protocol: anthropic`, or
`mode: passthrough`. Neither actually works: Bedrock's InvokeModel body is
*not* byte-identical to real Anthropic's — it rejects a `"model"` field (the
model is in the URL path instead) and requires
`"anthropic_version": "bedrock-2023-05-31"` in the body where real Anthropic
uses an HTTP header. `PassthroughAdapter` forwards the client's raw bytes
with only a model-field rewrite (`internal/upstream/passthrough.go`) —
insufficient. `AnthropicUpstream` sends `AnthropicConverter.RequestFromIR`'s
output unmodified — also insufficient.

`internal/upstream/bedrock.go` (`BedrockAdapter`, registered as `"bedrock"`
in `registry.go`) is a dedicated `UpstreamAdapter` that reuses
`wireformat.AnthropicConverter` for request/response conversion and the
existing `parseAnthropicSSE`/`anthropicSSEToIR` helpers for streaming
(`passthrough.go`) — adding only the URL shape
(`/model/{id}/invoke[-with-response-stream]`), the small body transform
(delete `model`, set `anthropic_version`), and, for streaming, a translator
(`bedrock_eventstream.go`) that decodes AWS's binary EventStream frames and
re-emits them as plain SSE so the existing Anthropic SSE parser needs no
Bedrock-specific changes.

### What this unblocks (deferred, not implemented)

- Bedrock Converse API (`protocol: bedrock-converse` in the original
  Pattern-2 sketch) — the cross-vendor unified format for non-Anthropic
  Bedrock models (Titan, Llama, etc.). Would need its own wire converter;
  today's `BedrockAdapter` only speaks Anthropic-on-Bedrock.
- A Bedrock-native *downstream* adapter (a client speaking Bedrock's own
  `/model/{id}/invoke` shape directly to miroxy) — unrelated to this change,
  which is upstream-only.

### Files

`core/cred/credential.go`, `internal/upstream/{bedrock,bedrock_eventstream,
registry}.go`, `core/dispatch/dispatcher.go`, `internal/server/{server,
upstream}.go`, `config/config.yaml.example`.
