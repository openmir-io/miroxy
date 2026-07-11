package cred

import (
	"fmt"
	"net/http"
	"net/url"
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
// Apply is intentionally unimplemented for the v1 open-source release.
// Route it to an SDKDispatcher (not HTTPDispatcher) when AWS Bedrock support
// is added — the AWS SDK handles request signing internally.
type SigV4Credential struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string // empty for long-term IAM credentials
	Region          string
	Service         string // e.g. "bedrock-runtime"
}

func (c *SigV4Credential) Apply(_ *http.Request) error {
	return fmt.Errorf("SigV4Credential.Apply: not implemented; use SDKDispatcher for AWS Bedrock")
}

func (c *SigV4Credential) Type() string { return "aws_sigv4" }

func (c *SigV4Credential) Redacted() string {
	if len(c.AccessKeyID) < 4 {
		return "sigv4:***"
	}
	return "sigv4:" + c.AccessKeyID[:4] + "***"
}
