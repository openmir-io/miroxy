package compress

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Stats accumulates compression statistics across all requests.
// Safe for concurrent use.
type Stats struct {
	mu sync.Mutex

	Requests         atomic.Int64
	OriginalTokens   atomic.Int64
	CompressedTokens atomic.Int64

	// strategy counters: strategy label → (count, tokens_saved)
	strategyCount  map[string]int64
	strategySaved  map[string]int64

	// latency histogram (microseconds)
	latencies []int64

	startedAt time.Time
}

// NewStats creates a zeroed Stats tracker.
func NewStats() *Stats {
	return &Stats{
		strategyCount: make(map[string]int64),
		strategySaved: make(map[string]int64),
		startedAt:     time.Now(),
	}
}

// Record registers one compression result.
// latencyUs is the compression duration in microseconds.
func (s *Stats) Record(result *Result, latencyUs int64) {
	s.Requests.Add(1)
	s.OriginalTokens.Add(int64(result.OriginalTokens))
	s.CompressedTokens.Add(int64(result.CompressedTokens))
	saved := int64(result.OriginalTokens - result.CompressedTokens)

	s.mu.Lock()
	for _, strat := range result.Strategies {
		// Strategy labels can carry counts like "crush(tool_result,2000→400)".
		// Use the label prefix up to the first '(' as the key.
		key := stratKey(strat)
		s.strategyCount[key]++
		s.strategySaved[key] += saved
	}
	if latencyUs > 0 {
		s.latencies = append(s.latencies, latencyUs)
	}
	s.mu.Unlock()
}

func stratKey(label string) string {
	if i := strings.IndexByte(label, '('); i > 0 {
		return label[:i]
	}
	return label
}

// Snapshot returns an immutable snapshot of the current counters.
func (s *Stats) Snapshot() StatsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	sc := make(map[string]int64, len(s.strategyCount))
	ss := make(map[string]int64, len(s.strategySaved))
	for k, v := range s.strategyCount {
		sc[k] = v
	}
	for k, v := range s.strategySaved {
		ss[k] = v
	}

	var p50, p95, maxLat int64
	lats := make([]int64, len(s.latencies))
	copy(lats, s.latencies)
	if len(lats) > 0 {
		sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
		p50 = lats[len(lats)/2]
		p95 = lats[int(float64(len(lats))*0.95)]
		maxLat = lats[len(lats)-1]
	}

	orig := s.OriginalTokens.Load()
	comp := s.CompressedTokens.Load()
	reqs := s.Requests.Load()

	return StatsSnapshot{
		Requests:         reqs,
		OriginalTokens:   orig,
		CompressedTokens: comp,
		SavedTokens:      orig - comp,
		StrategyCount:    sc,
		StrategySaved:    ss,
		LatencyP50Us:     p50,
		LatencyP95Us:     p95,
		LatencyMaxUs:     maxLat,
		StartedAt:        s.startedAt,
		SnapshotAt:       time.Now(),
	}
}

// StatsSnapshot is an immutable point-in-time view of Stats.
type StatsSnapshot struct {
	Requests         int64
	OriginalTokens   int64
	CompressedTokens int64
	SavedTokens      int64

	// StrategyCount maps strategy key → number of times applied.
	StrategyCount map[string]int64
	// StrategySaved maps strategy key → approximate tokens saved.
	StrategySaved map[string]int64

	// Latency percentiles in microseconds.
	LatencyP50Us int64
	LatencyP95Us int64
	LatencyMaxUs int64

	StartedAt  time.Time
	SnapshotAt time.Time
}

// ReductionPct returns the percentage of tokens removed (0–100).
func (s StatsSnapshot) ReductionPct() float64 {
	if s.OriginalTokens == 0 {
		return 0
	}
	return float64(s.SavedTokens) / float64(s.OriginalTokens) * 100
}

// Format returns a human-readable performance report similar to headroom perf.
func (s StatsSnapshot) Format() string {
	var b strings.Builder

	window := s.SnapshotAt.Sub(s.StartedAt).Round(time.Second)

	fmt.Fprintf(&b, "miroxy Compression Performance Report\n")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("=", 60))
	fmt.Fprintf(&b, "Window: %s (since %s)\n\n",
		window, s.StartedAt.Format("2006-01-02 15:04:05"))

	fmt.Fprintf(&b, "Requests:    %s\n", commaInt(s.Requests))
	fmt.Fprintf(&b, "Tokens:      %s -> %s (%.1f%% reduction)\n",
		commaInt(s.OriginalTokens), commaInt(s.CompressedTokens), s.ReductionPct())
	fmt.Fprintf(&b, "Total saved: %s tokens\n\n", commaInt(s.SavedTokens))

	if len(s.StrategyCount) > 0 {
		fmt.Fprintf(&b, "Strategy Breakdown\n")
		fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 40))

		// Sort strategies by tokens saved descending.
		type kv struct {
			key   string
			count int64
			saved int64
		}
		var rows []kv
		for k, cnt := range s.StrategyCount {
			rows = append(rows, kv{k, cnt, s.StrategySaved[k]})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].saved > rows[j].saved })
		for _, r := range rows {
			pct := 0.0
			if s.OriginalTokens > 0 {
				pct = float64(r.saved) / float64(s.OriginalTokens) * 100
			}
			fmt.Fprintf(&b, "  %-20s %6d uses, %s saved (%.1f%%)\n",
				r.key+":", r.count, commaInt(r.saved), pct)
		}
		fmt.Fprintf(&b, "\n")
	}

	if s.LatencyMaxUs > 0 {
		fmt.Fprintf(&b, "Compression Latency\n")
		fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 40))
		fmt.Fprintf(&b, "  p50: %s  p95: %s  max: %s\n\n",
			fmtUs(s.LatencyP50Us), fmtUs(s.LatencyP95Us), fmtUs(s.LatencyMaxUs))
	}

	return b.String()
}

func commaInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var result []byte
	for i, c := range s {
		pos := len(s) - i
		if i > 0 && pos%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}

func fmtUs(us int64) string {
	switch {
	case us < 1000:
		return fmt.Sprintf("%dµs", us)
	case us < 1_000_000:
		return fmt.Sprintf("%.1fms", float64(us)/1000)
	default:
		return fmt.Sprintf("%.2fs", float64(us)/1_000_000)
	}
}
