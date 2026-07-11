// Package stats collects in-process token usage counters.
// All counters are process-lifetime only; they reset on restart.
package stats

import (
	"sync"
	"sync/atomic"
)

// Counter holds cumulative token and request counts for one entity
// (global, a model route, or an individual key).
type Counter struct {
	Input    atomic.Int64
	Output   atomic.Int64
	Requests atomic.Int64
}

func (c *Counter) Add(input, output int64) {
	c.Input.Add(input)
	c.Output.Add(output)
	c.Requests.Add(1)
}

func (c *Counter) Snapshot() (input, output, requests int64) {
	return c.Input.Load(), c.Output.Load(), c.Requests.Load()
}

// ModelStats holds counters for one model route and its constituent keys.
type ModelStats struct {
	Counter
	mu   sync.Mutex
	keys map[string]*Counter
}

func (m *ModelStats) forKey(keyID string) *Counter {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keys == nil {
		m.keys = make(map[string]*Counter)
	}
	c, ok := m.keys[keyID]
	if !ok {
		c = &Counter{}
		m.keys[keyID] = c
	}
	return c
}

// KeySnapshots returns a stable copy of per-key counters sorted by name.
func (m *ModelStats) KeySnapshots() []KeySnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]KeySnapshot, 0, len(m.keys))
	for name, c := range m.keys {
		in, out2, req := c.Snapshot()
		out = append(out, KeySnapshot{Name: name, Input: in, Output: out2, Requests: req})
	}
	return out
}

// KeySnapshot is a point-in-time copy of one key's counters.
type KeySnapshot struct {
	Name     string
	Input    int64
	Output   int64
	Requests int64
}

// ModelSnapshot is a point-in-time copy of one model route's counters.
type ModelSnapshot struct {
	Name     string
	Input    int64
	Output   int64
	Requests int64
	Keys     []KeySnapshot
}

// Registry is the top-level stats store. Create once and share.
type Registry struct {
	Total  Counter
	mu     sync.Mutex
	models map[string]*ModelStats
}

// Record adds input/output tokens for a given model route and key.
func (r *Registry) Record(model, keyID string, input, output int64) {
	r.Total.Add(input, output)
	ms := r.modelStats(model)
	ms.Add(input, output)
	ms.forKey(keyID).Add(input, output)
}

func (r *Registry) modelStats(model string) *ModelStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.models == nil {
		r.models = make(map[string]*ModelStats)
	}
	ms, ok := r.models[model]
	if !ok {
		ms = &ModelStats{}
		r.models[model] = ms
	}
	return ms
}

// Snapshot returns a stable point-in-time copy of all counters.
func (r *Registry) Snapshot() (totalIn, totalOut, totalReq int64, models []ModelSnapshot) {
	totalIn, totalOut, totalReq = r.Total.Snapshot()

	r.mu.Lock()
	names := make([]string, 0, len(r.models))
	for name := range r.models {
		names = append(names, name)
	}
	r.mu.Unlock()

	for _, name := range names {
		ms := r.modelStats(name)
		in, out, req := ms.Snapshot()
		models = append(models, ModelSnapshot{
			Name:     name,
			Input:    in,
			Output:   out,
			Requests: req,
			Keys:     ms.KeySnapshots(),
		})
	}
	return
}
