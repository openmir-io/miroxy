# Gemini Adapter — Gap Analysis & Improvement Plan

> Reference implementation: `cc-switch` (`src-tauri/src/proxy/providers/`)
> Subject: `miroxy` (`internal/translator/gemini.go`)
> Date: 2026-06-23

---

## 1. Feature Comparison Matrix

| Feature | cc-switch | miroxy | Gap |
|---|---|---|---|
| **Request translation** | | | |
| system → systemInstruction | ✅ | ✅ | — |
| messages → contents | ✅ | ✅ | — |
| tools → functionDeclarations | ✅ | ✅ | — |
| tool_choice → toolConfig (AUTO/ANY/NONE) | ✅ | ✅ | — |
| tool_choice "tool" → allowedFunctionNames | ✅ | ❌ | maps to ANY instead |
| generationConfig: max_tokens | ✅ | ✅ | — |
| generationConfig: temperature | ✅ | ❌ | not forwarded |
| generationConfig: top_p / top_k | ✅ | ❌ | not forwarded |
| generationConfig: stop_sequences | ✅ | ❌ | not forwarded |
| BatchTool filtering (Claude Code internal) | ✅ | ❌ | no filter |
| thinking param forwarding | ✅ | ❌ | field not modelled |
| **Response translation** | | | |
| text parts → content blocks | ✅ | ✅ | — |
| functionCall → tool_use | ✅ | ✅ | — |
| thought parts → silently dropped | ✅ | ❌ | exposed as text |
| finishReason: STOP / MAX_TOKENS | ✅ | ✅ | — |
| finishReason: SAFETY / RECITATION / OTHER | ✅ | ❌ | falls to default |
| promptFeedback.blockReason → stop_reason="refusal" | ✅ | ❌ | not handled |
| responseId passthrough | ✅ | ❌ | always generates new ID |
| modelVersion passthrough in response model field | ✅ | ❌ | echoes request alias |
| **Tool call handling** | | | |
| tool_result → functionResponse by name | ✅ | ✅ | — |
| tool_use_id → function name lookup | ✅ | ✅ | — |
| synthesize missing tool call IDs | ✅ | partial | miroxy always assigns new; no pre-pass consistency |
| rectify malformed tool call args | ✅ | ❌ | no defensive unmarshaling |
| tool schema hints for arg coercion | ✅ | ❌ | not implemented |
| **Schema sanitization** | | | |
| allowlist filter for function parameters | ✅ | ✅ | — |
| recursive clean: properties / items / anyOf | ✅ | ✅ | — |
| **Error handling** | | | |
| Gemini error → meaningful Go error | ✅ | ✅ | — |
| Gemini HTTP status passthrough (400/403/429) | ✅ | ❌ | always wraps as 500 |
| 429 → retriable by KeyPool | ✅ | ❌ | deferred (CLAUDE.md) |
| **Streaming** | | | |
| Full 7-event SSE sequence | ✅ | ✅ | — |
| text block open/close lifecycle | ✅ | ✅ | — |
| tool_use block in stream | ✅ | ✅ | — |
| input_tokens populated in message_start | ✅ | ❌ | hardcoded to 0 |
| thinking blocks filtered from stream | ✅ | ❌ | would surface as text |
| incremental tool arg streaming | ✅ | ❌ | full args in one chunk |
| **Extended thinking** | | | |
| Thinking Optimizer (inject budget pre-request) | ✅ | ❌ | not implemented |
| Thinking Budget Rectifier (error-driven retry) | ✅ | ❌ | not implemented |
| Thinking Signature Rectifier (strip + retry) | ✅ | ❌ | not implemented |
| GeminiPart.Thought field modelled | ✅ | ❌ | struct incomplete |
| **URL / auth** | | | |
| API key in query string | ✅ | ✅ | — |
| Bearer token auth (relay providers) | ✅ | ❌ | not supported |
| Base URL path normalization | ✅ | ❌ | simple sprintf only |
| Query param merging (base + endpoint) | ✅ | ❌ | not needed yet |
| **Multi-turn / session state** | | | |
| Shadow store (preserve Gemini-native turns) | ✅ | ❌ | stateless only |
| Thought signature preservation across turns | ✅ | ❌ | not applicable yet |

