# internal/semantic — Semantic Content Transformation Layer

## Purpose

Pipeline plugins that transform the *content* of LLM requests and responses — distinct
from protocol conversion (irc/) which transforms *format*. Semantic plugins operate on
the IR after downstream parsing and before upstream sending, and optionally on the IR
response before it is rendered back to the client.

All plugins in this package implement `pipeline.Plugin` and declare `Mutates() bool`
to signal whether they modify the request body (body mutation invalidates pre-existing
auth signatures for transparent-proxy scenarios).

## Planned sub-modules

### language.go — Language Translation

Convert the prompt or response between human languages.

Examples:
- Chinese (Simplified) → English (for better LLM performance on English-optimised models)
- English → Chinese (response localisation)
- Any language pair supported by a translation backend

Implementation options:
- **Inline**: lightweight model (e.g. small NLLB or MarianMT via WASM plugin)
- **Sidecar**: call a dedicated translation service via `pluginrt/ext` gRPC

### dialect.go — Character Set / Dialect Conversion

Convert within the same base language.

Examples:
- Traditional Chinese → Simplified Chinese
- Simplified Chinese → Traditional Chinese
- British English ↔ American English ↔ Australian English ↔ Singapore English ↔ Indian English

### register.go — Register / Tone Rewriting

Change the register or tone of the text without changing the language.

Examples:
- Formal → Casual
- Technical → Plain-language
- Academic → Blog post style

### encode.go — Special Encoding Modes

Encode or transform text into non-standard representations.

Examples:
- Caveman speak (like GitHub's caveman extension): "me want file, me not understand error"
- Pirate speak: "Arrr, yer code be broken, matey"
- Simplified shorthand for token compression

### compress.go — Semantic Token Compression

Reduce input token count while preserving meaning.

Examples:
- Remove redundant tool_result noise (similar to RTK from 9router)
- Summarise long conversation history
- Strip irrelevant context blocks

Reference: 9router implements RTK (token reduction kit) claiming 20–40% input savings.

## Architecture note

Each sub-module is a `pipeline.Plugin`. The pipeline sorts by Priority and calls
`Execute(c *LLMContext, next Handler)`. Semantic plugins run after auth/security and
before the upstream executor.

```
pipeline order:
  PriorityAuth      (0)
  PriorityObserve   (100)
  PrioritySecurity  (300)
  PriorityRectifier (400)  ← future: arg rectification moves here
  PrioritySemantic  (500)  ← this package
  PriorityRouter    (600)
  PriorityTerminal  (1000) ← upstream executor
```

## Three execution modes

Following miroxy's universal three-tier model, each semantic plugin can run as:
- **Native** (in-process Go): fast, zero IPC, good for rule-based transforms
- **WASM** (`pluginrt/wasm`): sandboxed, good for stateless ML models
- **RPC** (`pluginrt/ext` via `core/rpc`): external service, good for heavy models

## Dependencies

- `core/ir` — operates on IRRequest/IRResponse content
- `core/rpc` — when calling a semantic sidecar service
- `pluginrt/wasm` — when running a WASM semantic plugin
- `pluginrt/ext` — when delegating to an external process
