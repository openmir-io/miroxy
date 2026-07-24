package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newAllowedRequest(t *testing.T) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
}

func TestValidator_BearerAuthorizationHeader_Accepted(t *testing.T) {
	v := NewValidator([]string{"secret"})
	r := newAllowedRequest(t)
	r.Header.Set("Authorization", "Bearer secret")
	if !v.valid(r) {
		t.Fatal("expected Bearer Authorization header to be accepted")
	}
}

func TestValidator_AnthropicXAPIKeyHeader_Accepted(t *testing.T) {
	v := NewValidator([]string{"secret"})
	r := newAllowedRequest(t)
	r.Header.Set("x-api-key", "secret")
	if !v.valid(r) {
		t.Fatal("expected x-api-key header to be accepted (Anthropic protocol auth convention)")
	}
}

func TestValidator_AuthorizationTakesPrecedenceOverXAPIKey(t *testing.T) {
	v := NewValidator([]string{"secret"})
	r := newAllowedRequest(t)
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("x-api-key", "wrong")
	if !v.valid(r) {
		t.Fatal("expected the valid Authorization header to be used")
	}
}

func TestValidator_WrongKey_Rejected(t *testing.T) {
	v := NewValidator([]string{"secret"})
	r := newAllowedRequest(t)
	r.Header.Set("x-api-key", "wrong")
	if v.valid(r) {
		t.Fatal("expected wrong key to be rejected")
	}
}

func TestValidator_NoHeaders_Rejected(t *testing.T) {
	v := NewValidator([]string{"secret"})
	r := newAllowedRequest(t)
	if v.valid(r) {
		t.Fatal("expected request with no auth headers to be rejected")
	}
}

func TestValidator_OpenMode_NoAllowedKeys_AlwaysAccepted(t *testing.T) {
	v := NewValidator(nil)
	r := newAllowedRequest(t)
	if !v.valid(r) {
		t.Fatal("expected open mode (no allowed keys) to accept any request")
	}
}