---

## 2. Gaps — Detailed Analysis

### 2.1 GenerationConfig Passthrough (P1 — correctness)

**miroxy current:** `GenerationConfig` only forwards `MaxOutputTokens`.

**Gap:** `temperature`, `top_p`, `top_k`, `stop_sequences` from the Anthropic request are silently dropped. A client that sets `temperature: 0` for deterministic output will get Gemini's default temperature instead.

**Fix:** Extend `GenerationConfig` struct and populate from `req` fields.

```go
// types/gemini.go
type GenerationConfig struct {
    MaxOutputTokens   int      `json:"maxOutputTokens,omitempty"`
    Temperature       *float64 `json:"temperature,omitempty"`
    TopP              *float64 `json:"topP,omitempty"`
    TopK              *int     `json:"topK,omitempty"`
    StopSequences     []string `json:"stopSequences,omitempty"`
}
```

Also add the corresponding fields to `MessageRequest` in `types/anthropic.go`:

```go
type MessageRequest struct {
    // existing fields ...
    Temperature   *float64 `json:"temperature,omitempty"`
    TopP          *float64 `json:"top_p,omitempty"`
    TopK          *int     `json:"top_k,omitempty"`
    StopSequences []string `json:"stop_sequences,omitempty"`
}
```

Anthropic's `stop_sequences` maps directly to `generationConfig.stopSequences`.

---

### 2.2 Thought Parts Leaking into Response (P1 — correctness)

**miroxy current:** `GeminiPart` struct has no `Thought` field. When Gemini returns extended-thinking parts (`"thought": true`), `json.Unmarshal` silently ignores the field, but the `text` field in the same part is non-empty — it contains the raw thinking text and will be exposed to the client as a `text` content block.

**Gap:** The client sees internal Gemini thinking as first-class assistant text.

**Fix in types/gemini.go:**

```go
type GeminiPart struct {
    Text             string                  `json:"text,omitempty"`
    Thought          bool                    `json:"thought,omitempty"`
    FunctionCall     *GeminiFunctionCall     `json:"functionCall,omitempty"`
    FunctionResponse *GeminiFunctionResponse `json:"functionResponse,omitempty"`
}
```

**Fix in gemini.go response parser:** skip parts where `part.Thought == true` before building content blocks. Apply in both `FromUpstream` and `streamGeminiToAnthropic`.

---

### 2.3 Safety Filter / PromptFeedback Handling (P1 — correctness)

**miroxy current:** If Gemini returns a response with no candidates but a `promptFeedback.blockReason` (e.g. `SAFETY`, `PROHIBITED_CONTENT`), the proxy hits the "no candidates" error path and returns a generic 500.

**Gap:** The client receives an opaque server error instead of a meaningful refusal response.

**cc-switch approach:** Check `promptFeedback.blockReason` first; return a well-formed `MessageResponse` with `stop_reason: "refusal"` and a human-readable text block.

**Fix:**

```go
// in FromUpstream, before checking len(Candidates):
if fr := geminiResp.PromptFeedback; fr != nil && fr.BlockReason != "" {
    text := fmt.Sprintf("Request blocked by Gemini safety filters: %s", fr.BlockReason)
    return &types.MessageResponse{
        ID:         idgen.NewMsgID(),
        Type:       "message",
        Role:       "assistant",
        Content:    []types.ContentBlock{{Type: "text", Text: text}},
        StopReason: "refusal",
    }, nil
}
```

Add `PromptFeedback` to `GeminiResponse` struct:

```go
type GeminiResponse struct {
    Candidates     []GeminiCandidate     `json:"candidates"`
    UsageMetadata  GeminiUsageMetadata   `json:"usageMetadata"`
    PromptFeedback *GeminiPromptFeedback `json:"promptFeedback,omitempty"`
    Error          *GeminiError          `json:"error,omitempty"`
}

type GeminiPromptFeedback struct {
    BlockReason string `json:"blockReason"`
}
```

