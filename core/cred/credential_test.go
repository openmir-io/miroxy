package cred

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// independentSigV4 recomputes the expected Authorization header from scratch
// (not sharing any code with SigV4Credential.Apply) so the test acts as an
// oracle check on the production implementation, not a tautology.
func independentSigV4(t *testing.T, req *http.Request, accessKey, secretKey, region, service string) string {
	t.Helper()

	amzDate := req.Header.Get("x-amz-date")
	if amzDate == "" {
		t.Fatal("x-amz-date not set on request")
	}
	dateStamp := amzDate[:8]
	payloadHash := req.Header.Get("x-amz-content-sha256")
	if payloadHash == "" {
		t.Fatal("x-amz-content-sha256 not set on request")
	}

	var signedHeaders []string
	for h := range req.Header {
		lower := strings.ToLower(h)
		if lower == "content-type" || strings.HasPrefix(lower, "x-amz-") {
			signedHeaders = append(signedHeaders, lower)
		}
	}
	signedHeaders = append(signedHeaders, "host")
	sort.Strings(signedHeaders)

	var canonicalHeaders strings.Builder
	for _, h := range signedHeaders {
		val := req.Header.Get(h)
		if h == "host" {
			val = req.URL.Host
		}
		canonicalHeaders.WriteString(h + ":" + strings.TrimSpace(val) + "\n")
	}

	var canonicalPath strings.Builder
	for i := 0; i < len(req.URL.Path); i++ {
		c := req.URL.Path[i]
		unreserved := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~'
		if c == '/' || unreserved {
			canonicalPath.WriteByte(c)
		} else {
			canonicalPath.WriteString("%")
			hexStr := hex.EncodeToString([]byte{c})
			canonicalPath.WriteString(strings.ToUpper(hexStr))
		}
	}

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalPath.String(),
		req.URL.RawQuery,
		canonicalHeaders.String(),
		strings.Join(signedHeaders, ";"),
		payloadHash,
	}, "\n")

	hash := func(b []byte) string {
		h := sha256.Sum256(b)
		return hex.EncodeToString(h[:])
	}
	hmacSum := func(key, data []byte) []byte {
		m := hmac.New(sha256.New, key)
		m.Write(data)
		return m.Sum(nil)
	}

	credentialScope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + credentialScope + "\n" + hash([]byte(canonicalRequest))

	signingKey := hmacSum(hmacSum(hmacSum(hmacSum(
		[]byte("AWS4"+secretKey), []byte(dateStamp)),
		[]byte(region)),
		[]byte(service)),
		[]byte("aws4_request"))
	signature := hex.EncodeToString(hmacSum(signingKey, []byte(stringToSign)))

	return "AWS4-HMAC-SHA256 Credential=" + accessKey + "/" + credentialScope +
		", SignedHeaders=" + strings.Join(signedHeaders, ";") + ", Signature=" + signature
}

func newSignableRequest(t *testing.T, target string, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if req.GetBody == nil {
		t.Fatal("expected http.NewRequest to auto-populate GetBody for a *bytes.Reader body")
	}
	return req
}

func TestSigV4Credential_Apply_MatchesIndependentComputation(t *testing.T) {
	cred := &SigV4Credential{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Region:          "us-east-1",
		Service:         "bedrock-runtime",
	}
	req := newSignableRequest(t, "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke",
		[]byte(`{"anthropic_version":"bedrock-2023-05-31","messages":[]}`))

	if err := cred.Apply(req); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := req.Header.Get("Authorization")
	if got == "" {
		t.Fatal("Authorization header not set")
	}
	want := independentSigV4(t, req, cred.AccessKeyID, cred.SecretAccessKey, cred.Region, cred.Service)
	if got != want {
		t.Errorf("Authorization mismatch:\n got:  %s\n want: %s", got, want)
	}
}

func TestSigV4Credential_Apply_SessionToken(t *testing.T) {
	cred := &SigV4Credential{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		SessionToken:    "example-session-token",
		Region:          "us-east-1",
		Service:         "bedrock-runtime",
	}
	req := newSignableRequest(t, "https://bedrock-runtime.us-east-1.amazonaws.com/model/foo/invoke", []byte(`{}`))

	if err := cred.Apply(req); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := req.Header.Get("x-amz-security-token"); got != cred.SessionToken {
		t.Errorf("x-amz-security-token = %q, want %q", got, cred.SessionToken)
	}
	auth := req.Header.Get("Authorization")
	if !strings.Contains(auth, "x-amz-security-token") {
		t.Errorf("Authorization SignedHeaders missing x-amz-security-token: %s", auth)
	}
	want := independentSigV4(t, req, cred.AccessKeyID, cred.SecretAccessKey, cred.Region, cred.Service)
	if auth != want {
		t.Errorf("Authorization mismatch:\n got:  %s\n want: %s", auth, want)
	}
}

func TestSigV4Credential_Apply_RequiresReReadableBody(t *testing.T) {
	cred := &SigV4Credential{AccessKeyID: "a", SecretAccessKey: "b", Region: "us-east-1", Service: "bedrock-runtime"}

	// A body that is neither *bytes.Buffer, *bytes.Reader, nor *strings.Reader
	// leaves req.GetBody nil — Apply must reject it rather than silently
	// signing an empty/wrong payload.
	req, err := http.NewRequest(http.MethodPost, "https://example.com/", struct{ *bytes.Reader }{bytes.NewReader([]byte("x"))})
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if req.GetBody != nil {
		t.Fatal("expected GetBody to be nil for this body type")
	}

	if err := cred.Apply(req); err == nil {
		t.Fatal("expected Apply to error when GetBody is nil")
	}
}

func TestSigV4Credential_Redacted(t *testing.T) {
	cred := &SigV4Credential{AccessKeyID: "AKIDEXAMPLE"}
	if got := cred.Redacted(); got != "sigv4:AKID***" {
		t.Errorf("Redacted() = %q, want %q", got, "sigv4:AKID***")
	}
}
