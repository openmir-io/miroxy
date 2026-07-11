package cred

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	credbrokerv1 "github.com/openmir-io/miroxy-rpc/gen/go/credbroker/v1"
	"github.com/openmir-io/miroxy-rpc/gen/go/credbroker/v1/credbrokerv1connect"

	corecred "miroxy/core/cred"
)

// ConnectBrokerClient implements core/cred.CredBroker via the generated ConnectRPC client.
// Wire encoding: Connect JSON (HTTP/1.1 + JSON) — no gRPC library required on the server side.
type ConnectBrokerClient struct {
	rpc credbrokerv1connect.CredBrokerServiceClient
}

// NewConnectBrokerClient creates a client for the given server URL and optional bearer token.
// baseURL must include the scheme (e.g. "http://localhost:50051").
// token is sent as "Authorization: Bearer <token>" when non-empty.
func NewConnectBrokerClient(baseURL, token string) *ConnectBrokerClient {
	httpClient := &http.Client{Timeout: 10 * time.Second}
	if token != "" {
		httpClient.Transport = &bearerTransport{token: token, base: http.DefaultTransport}
	}
	rpc := credbrokerv1connect.NewCredBrokerServiceClient(
		httpClient,
		baseURL,
		connect.WithProtoJSON(), // JSON encoding — keeps HTTP/1.1 compatibility
	)
	return &ConnectBrokerClient{rpc: rpc}
}

var _ corecred.CredBroker = (*ConnectBrokerClient)(nil)

func (c *ConnectBrokerClient) Acquire(ctx context.Context, poolID, callerID string) (string, corecred.Credential, error) {
	resp, err := c.rpc.Acquire(ctx, connect.NewRequest(&credbrokerv1.AcquireRequest{
		PoolId:   poolID,
		CallerId: callerID,
	}))
	if err != nil {
		return "", nil, fmt.Errorf("cred broker acquire: %w", err)
	}
	msg := resp.Msg
	crd, err := credFromKind(msg.Kind, msg.Value, msg.HeaderName, msg.ParamName)
	if err != nil {
		return "", nil, err
	}
	return msg.LeaseId, crd, nil
}

func (c *ConnectBrokerClient) Release(ctx context.Context, leaseID string, rateLimited, serverOverload, callError bool) error {
	_, err := c.rpc.Release(ctx, connect.NewRequest(&credbrokerv1.ReleaseRequest{
		LeaseId:        leaseID,
		RateLimited:    rateLimited,
		ServerOverload: serverOverload,
		CallError:      callError,
	}))
	if err != nil {
		return fmt.Errorf("cred broker release: %w", err)
	}
	return nil
}

func (c *ConnectBrokerClient) HealthyCount(ctx context.Context, poolID string) (int, error) {
	resp, err := c.rpc.PoolStatus(ctx, connect.NewRequest(&credbrokerv1.PoolStatusRequest{
		PoolId: poolID,
	}))
	if err != nil {
		return 0, fmt.Errorf("cred broker pool status: %w", err)
	}
	for _, p := range resp.Msg.Pools {
		if p.PoolId == poolID {
			return int(p.Healthy), nil
		}
	}
	return 0, nil
}

// credFromKind maps CredBroker credential kind + fields to a miroxy Credential.
func credFromKind(kind credbrokerv1.CredentialKind, value, headerName, paramName string) (corecred.Credential, error) {
	switch kind {
	case credbrokerv1.CredentialKind_CREDENTIAL_KIND_HEADER:
		return &corecred.HeaderCredential{Header: headerName, Value: value}, nil
	case credbrokerv1.CredentialKind_CREDENTIAL_KIND_QUERY:
		return &corecred.QueryCredential{Param: paramName, Value: value}, nil
	default:
		return nil, fmt.Errorf("cred broker: unsupported credential kind %v (SIGV4 requires SDKDispatcher)", kind)
	}
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(req)
}