---

### 2.4 FinishReason Completeness (P1 — correctness)

**miroxy current:**

```go
func mapFinishReason(r string) string {
    switch r {
    case "STOP":       return "end_turn"
    case "MAX_TOKENS": return "max_tokens"
    default:           return "end_turn"   // swallows SAFETY, RECITATION, TOOL_CODE, etc.
    }
}
```

**Gap:** `SAFETY`, `RECITATION`, `BLOCKLIST`, `PROHIBITED_CONTENT`, `SPII`, `OTHER`, `TOOL_CODE`, `FUNCTION_CALL` all silently become `end_turn`, hiding why the model stopped.

**Fix:**

```go
func mapFinishReason(r string) string {
    switch r {
    case "STOP":
        return "end_turn"
    case "MAX_TOKENS":
        return "max_tokens"
    case "TOOL_CODE", "FUNCTION_CALL":
        return "tool_use"
    case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
        return "end_turn" // consider "refusal" once Anthropic clients handle it
    default:
        return "end_turn"
    }
}
```

---

### 2.5 Upstream HTTP Status Passthrough (P1 — correctness)

**miroxy current:** All Gemini errors (400, 403, 429, 500) are wrapped as a Go `error` and the server returns 500 to the client.

**Gap:** A client receiving 429 should back off and retry. Getting 500 instead breaks standard retry logic. A 400 from Gemini means a client bug — it should not appear as a server error.

**Fix:** Introduce a structured upstream error type:

```go
// internal/translator/translator.go (or a new internal/apierr/ package)
type UpstreamError struct {
    HTTPStatus int
    Code       int
    Message    string
}
func (e *UpstreamError) Error() string {
    return fmt.Sprintf("upstream %d: %s", e.HTTPStatus, e.Message)
}
```

`FromUpstream` returns `*UpstreamError` when Gemini's HTTP status is not 2xx. The server handler uses `errors.As` to forward the original status code instead of 500.

---

### 2.6 Tool Choice "tool" → allowedFunctionNames (P2 — correctness)

**miroxy current:** `tool_choice: {type: "tool", name: "my_func"}` maps to `ANY`, which lets Gemini pick any function.

**Gap:** The client intended to force one specific function call.

**Fix:**

```go
// types/gemini.go
type GeminiFunctionCallingConfig struct {
    Mode                 string   `json:"mode"`
    AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

// translator/gemini.go
func toolChoiceMode(tc *types.ToolChoice) (string, []string) {
    if tc == nil {
        return "AUTO", nil
    }
    switch tc.Type {
    case "any":
        return "ANY", nil
    case "tool":
        return "ANY", []string{tc.Name}
    case "none":
        return "NONE", nil
    default:
        return "AUTO", nil
    }
}
```

---

### 2.7 Streaming: input_tokens Hardcoded to 0 (P2 — correctness)

**miroxy current:**

```go
Usage: types.Usage{InputTokens: 0, OutputTokens: 1},  // in message_start
```

**Gap:** Clients that track token usage via the stream (Claude Code does this) see 0 input tokens on every request.

**Fix:** Gemini includes `promptTokenCount` in the final SSE chunk's `usageMetadata`. The existing scan already captures `usage`; extend it to also track `PromptTokenCount`. Emit the real `InputTokens` in `message_delta` usage (same position Anthropic's own streaming uses):

```go
// in message_delta send:
Usage: types.DeltaUsage{
    OutputTokens: usage.CandidatesTokenCount,
    InputTokens:  usage.PromptTokenCount,   // add this
},
```

Also add `InputTokens` to `DeltaUsage` in `types/sse.go` if not already present.

---

### 2.8 Bearer Token Auth for Relay Providers (P2 — robustness)

**miroxy current:** Always appends `?key=<apikey>` to the URL. Relay providers (PackyCode, AIGoCode, etc.) typically expect `Authorization: Bearer <key>` instead.

**Fix:** Add an `AuthStyle` field to the model/provider config:

