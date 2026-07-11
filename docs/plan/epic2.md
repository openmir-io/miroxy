# Epic 2 — WASM Runtime + Multi-Provider (v2.x)

> **Rule:** every story ends in a **runnable binary**.  
> **Prerequisite:** Epic 1 E2E gate must pass before Epic 2 starts.  
> Last updated: 2026-06-27

---

## Status

| Story | State |
|---|---|
| 2.1 WASM skeleton — passthrough plugin | 🔲 planned |
| 2.2 OpenAI translator | 🔲 planned |
| 2.3 SecurityPlugin (Rust WASM, PII) | 🔲 planned |
| 2.4 Encrypted storage + Web UI | 🔲 planned |
| 2.5 Plugin SDK + hot-reload | 🔲 planned |

---

## Todolist

### Story 2.1 — WASM Skeleton

**Prerequisite:** `miroxy-ir` repo must exist with stable `ir.proto` before this story begins.
The WASM ABI uses Protobuf serialization (not raw JSON) so the same data contract works across
Native, WASM, and gRPC backends. See §10 in `docs/design/architecture-v2-three-layer-ward.md`.

- [ ] Create `miroxy-ir` repo: `proto/ir.proto` + `proto/service.proto`; `go generate` produces `gen/go/`; miroxy `go.mod` adds `miroxy-ir` dependency
- [ ] `go get github.com/tetratelabs/wazero`; confirm `CGO_ENABLED=0 go build ./...` passes
- [ ] `internal/pipeline/loader.go` — WASMLoader: read .wasm, compile with wazero, cache compiled module; sync.Pool of instances
- [ ] `internal/pipeline/wasm_plugin.go` — WASMPlugin adapter: `proto.Marshal(ir.Request projection)` → ABI → `proto.Unmarshal(diff)` → apply
- [ ] `docs/design/wasm-abi.md` — ABI v1 spec (exports: execute/alloc/free; Protobuf IRRequest projection; diff schema `{action, patches?}`)
- [ ] `plugins/passthrough/` — Rust guest returning `{action:"continue"}`; `make plugins` target builds passthrough.wasm
- [ ] `config/config.yaml.example` — commented passthrough plugin entry (exec_model: wasm, path, priority)
- [ ] Integration test: passthrough.wasm enabled → Gemini traffic flows unchanged; behavior identical to disabled
- [ ] `go test ./...` green; `CGO_ENABLED=0 go build ./...` succeeds

### Story 2.2 — OpenAI Translator
- [ ] `internal/translator/openai.go` — Anthropic→OpenAI request: messages, tools, tool_choice, sampling params, stop
- [ ] `internal/translator/openai.go` — OpenAI→Anthropic response: content, tool_calls, finish_reason map, usage
- [ ] `internal/translator/openai.go` — streaming: consume OpenAI SSE chunks → emit 7-event Anthropic SSE sequence; accumulate tool call deltas
- [ ] `parseOpenAIRetryAfter` — parse Retry-After header (integer seconds or HTTP date) wired into 429 branch
- [ ] `internal/rectifier/thinking_rectifier.go` — strip thinking/redacted_thinking blocks before OpenAI request
- [ ] `config/config.yaml.example` — OpenAI model entry with APIBase + AuthStyle: bearer
- [ ] Unit tests: request/response conversion; streaming accumulation; tool_calls round-trip; thinking_rectifier strips correctly
- [ ] Integration test: stub OpenAI server → non-stream and stream responses byte-correct
- [ ] `go test ./...` green

### Story 2.3 — SecurityPlugin (Rust WASM)
- [ ] `plugins/security_pii/` — Rust WASM guest: regex PII scan (email, phone, SSN XXX-XX-XXXX, credit card) + injection keyword list
- [ ] Guest returns `{action:"block"|"redact"|"continue", redacted_messages?}`; action_on_detect from config
- [ ] `Makefile` `make plugins` — builds security_pii.wasm via `cargo build --target wasm32-unknown-unknown --release`
- [ ] CI — Rust build in GitHub Actions; artifact stored in plugins/
- [ ] WASMPlugin adapter fail-closed: WASM panic or malformed diff → return block error; miroxy process never crashes
- [ ] Integration test: fake SSN in message → 400 security_violation; clean request passes; WASM panic → 400 not 500
- [ ] Privacy test: mock HTTP client confirms zero outbound calls to non-allowlisted hosts during SecurityPlugin.Execute
- [ ] `go test ./...` green

