package selector

import "time"

// RateLimitError is passed to Release when the upstream returned HTTP 429.
// RetryAfter, if positive, is the authoritative cooldown from the response body;
// when zero the pool uses its escalating backoff schedule.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string { return "upstream rate limit (429)" }

// Is makes errors.Is(err, &RateLimitError{}) match any *RateLimitError value.
func (e *RateLimitError) Is(target error) bool {
	_, ok := target.(*RateLimitError)
	return ok
}

// ErrRateLimit is a zero-RetryAfter sentinel usable in errors.Is checks.
var ErrRateLimit error = &RateLimitError{}

// ServerOverloadError is passed to Release when the upstream returned a
// transient 5xx (e.g. 503 "model overloaded"). It parks the credential
// briefly so the next Select() call routes to a different key, but does NOT
// touch rateLimitFailures — preserving the 429 escalation schedule.
//
// When all credentials are parked this way, Select() returns ErrNoSelection
// and the prober kicks in — the caller sees an error only at that point.
type ServerOverloadError struct {
	RetryAfter time.Duration // park duration; defaults to 5s when zero
}

func (e *ServerOverloadError) Error() string { return "upstream server overload (5xx)" }

func (e *ServerOverloadError) Is(target error) bool {
	_, ok := target.(*ServerOverloadError)
	return ok
}

var ErrServerOverload error = &ServerOverloadError{}
