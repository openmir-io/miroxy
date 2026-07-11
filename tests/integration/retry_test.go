package integration_test

// §1-A: Transparent 429 pre-stream retry tests.
//
// Key invariant: a 429 received BEFORE any byte is sent to the client is
// completely invisible — the server retries with the next available key and
// the client receives a normal response. The retry loop in handleNonStream
// and handleStream already implements this; these tests prove it end-to-end.
//
// Round-robin note: CredPool.roundRobinFiltered starts counter at 0 and
// increments before selecting (counter%n). With keys=["good","bad"] (n=2):
//   attempt 0 → counter=1, index=1%2=1 → "bad" (429)
//   attempt 1 → counter=2, index=2%2=0 → "good" (200) ✓

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"miroxy/internal/types"
)

// TestRetry_NonStream_429OnFirstKey_RetriesToSecondKey verifies that a 429 on
// the first key chosen by round-robin causes an invisible failover to the next key.
func TestRetry_NonStream_429OnFirstKey_RetriesToSecondKey(t *testing.T) {
	// keys[1]="bad-key" is tried first (round-robin counter starts at 0, first
	// increment gives index 1). keys[0]="good-key" is tried on retry.
	const (
		goodKey = "good-key"
		badKey  = "bad-key"
	)

	stub := newStubGemini(t, map[string]http.HandlerFunc{
		badKey: gemini429Handler,
	}, nil)
	defer stub.Close()

	miroxy := newTestServer(t, miroxyConfig{
		keys:    []string{goodKey, badKey}, // round-robin picks badKey first
		stubURL: stub.URL,
	})
	defer miroxy.Close()

	start := time.Now()
	resp := doPost(t, miroxy.URL, nonStreamBody)
	defer resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after transparent retry, got %d", resp.StatusCode)
	}

	var msg types.MessageResponse
	decodeJSON(t, resp.Body, &msg)
	if msg.Role != "assistant" {
		t.Errorf("role after retry: got %q, want assistant", msg.Role)
	}
	if msg.StopReason != "end_turn" {
		t.Errorf("stop_reason after retry: got %q, want end_turn", msg.StopReason)
	}

	// Retry must complete within the 500 ms SLA defined in milestones.md.
	if elapsed > 500*time.Millisecond {
		t.Errorf("retry latency %v exceeded 500 ms SLA", elapsed)
	}
}

// TestRetry_Stream_429OnFirstKey_RetriesToSecondKey verifies that a streaming
// request transparently retries on 429 — the client receives a complete,
// unbroken SSE stream from the second key.
func TestRetry_Stream_429OnFirstKey_RetriesToSecondKey(t *testing.T) {
	const (
		goodKey = "good-key"
		badKey  = "bad-key"
	)

	// Stream handler is key-aware: badKey → 429; others → normal stream.
	streamHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") == badKey {
			gemini429Handler(w, r)
			return
		}
		defaultStreamResp(w, r)
	}

	stub := newStubGemini(t, nil, streamHandler)
	defer stub.Close()

	miroxy := newTestServer(t, miroxyConfig{
		keys:    []string{goodKey, badKey},
		stubURL: stub.URL,
	})
	defer miroxy.Close()

	start := time.Now()
	resp := doPost(t, miroxy.URL, streamBody)
	defer resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 streaming response after retry, got %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type: got %q, want text/event-stream", ct)
	}

	events := readSSEEvents(t, resp.Body)
	var eventNames []string
	for _, e := range events {
		eventNames = append(eventNames, e.event)
	}

	hasStart, hasStop := false, false
	for _, name := range eventNames {
		if name == "message_start" {
			hasStart = true
		}
		if name == "message_stop" {
			hasStop = true
		}
	}
	if !hasStart {
		t.Errorf("missing message_start; got events: %v", eventNames)
	}
	if !hasStop {
		t.Errorf("missing message_stop; got events: %v", eventNames)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("stream retry latency %v exceeded 500 ms SLA", elapsed)
	}
}

