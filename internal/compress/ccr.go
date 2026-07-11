// Module 3 — CCR (Compress-Cache-Retrieve).
// Stores original content keyed by a short hash so a future LLM call can
// retrieve omitted data via the admin API or a retrieve tool.
//
// Two implementations:
//
//	MemCCRStore  — in-memory, no external deps, session-scoped (default)
//	BoltCCRStore — bbolt-backed, persistent across restarts
//	              (add go.etcd.io/bbolt when persistence is required)
package compress

import (
	"crypto/sha256"
	"fmt"
	"sync"

	ccomp "miroxy/core/compress"
)

// Ensure both implementations satisfy the interface.
var _ ccomp.CCRStore = (*MemCCRStore)(nil)
var _ ccomp.CCRStore = (*NoopCCRStore)(nil)

// ── MemCCRStore ───────────────────────────────────────────────────────────────

// MemCCRStore is a thread-safe in-memory CCR store.
// Data is lost when the process exits; suitable for single-session use.
type MemCCRStore struct {
	mu sync.RWMutex
	m  map[string][]byte
}

// NewMemCCRStore creates an empty in-memory CCR store.
func NewMemCCRStore() *MemCCRStore {
	return &MemCCRStore{m: make(map[string][]byte)}
}

// Store saves content and returns a 12-hex-char hash key.
func (s *MemCCRStore) Store(content []byte) (string, error) {
	hash := shortHash(content)
	s.mu.Lock()
	s.m[hash] = append([]byte(nil), content...) // defensive copy
	s.mu.Unlock()
	return hash, nil
}

// Retrieve fetches previously stored content by hash.
// Returns nil, nil when the hash is not found.
func (s *MemCCRStore) Retrieve(hash string) ([]byte, error) {
	s.mu.RLock()
	v := s.m[hash]
	s.mu.RUnlock()
	if v == nil {
		return nil, nil
	}
	return append([]byte(nil), v...), nil
}

// Close is a no-op for the in-memory store.
func (s *MemCCRStore) Close() error { return nil }

// Len returns the number of entries in the store.
func (s *MemCCRStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.m)
}

// ── NoopCCRStore ──────────────────────────────────────────────────────────────

// NoopCCRStore discards all data silently.
// Used when CCR is disabled (no retrieval markers are needed).
type NoopCCRStore struct{}

func (NoopCCRStore) Store(_ []byte) (string, error) { return "", nil }
func (NoopCCRStore) Retrieve(_ string) ([]byte, error) { return nil, nil }
func (NoopCCRStore) Close() error                     { return nil }

// ── helpers ───────────────────────────────────────────────────────────────────

// shortHash returns the first 12 hex chars of SHA-256(content).
func shortHash(content []byte) string {
	sum := sha256.Sum256(content)
	return fmt.Sprintf("%x", sum[:6]) // 6 bytes → 12 hex chars
}

// injectMarker appends a CCR retrieval marker to compressed output.
// Format: [N items omitted. Retrieve: hash=<hash>]
func injectMarker(compressed []byte, omitted int, hash string) []byte {
	if hash == "" || omitted == 0 {
		return compressed
	}
	marker := fmt.Sprintf("\n[%d items omitted. Retrieve: hash=%s]", omitted, hash)
	return append(compressed, []byte(marker)...)
}

// storeAndMark stores original in ccr, then appends the retrieval marker to
// compressed.  Returns compressed unchanged when ccr is nil or a NoopCCRStore.
func storeAndMark(ccr ccomp.CCRStore, original, compressed []byte, omitted int) ([]byte, error) {
	if ccr == nil {
		return compressed, nil
	}
	if _, ok := ccr.(NoopCCRStore); ok {
		return compressed, nil
	}
	hash, err := ccr.Store(original)
	if err != nil {
		return compressed, err
	}
	return injectMarker(compressed, omitted, hash), nil
}
