// Package localstate provides an optional, self-healing on-disk cache of
// local runtime state (currently: per-credential health) for standalone-mode
// deployments. It is never a system of record — the in-memory CredPool
// remains the sole source of truth while serving traffic; this package only
// mirrors that state to disk periodically so a restart doesn't start from
// total amnesia. See docs/design/architecture-v3.md for the full rationale.
package localstate

import (
	"encoding/json"
	"log/slog"
	"os"
	"strings"

	"github.com/tidwall/buntdb"
)

// CredHealth is a serializable snapshot of one credential's health state.
type CredHealth struct {
	State             string `json:"state"`
	CoolEndUnixNano   int64  `json:"cool_end_unix_nano"`
	RateLimitFailures int    `json:"rate_limit_failures"`
	Failures          int    `json:"failures"`
}

// Store is a small buntdb-backed cache. Every operation degrades gracefully:
// a corrupt or unreadable file is deleted and recreated, and if that still
// fails, Store falls back to an in-memory-only buntdb instance so callers
// never fail to start because of this cache.
type Store struct {
	db *buntdb.DB
}

// Open opens (or creates) the buntdb file at path, self-healing on any error.
func Open(path string) *Store {
	db, err := buntdb.Open(path)
	if err != nil {
		slog.Warn("local_state: file unreadable, deleting and recreating", "path", path, "error", err)
		_ = os.Remove(path)
		db, err = buntdb.Open(path)
	}
	if err != nil {
		slog.Warn("local_state: still unreadable after recreate, falling back to in-memory (nothing will persist)",
			"path", path, "error", err)
		db, err = buntdb.Open(":memory:")
		if err != nil {
			// buntdb's :memory: mode does no file I/O and is not expected to
			// fail; if it somehow does, run with an unusable Store rather
			// than panicking — every method below is a no-op on a nil db.
			slog.Warn("local_state: in-memory fallback also failed, local state caching disabled", "error", err)
		}
	}
	return &Store{db: db}
}

// Close releases the underlying buntdb file/handle.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

func credKeyPrefix(poolName string) string {
	return "cred:" + poolName + ":"
}

// SaveAllCredHealth persists every entry in snap for poolName in one
// transaction. A single bad (unmarshalable) entry is skipped, not fatal —
// this is a cache; losing one credential's snapshot costs nothing but that
// credential starting fresh.
func (s *Store) SaveAllCredHealth(poolName string, snap map[string]CredHealth) error {
	if s.db == nil || len(snap) == 0 {
		return nil
	}
	prefix := credKeyPrefix(poolName)
	return s.db.Update(func(tx *buntdb.Tx) error {
		for credID, h := range snap {
			data, err := json.Marshal(h)
			if err != nil {
				continue
			}
			if _, _, err := tx.Set(prefix+credID, string(data), nil); err != nil {
				return err
			}
		}
		return nil
	})
}

// LoadAllCredHealth returns every persisted health snapshot for poolName,
// keyed by credential ID. Missing or corrupt entries are silently skipped.
func (s *Store) LoadAllCredHealth(poolName string) map[string]CredHealth {
	out := map[string]CredHealth{}
	if s.db == nil {
		return out
	}
	prefix := credKeyPrefix(poolName)
	_ = s.db.View(func(tx *buntdb.Tx) error {
		return tx.AscendKeys(prefix+"*", func(key, value string) bool {
			var h CredHealth
			if err := json.Unmarshal([]byte(value), &h); err == nil {
				out[strings.TrimPrefix(key, prefix)] = h
			}
			return true
		})
	})
	return out
}

// WardenStats is a serializable snapshot of warden's cumulative counters —
// same shape/purpose as CredHealth, just one global blob instead of one
// entry per credential (there's exactly one warden instance per process).
type WardenStats struct {
	RequestsInspected int64            `json:"requests_inspected"`
	SecretsFound      int64            `json:"secrets_found"`
	PIIFound          int64            `json:"pii_found"`
	InjectionsBlocked int64            `json:"injections_blocked"`
	JailbreaksBlocked int64            `json:"jailbreaks_blocked"`
	TokensVaulted     int64            `json:"tokens_vaulted"`
	ByType            map[string]int64 `json:"by_type"`
	StartedAtUnixNano int64            `json:"started_at_unix_nano"`
}

const wardenStatsKey = "warden:stats"

// SaveWardenStats persists stats as a single JSON blob, overwriting any
// prior snapshot.
func (s *Store) SaveWardenStats(stats WardenStats) error {
	if s.db == nil {
		return nil
	}
	data, err := json.Marshal(stats)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *buntdb.Tx) error {
		_, _, err := tx.Set(wardenStatsKey, string(data), nil)
		return err
	})
}

