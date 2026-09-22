package handlers_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"claimops-api/internal/handlers"

	"github.com/gofiber/fiber/v2"
)

const apa9Secret = "test-hit-webhook-secret-apa9-01"

func apa9Sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

func apa9App(secret string) *fiber.App {
	app := fiber.New()
	app.Post("/v1/claims/:id/decision", handlers.DecisionHandler(nil, secret))
	return app
}

func apa9Do(t *testing.T, app *fiber.App, claimID, tenant, body, sig string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/claims/"+claimID+"/decision", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if tenant != "" {
		req.Header.Set("X-Tenant-ID", tenant)
	}
	if sig != "" {
		req.Header.Set("X-Signature", sig)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func apa9Body() string {
	return `{"action":"APPROVE","reason":"ok","actor":"human","request_id":"req-apa9-01"}`
}

// Missing signature must fail closed before any tenant/state logic.
func TestDecision_MissingSignatureRejected(t *testing.T) {
	app := apa9App(apa9Secret)
	status, body := apa9Do(t, app, "clm-apa9-01", "tnt-apa9", apa9Body(), "")
	if status != http.StatusUnauthorized {
		t.Fatalf("missing signature status = %d, want 401", status)
	}
	if !strings.Contains(body, "WEBHOOK_UNAUTHORIZED") {
		t.Fatalf("missing signature body = %s, want WEBHOOK_UNAUTHORIZED", body)
	}
}

// Invalid signature must fail closed.
func TestDecision_InvalidSignatureRejected(t *testing.T) {
	app := apa9App(apa9Secret)
	status, body := apa9Do(t, app, "clm-apa9-01", "tnt-apa9", apa9Body(), "deadbeef")
	if status != http.StatusUnauthorized {
		t.Fatalf("invalid signature status = %d, want 401", status)
	}
	if !strings.Contains(body, "WEBHOOK_UNAUTHORIZED") {
		t.Fatalf("invalid signature body = %s, want WEBHOOK_UNAUTHORIZED", body)
	}
}

// Tampered body (signed body differs from sent body) must fail.
func TestDecision_TamperedBodyRejected(t *testing.T) {
	app := apa9App(apa9Secret)
	sent := `{"action":"APPROVE","reason":"tampered","actor":"human","request_id":"req-apa9-01"}`
	sig := apa9Sign(apa9Secret, apa9Body()) // signature for a different body
	status, body := apa9Do(t, app, "clm-apa9-01", "tnt-apa9", sent, sig)
	if status != http.StatusUnauthorized {
		t.Fatalf("tampered body status = %d, want 401", status)
	}
	if !strings.Contains(body, "WEBHOOK_UNAUTHORIZED") {
		t.Fatalf("tampered body response = %s, want WEBHOOK_UNAUTHORIZED", body)
	}
}

// Wrong secret must fail.
func TestDecision_WrongSecretRejected(t *testing.T) {
	app := apa9App(apa9Secret)
	sig := apa9Sign("wrong-secret", apa9Body())
	status, body := apa9Do(t, app, "clm-apa9-01", "tnt-apa9", apa9Body(), sig)
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong secret status = %d, want 401", status)
	}
	if !strings.Contains(body, "WEBHOOK_UNAUTHORIZED") {
		t.Fatalf("wrong secret body = %s, want WEBHOOK_UNAUTHORIZED", body)
	}
}

// Valid signature must pass authentication (nil pool then fails at NO_DB,
// proving auth succeeded and the request reached the authoritative path).
func TestDecision_ValidSignatureSucceeds(t *testing.T) {
	app := apa9App(apa9Secret)
	body := apa9Body()
	sig := apa9Sign(apa9Secret, body)
	status, respBody := apa9Do(t, app, "clm-apa9-01", "tnt-apa9", body, sig)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("valid signature status = %d, want 503 NO_DB (auth passed, no DB)", status)
	}
	if !strings.Contains(respBody, "NO_DB") {
		t.Fatalf("valid signature body = %s, want NO_DB (auth passed)", respBody)
	}
}

// Tenant still comes from the header only: valid signature but missing
// tenant must reject with tenant error, not proceed.
func TestDecision_TrustedTenantStillRequired(t *testing.T) {
	app := apa9App(apa9Secret)
	body := apa9Body()
	sig := apa9Sign(apa9Secret, body)
	status, respBody := apa9Do(t, app, "clm-apa9-01", "", body, sig)
	if status != http.StatusBadRequest {
		t.Fatalf("missing tenant status = %d, want 400", status)
	}
	if !strings.Contains(respBody, "X-Tenant-ID required") {
		t.Fatalf("missing tenant body = %s, want X-Tenant-ID required", respBody)
	}
}

// Body-carried tenant_id must never become authority: extra JSON field is
// ignored, header remains the source (missing header still 400 even when
// body names a tenant).
func TestDecision_BodyTenantNeverAuthority(t *testing.T) {
	app := apa9App(apa9Secret)
	body := `{"action":"APPROVE","reason":"ok","actor":"human","request_id":"req-apa9-01","tenant_id":"tnt-body-evil"}`
	sig := apa9Sign(apa9Secret, body)
	status, respBody := apa9Do(t, app, "clm-apa9-01", "", body, sig)
	if status != http.StatusBadRequest {
		t.Fatalf("body-tenant-only status = %d, want 400 (header required)", status)
	}
	if !strings.Contains(respBody, "X-Tenant-ID required") {
		t.Fatalf("body-tenant-only body = %s, want X-Tenant-ID required", respBody)
	}
}

// Empty secret fails closed at request time (never bypassed).
func TestDecision_EmptySecretMisconfigured(t *testing.T) {
	app := apa9App("")
	body := apa9Body()
	sig := apa9Sign(apa9Secret, body)
	status, respBody := apa9Do(t, app, "clm-apa9-01", "tnt-apa9", body, sig)
	if status != http.StatusInternalServerError {
		t.Fatalf("empty secret status = %d, want 500", status)
	}
	if !strings.Contains(respBody, "WEBHOOK_MISCONFIGURED") {
		t.Fatalf("empty secret body = %s, want WEBHOOK_MISCONFIGURED", respBody)
	}
}

// Error responses must never echo the secret or raw payload.
func TestDecision_NoSecretLeak(t *testing.T) {
	app := apa9App(apa9Secret)
	body := apa9Body()
	status, respBody := apa9Do(t, app, "clm-apa9-01", "tnt-apa9", body, "bad-sig")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if strings.Contains(respBody, apa9Secret) {
		t.Fatalf("response leaks secret: %s", respBody)
	}
	if strings.Contains(respBody, "tampered") || strings.Contains(respBody, body) {
		t.Fatalf("response echoes payload: %s", respBody)
	}
}