```yaml
# config.yaml
model_list:
  - model_name: claude-sonnet-4-6
    provider: gemini
    provider_model: gemini-2.5-pro
    api_base: https://relay.example.com/v1beta
    auth_style: bearer   # "query_key" (default) | "bearer"
```

In `buildHTTPRequest`, branch on `authStyle`:

```go
if authStyle == "bearer" {
    httpReq.Header.Set("Authorization", "Bearer "+key)
    // omit ?key= from URL
} else {
    // existing: append ?key= to URL
}
```

---

### 2.9 BatchTool Filtering (P2 — robustness)

**miroxy current:** All tools in `req.Tools` are forwarded to Gemini, including `BatchTool` (a Claude Code internal tool type). Gemini rejects unknown tool types.

**Fix:**

```go
for _, tool := range req.Tools {
    if tool.Name == "BatchTool" {
        continue
    }
    decls = append(decls, buildDeclaration(tool))
}
```

---

### 2.10 Malformed Tool Call Arg Rectification (P2 — robustness)

**miroxy current:** `part.FunctionCall.Args` is forwarded as-is. Relay channels occasionally return args as a JSON string instead of an object, which causes schema validation failures on the client.

**Fix:** Defensive unmarshal on `FunctionCall.Args`:

```go
func rectifyArgs(raw json.RawMessage) json.RawMessage {
    if len(raw) == 0 {
        return json.RawMessage(`{}`)
    }
    if raw[0] == '{' {
        return raw // already an object
    }
    // string-encoded JSON — unwrap one level
    var s string
    if json.Unmarshal(raw, &s) == nil && len(s) > 0 && s[0] == '{' {
        return json.RawMessage(s)
    }
    return raw
}
```

Apply in both `FromUpstream` and `streamGeminiToAnthropic` when processing `FunctionCall` parts.

---

### 2.11 Extended Thinking Support (P3 — future)

cc-switch implements a three-stage thinking pipeline. These should be implemented as server-layer middleware in miroxy (not inside the `Translator` interface) to keep the translator format-only.

#### 2.11.1 Thinking Optimizer (pre-request injection)

When provider config enables `thinking_optimizer: true`:

- **Adaptive-capable models** (check model name): inject `thinking: {type: "adaptive"}` + `output_config: {effort: "max"}` + beta header `anthropic-beta: context-1m-2025-08-07`
- **Legacy models**: inject `thinking: {type: "enabled", budget_tokens: max_tokens - 1}` + beta header `anthropic-beta: interleaved-thinking-2025-05-14`
- **Haiku**: skip (not capable)

Requires adding `Thinking` and `OutputConfig` fields to `MessageRequest` and forwarding them through `buildGeminiRequest`.

#### 2.11.2 Thinking Budget Rectifier (error-driven retry)

When upstream error message contains all of: `budget_tokens` + `thinking` + `>= 1024`:

- Re-send request with `thinking.budget_tokens = 32000` (MAX) and `max_tokens` bumped to at least `32001`.
- Skip if `thinking.type == "adaptive"`.
- Lives in the server request lifecycle, not the translator.

#### 2.11.3 Thinking Signature Rectifier (error-driven retry)

When upstream rejects thinking block signatures — 7 known patterns from relay channels:

1. `"invalid" + "signature" + "thinking" + "block"`
2. `"thought signature" + "not valid"`
3. `"must start with a thinking block"`
4. `"expected thinking/redacted_thinking" + "found tool_use"`
5. `"signature" + "field required"`
6. `"signature" + "extra inputs are not permitted"`
7. `"非法请求"` / `"illegal request"` / `"invalid request"` (catch-all)

On match: strip all `thinking` / `redacted_thinking` blocks from prior assistant messages, retry once.

---

### 2.12 Shadow Store for Multi-Turn Thinking (P3 — future)

cc-switch maintains a per-session shadow store of Gemini-native assistant turns. Needed when:

1. Extended thinking is active and the model returns `thought` parts with opaque signatures.
2. The next turn must replay exact Gemini-format history — the Anthropic client round-trips thinking as `redacted_thinking`, which relay channels may strip or corrupt.

