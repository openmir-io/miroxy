// Package dump provides request/response capture for debugging.
// Enabled only at trace log level — never in production without explicit config.
//
// Two dump modes:
//
//	transparent: raw_request + upstream_response (no processing, 1-to-1 via trace_id)
//	proxy:       raw_request + miroxy_request + response (shows before/after transformation)
//
// All records in one JSONL file; correlate by trace_id.
package dump

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Dir labels the direction / stage of a captured record.
const (
	DirRawRequest      = "raw_request"      // client → miroxy, before any processing
	DirMiroxyRequest   = "miroxy_request"   // miroxy → upstream, after translation
	DirUpstreamEvent   = "upstream_event"   // upstream SSE event (streaming)
	DirResponse        = "response"         // final response returned to client
	DirComplete        = "complete"         // request lifecycle summary
)

// Record is one line in the JSONL dump file.
type Record struct {
	TraceID    string          `json:"trace_id"`
	Dir        string          `json:"dir"`
	Timestamp  string          `json:"ts"`
	Protocol   string          `json:"proto,omitempty"`
	Model      string          `json:"model,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`   // for request/response bodies
	Event      string          `json:"event,omitempty"`  // SSE event type
	Data       json.RawMessage `json:"data,omitempty"`   // SSE event data
	DurationMs int64           `json:"duration_ms,omitempty"`
	Status     int             `json:"status,omitempty"`
}

// Store writes dump records.
type Store interface {
	Write(r Record) error
	Close() error
}

// ── JSONL store ───────────────────────────────────────────────────────────────

const defaultMaxSizeMB = 10
const defaultMaxBackups = 2

// JSONLStore writes one record per line to a file (or stdout when path is "").
type JSONLStore struct {
	mu      sync.Mutex
	f       *os.File
	path    string // empty = stdout
	own     bool   // true = we opened the file, we close it
	written int64  // bytes written to current file (tracked to avoid stat on every write)
	maxSize int64  // rotate threshold in bytes; 0 = unlimited
	backups int    // number of rotated files to retain
}

// NewJSONLStore opens path for append-write. Pass "" to use stdout.
// Uses default limits (10 MiB rotation, 2 backups). Use NewJSONLStoreWithLimits
// to customise or disable rotation.
func NewJSONLStore(path string) (*JSONLStore, error) {
	return NewJSONLStoreWithLimits(path, defaultMaxSizeMB, defaultMaxBackups)
}

// NewJSONLStoreWithLimits opens path with explicit rotation settings.
// maxSizeMB=0 disables rotation. maxBackups=0 uses the default (2).
func NewJSONLStoreWithLimits(path string, maxSizeMB, maxBackups int) (*JSONLStore, error) {
	if path == "" {
		return &JSONLStore{f: os.Stdout}, nil
	}
	f, err := openDumpFile(path)
	if err != nil {
		return nil, err
	}
	var maxSize int64
	if maxSizeMB > 0 {
		maxSize = int64(maxSizeMB) * 1024 * 1024
	}
	if maxBackups <= 0 {
		maxBackups = defaultMaxBackups
	}
	// Seed written counter from current file size so we honour an existing file.
	if info, err := f.Stat(); err == nil {
		_ = info // written stays 0; rotation triggers after maxSize new bytes
	}
	return &JSONLStore{f: f, path: path, own: true, maxSize: maxSize, backups: maxBackups}, nil
}

func openDumpFile(path string) (*os.File, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("dump: create dir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("dump: open %s: %w", path, err)
	}
	return f, nil
}

func (s *JSONLStore) Write(r Record) error {
	if r.Timestamp == "" {
		r.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.own {
		s.reopenIfDeleted()
	}
	n, err := s.f.Write(b)
	s.written += int64(n)
	if err == nil && s.own && s.maxSize > 0 && s.written >= s.maxSize {
		s.rotate()
	}
	return err
}

// rotate closes the current file, renames it to dump.jsonl.YYYYMMDDHHMMSS,
// prunes old backups beyond s.backups, then opens a fresh dump.jsonl.
// Called under s.mu.
func (s *JSONLStore) rotate() {
	_ = s.f.Close()

	ts := time.Now().UTC().Format("20060102150405")
	archived := fmt.Sprintf("%s.%s", s.path, ts)
	_ = os.Rename(s.path, archived)

	s.pruneBackups()

	f, err := openDumpFile(s.path)
	if err != nil {
		f, _ = openDumpFile(s.path) // best-effort retry
	}
	s.f = f
	s.written = 0
	slog.Info("dump rotated", "archived", archived)
}

// pruneBackups deletes the oldest timestamped backup files when the count
// exceeds s.backups. Called under s.mu.
func (s *JSONLStore) pruneBackups() {
	dir := filepath.Dir(s.path)
	base := filepath.Base(s.path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := base + "."
	var backups []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			backups = append(backups, filepath.Join(dir, e.Name()))
		}
	}
	// backups are named with UTC timestamps — lexicographic sort = chronological order.
	sort.Strings(backups)
	for len(backups) > s.backups {
		_ = os.Remove(backups[0])
		backups = backups[1:]
	}
}

// reopenIfDeleted recreates the dump file if it was deleted while the process
// was running. Called under s.mu. No-op on stdout or if the file still exists.
func (s *JSONLStore) reopenIfDeleted() {
	if _, err := os.Lstat(s.path); !os.IsNotExist(err) {
		return
	}
	f, err := openDumpFile(s.path)
	if err != nil {
		return // can't recreate — continue writing to old fd (data goes to unlinked inode)
	}
	_ = s.f.Close()
	s.f = f
	s.written = 0
}

func (s *JSONLStore) Close() error {
	if s.own {
		return s.f.Close()
	}
	return nil
}

// ── context helpers ───────────────────────────────────────────────────────────

type ctxKeyTraceID struct{}
type ctxKeyStore struct{}

// WithTrace attaches a trace_id and Store to ctx.
func WithTrace(ctx context.Context, traceID string, store Store) context.Context {
	ctx = context.WithValue(ctx, ctxKeyTraceID{}, traceID)
	ctx = context.WithValue(ctx, ctxKeyStore{}, store)
	return ctx
}

// TraceIDFrom returns the trace_id stored in ctx, or "".
func TraceIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyTraceID{}).(string)
	return v
}

// StoreFrom returns the Store stored in ctx, or nil.
func StoreFrom(ctx context.Context) Store {
	v, _ := ctx.Value(ctxKeyStore{}).(Store)
	return v
}

// WriteIfEnabled writes r to the Store in ctx.
// No-op when ctx has no Store (dump disabled).
func WriteIfEnabled(ctx context.Context, r Record) {
	s := StoreFrom(ctx)
	if s == nil {
		return
	}
	r.TraceID = TraceIDFrom(ctx)
	_ = s.Write(r)
}
