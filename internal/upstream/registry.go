package upstream

import (
	coreup "miroxy/core/upstream"
)

// registry maps a protocol name to its upstream adapter constructor.
// Adding a new upstream provider = implement it in one file, add one line
// here — no changes to server.go required.
var registry = map[string]func(upstreamModel, apiBase string) coreup.UpstreamAdapter{
	"anthropic": func(m, b string) coreup.UpstreamAdapter { return NewAnthropicUpstream(m, b) },
	"openai":    func(m, b string) coreup.UpstreamAdapter { return NewOpenAI(m, b) },
	"deepseek":  func(m, b string) coreup.UpstreamAdapter { return NewDeepSeek(m, b) },
	"grok":      func(m, b string) coreup.UpstreamAdapter { return NewGrok(m, b) },
	"glm":       func(m, b string) coreup.UpstreamAdapter { return NewGLM(m, b) },
	"bedrock":   func(m, b string) coreup.UpstreamAdapter { return NewBedrock(m, b) },
}

// Get returns the real IR-transform adapter for proto, defaulting to
// Gemini when proto is empty or unrecognized (config's documented default).
func Get(proto, upstreamModel, apiBase string) coreup.UpstreamAdapter {
	if ctor, ok := registry[proto]; ok {
		return ctor(upstreamModel, apiBase)
	}
	return NewGeminiWithConfig(upstreamModel, apiBase)
}
