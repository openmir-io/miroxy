// Package stats collects in-process token usage counters.
// All counters are process-lifetime only unless restored via Restore.
package stats

import (
	"sort"
	"sync"
	"sync/atomic"
)

// Counter holds cumulative token and request counts for one entity
// (global, a model route, a credpool, or an individual credential).
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

// keyRef identifies one credential uniquely — a key's name (SelectionID) is
// only guaranteed unique within its own credpool, not across pools.
type keyRef struct {
	pool string
	key  string
}

// ModelStats holds counters for one model route and its constituent
// credpools and keys.
type ModelStats struct {
	Counter
	mu    sync.Mutex
	pools map[string]*Counter
	keys  map[keyRef]*Counter
}

func (m *ModelStats) forPool(poolName string) *Counter {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pools == nil {
		m.pools = make(map[string]*Counter)
	}
	c, ok := m.pools[poolName]
	if !ok {
		c = &Counter{}
		m.pools[poolName] = c
	}
	return c
}

func (m *ModelStats) forKey(poolName, keyID string) *Counter {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keys == nil {
		m.keys = make(map[keyRef]*Counter)
	}
	ref := keyRef{pool: poolName, key: keyID}
	c, ok := m.keys[ref]
	if !ok {
		c = &Counter{}
		m.keys[ref] = c
	}
	return c
}

// PoolSnapshots returns a stable copy of per-credpool counters (each with
// its own key breakdown), sorted by pool name.
func (m *ModelStats) PoolSnapshots() []PoolSnapshot {
	m.mu.Lock()
	poolNames := make([]string, 0, len(m.pools))
	for name := range m.pools {
		poolNames = append(poolNames, name)
	}
	keysByPool := make(map[string][]KeySnapshot)
	for ref, c := range m.keys {
		in, out, req := c.Snapshot()
		keysByPool[ref.pool] = append(keysByPool[ref.pool], KeySnapshot{Name: ref.key, Input: in, Output: out, Requests: req})
	}
	pools := m.pools
	m.mu.Unlock()

	sort.Strings(poolNames)
	out := make([]PoolSnapshot, 0, len(poolNames))
	for _, name := range poolNames {
		in, o, req := pools[name].Snapshot()
		keys := keysByPool[name]
		sort.Slice(keys, func(i, j int) bool { return keys[i].Name < keys[j].Name })
		out = append(out, PoolSnapshot{Name: name, Input: in, Output: o, Requests: req, Keys: keys})
	}
	return out
}

// KeySnapshot is a point-in-time copy of one credential's counters.
type KeySnapshot struct {
	Name     string
	Input    int64
	Output   int64
	Requests int64
}

// PoolSnapshot is a point-in-time copy of one credpool's counters within a
// model route, plus its constituent keys.
type PoolSnapshot struct {
	Name     string
	Input    int64
	Output   int64
	Requests int64
	Keys     []KeySnapshot
}

// ModelSnapshot is a point-in-time copy of one model route's counters.
type ModelSnapshot struct {
	Name     string
	Input    int64
	Output   int64
	Requests int64
	Pools    []PoolSnapshot
}

// Registry is the top-level stats store. Create once and share.
type Registry struct {
	Total  Counter
	mu     sync.Mutex
	models map[string]*ModelStats
}

// Record adds input/output tokens for a given model route, credpool, and key.
func (r *Registry) Record(model, poolName, keyID string, input, output int64) {
	r.Total.Add(input, output)
	ms := r.modelStats(model)
	ms.Add(input, output)
	ms.forPool(poolName).Add(input, output)
	ms.forKey(poolName, keyID).Add(input, output)
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

// Snapshot returns a stable point-in-time copy of all counters, models
// sorted by name for deterministic output.
func (r *Registry) Snapshot() (totalIn, totalOut, totalReq int64, models []ModelSnapshot) {
	totalIn, totalOut, totalReq = r.Total.Snapshot()

	r.mu.Lock()
	names := make([]string, 0, len(r.models))
	for name := range r.models {
		names = append(names, name)
	}
	r.mu.Unlock()
	sort.Strings(names)

	for _, name := range names {
		ms := r.modelStats(name)
		in, out, req := ms.Snapshot()
		models = append(models, ModelSnapshot{
			Name:     name,
			Input:    in,
			Output:   out,
			Requests: req,
			Pools:    ms.PoolSnapshots(),
		})
	}
	return
}

// Restore reseeds counters from a prior snapshot (e.g. loaded from
// persistence at startup) so process-lifetime totals build on the restored
// baseline instead of starting from zero. Call once, before any concurrent
// Record calls begin.
func (r *Registry) Restore(totalIn, totalOut, totalReq int64, models []ModelSnapshot) {
	r.Total.Input.Store(totalIn)
	r.Total.Output.Store(totalOut)
	r.Total.Requests.Store(totalReq)

	r.mu.Lock()
	if r.models == nil {
		r.models = make(map[string]*ModelStats)
	}
	for _, msnap := range models {
		ms := &ModelStats{}
		ms.Input.Store(msnap.Input)
		ms.Output.Store(msnap.Output)
		ms.Requests.Store(msnap.Requests)
		for _, psnap := range msnap.Pools {
			pc := ms.forPool(psnap.Name)
			pc.Input.Store(psnap.Input)
			pc.Output.Store(psnap.Output)
			pc.Requests.Store(psnap.Requests)
			for _, ksnap := range psnap.Keys {
				kc := ms.forKey(psnap.Name, ksnap.Name)
				kc.Input.Store(ksnap.Input)
				kc.Output.Store(ksnap.Output)
				kc.Requests.Store(ksnap.Requests)
			}
		}
		r.models[msnap.Name] = ms
	}
	r.mu.Unlock()
}