// TestRetry_AllKeys429_Returns503 verifies that when all keys are rate-limited
// simultaneously, the server returns 503, not 500 or a hang.
//
// Mechanism with 2 keys and maxRetries=3:
//
//	attempt 0: key B → 429 (60 s cooldown) → RateLimitError{60s}
//	attempt 1: key A → 429 (60 s cooldown) → RateLimitError{60s}
//	attempt 2: Select → ErrNoSelection (both keys in 60 s cooldown) → 503
func TestRetry_AllKeys429_Returns503(t *testing.T) {
	const (
		keyA = "key-a"
		keyB = "key-b"
	)

	stub := newStubGemini(t, map[string]http.HandlerFunc{
		keyA: gemini429LongCooldownHandler,
		keyB: gemini429LongCooldownHandler,
	}, nil)
	defer stub.Close()

	miroxy := newTestServer(t, miroxyConfig{
		keys:    []string{keyA, keyB},
		stubURL: stub.URL,
	})
	defer miroxy.Close()

	resp := doPost(t, miroxy.URL, nonStreamBody)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when all keys rate-limited, got %d", resp.StatusCode)
	}

	var errResp types.ErrorResponse
	decodeJSON(t, resp.Body, &errResp)
	if errResp.Error.Type != "overloaded_error" {
		t.Errorf("error type: got %q, want overloaded_error", errResp.Error.Type)
	}
}

// TestRetry_AllKeysStream429_Returns503 is the streaming equivalent.
func TestRetry_AllKeysStream429_Returns503(t *testing.T) {
	streamHandler := func(w http.ResponseWriter, r *http.Request) {
		gemini429LongCooldownHandler(w, r)
	}

	stub := newStubGemini(t, nil, streamHandler)
	defer stub.Close()

	miroxy := newTestServer(t, miroxyConfig{
		keys:    []string{"key-a", "key-b"},
		stubURL: stub.URL,
	})
	defer miroxy.Close()

	resp := doPost(t, miroxy.URL, streamBody)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when all stream keys rate-limited, got %d", resp.StatusCode)
	}

	var errResp types.ErrorResponse
	decodeJSON(t, resp.Body, &errResp)
	if errResp.Error.Type != "overloaded_error" {
		t.Errorf("error type: got %q, want overloaded_error", errResp.Error.Type)
	}
}

// TestRetry_RateLimitCooldown_KeyNotReusedDuringWindow verifies that a
// rate-limited key is not re-selected while its 60 s cooldown is active.
//
// 3-key setup; keys=["good","bad","third"]:
//
//	request 1: counter=1 → bad (429+60s cooldown); counter=2 → third (200) ✓
//	request 2: counter=3 → good (200) ✓   (bad still in 60 s cooldown)
//
// bad must be hit exactly once.
func TestRetry_RateLimitCooldown_KeyNotReusedDuringWindow(t *testing.T) {
	hitCount := make(map[string]int)

	stub := newStubGemini(t, map[string]http.HandlerFunc{
		"bad-key": func(w http.ResponseWriter, r *http.Request) {
			hitCount["bad-key"]++
			gemini429LongCooldownHandler(w, r)
		},
		"good-key": func(w http.ResponseWriter, r *http.Request) {
			hitCount["good-key"]++
			defaultNonStreamResp(w, "success")
		},
		"third-key": func(w http.ResponseWriter, r *http.Request) {
			hitCount["third-key"]++
			defaultNonStreamResp(w, "success")
		},
	}, nil)
	defer stub.Close()

	miroxy := newTestServer(t, miroxyConfig{
		keys:    []string{"good-key", "bad-key", "third-key"},
		stubURL: stub.URL,
	})
	defer miroxy.Close()

	resp1 := doPost(t, miroxy.URL, nonStreamBody)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("request 1: expected 200, got %d", resp1.StatusCode)
	}

	resp2 := doPost(t, miroxy.URL, nonStreamBody)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("request 2: expected 200, got %d", resp2.StatusCode)
	}

	if hitCount["bad-key"] != 1 {
		t.Errorf("bad-key hit %d times — should be exactly 1 (in 60 s cooldown for request 2)", hitCount["bad-key"])
	}
}
