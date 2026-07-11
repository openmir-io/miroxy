package server

import (
	"testing"
	"time"
)

func TestParseRetryDelay_EmptyBody(t *testing.T) {
	if d := parseRetryDelay(nil); d != 0 {
		t.Errorf("nil body: want 0, got %v", d)
	}
	if d := parseRetryDelay([]byte{}); d != 0 {
		t.Errorf("zero-length body: want 0, got %v", d)
	}
}

func TestParseRetryDelay_InvalidJSON(t *testing.T) {
	if d := parseRetryDelay([]byte("not json")); d != 0 {
		t.Errorf("invalid JSON: want 0, got %v", d)
	}
}

func TestParseRetryDelay_NoErrorDetails(t *testing.T) {
	body := []byte(`{"error":{"code":429,"message":"quota exceeded"}}`)
	if d := parseRetryDelay(body); d != 0 {
		t.Errorf("no details field: want 0, got %v", d)
	}
}

func TestParseRetryDelay_DetailWithoutRetryDelay(t *testing.T) {
	body := []byte(`{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure"}]}}`)
	if d := parseRetryDelay(body); d != 0 {
		t.Errorf("detail without retryDelay: want 0, got %v", d)
	}
}

func TestParseRetryDelay_SecondsOnly(t *testing.T) {
	body := []byte(`{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"42s"}]}}`)
	d := parseRetryDelay(body)
	if d != 42*time.Second {
		t.Errorf("want 42s, got %v", d)
	}
}

func TestParseRetryDelay_MinutesAndSeconds(t *testing.T) {
	body := []byte(`{"error":{"details":[{"retryDelay":"1m30s"}]}}`)
	d := parseRetryDelay(body)
	if d != 90*time.Second {
		t.Errorf("want 90s, got %v", d)
	}
}

func TestParseRetryDelay_FractionalSeconds(t *testing.T) {
	// Gemini sometimes returns fractional durations like "156h14m36.752s".
	body := []byte(`{"error":{"details":[{"retryDelay":"1.5s"}]}}`)
	d := parseRetryDelay(body)
	if d != 1500*time.Millisecond {
		t.Errorf("want 1.5s, got %v", d)
	}
}

func TestParseRetryDelay_MalformedDuration(t *testing.T) {
	body := []byte(`{"error":{"details":[{"retryDelay":"not-a-duration"}]}}`)
	if d := parseRetryDelay(body); d != 0 {
		t.Errorf("malformed duration: want 0, got %v", d)
	}
}

func TestParseRetryDelay_ZeroDurationString(t *testing.T) {
	body := []byte(`{"error":{"details":[{"retryDelay":"0s"}]}}`)
	if d := parseRetryDelay(body); d != 0 {
		t.Errorf("zero duration string: want 0, got %v", d)
	}
}

// TestParseRetryDelay_FirstValidDetailWins verifies that the first parseable
// retryDelay is returned even when multiple detail entries are present.
func TestParseRetryDelay_FirstValidDetailWins(t *testing.T) {
	body := []byte(`{"error":{"details":[
		{"@type":"type.googleapis.com/google.rpc.QuotaFailure"},
		{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"20s"},
		{"retryDelay":"99s"}
	]}}`)
	d := parseRetryDelay(body)
	if d != 20*time.Second {
		t.Errorf("want 20s (first valid), got %v", d)
	}
}
