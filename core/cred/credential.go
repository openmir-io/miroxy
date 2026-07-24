package cred

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Credential represents authentication material for an upstream provider.
// Each implementation knows how to attach itself to an HTTP request via Apply.
//
// Design: behavior-based rather than type-enum-based. Adding a new auth scheme
// requires only a new struct implementing this interface — no changes to the
// selector, credpool, pipeline, or routing layers.
type Credential interface {
	// Apply attaches this credential to the outgoing HTTP request.
	// Called by the Dispatcher immediately before sending.
	Apply(req *http.Request) error

	// Type returns a short identifier for logging/debugging only.
	// Must never contain the secret value.
	Type() string

	// Redacted returns a log-safe representation e.g. "Authorization:***".
	Redacted() string
}

// HeaderCredential adds a single HTTP header to the request.
//
// Covers:
//   - Anthropic API key: Header="x-api-key"
//   - OpenAI / most providers: Header="Authorization", Value="Bearer <token>"
//   - Azure OpenAI: Header="api-key"
//   - OAuth access tokens: Header="Authorization", Value="Bearer <token>"
type HeaderCredential struct {
	Header string
	Value  string
}

func (c *HeaderCredential) Apply(req *http.Request) error {
	req.Header.Set(c.Header, c.Value)
	return nil
}

func (c *HeaderCredential) Type() string     { return "header" }
func (c *HeaderCredential) Redacted() string { return c.Header + ":***" }

// QueryCredential appends a key=value pair to the request URL query string.
//
// Covers:
//   - Google Gemini API key auth: Param="key"
type QueryCredential struct {
	Param string
	Value string
}

func (c *QueryCredential) Apply(req *http.Request) error {
	q := req.URL.Query()
	q.Set(c.Param, c.Value)
	req.URL.RawQuery = q.Encode()
	return nil
}

func (c *QueryCredential) Type() string     { return "query" }
func (c *QueryCredential) Redacted() string { return "?" + url.QueryEscape(c.Param) + "=***" }

// SigV4Credential holds AWS Signature Version 4 signing material.
//
// Apply signs the request in place (Authorization + x-amz-* headers) using
// the request's own body via GetBody — no AWS SDK dependency. Covers AWS
// Bedrock and any other SigV4-authenticated service.
type SigV4Credential struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string // empty for long-term IAM credentials
	Region          string
	Service         string // e.g. "bedrock-runtime"
}

func (c *SigV4Credential) Apply(req *http.Request) error {
	if req.GetBody == nil {
		return fmt.Errorf("sigv4: request body must be re-readable (built from bytes.Reader)")
	}
	bodyReader, err := req.GetBody()
	if err != nil {
		return fmt.Errorf("sigv4: re-reading body: %w", err)
	}
	body, err := io.ReadAll(bodyReader)
	if err != nil {
		return fmt.Errorf("sigv4: reading body: %w", err)
	}

	now := time.Now().UTC()
	dateStamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	req.Header.Set("x-amz-date", amzDate)
	if c.SessionToken != "" {
		req.Header.Set("x-amz-security-token", c.SessionToken)
	}

	payloadHash := sigV4SHA256Hex(body)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	// Signed headers: content-type, host, and every x-amz-* header already
	// set on the request (date, security-token, content-sha256, ...).
	signedHeaders := []string{"content-type", "host"}
	for h := range req.Header {
		if lower := strings.ToLower(h); strings.HasPrefix(lower, "x-amz-") {
			signedHeaders = append(signedHeaders, lower)
		}
	}
	slices.Sort(signedHeaders)

	var canonicalHeaders strings.Builder
	for _, h := range signedHeaders {
		val := req.Header.Get(h)
		if h == "host" {
			val = req.URL.Host
		}
		canonicalHeaders.WriteString(h + ":" + strings.TrimSpace(val) + "\n")
	}

	canonicalRequest := strings.Join([]string{
		req.Method,
		sigV4URIEncodePath(req.URL.Path),
		req.URL.RawQuery,
		canonicalHeaders.String(),
		strings.Join(signedHeaders, ";"),
		payloadHash,
	}, "\n")

	credentialScope := dateStamp + "/" + c.Region + "/" + c.Service + "/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + credentialScope + "\n" + sigV4SHA256Hex([]byte(canonicalRequest))

	signingKey := sigV4HMACSHA256(sigV4HMACSHA256(sigV4HMACSHA256(sigV4HMACSHA256(
		[]byte("AWS4"+c.SecretAccessKey), []byte(dateStamp)),
		[]byte(c.Region)),
		[]byte(c.Service)),
		[]byte("aws4_request"))

	signature := hex.EncodeToString(sigV4HMACSHA256(signingKey, []byte(stringToSign)))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.AccessKeyID, credentialScope, strings.Join(signedHeaders, ";"), signature,
	))
	return nil
}

// sigV4URIEncodePath URI-encodes each path segment per AWS SigV4 canonical
// request rules (RFC 3986 unreserved characters only; slashes preserved).
// Distinct from Go's own URL escaping, which permits a wider character set.
func sigV4URIEncodePath(path string) string {
	var buf strings.Builder
	for i := 0; i < len(path); i++ {
		c := path[i]
		if c == '/' || sigV4IsUnreserved(c) {
			buf.WriteByte(c)
		} else {
			fmt.Fprintf(&buf, "%%%02X", c)
		}
	}
	return buf.String()
}

func sigV4IsUnreserved(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
		(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~'
}

func sigV4SHA256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func sigV4HMACSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func (c *SigV4Credential) Type() string { return "aws_sigv4" }

func (c *SigV4Credential) Redacted() string {
	if len(c.AccessKeyID) < 4 {
		return "sigv4:***"
	}
	return "sigv4:" + c.AccessKeyID[:4] + "***"
}
