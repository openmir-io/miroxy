package compress

import "testing"

func TestStats_Record_PerModelBreakdown(t *testing.T) {
	s := NewStats()
	s.Record("miroxy-free-long", &Result{OriginalTokens: 1000, CompressedTokens: 600}, 500)
	s.Record("miroxy-free-long", &Result{OriginalTokens: 500, CompressedTokens: 300}, 200)
	s.Record("mistral-test", &Result{OriginalTokens: 2000, CompressedTokens: 2000}, 100)

	snap := s.Snapshot()
	if snap.Requests != 3 || snap.OriginalTokens != 3500 || snap.CompressedTokens != 2900 {
		t.Fatalf("global totals = %+v", snap)
	}
	if len(snap.Models) != 2 {
		t.Fatalf("expected 2 models, got %d: %+v", len(snap.Models), snap.Models)
	}
	// Sorted by name: miroxy-free-long, mistral-test.
	long := snap.Models[0]
	if long.Name != "miroxy-free-long" || long.Requests != 2 || long.OriginalTokens != 1500 || long.CompressedTokens != 900 {
		t.Fatalf("miroxy-free-long snapshot = %+v", long)
	}
	if long.SavedTokens() != 600 {
		t.Errorf("SavedTokens() = %d, want 600", long.SavedTokens())
	}

	test := snap.Models[1]
	if test.Name != "mistral-test" || test.Requests != 1 || test.SavedTokens() != 0 {
		t.Fatalf("mistral-test snapshot = %+v", test)
	}
}

func TestStats_Restore_ReseedsGlobalAndPerModel(t *testing.T) {
	s := NewStats()
	s.Restore(10, 100000, 60000, []ModelSnapshot{
		{Name: "model-a", Requests: 10, OriginalTokens: 100000, CompressedTokens: 60000},
	})

	s.Record("model-a", &Result{OriginalTokens: 100, CompressedTokens: 50}, 10)

	snap := s.Snapshot()
	if snap.Requests != 11 || snap.OriginalTokens != 100100 || snap.CompressedTokens != 60050 {
		t.Fatalf("totals after restore+record = %+v", snap)
	}
	if len(snap.Models) != 1 || snap.Models[0].Requests != 11 {
		t.Fatalf("model-a after restore+record = %+v", snap.Models)
	}
}
