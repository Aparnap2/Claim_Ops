package app

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsEndpointExposesCounters(t *testing.T) {
	app := New()
	// One counted request first (/metrics itself is excluded from counting).
	gen, err := app.Test(httptest.NewRequest("GET", "/nonexistent", nil))
	if err != nil {
		t.Fatal(err)
	}
	gen.Body.Close()
	req := httptest.NewRequest("GET", "/metrics", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "http_requests_total") {
		t.Fatalf("metrics body missing http_requests_total:\n%s", body)
	}
}

func TestCorrelationHeaderPresent(t *testing.T) {
	app := New()
	req := httptest.NewRequest("GET", "/healthz", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("X-Correlation-ID") == "" {
		t.Fatal("missing X-Correlation-ID response header")
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Fatal("missing X-Request-ID response header")
	}
}
