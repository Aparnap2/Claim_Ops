package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime/pprof"
	"strings"
	"testing"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate/orchestrate"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	trustTenantA = "tnt-54-tools"
	trustTenantB = "tnt-54-other"
	trustClaim   = "clm-54-tools"
	trustInvID   = "inv-0123456789abcdef0123456789abcdef"
	trustExID    = "ex-0123456789abcdef0123456789abcdef"
	trustReqID   = "req-54-tools"
)

func validEnvelopeForTenant(tenant, claimID, invID, exID, reqID string) invest.UnresolvedException {
	return invest.UnresolvedException{
		TenantID:        tenant,
		ClaimID:         claimID,
		ExceptionID:     exID,
		InvestigationID: invID,
		RuleFindings: []invest.RuleFinding{{
			Code:           invest.RulePolicyNumberConflict,
			Severity:       invest.SeverityHigh,
			Message:        "policy number conflict",
			EvidenceIDs:    []string{"ev-01"},
			AffectedFields: []string{"policy_number"},
		}},
		Scope: invest.ScopeConstraints{
			TenantID:     tenant,
			ClaimID:      claimID,
			AllowTools:   []invest.ToolName{invest.ToolGetClaim},
			MaxToolCalls: 5,
			DeadlineMs:   5000,
			RequestID:    reqID,
		},
		EvidenceRefs: []invest.EvidenceRef{{
			EvidenceID: "ev-01",
			SourceType: invest.EvidenceSourceDocument,
			SourceID:   "doc-54-1",
			TenantID:   tenant,
			ClaimID:    claimID,
			DocumentID: "doc-54-1",
			Page:       1,
		}},
	}
}

func newInvestigationApp(t *testing.T, pool *pgxpool.Pool) *fiber.App {
	t.Helper()
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(recover.New())
	handler := investigationHandler(pool, orchestrate.NewMockModelClient(nil))
	app.Post("/v1/investigations", handler)
	return app
}

func newTestAgentApp(appEnv string) *fiber.App {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(recover.New())
	registerLocalDebug(app, appEnv)
	if appEnv == "local" {
		app.Post("/v1/investigations/mock-script", mockScriptHandler(nil))
	}
	return app
}

func decodeBody(t *testing.T, resp *http.Response) map[string]interface{} {
	t.Helper()
	defer resp.Body.Close()
	var m map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return m
}

func TestAgentTrust_MissingTrustedTenantRejected(t *testing.T) {
	app := newInvestigationApp(t, nil)
	body := map[string]string{
		"tenant_id":        trustTenantA,
		"investigation_id": trustInvID,
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/investigations", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	m := decodeBody(t, resp)
	if m["code"] != "BAD_REQUEST" {
		t.Fatalf("code = %v, want BAD_REQUEST", m["code"])
	}
	msg, _ := m["message"].(string)
	if !strings.Contains(msg, "tenant_id required via X-Tenant-ID") {
		t.Fatalf("message = %q, want contains tenant_id required via X-Tenant-ID", msg)
	}
}

func TestAgentTrust_BodyTenantOnlyRejected(t *testing.T) {
	app := newInvestigationApp(t, nil)
	body := map[string]string{
		"tenant_id":        trustTenantA,
		"investigation_id": trustInvID,
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/investigations", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	// Intentionally no X-Tenant-ID header: body tenant must not be authority.
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	m := decodeBody(t, resp)
	if m["code"] != "BAD_REQUEST" {
		t.Fatalf("code = %v, want BAD_REQUEST", m["code"])
	}
	msg, _ := m["message"].(string)
	if !strings.Contains(msg, "tenant_id required via X-Tenant-ID") {
		t.Fatalf("message = %q, want contains tenant_id required via X-Tenant-ID", msg)
	}
}

func TestAgentTrust_TrustedA_BodyB_Rejected(t *testing.T) {
	app := newInvestigationApp(t, nil)
	body := map[string]string{
		"tenant_id":        trustTenantB,
		"investigation_id": trustInvID,
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/investigations", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", trustTenantA)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != 403 {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	m := decodeBody(t, resp)
	if m["code"] != "TENANT_MISMATCH" {
		t.Fatalf("code = %v, want TENANT_MISMATCH", m["code"])
	}
	msg, _ := m["message"].(string)
	if !strings.Contains(msg, "body tenant_id does not match trusted header") {
		t.Fatalf("message = %q, want contains body tenant_id does not match trusted header", msg)
	}
}

func TestAgentTrust_TrustedA_InvestigationOfB_Rejected(t *testing.T) {
	t.Setenv("APP_ENV", "local")
	t.Setenv("ALLOW_INLINE_ENVELOPE", "true")

	// Dummy non-nil pool bypasses the early 503 but is never used for inline path
	// tenant mismatch, which returns 403 before any store access.
	pool := &pgxpool.Pool{}
	app := newInvestigationApp(t, pool)

	// Envelope B is valid per invest.Validate but belongs to tenant B.
	envB := validEnvelopeForTenant(trustTenantB, trustClaim, trustInvID, trustExID, trustReqID)
	if err := invest.Validate(envB); err != nil {
		t.Fatalf("validEnvelopeForTenant Validate: %v", err)
	}

	reqBody := InvestigationRequest{
		TenantID:        trustTenantA,
		InvestigationID: trustInvID,
		Exception:       &envB,
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/investigations", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", trustTenantA)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != 403 {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	m := decodeBody(t, resp)
	if m["code"] != "TENANT_MISMATCH" {
		t.Fatalf("code = %v, want TENANT_MISMATCH", m["code"])
	}
	msg, _ := m["message"].(string)
	if !strings.Contains(msg, "tenant mismatch") {
		t.Fatalf("message = %q, want contains tenant mismatch", msg)
	}
}

func TestAgentTrust_ProductionMockDebugNotExposed(t *testing.T) {
	// Ensure goroutineleak profile exists so local handler returns 200 not 500.
	if pprof.Lookup("goroutineleak") == nil {
		pprof.NewProfile("goroutineleak")
	}

	// Production app: should not expose local-only surfaces.
	prod := newTestAgentApp("production")
	// Also test empty env behaves like production (not local).
	emptyEnv := newTestAgentApp("")
	// Local app: both surfaces exposed.
	local := newTestAgentApp("local")

	cases := []struct {
		name string
		app  *fiber.App
		want int
		path string
		meth string
	}{
		{"prod debug 404", prod, 404, "/debug/pprof/goroutineleak", http.MethodGet},
		{"empty debug 404", emptyEnv, 404, "/debug/pprof/goroutineleak", http.MethodGet},
		{"prod mock-script 404", prod, 404, "/v1/investigations/mock-script", http.MethodPost},
		{"empty mock-script 404", emptyEnv, 404, "/v1/investigations/mock-script", http.MethodPost},
		{"local debug 200", local, 200, "/debug/pprof/goroutineleak", http.MethodGet},
		{"local mock-script 200", local, 200, "/v1/investigations/mock-script", http.MethodPost},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.meth, tc.path, nil)
			if tc.meth == http.MethodPost {
				req.Header.Set("Content-Type", "application/json")
			}
			resp, err := tc.app.Test(req, -1)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d for %s %s", resp.StatusCode, tc.want, tc.meth, tc.path)
			}
		})
	}
}
