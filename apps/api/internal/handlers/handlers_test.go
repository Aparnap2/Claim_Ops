package handlers_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"claimops-api/internal/app"
)

func do(t *testing.T, app interface {
	Test(*http.Request, ...int) (*http.Response, error)
}, req *http.Request) *http.Response {
	t.Helper()
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

func TestHealthEndpoint(t *testing.T) {
	app := app.New()
	req, _ := http.NewRequest(http.MethodGet, "/healthz", nil)
	resp := do(t, app, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("unexpected body: %s", body)
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Fatal("expected X-Request-ID to be set")
	}
}

func TestRequestIDPropagation(t *testing.T) {
	app := app.New()
	req, _ := http.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-ID", "req-test-123")
	resp := do(t, app, req)
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Request-ID"); got != "req-test-123" {
		t.Fatalf("expected echoed request id, got %q", got)
	}
}

func TestMalformedRequestRejected(t *testing.T) {
	app := app.New()
	req, _ := http.NewRequest(http.MethodPost, "/claims", strings.NewReader("{bad json"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "t-apollo")
	resp := do(t, app, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "VALIDATION_ERROR") {
		t.Fatalf("expected VALIDATION_ERROR envelope, got %s", body)
	}
}

func TestTenantContextMissingRejected(t *testing.T) {
	app := app.New()
	req, _ := http.NewRequest(http.MethodPost, "/claims",
		strings.NewReader(`{"claim_reference":"CLM-1","policy_id":"P-1"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, app, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "TENANT_MISSING") {
		t.Fatalf("expected TENANT_MISSING envelope, got %s", body)
	}
}

func TestValidClaimAcceptedWithTenant(t *testing.T) {
	app := app.New()
	req, _ := http.NewRequest(http.MethodPost, "/claims",
		strings.NewReader(`{"claim_reference":"CLM-1","policy_id":"P-1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "t-apollo")
	resp := do(t, app, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}
}