**For miroxy v1:** `findFunctionName` by scanning prior messages is sufficient for tool call resolution. The shadow store becomes necessary once extended thinking is implemented and relay channel signature corruption is observed.

If implemented, it belongs in a new `internal/shadow/` package, keyed by `(provider_id, session_id)` derived from the request context.

---

## 3. Improvement Backlog (Prioritised)

### P1 — Fix Now (Correctness Gaps)

| ID | Feature | Files | Effort |
|---|---|---|---|
| G-01 | GenerationConfig: forward temperature, top_p, top_k, stop_sequences | `types/gemini.go`, `types/anthropic.go`, `translator/gemini.go` | S |
| G-02 | Thought parts: add `Thought bool` to GeminiPart; filter in response + stream | `types/gemini.go`, `translator/gemini.go` | S |
| G-03 | Safety filter: handle promptFeedback.blockReason → refusal response | `types/gemini.go`, `translator/gemini.go` | S |
| G-04 | finishReason completeness: SAFETY, RECITATION, TOOL_CODE, FUNCTION_CALL | `translator/gemini.go` | XS |
| G-05 | Upstream HTTP status passthrough (especially 429, 400, 403) | `translator/gemini.go`, `internal/server/server.go` | M |

### P2 — Fix Soon (Robustness / Relay Compatibility)

| ID | Feature | Files | Effort |
|---|---|---|---|
| G-06 | Tool choice "tool" type → allowedFunctionNames | `types/gemini.go`, `translator/gemini.go` | S |
| G-07 | Streaming: emit real input_tokens in message_delta | `translator/gemini.go`, `types/sse.go` | XS |
| G-08 | Bearer token auth style for relay providers | `translator/gemini.go`, `internal/config/config.go` | S |
| G-09 | BatchTool filtering (skip Claude Code internal tools) | `translator/gemini.go` | XS |
| G-10 | Malformed tool call arg rectification | `translator/gemini.go` | S |

### P3 — Future (Extended Thinking)

| ID | Feature | Files | Effort |
|---|---|---|---|
| G-11 | Thinking Optimizer: pre-request thinking param injection | `translator/gemini.go`, `internal/config/config.go` | M |
| G-12 | Thinking Budget Rectifier: error-driven retry on 1024 constraint | `internal/server/server.go` | M |
| G-13 | Thinking Signature Rectifier: strip invalid signatures, retry | `internal/server/server.go`, `translator/gemini.go` | L |
| G-14 | Shadow store: preserve Gemini-native turns across multi-turn thinking | new `internal/shadow/` package | L |

---

## 4. Implementation Notes

### Interface constraints

- **G-05** (HTTP status passthrough) must not change the `Translator` interface signature. Use a sentinel error type (`UpstreamError`) checked with `errors.As` in the server handler.
- **G-08** (Bearer auth) belongs in config, not the translator. The server passes a pre-built auth credential to the translator; the translator doesn't decide the auth style.
- **G-11–G-14** (thinking) are server-layer middleware — pre-request hooks and retry loops. The `Translator` interface stays format-translation-only; it does not own retry logic.

### Testing requirements

- Each G-01 through G-10 item needs a unit test case in `tests/unit/translator_test.go`.
- G-03 (safety filter) and G-05 (status passthrough) also need integration test coverage in `tests/integration/messages_test.go`.
- G-02 (thought filtering) needs a test fixture with a Gemini response body containing `"thought": true` parts, verifying they are absent from the returned `MessageResponse.Content`.

---

## 5. Not Worth Porting

| cc-switch feature | Reason to skip in miroxy |
|---|---|
| Shadow store for non-thinking multi-turn | `findFunctionName` scan is sufficient for v1 |
| responseId passthrough | miroxy owns its own ID namespace; no interop requirement |
| modelVersion passthrough in response `model` field | miroxy intentionally echoes the alias the client sent |
| URL fragment stripping / query param merging | only matters for relay providers with complex base URLs; defer until relay is a use case |
| Copilot optimizer | GitHub Copilot-specific; out of scope |
| Tool schema hints for arg type coercion | advanced relay-channel workaround; not needed for direct Gemini |
