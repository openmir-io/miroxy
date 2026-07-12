package irc

import (
	"sync"
	"time"
)

// Gemini's "thinking" models attach a thoughtSignature to functionCall
// parts — an opaque blob binding that call to the model's internal
// reasoning. Once a conversation has used one, every functionCall replayed
// in a later turn's history must carry its signature back verbatim, or
// Gemini rejects the request with INVALID_ARGUMENT.
//
// The client between turns speaks the Anthropic Messages API (see
// anthropic_irc.go): it stores the assistant's tool_use block and echoes it
// back as history on the next request. Anthropic's tool_use block has no
// field for an opaque provider-specific blob, so the signature can't be
// round-tripped through the client — it's correlated server-side instead,
// keyed by the tool-call ID that IS preserved verbatim through that
// round-trip (the same ID already relied on for tool_result matching).
//
// This cache is package-level rather than a GeminiConverter field because
// a single multi-turn conversation can be dispatched through different
// GeminiConverter instances across turns (e.g. round-robin across multiple
// credpools on the same model_routes target) — the signature captured on
// one turn must be visible regardless of which instance handles the next.
var geminiThoughtSigs = newThoughtSigCache(time.Hour)

// thoughtSigCache correlates a tool-call ID with the Gemini thought
// signature captured when that call was parsed out of a response. Entries
// expire after ttl — process-memory only, like the rest of miroxy's v1
// state; a proxy restart mid-conversation loses it, and the next turn of
// that specific tool exchange will hit the same Gemini error this cache
// exists to avoid. Bounded by ttl, not by count: at the scale of one entry
// per tool call this is cheap to sweep in full on every write.
type thoughtSigCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]thoughtSigEntry
}

type thoughtSigEntry struct {
	signature string
	storedAt  time.Time
}

func newThoughtSigCache(ttl time.Duration) *thoughtSigCache {
	return &thoughtSigCache{ttl: ttl, entries: make(map[string]thoughtSigEntry)}
}

func (c *thoughtSigCache) store(id, signature string) {
	if id == "" || signature == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[id] = thoughtSigEntry{signature: signature, storedAt: time.Now()}
	for entryID, e := range c.entries {
		if time.Since(e.storedAt) > c.ttl {
			delete(c.entries, entryID)
		}
	}
}

func (c *thoughtSigCache) lookup(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[id]
	if !ok || time.Since(e.storedAt) > c.ttl {
		return ""
	}
	return e.signature
}
