package stats

import "testing"

func TestRegistry_Record_AccumulatesAcrossLevels(t *testing.T) {
	var r Registry
	r.Record("model-a", "pool-a", "key-1", 100, 10)
	r.Record("model-a", "pool-a", "key-1", 50, 5)

	totalIn, totalOut, totalReq, models := r.Snapshot()
	if totalIn != 150 || totalOut != 15 || totalReq != 2 {
		t.Fatalf("totals = (%d, %d, %d), want (150, 15, 2)", totalIn, totalOut, totalReq)
	}
	if len(models) != 1 || models[0].Input != 150 || models[0].Requests != 2 {
		t.Fatalf("model snapshot = %+v", models)
	}
	if len(models[0].Pools) != 1 || models[0].Pools[0].Name != "pool-a" || models[0].Pools[0].Input != 150 {
		t.Fatalf("pool snapshot = %+v", models[0].Pools)
	}
	keys := models[0].Pools[0].Keys
	if len(keys) != 1 || keys[0].Name != "key-1" || keys[0].Input != 150 || keys[0].Requests != 2 {
		t.Fatalf("key snapshot = %+v", keys)
	}
}

// TestRegistry_Record_SameKeyNameDifferentPools guards the fix for a key
// name only being unique within its own credpool — two different pools
// reusing the same key name under one model route must not merge counters.
func TestRegistry_Record_SameKeyNameDifferentPools(t *testing.T) {
	var r Registry
	r.Record("model-a", "pool-a", "primary", 100, 0)
	r.Record("model-a", "pool-b", "primary", 999, 0)

	_, _, _, models := r.Snapshot()
	if len(models[0].Pools) != 2 {
		t.Fatalf("expected 2 distinct pools, got %d: %+v", len(models[0].Pools), models[0].Pools)
	}
	byName := map[string]int64{}
	for _, p := range models[0].Pools {
		if len(p.Keys) != 1 {
			t.Fatalf("pool %s: expected 1 key, got %d", p.Name, len(p.Keys))
		}
		byName[p.Name] = p.Keys[0].Input
	}
	if byName["pool-a"] != 100 || byName["pool-b"] != 999 {
		t.Fatalf("counters bled across pools: %+v", byName)
	}
}

func TestRegistry_Snapshot_SortedByName(t *testing.T) {
	var r Registry
	r.Record("model-z", "pool-z", "k", 1, 0)
	r.Record("model-a", "pool-b", "k", 1, 0)
	r.Record("model-a", "pool-a", "k", 1, 0)

	_, _, _, models := r.Snapshot()
	if models[0].Name != "model-a" || models[1].Name != "model-z" {
		t.Fatalf("models not sorted: %v, %v", models[0].Name, models[1].Name)
	}
	pools := models[0].Pools
	if pools[0].Name != "pool-a" || pools[1].Name != "pool-b" {
		t.Fatalf("pools not sorted: %v, %v", pools[0].Name, pools[1].Name)
	}
}

func TestRegistry_Restore_ReseedsBaseline(t *testing.T) {
	var r Registry
	r.Restore(1000, 200, 5, []ModelSnapshot{
		{
			Name: "model-a", Input: 1000, Output: 200, Requests: 5,
			Pools: []PoolSnapshot{
				{Name: "pool-a", Input: 1000, Output: 200, Requests: 5,
					Keys: []KeySnapshot{{Name: "key-1", Input: 1000, Output: 200, Requests: 5}}},
			},
		},
	})

	// A live increment after restore must build on the restored baseline.
	r.Record("model-a", "pool-a", "key-1", 10, 1)

	totalIn, totalOut, totalReq, models := r.Snapshot()
	if totalIn != 1010 || totalOut != 201 || totalReq != 6 {
		t.Fatalf("totals after restore+record = (%d, %d, %d), want (1010, 201, 6)", totalIn, totalOut, totalReq)
	}
	if models[0].Pools[0].Keys[0].Input != 1010 {
		t.Fatalf("key input after restore+record = %d, want 1010", models[0].Pools[0].Keys[0].Input)
	}
}