### Story 2.4 — Encrypted Storage + Web UI
- [ ] `internal/secret/secret.go` — miroxy.s: magic + version + argon2id salt + AES-GCM nonce + ciphertext
- [ ] Unlock priority: MIROXY_MASTER_KEY env → password prompt → ~/.config/miroxy/.key autokey
- [ ] CLI subcommands: `miroxy keys add`, `miroxy keys list`, `miroxy rekey`
- [ ] `ui/` — Alpine.js + plain HTML; tabs: Status, API Keys, Routing, Models, Settings
- [ ] `cmd/miroxy/main.go` — dual-port: proxy 7777, UI 7778; `//go:embed ui/dist`; fully offline (no CDN)
- [ ] Claude Code integration: platform auto-detect (macOS/Linux/Windows paths); Apply (backup + inject apiUrl/authToken); Restore from .miroxy.backup
- [ ] Integration test: write keys to miroxy.s; restart binary; keys available via autokey without re-entry
- [ ] `go test ./...` green; `CGO_ENABLED=0 go build ./...` succeeds

### Story 2.5 — Plugin SDK + Hot-Reload
- [ ] `miroxy-sdk/rust/` — `miroxy_plugin` crate with `#[miroxy_plugin]` proc-macro generating execute/alloc/free + JSON boilerplate
- [ ] `miroxy-sdk/go/` — TinyGo-compatible SDK with same projection/diff types as host; example: token_counter
- [ ] `miroxy-sdk/harness/` — test harness binary: load any .wasm with synthetic LLMContext, print diff; no miroxy binary required
- [ ] Publish ABI spec as `miroxy-sdk/docs/wasm-abi.md` (version field v1 for future negotiation)
- [ ] `PluginSpec.Reload bool` config field (default false)
- [ ] `WASMLoader` — fsnotify watcher on spec.Path when Reload=true; on event: compile new module → write-lock → atomic swap
- [ ] In-flight requests complete against old sync.Pool instance; new requests pick up updated module
- [ ] Hot-reload integration test: replace .wasm mid-load-test; in-flight complete without error; updated behavior active on next request
- [ ] `go test ./...` green

---

## Story 2.1 — WASM Skeleton

**Title:** wazero embedded — passthrough.wasm runs in pipeline, traffic flows end-to-end

**Description:**  
Add `wazero` (zero CGO, zero IPC). Implement `WASMLoader` that compiles `.wasm` files at
startup and caches compiled modules. A `sync.Pool` of instances amortizes instantiation cost.
`WASMPlugin` adapter marshals a minimal `LLMContext` projection to JSON, calls the guest
`execute()` export, and applies the returned diff. Ship a trivial `passthrough.wasm` Rust guest
returning `{action:"continue"}`. Wire as optional plugin in `config.yaml.example`. Traffic
flows unchanged through the passthrough — proves the full WASM machinery works in production
before any real logic runs inside it.

**Runnable check:** passthrough.wasm enabled → Gemini traffic unchanged; `CGO_ENABLED=0 go build ./...` succeeds; `go test ./...` green.

---

## Story 2.2 — OpenAI Translator

**Title:** OpenAI translator — second provider; Gemini + OpenAI both work with retry

**Description:**  
Implement OpenAI chat completions translator. The existing `UpstreamExecutor` retry loop
handles OpenAI 429s at HTTP status level identically to Gemini — no new server code needed.
Add `parseOpenAIRetryAfter` for the `Retry-After` header. Add `thinking_rectifier` Rectifier
rule to strip `thinking`/`redacted_thinking` blocks before OpenAI requests (OpenAI-compatible
endpoints reject unknown content types). `KeywordBackend` now routes to either `gemini` or
`openai` providers.

**Request mapping (Anthropic → OpenAI):**

| Anthropic | OpenAI |
|---|---|
| `messages[]` (system block) | first `role:"system"` message |
| `tools[]` | `tools[]` with `type:"function"` wrapper |
| `tool_choice: {type:"tool"}` | `tool_choice: "required"` / named function |
| `max_tokens`, `temperature`, `top_p` | direct pass-through |
| `stop_sequences` | `stop` |

