# internal/irc — Intermediate Representation Converters

## Purpose

Bidirectional format converters between LLM provider wire formats and the neutral
`core/ir` types. Each file handles one protocol dialect in both directions:

```
ParseRequest(wire) → *ir.IRRequest       (downstream: parse client input)
BuildRequest(*ir.IRRequest) → wire       (upstream: build provider payload)
ParseResponse(wire) → *ir.IRResponse     (upstream: parse provider response)
BuildResponse(*ir.IRResponse) → wire     (downstream: render client response)
ParseStream(body) → <-chan ir.StreamEvent (upstream: parse provider SSE)
BuildStream(events) → client SSE         (downstream: render client SSE)
```

## Current files

- `irc.go` — `FrontendConverter`, `ProviderConverter`, `TranslatorBackend` interfaces
  + `InProcessBackend` (v1 in-process execution seam)
- `anthropic_irc.go` — `AnthropicConverter` (Anthropic ↔ IR, downstream protocol)
- `gemini_irc.go` — `GeminiConverter` (Gemini ↔ IR, upstream protocol)

## Adding a new provider

1. Create `openai_irc.go` implementing `ProviderConverter` (upstream direction)
   OR `FrontendConverter` (if adding OpenAI as a client-facing protocol)
2. Register in the IRC registry (once registry pattern is wired in — see below)
3. Create corresponding translator in `internal/translator/openai.go`
4. Wire in `internal/server/server.go` via `provider:` config field

No changes needed to: server, pipeline, executor, router, selector.

## Future: IRC registry

Currently each translator explicitly instantiates its IRC (e.g.
`irc.NewInProcessBackend(&irc.GeminiConverter{})`). A registry pattern will allow
runtime selection based on the `provider` config field:

```go
// irc/registry.go (future)
var registry = map[string]ProviderConverter{
    "gemini":   &GeminiConverter{},
    "openai":   &OpenAIConverter{},    // add here when implemented
    "deepseek": &DeepSeekConverter{}, // add here when implemented
}

func GetProvider(name string) ProviderConverter { return registry[name] }
```

## Planned provider IRCs

| File | Provider | Status |
|---|---|---|
| `anthropic_irc.go` | Anthropic (client-facing) | ✓ implemented |
| `gemini_irc.go` | Google Gemini | ✓ implemented |
| `openai_irc.go` | OpenAI / compatible | planned Phase 2 |
| `deepseek_irc.go` | DeepSeek | planned |
| `glm_irc.go` | GLM (Zhipu AI) | planned |
| `mistral_irc.go` | Mistral | planned |
| `llama_irc.go` | Llama / Ollama | planned |
| `minimax_irc.go` | MiniMax | planned |
