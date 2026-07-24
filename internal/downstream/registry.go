package downstream

import (
	coredown "miroxy/core/downstream"
)

// DefaultAdapters returns the built-in downstream protocol adapters.
// Adding a new client protocol = add one entry here (and write the adapter file).
func DefaultAdapters() []coredown.DownstreamAdapter {
	return []coredown.DownstreamAdapter{
		&AnthropicAdapter{},
		&OpenAIAdapter{},    // POST /v1/chat/completions
		&ResponsesAdapter{}, // POST /v1/responses — Codex CLI (wire_api=responses)
	}
}