**Response mapping (OpenAI → Anthropic):**

| OpenAI | Anthropic |
|---|---|
| `choices[0].message.content` | `content[].type:"text"` |
| `choices[0].message.tool_calls[]` | `content[].type:"tool_use"` |
| finish_reason: stop/length/tool_calls | end_turn/max_tokens/tool_use |
| `usage.prompt_tokens` / `completion_tokens` | Anthropic usage |

**Runnable check:** OpenAI model entry in config → non-stream and stream responses correct; Gemini still works; `go test ./...` green.

---

## Story 2.3 — SecurityPlugin (Rust WASM)

**Title:** SecurityPlugin — Rust WASM plugin, PII + prompt injection guard, fail-closed

**Description:**  
First production WASM plugin with real logic. Scans `Messages[*].content` for PII patterns
and prompt injection markers. If the guest panics or returns malformed diff, `WASMPlugin`
blocks the request (fail-closed) — miroxy process never crashes. No request data leaves the
process — stronger privacy than TrueFoundry cloud scan, verifiable by integration test.

```yaml
pipeline:
  plugins:
    - name: security_pii
      exec_model: wasm
      path: /plugins/security_pii.wasm
      priority: 300
      config:
        action_on_detect: block   # or "redact"
        fail_closed: true
```

**Runnable check:** PII-containing request → 400 security_violation; clean request → Gemini response; WASM panic → 400 not crash; `go test ./...` green.

---

## Story 2.4 — Encrypted Storage + Web UI

**Title:** miroxy.s AES-GCM encrypted key storage + embedded Alpine.js UI on port 7778

**Description:**  
Two user-facing features shipped together to unblock zero-CLI onboarding.

**`miroxy.s` file format:**
```
[8 bytes]  magic "MIROXY"
[1 byte]   version 0x01
[1 byte]   mode: 0x01=password, 0x02=autokey
[32 bytes] argon2id salt (time=1, memory=64MB, threads=4)
[12 bytes] AES-GCM nonce
[N bytes]  AES-GCM ciphertext of JSON{keys}
```
Unlock priority: `MIROXY_MASTER_KEY` env → password prompt → `~/.config/miroxy/.key` autokey.

**Web UI:** Alpine.js + plain HTML, all assets via `//go:embed ui/dist`, no CDN. Proxy port
7777, UI port 7778. Claude Code Apply backs up `settings.json` and injects `apiUrl`+`authToken`
pointing at miroxy. Platform auto-detect: macOS/Linux/Windows paths.

**Runnable check:** paste key in UI → saved to miroxy.s; restart → key available via autokey; Apply wires Claude Code settings; `go test ./...` green.

---

## Story 2.5 — Plugin SDK + Hot-Reload

**Title:** miroxy-sdk repo + WASM plugin hot-reload without restart

**Description:**  
**SDK:** Plugin authors get ABI spec, Rust scaffold with `#[miroxy_plugin]` proc-macro
(generates boilerplate), TinyGo scaffold, and a local test harness binary. No miroxy binary
needed to test a plugin — lowers contribution friction.

**Hot-reload:** `PluginSpec.Reload: true` → `fsnotify` watches `.wasm` path. On file event:
compile new module; atomic swap via write-lock on module slot. In-flight requests complete
against old `sync.Pool` instance. Zero downtime for plugin iteration.

**Runnable check:** swap passthrough.wasm while serving; in-flight complete; next request uses updated behavior; `go test ./...` green.

---

## Epic 2 Success Criteria

- [ ] `CGO_ENABLED=0 go build ./...` succeeds (wazero is zero-CGO)
- [ ] passthrough.wasm proxies Gemini traffic unchanged (WASM machinery end-to-end proven)
- [ ] OpenAI non-stream + stream round-trip correct (unit + integration tests with stub server)
- [ ] SecurityPlugin blocks PII request; zero outbound HTTP from miroxy process (privacy test)
- [ ] WASM guest panic → 400 block; miroxy process does not crash
- [ ] miroxy.s round-trip: write keys, restart, keys available without re-entry (autokey mode)
- [ ] Fresh user: binary → UI → paste keys → Apply → Claude Code works — under 3 minutes
- [ ] WASM hot-reload: plugin swap mid-load → in-flight complete; new behavior active
- [ ] `go test ./...` green throughout all stories
