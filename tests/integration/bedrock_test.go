package integration_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"miroxy/core/cred"
	"miroxy/core/ir"
	intup "miroxy/internal/upstream"
)

// verifySigV4 independently recomputes the expected AWS SigV4 Authorization
// header from what the stub server actually received (method, path, headers,
// body) — the same check a real AWS endpoint performs server-side. Failing
// this means a real Bedrock endpoint would reject the request with a
// signature mismatch, not just that some in-process struct looks right.
func verifySigV4(t *testing.T, r *http.Request, secretKey, region, service string) {
	t.Helper()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("reading request body: %v", err)
	}

	amzDate := r.Header.Get("x-amz-date")
	dateStamp := amzDate[:8]
	payloadHash := r.Header.Get("x-amz-content-sha256")

	var signedHeaders []string
	for h := range r.Header {
		lower := strings.ToLower(h)
		if lower == "content-type" || strings.HasPrefix(lower, "x-amz-") {
			signedHeaders = append(signedHeaders, lower)
		}
	}
	signedHeaders = append(signedHeaders, "host")
	sort.Strings(signedHeaders)

	var canonicalHeaders strings.Builder
	for _, h := range signedHeaders {
		val := r.Header.Get(h)
		if h == "host" {
			val = r.Host
		}
		canonicalHeaders.WriteString(h + ":" + strings.TrimSpace(val) + "\n")
	}

	var canonicalPath strings.Builder
	for i := 0; i < len(r.URL.Path); i++ {
		c := r.URL.Path[i]
		unreserved := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~'
		if c == '/' || unreserved {
			canonicalPath.WriteByte(c)
		} else {
			canonicalPath.WriteString(strings.ToUpper("%" + hex.EncodeToString([]byte{c})))
		}
	}

	canonicalRequest := strings.Join([]string{
		r.Method,
		canonicalPath.String(),
		r.URL.RawQuery,
		canonicalHeaders.String(),
		strings.Join(signedHeaders, ";"),
		payloadHash,
	}, "\n")

	sha256hex := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	hmacSum := func(key, data []byte) []byte { m := hmac.New(sha256.New, key); m.Write(data); return m.Sum(nil) }

	credentialScope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + credentialScope + "\n" + sha256hex([]byte(canonicalRequest))
	signingKey := hmacSum(hmacSum(hmacSum(hmacSum([]byte("AWS4"+secretKey), []byte(dateStamp)), []byte(region)), []byte(service)), []byte("aws4_request"))
	wantSig := hex.EncodeToString(hmacSum(signingKey, []byte(stringToSign)))

	auth := r.Header.Get("Authorization")
	if !strings.Contains(auth, "Signature="+wantSig) {
		t.Errorf("server-side SigV4 verification failed:\n  Authorization: %s\n  expected signature: %s\n  canonical request:\n%s", auth, wantSig, canonicalRequest)
	}

	r.Body = io.NopCloser(strings.NewReader(string(body)))
}

func TestBedrockAdapter_NonStreaming_EndToEnd(t *testing.T) {
	const (
		accessKey = "AKIDEXAMPLE"
		secretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
		region    = "us-east-1"
		service   = "bedrock-runtime"
		modelID   = "anthropic.claude-3-5-sonnet-20241022-v2:0"
	)

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verifySigV4(t, r, secretKey, region, service)

		var reqBody map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if _, ok := reqBody["model"]; ok {
			t.Error(`request body has "model" field — Bedrock InvokeModel rejects it`)
		}
		var version string
		json.Unmarshal(reqBody["anthropic_version"], &version)
		if version != "bedrock-2023-05-31" {
			t.Errorf("anthropic_version = %q, want bedrock-2023-05-31", version)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"id": "msg_stub", "type": "message", "role": "assistant",
			"content": [{"type": "text", "text": "hello from bedrock"}],
			"model": "` + modelID + `", "stop_reason": "end_turn",
			"usage": {"input_tokens": 7, "output_tokens": 4}
		}`))
	}))
	defer stub.Close()

	adapter := intup.NewBedrock(modelID, stub.URL)
	credential := &cred.SigV4Credential{
		AccessKeyID: accessKey, SecretAccessKey: secretKey, Region: region, Service: service,
	}

	irReq := &ir.IRRequest{
		Messages: []ir.IRMessage{
			{Role: "user", Parts: []ir.IRContentPart{{Text: &ir.IRTextPart{Text: "hi"}}}},
		},
		Gen: ir.IRGenerationConfig{MaxTokens: 100},
	}

	httpReq, err := adapter.ToUpstream(context.Background(), irReq, credential)
	if err != nil {
		t.Fatalf("ToUpstream: %v", err)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	irResp, err := adapter.FromUpstream(resp)
	if err != nil {
		t.Fatalf("FromUpstream: %v", err)
	}

	if len(irResp.Content) != 1 || irResp.Content[0].Text == nil || irResp.Content[0].Text.Text != "hello from bedrock" {
		t.Errorf("Content = %+v, want text \"hello from bedrock\"", irResp.Content)
	}
	if irResp.Usage.InputTokens != 7 || irResp.Usage.OutputTokens != 4 {
		t.Errorf("Usage = %+v, want input=7 output=4", irResp.Usage)
	}
}
