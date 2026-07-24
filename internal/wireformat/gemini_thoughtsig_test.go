package wireformat

import (
	"testing"
	"time"
)

func TestThoughtSigCache_StoreAndLookup(t *testing.T) {
	c := newThoughtSigCache(time.Hour)
	c.store("tool-1", "sig-1")
	if got := c.lookup("tool-1"); got != "sig-1" {
		t.Errorf("lookup = %q, want %q", got, "sig-1")
	}
}

func TestThoughtSigCache_LookupMissingReturnsEmpty(t *testing.T) {
	c := newThoughtSigCache(time.Hour)
	if got := c.lookup("never-stored"); got != "" {
		t.Errorf("lookup = %q, want empty", got)
	}
}

func TestThoughtSigCache_StoreIgnoresEmptyIDOrSignature(t *testing.T) {
	c := newThoughtSigCache(time.Hour)
	c.store("", "sig")
	c.store("tool-2", "")
	if got := c.lookup(""); got != "" {
		t.Errorf(`lookup("") = %q, want empty`, got)
	}
	if got := c.lookup("tool-2"); got != "" {
		t.Errorf("lookup(tool-2) = %q, want empty (empty signature is never stored)", got)
	}
}

func TestThoughtSigCache_ExpiresAfterTTL(t *testing.T) {
	c := newThoughtSigCache(10 * time.Millisecond)
	c.store("tool-3", "sig-3")
	time.Sleep(20 * time.Millisecond)
	if got := c.lookup("tool-3"); got != "" {
		t.Errorf("lookup after TTL = %q, want empty", got)
	}
}

func TestThoughtSigCache_SweepOnStoreRemovesExpired(t *testing.T) {
	c := newThoughtSigCache(10 * time.Millisecond)
	c.store("tool-4", "sig-4")
	time.Sleep(20 * time.Millisecond)
	c.store("tool-5", "sig-5") // triggers the opportunistic sweep in store()

	c.mu.Lock()
	_, stillThere := c.entries["tool-4"]
	c.mu.Unlock()
	if stillThere {
		t.Error("expired entry tool-4 should have been swept on the next store()")
	}
}