// LoadWardenStats returns the persisted snapshot, or (zero, false) when
// none exists yet or the store is a no-op (nil db) — same "missing or
// corrupt is not fatal" tolerance as LoadAllCredHealth.
func (s *Store) LoadWardenStats() (WardenStats, bool) {
	var out WardenStats
	if s.db == nil {
		return out, false
	}
	found := false
	_ = s.db.View(func(tx *buntdb.Tx) error {
		val, err := tx.Get(wardenStatsKey)
		if err != nil {
			return nil
		}
		if json.Unmarshal([]byte(val), &out) == nil {
			found = true
		}
		return nil
	})
	return out, found
}

// TokenStats is a serializable snapshot of process-wide token usage
// counters — model route → credpool → credential — same single-blob
// approach as WardenStats. localstate has no dependency on internal/stats;
// callers convert at the boundary (internal/server).
type TokenStats struct {
	TotalInput    int64             `json:"total_input"`
	TotalOutput   int64             `json:"total_output"`
	TotalRequests int64             `json:"total_requests"`
	Models        []TokenModelStats `json:"models"`
}

type TokenModelStats struct {
	Name     string           `json:"name"`
	Input    int64            `json:"input"`
	Output   int64            `json:"output"`
	Requests int64            `json:"requests"`
	Pools    []TokenPoolStats `json:"pools"`
}

type TokenPoolStats struct {
	Name     string          `json:"name"`
	Input    int64           `json:"input"`
	Output   int64           `json:"output"`
	Requests int64           `json:"requests"`
	Keys     []TokenKeyStats `json:"keys"`
}

type TokenKeyStats struct {
	Name     string `json:"name"`
	Input    int64  `json:"input"`
	Output   int64  `json:"output"`
	Requests int64  `json:"requests"`
}

const tokenStatsKey = "token:stats"

// SaveTokenStats persists stats as a single JSON blob, overwriting any
// prior snapshot.
func (s *Store) SaveTokenStats(stats TokenStats) error {
	if s.db == nil {
		return nil
	}
	data, err := json.Marshal(stats)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *buntdb.Tx) error {
		_, _, err := tx.Set(tokenStatsKey, string(data), nil)
		return err
	})
}

// LoadTokenStats returns the persisted snapshot, or (zero, false) when none
// exists yet or the store is a no-op (nil db).
func (s *Store) LoadTokenStats() (TokenStats, bool) {
	var out TokenStats
	if s.db == nil {
		return out, false
	}
	found := false
	_ = s.db.View(func(tx *buntdb.Tx) error {
		val, err := tx.Get(tokenStatsKey)
		if err != nil {
			return nil
		}
		if json.Unmarshal([]byte(val), &out) == nil {
			found = true
		}
		return nil
	})
	return out, found
}

// CompressStats is a serializable snapshot of process-wide compression
// counters — mirrors core/compress.Stats' billing-relevant totals, not its
// strategy/latency history (session-scoped diagnostics, not worth persisting).
type CompressStats struct {
	TotalRequests   int64                `json:"total_requests"`
	TotalOriginal   int64                `json:"total_original_tokens"`
	TotalCompressed int64                `json:"total_compressed_tokens"`
	Models          []CompressModelStats `json:"models"`
}

type CompressModelStats struct {
	Name             string `json:"name"`
	Requests         int64  `json:"requests"`
	OriginalTokens   int64  `json:"original_tokens"`
	CompressedTokens int64  `json:"compressed_tokens"`
}

const compressStatsKey = "compress:stats"

// SaveCompressStats persists stats as a single JSON blob, overwriting any
// prior snapshot.
func (s *Store) SaveCompressStats(stats CompressStats) error {
	if s.db == nil {
		return nil
	}
	data, err := json.Marshal(stats)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *buntdb.Tx) error {
		_, _, err := tx.Set(compressStatsKey, string(data), nil)
		return err
	})
}

// LoadCompressStats returns the persisted snapshot, or (zero, false) when
// none exists yet or the store is a no-op (nil db).
func (s *Store) LoadCompressStats() (CompressStats, bool) {
	var out CompressStats
	if s.db == nil {
		return out, false
	}
	found := false
	_ = s.db.View(func(tx *buntdb.Tx) error {
		val, err := tx.Get(compressStatsKey)
		if err != nil {
			return nil
		}
		if json.Unmarshal([]byte(val), &out) == nil {
			found = true
		}
		return nil
	})
	return out, found
}
