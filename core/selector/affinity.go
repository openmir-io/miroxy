package selector

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"miroxy/core/ir"
)

const (
	affinityFingerprintTextLen = 256
	affinityPruneThreshold     = 4096
)

type affinityEntry struct {
	id       string
	lastSeen time.Time
}

// affinityMap is a sticky session-key -> id cache with idle-TTL expiry.
// CredPool and RoutingSelector each own their own instance and lock.
type affinityMap struct {
	mu      sync.Mutex
	entries map[string]affinityEntry
	ttl     time.Duration
}

func newAffinityMap(ttl time.Duration) *affinityMap {
	return &affinityMap{entries: make(map[string]affinityEntry), ttl: ttl}
}

// Get returns the sticky id for key, or ok=false if absent or idle-expired.
func (a *affinityMap) Get(key string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.entries[key]
	if !ok {
		return "", false
	}
	if time.Since(e.lastSeen) > a.ttl {
		delete(a.entries, key)
		return "", false
	}
	return e.id, true
}

// Set records/refreshes the sticky binding for key.
func (a *affinityMap) Set(key, id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries[key] = affinityEntry{id: id, lastSeen: time.Now()}
	if len(a.entries) > affinityPruneThreshold {
		a.pruneLocked()
	}
}

// pruneLocked drops idle-expired entries; caller must hold a.mu.
func (a *affinityMap) pruneLocked() {
	now := time.Now()
	for k, e := range a.entries {
		if now.Sub(e.lastSeen) > a.ttl {
			delete(a.entries, k)
		}
	}
}

// SessionKeyFromRequest fingerprints model + system prompt + first message
// (stable across turns, unlike a growing prefix). Returns "" if unkeyable.
func SessionKeyFromRequest(req *ir.IRRequest, model string) string {
	if req == nil || len(req.Messages) == 0 {
		return ""
	}
	if req.UserID != "" {
		return "uid:" + model + ":" + req.UserID
	}

	var b strings.Builder
	b.WriteString(model)
	b.WriteByte(0)
	b.WriteString(req.System)
	b.WriteByte(0)
	b.WriteString(req.Messages[0].Role)
	b.WriteByte(':')
	writeFingerprintContent(&b, req.Messages[0])

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// writeFingerprintContent appends a stable, truncated representation of a
// message's content parts — text is capped in length, tool ids are used verbatim.
func writeFingerprintContent(b *strings.Builder, m ir.IRMessage) {
	for _, p := range m.Parts {
		switch {
		case p.Text != nil:
			b.WriteString(truncate(p.Text.Text, affinityFingerprintTextLen))
		case p.ToolUse != nil:
			b.WriteString("tu:" + p.ToolUse.ID)
		case p.ToolResult != nil:
			b.WriteString("tr:" + p.ToolResult.ToolUseID)
		case p.Reasoning != nil:
			if p.Reasoning.Text != "" {
				b.WriteString(truncate(p.Reasoning.Text, affinityFingerprintTextLen))
			} else {
				b.WriteString("re:" + truncate(p.Reasoning.Signature, affinityFingerprintTextLen))
			}
		}
		b.WriteByte('|')
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
