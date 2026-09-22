package handlers_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"claimops-api/internal/handlers"

	"github.com/gofiber/fiber/v2"
)

const apa9Secret = "test-hit-webhook-secret-apa9-01"

func apa9Path(claimID string) string {
	return "/v1/claims/" + claimID + "/decision"
}

// apa9Sign signs the full logical request: method + path (claim binding) +
// tenant (tenant binding) + raw body.
func apa9Sign(secret, claimID, tenant, body string) string {
	return handlers.SignWebhookRequest(secret, http.MethodPost, apa9Path(claimID), tenant, []byte(body))
}

func apa9App(secret string) *fiber.App {
	app := fiber.New()
	app.Post("/v1/claims/:id/decision", handlers.DecisionHandler(nil, secret))
	return app
}

func apa9Do(t *testing.T, app *fiber.App, claimID, tenant, body, sig string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, apa9Path(claimID), strings.NewReader(body))
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
	sig := apa9Sign(apa9Secret, "clm-apa9-01", "tnt-apa9", apa9Body()) // signature for a different body
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
	sig := apa9Sign("wrong-secret", "clm-apa9-01", "tnt-apa9", apa9Body())
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
	sig := apa9Sign(apa9Secret, "clm-apa9-01", "tnt-apa9", body)
	status, respBody := apa9Do(t, app, "clm-apa9-01", "tnt-apa9", body, sig)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("valid signature status = %d, want 503 NO_DB (auth passed, no DB)", status)
	}
	if !strings.Contains(respBody, "NO_DB") {
		t.Fatalf("valid signature body = %s, want NO_DB (auth passed)", respBody)
	}
}

// Altered X-Tenant-ID with an otherwise valid signature must fail: the
// tenant is part of the signed material.
func TestDecision_AlteredTenantRejected(t *testing.T) {
	app := apa9App(apa9Secret)
	body := apa9Body()
	sig := apa9Sign(apa9Secret, "clm-apa9-01", "tnt-apa9", body)
	status, respBody := apa9Do(t, app, "clm-apa9-01", "tnt-apa9-evil", body, sig)
	if status != http.StatusUnauthorized {
		t.Fatalf("altered tenant status = %d, want 401", status)
	}
	if !strings.Contains(respBody, "WEBHOOK_UNAUTHORIZED") {
		t.Fatalf("altered tenant body = %s, want WEBHOOK_UNAUTHORIZED", respBody)
	}
}

// Signature generated for tenant A sent with header tenant B must fail.
func TestDecision_CrossTenantSignatureRejected(t *testing.T) {
	app := apa9App(apa9Secret)
	body := apa9Body()
	sigA := apa9Sign(apa9Secret, "clm-apa9-01", "tnt-apa9-a", body)
	status, respBody := apa9Do(t, app, "clm-apa9-01", "tnt-apa9-b", body, sigA)
	if status != http.StatusUnauthorized {
		t.Fatalf("cross-tenant signature status = %d, want 401", status)
	}
	if !strings.Contains(respBody, "WEBHOOK_UNAUTHORIZED") {
		t.Fatalf("cross-tenant signature body = %s, want WEBHOOK_UNAUTHORIZED", respBody)
	}
}

// Altered claim URL with an otherwise valid signature must fail: the
// path (claim binding) is part of the signed material.
func TestDecision_AlteredClaimURLRejected(t *testing.T) {
	app := apa9App(apa9Secret)
	body := apa9Body()
	sig := apa9Sign(apa9Secret, "clm-apa9-01", "tnt-apa9", body)
	status, respBody := apa9Do(t, app, "clm-apa9-02", "tnt-apa9", body, sig)
	if status != http.StatusUnauthorized {
		t.Fatalf("altered claim URL status = %d, want 401", status)
	}
	if !strings.Contains(respBody, "WEBHOOK_UNAUTHORIZED") {
		t.Fatalf("altered claim URL body = %s, want WEBHOOK_UNAUTHORIZED", respBody)
	}
}

