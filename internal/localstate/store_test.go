package localstate

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOpen_CreatesFileAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := Open(path)
	defer s.Close()

	if err := s.SaveAllCredHealth("pool-a", map[string]CredHealth{
		"key_0": {State: "cooling_down", CoolEndUnixNano: 12345, RateLimitFailures: 1, Failures: 0},
	}); err != nil {
		t.Fatalf("SaveAllCredHealth: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected buntdb file to exist at %s: %v", path, err)
	}
}

func TestSaveAndLoadAllCredHealth_RoundTrip(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "state.db"))
	defer s.Close()

	want := map[string]CredHealth{
		"key_0": {State: "cooling_down", CoolEndUnixNano: 1000, RateLimitFailures: 2, Failures: 0},
		"key_1": {State: "healthy", CoolEndUnixNano: 0, RateLimitFailures: 0, Failures: 0},
	}
	if err := s.SaveAllCredHealth("pool-a", want); err != nil {
		t.Fatalf("SaveAllCredHealth: %v", err)
	}

	got := s.LoadAllCredHealth("pool-a")
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for id, wantH := range want {
		if gotH, ok := got[id]; !ok || gotH != wantH {
			t.Errorf("entry %q: got %+v, want %+v (present=%v)", id, gotH, wantH, ok)
		}
	}
}

func TestLoadAllCredHealth_DoesNotLeakAcrossPools(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "state.db"))
	defer s.Close()

	_ = s.SaveAllCredHealth("pool-a", map[string]CredHealth{"key_0": {State: "healthy"}})
	_ = s.SaveAllCredHealth("pool-b", map[string]CredHealth{"key_0": {State: "cooling_down"}})

	gotA := s.LoadAllCredHealth("pool-a")
	if len(gotA) != 1 || gotA["key_0"].State != "healthy" {
		t.Errorf("pool-a: got %+v", gotA)
	}
	gotB := s.LoadAllCredHealth("pool-b")
	if len(gotB) != 1 || gotB["key_0"].State != "cooling_down" {
		t.Errorf("pool-b: got %+v", gotB)
	}
}

func TestLoadAllCredHealth_EmptyWhenNothingSaved(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "state.db"))
	defer s.Close()

	got := s.LoadAllCredHealth("never-saved")
	if len(got) != 0 {
		t.Errorf("expected empty map, got %+v", got)
	}
}

func TestOpen_SelfHealsOnCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(path, []byte("this is not a valid buntdb aof file\x00\x01garbage"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}

	s := Open(path)
	defer s.Close()

	// A freshly-recreated store should work normally.
	if err := s.SaveAllCredHealth("pool-a", map[string]CredHealth{"key_0": {State: "healthy"}}); err != nil {
		t.Fatalf("SaveAllCredHealth after self-heal: %v", err)
	}
	got := s.LoadAllCredHealth("pool-a")
	if got["key_0"].State != "healthy" {
		t.Errorf("got %+v after self-heal", got)
	}
}

func TestOpen_FallsBackToMemoryWhenPathUnwritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-based unwritable-dir test not meaningful on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root — permission checks are bypassed, can't force this failure")
	}

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil { // read+execute, no write
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(dir, 0o700) // restore so t.TempDir() cleanup can remove it

	path := filepath.Join(dir, "state.db")
	s := Open(path) // both the direct open and the self-heal retry fail here

	// Falls back to :memory: — still fully usable, just doesn't touch disk.
	if err := s.SaveAllCredHealth("pool-a", map[string]CredHealth{"key_0": {State: "healthy"}}); err != nil {
		t.Fatalf("SaveAllCredHealth on in-memory fallback: %v", err)
	}
	got := s.LoadAllCredHealth("pool-a")
	if got["key_0"].State != "healthy" {
		t.Errorf("got %+v from in-memory fallback", got)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("expected no file to be created under the unwritable dir")
	}
	_ = s.Close()
}

func TestSaveAndLoadTokenStats_RoundTrip(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "state.db"))
	defer s.Close()

	want := TokenStats{
		TotalInput: 1000, TotalOutput: 200, TotalRequests: 5,
		Models: []TokenModelStats{
			{
				Name: "mistral-test", Input: 1000, Output: 200, Requests: 5,
				Pools: []TokenPoolStats{
					{
						Name: "mistral-free", Input: 1000, Output: 200, Requests: 5,
						Keys: []TokenKeyStats{{Name: "mistral_bytebyteops", Input: 1000, Output: 200, Requests: 5}},
					},
				},
			},
		},
	}
	if err := s.SaveTokenStats(want); err != nil {
		t.Fatalf("SaveTokenStats: %v", err)
	}
	got, ok := s.LoadTokenStats()
	if !ok {
		t.Fatal("LoadTokenStats: expected ok=true")
	}
	if got.TotalInput != want.TotalInput || len(got.Models) != 1 ||
		len(got.Models[0].Pools) != 1 || len(got.Models[0].Pools[0].Keys) != 1 ||
		got.Models[0].Pools[0].Keys[0].Name != "mistral_bytebyteops" {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, want)
	}
}

func TestLoadTokenStats_FalseWhenNothingSaved(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "state.db"))
	defer s.Close()

	if _, ok := s.LoadTokenStats(); ok {
		t.Error("expected ok=false when nothing was ever saved")
	}
}

func TestSaveAndLoadCompressStats_RoundTrip(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "state.db"))
	defer s.Close()

	want := CompressStats{
		TotalRequests: 10, TotalOriginal: 50000, TotalCompressed: 30000,
		Models: []CompressModelStats{
			{Name: "miroxy-free-long", Requests: 10, OriginalTokens: 50000, CompressedTokens: 30000},
		},
	}
	if err := s.SaveCompressStats(want); err != nil {
		t.Fatalf("SaveCompressStats: %v", err)
	}
	got, ok := s.LoadCompressStats()
	if !ok {
		t.Fatal("LoadCompressStats: expected ok=true")
	}
	if got.TotalRequests != want.TotalRequests || len(got.Models) != 1 || got.Models[0].Name != "miroxy-free-long" {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, want)
	}
}
