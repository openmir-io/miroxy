package warden

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corewarden "miroxy/core/warden"
)

// Stats accumulates warden activity counters across all requests.
// Safe for concurrent use. Mirrors core/compress/stats.go's shape.
type Stats struct {
	mu sync.Mutex

	RequestsInspected atomic.Int64
	SecretsFound      atomic.Int64
	PIIFound          atomic.Int64
	InjectionsBlocked atomic.Int64
	JailbreaksBlocked atomic.Int64
	TokensVaulted     atomic.Int64

	byType map[string]int64 // "category:type" -> count

	startedAt time.Time
}

func NewStats() *Stats {
	return &Stats{byType: make(map[string]int64), startedAt: time.Now()}
}

// Record registers one request's findings (from a single Inspect pass —
// callers must not call Record once per representation scanned, or
// per-type counts double up; see WardenPlugin, which scans c.Request once
// and derives the raw-body redaction from those same findings rather than
// re-inspecting twice) plus how many vault tokens were minted for it.
func (s *Stats) Record(findings []corewarden.Finding, tokensVaulted int) {
	s.RequestsInspected.Add(1)
	if tokensVaulted > 0 {
		s.TokensVaulted.Add(int64(tokensVaulted))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range findings {
		s.byType[f.Category.String()+":"+f.Type]++
		switch f.Category {
		case corewarden.CategorySecret:
			s.SecretsFound.Add(1)
		case corewarden.CategoryPII:
			s.PIIFound.Add(1)
		case corewarden.CategoryInjection:
			if f.Verdict == corewarden.VerdictBlock {
				s.InjectionsBlocked.Add(1)
			}
		case corewarden.CategoryJailbreak:
			if f.Verdict == corewarden.VerdictBlock {
				s.JailbreaksBlocked.Add(1)
			}
		}
	}
}

// Snapshot returns an immutable point-in-time view of the counters.
func (s *Stats) Snapshot() StatsSnapshot {
	s.mu.Lock()
	byType := make(map[string]int64, len(s.byType))
	for k, v := range s.byType {
		byType[k] = v
	}
	s.mu.Unlock()

	return StatsSnapshot{
		RequestsInspected: s.RequestsInspected.Load(),
		SecretsFound:      s.SecretsFound.Load(),
		PIIFound:          s.PIIFound.Load(),
		InjectionsBlocked: s.InjectionsBlocked.Load(),
		JailbreaksBlocked: s.JailbreaksBlocked.Load(),
		TokensVaulted:     s.TokensVaulted.Load(),
		ByType:            byType,
		StartedAt:         s.startedAt,
		SnapshotAt:        time.Now(),
	}
}

// StatsSnapshot is an immutable point-in-time view of Stats.
type StatsSnapshot struct {
	RequestsInspected int64
	SecretsFound      int64
	PIIFound          int64
	InjectionsBlocked int64
	JailbreaksBlocked int64
	TokensVaulted     int64

	// ByType maps "category:type" (e.g. "secret:aws_access_key_id") to a
	// cumulative count.
	ByType map[string]int64

	StartedAt  time.Time
	SnapshotAt time.Time
}

// Format returns a human-readable report for StatsText()'s security section.
func (s StatsSnapshot) Format() string {
	var b strings.Builder

	window := s.SnapshotAt.Sub(s.StartedAt).Round(time.Second)
	fmt.Fprintf(&b, "Window: %s (since %s)\n\n", window, s.StartedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "Requests inspected: %d\n", s.RequestsInspected)
	fmt.Fprintf(&b, "Secrets found:      %d\n", s.SecretsFound)
	fmt.Fprintf(&b, "PII found:          %d\n", s.PIIFound)
	fmt.Fprintf(&b, "Injections blocked: %d\n", s.InjectionsBlocked)
	fmt.Fprintf(&b, "Jailbreaks blocked: %d\n", s.JailbreaksBlocked)
	fmt.Fprintf(&b, "Tokens vaulted:     %d\n", s.TokensVaulted)

	if len(s.ByType) > 0 {
		type kv struct {
			key   string
			count int64
		}
		rows := make([]kv, 0, len(s.ByType))
		for k, v := range s.ByType {
			rows = append(rows, kv{k, v})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].count > rows[j].count })
		fmt.Fprintf(&b, "\nBy type:\n")
		for _, r := range rows {
			fmt.Fprintf(&b, "  %-30s %d\n", r.key, r.count)
		}
	}

	return b.String()
}
