// Package httpadapter implements outbound port adapters over HTTP.
//
// Each file in this package adapts one ports interface (PolicyPort,
// ClaimsPort, ProviderPort, RiskPort) to a JSON HTTP upstream. The shared
// Client owns transport concerns (base URL, timeout, request IDs, status
// mapping) so per-port files only handle decode plus validation.
package httpadapter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"claimops-api/internal/ports"
)

// requestIDContextKey is the context value key carrying the inbound
// request ID. It mirrors middleware's c.Locals("request_id") propagation:
// callers store the ID under the plain string key "request_id".
const requestIDContextKey = "request_id"

// Client is a shared JSON-over-HTTP upstream client.
//
// BaseURL is the upstream origin (scheme + host, no trailing slash).
// HTTP carries the timeout; retry policy lives in the workflow layer,
// never inside this client.
type Client struct {
	HTTP *http.Client
	// BaseURL is the upstream origin, e.g. "http://localhost:3001".
	BaseURL string
}

// New returns a Client targeting baseURL with the given timeout.
// A non-positive timeout defaults to 3s.
func New(baseURL string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &Client{
		HTTP:    &http.Client{Timeout: timeout},
		BaseURL: strings.TrimSuffix(strings.TrimSpace(baseURL), "/"),
	}
}

// Retryable reports whether err is a transient upstream failure worth
// retrying in the workflow layer. Only ErrUpstream (5xx, timeout,
// network) is retryable; ErrNotFound, ErrContract and ErrTenantMismatch
// are terminal.
func Retryable(err error) bool {
	return errors.Is(err, ports.ErrUpstream)
}

// do executes method against path with query and returns the raw body.
//
// It attaches X-Request-ID from ctx value "request_id" (generating one
// when absent) and sets Accept: application/json. Status mapping:
// 2xx -> body; 404 -> ErrNotFound; 5xx -> ErrUpstream (retryable);
// any other non-2xx -> ErrContract. Transport, timeout and body-read
// failures map to ErrUpstream. No retries happen here.
func (c *Client) do(ctx context.Context, method string, path string, query url.Values) ([]byte, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	target := c.BaseURL + path
	if query != nil && len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request %s %s: %v", ports.ErrUpstream, method, path, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Request-ID", requestIDFromContext(ctx))

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: transport %s %s: %v", ports.ErrUpstream, method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: read body %s %s: %v", ports.ErrUpstream, method, path, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: upstream status 404 for %s %s", ports.ErrNotFound, method, path)
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("%w: upstream status %d for %s %s", ports.ErrUpstream, resp.StatusCode, method, path)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("%w: unexpected upstream status %d for %s %s", ports.ErrContract, resp.StatusCode, method, path)
	default:
		return body, nil
	}
}

// requestIDFromContext returns the X-Request-ID for ctx, generating a
// fallback when the context carries no usable value under "request_id".
func requestIDFromContext(ctx context.Context) string {
	if ctx != nil {
		if v, ok := ctx.Value(requestIDContextKey).(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return newRequestID()
}

// newRequestID generates a req- prefixed random correlation ID.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-fallback"
	}
	return "req-" + hex.EncodeToString(b[:])
}