// Same signed body replayed to another claim must fail.
func TestDecision_ReplayToAnotherClaimRejected(t *testing.T) {
	app := apa9App(apa9Secret)
	body := apa9Body()
	sig := apa9Sign(apa9Secret, "clm-apa9-01", "tnt-apa9", body)
	status, respBody := apa9Do(t, app, "clm-apa9-other", "tnt-apa9", body, sig)
	if status != http.StatusUnauthorized {
		t.Fatalf("replay-to-another-claim status = %d, want 401", status)
	}
	if !strings.Contains(respBody, "WEBHOOK_UNAUTHORIZED") {
		t.Fatalf("replay-to-another-claim body = %s, want WEBHOOK_UNAUTHORIZED", respBody)
	}
}

// Tenant still comes from the header only: valid signature but missing
// tenant must reject with tenant error, not proceed.
func TestDecision_TrustedTenantStillRequired(t *testing.T) {
	app := apa9App(apa9Secret)
	body := apa9Body()
	sig := apa9Sign(apa9Secret, "clm-apa9-01", "tnt-apa9", body)
	status, respBody := apa9Do(t, app, "clm-apa9-01", "", body, sig)
	if status != http.StatusBadRequest {
		t.Fatalf("missing tenant status = %d, want 400", status)
	}
	if !strings.Contains(respBody, "X-Tenant-ID required") {
		t.Fatalf("missing tenant body = %s, want X-Tenant-ID required", respBody)
	}
}

// The signer binds the tenant byte-for-byte: a padded tenant never shares
// a MAC with its trimmed form, so no layer can silently normalize one
// identity into the other.
func TestDecision_SignerBindsExactTenantBytes(t *testing.T) {
	body := []byte(apa9Body())
	a := handlers.SignWebhookRequest(apa9Secret, http.MethodPost, apa9Path("clm-apa9-01"), "tnt-apa9", body)
	b := handlers.SignWebhookRequest(apa9Secret, http.MethodPost, apa9Path("clm-apa9-01"), " tnt-apa9 ", body)
	if a == b {
		t.Fatal("padded tenant shares MAC with trimmed tenant (silent normalization)")
	}
}

// The HTTP stack strips header OWS before the handler runs, so a padded
// header arrives canonicalized: it authenticates only under the trimmed
// identity, and that same identity flows to RLS. MAC identity == RLS
// identity by construction (single tenantID variable, exact-byte MAC).
func TestDecision_PaddedHeaderUsesCanonicalIdentity(t *testing.T) {
	app := apa9App(apa9Secret)
	body := apa9Body()
	trimmedSig := apa9Sign(apa9Secret, "clm-apa9-01", "tnt-apa9", body)
	paddedSig := apa9Sign(apa9Secret, "clm-apa9-01", " tnt-apa9 ", body)
	// Padded-tenant MAC must not verify against the canonical identity.
	status, _ := apa9Do(t, app, "clm-apa9-01", "tnt-apa9", body, paddedSig)
	if status != http.StatusUnauthorized {
		t.Fatalf("padded-tenant MAC vs canonical identity status = %d, want 401", status)
	}
	// Trimmed-tenant MAC verifies (nil pool then proves auth passed).
	status, respBody := apa9Do(t, app, "clm-apa9-01", " tnt-apa9 ", body, trimmedSig)
	if status != http.StatusServiceUnavailable || !strings.Contains(respBody, "NO_DB") {
		t.Fatalf("canonical identity status = %d (%s), want 503 NO_DB", status, respBody)
	}
}

// Body-carried tenant_id must never become authority: extra JSON field is
// ignored, header remains the source (missing header still 400 even when
// body names a tenant).
func TestDecision_BodyTenantNeverAuthority(t *testing.T) {
	app := apa9App(apa9Secret)
	body := `{"action":"APPROVE","reason":"ok","actor":"human","request_id":"req-apa9-01","tenant_id":"tnt-body-evil"}`
	sig := apa9Sign(apa9Secret, "clm-apa9-01", "tnt-body-evil", body)
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
	sig := apa9Sign(apa9Secret, "clm-apa9-01", "tnt-apa9", body)
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
