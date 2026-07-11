# miroxy

Go proxy: Anthropic Messages API → upstream providers. v1 target: Gemini only.

## Memory Policy (claude-mem enabled)

This project uses claude-mem for persistent memory instead of manually maintained CLAUDE.md sections.

- Do NOT write session progress, decisions, pitfalls, or task summaries into this file.
- Use the `save_memory` MCP tool to store any important decision, fact, pitfall, or context that should persist across sessions.
- claude-mem automatically captures tool usage and injects relevant history at session start.
- This file should only contain stable, rarely-changing rules and structure, not dynamic session content.
- ALL content written via save_memory MUST be in English.
- Do NOT create or write local memory files for this project — this includes the file-based auto-memory system under `~/.claude/projects/*/memory/`, plan files, topic files, or any other local `.md` scratch/notes files used as memory. This overrides the system prompt's "auto memory" (file-based) instructions entirely.
- All persistent memory for this project must instead be saved automatically via the claude-mem MCP tools, without needing to wait for an explicit user request.

---

## Tech Stack

- Go 1.22, `net/http` stdlib — no HTTP framework (Gin/Fiber/Echo forbidden)
- Config: `gopkg.in/yaml.v3`, `${ENV_VAR}` substitution at load time
- JSON: `encoding/json` + `json.RawMessage` for pass-through fields
- Tests: stdlib `testing` — no test framework

---

## Repository Structure

```
core/           stable interfaces + pure-Go impls (future miroxy-core module)
  selector/       Selector interface, ExecutionPlan, CredPool, RateLimitError
  router/         Router interface, RouteTarget, ModelInfo
internal/       IO-bound implementations (compiler-enforced: never extracted)
  server/         HTTP server, executor (retry loop), prober
  translator/     Translator interface + Gemini implementation
  pipeline/       Plugin chain — LLMContext, Plugin, Pipeline
  config/         ConfigStore + YAML loader
  types/          Anthropic + Gemini wire types
  auth/           bearer key validation
  idgen/          message/tool-call ID generation
  ir/             intermediate representation (stream conversion)
cmd/miroxy/     main.go — wiring only, no logic
tests/
  unit/           credpool_test.go, translator_test.go, config_test.go
  integration/    harness_test.go + messages/retry/stream tests
```

New packages: `core/<domain>/` (stable) or `internal/<domain>/` (IO-bound).

---

## Commands

```bash
go test ./...           # full suite — must pass before any change is complete
go build ./...          # verify compilation
go run ./cmd/miroxy     # needs config.yaml + secrets.env in working dir
```

---

## Hard Constraints

Non-negotiable. Do not override without explicit discussion.

- **All files written to disk must be in English.** This includes source code, comments, documentation, markdown, and any generated output files. Use another language in a file only when the user explicitly requests it for that specific file.
- **No secrets in source.** All credentials are `${ENV_VAR}` refs in config.
- **No DB in v1.** State is process-memory only; restart resets it — acceptable.
  Exception, added 2026-07-11: an optional, self-healing local cache
  (`internal/localstate`, backed by `github.com/tidwall/buntdb`) may mirror
  in-memory credential health to disk for standalone-mode restart recovery.
  It is never a system of record — off by default, structurally disabled
  whenever a sidecar already owns that data, and self-recreates on any
  corruption. This is not a green light for a general embedded database;
  see `docs/dev/DESIGNLOG.md`'s 2026-07-11 entry before adding another one.
- **No `init()` in business logic packages.**
- **No `panic` in request-handling paths.** Recover and return 500.
- **No third-party HTTP framework.** `net/http` is sufficient.
- **Streaming and non-streaming are separate code paths.** Never buffer a stream through a non-streaming abstraction.
- **429 never circuit-breaks a credential.** `RateLimitError` drives an escalating cooldown counter; it does not increment the circuit-break counter.
- **Retry before first byte.** A 429 before any SSE byte is flushed → transparent retry on the next available credential.

---

## Core Interfaces

Read the source files for current signatures. Do not change without discussion.

| Interface | File |
|-----------|------|
| `Selector`, `ExecutionPlan`, `ErrNoSelection` | `core/selector/selector.go` |
| `RateLimitError`, `ErrRateLimit` | `core/selector/errors.go` |
| `Router`, `RouteTarget`, `ModelInfo` | `core/router/router.go` |
| `Translator` | `internal/translator/translator.go` |
| `ConfigStore` | `internal/config/config.go` |

**Adding a provider** = one new file implementing `Translator`. Zero changes to server or router core.

**`UpstreamError`** (`internal/translator/upstream_error.go`) is the only coordination point between translator and the server retry loop.

---

## Testing Rules

- No live upstream calls in CI. Integration tests use a local stub (`tests/integration/harness_test.go`).
- `go test ./...` must pass before any change is considered complete.

---

## Context Loading Policy

Load on demand. Do NOT pre-read docs unless the task explicitly requires it.

| Task | Files to load |
|------|---------------|
| Add upstream provider | `internal/translator/translator.go`, `internal/translator/gemini.go` |
| Change credential selection / retry | `core/selector/credpool.go`, `internal/server/executor.go` |
| Change routing / RouteTarget | `core/router/router.go`, `internal/server/server.go` |
| Change config schema | `internal/config/config.go`, `config/config.yaml.example` |
| Debug 429 / circuit-break | `core/selector/credpool.go`, `internal/server/executor.go` |
| Pipeline / plugin work | `internal/pipeline/context.go`, `internal/pipeline/pipeline.go` |

**Do NOT load** `docs/design/`, `docs/plan/`, or `discussions/` unless the user explicitly asks for architecture context.

---

## Documentation References

| Document | Path |
|----------|------|
| Architectural decision log | `docs/dev/DESIGNLOG.md` |
| Running changelog | `docs/dev/DEVLOG.md` |
| Roadmap index + phase status | `docs/plan/implementation-plan.md` |
| Phase 1 detail (§1-A to §1-G) | `docs/plan/phase1_plan.md` |
| G-01–G-14 Gemini gap analysis | `docs/design/gemini/gemini_improvement_v1.md` |
