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
