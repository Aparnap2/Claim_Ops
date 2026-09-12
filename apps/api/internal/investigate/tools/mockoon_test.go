// Live Mockoon matrix for the outbound-backed tools (issue #54: T2/T8/T9/T10).
//
// Runs against the Mockoon LIVE container at http://localhost:3001 and
// skips when Mockoon is unreachable: the gate dials GET /v1/policies/POL-001
// with a 3s timeout and calls t.Skip on any error or non-200 status.
// Scenario selection uses a test-only RoundTripper
// (mockoonScenarioTripper) that appends ?scenario=<case>; the production
// adapters never send it.
//
// Each tool is wired to its REAL HTTP adapter (as ports) with an in-memory
// fakePinner: every assertion exercises real adapter decode/validate logic
// against real Mockoon bodies plus the tool-side projection/tenant gates.
package tools

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	httpadapter "claimops-api/internal/adapters/http"
	"claimops-api/internal/ports"
)

// liveMockoonOrigin is the live Mockoon container origin under test.
const liveMockoonOrigin = "http://localhost:3001"

// requireLiveMockoon gates the live tool matrix: dial GET
// /v1/policies/POL-001 with a 3s timeout and skip when Mockoon is
// unreachable or unhealthy.
func requireLiveMockoon(t *testing.T) {
	t.Helper()
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(liveMockoonOrigin + "/v1/policies/POL-001")
	if err != nil {
		t.Skipf("Mockoon unreachable at %s: %v", liveMockoonOrigin, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Skipf("Mockoon gate returned status %d, skipping live tool matrix", resp.StatusCode)
	}
}

// mockoonScenarioTripper is a test RoundTripper that appends
// ?scenario=<case> to every outgoing request so Mockoon rule-based
// responses are selectable. The adapters themselves never send this param.
type mockoonScenarioTripper struct {
	scenario string
	base     http.RoundTripper
}

func (s mockoonScenarioTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	q := clone.URL.Query()
	q.Set("scenario", s.scenario)
	clone.URL.RawQuery = q.Encode()
	base := s.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

// liveToolBaseClient builds an adapter Client pointed at Mockoon, injecting
// the scenario selector when non-empty.
func liveToolBaseClient(scenario string, timeout time.Duration) *httpadapter.Client {
	c := httpadapter.New(liveMockoonOrigin, timeout)
	if scenario != "" {
		c.HTTP.Transport = mockoonScenarioTripper{scenario: scenario}
	}
	return c
}

// requireLiveSentinel fails unless err wraps sentinel via errors.Is.
func requireLiveSentinel(t *testing.T, err error, sentinel error) {
	t.Helper()
	if !errors.Is(err, sentinel) {
		t.Fatalf("want error wrapping %v, got %v", sentinel, err)
	}
}

func TestLiveToolMatrix(t *testing.T) {
	requireLiveMockoon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	t.Run("T2_policy", func(t *testing.T) {
		t.Run("happy", func(t *testing.T) {
			var port ports.PolicyPort = httpadapter.NewPolicyClient(liveToolBaseClient("", 5*time.Second))
			resp, err := NewPolicyTool(port, &fakePinner{id: "ev-live-t2"})(ctx, PolicyRequest{
				TenantID: "tenant-a", ClaimID: toolsClaim,
				InvestigationID: toolsInv, RequestID: toolsReq,
				PolicyID: "POL-001",
			})
			if err != nil {
				t.Fatalf("happy: unexpected error: %v", err)
			}
			if resp.Policy.PolicyID != "POL-001" || resp.Policy.TenantID != "tenant-a" {
				t.Fatalf("happy: want POL-001/tenant-a, got %q/%q", resp.Policy.PolicyID, resp.Policy.TenantID)
			}
			if resp.EvidenceID == "" {
				t.Error("happy: blank EvidenceID")
			}
			if len(resp.ContentHash) != 64 {
				t.Errorf("happy: ContentHash len = %d, want 64", len(resp.ContentHash))
			}
		})
		t.Run("wrongtenant", func(t *testing.T) {
			var port ports.PolicyPort = httpadapter.NewPolicyClient(liveToolBaseClient("wrongtenant", 5*time.Second))
			_, err := NewPolicyTool(port, &fakePinner{id: "ev-live-t2x"})(ctx, PolicyRequest{
				TenantID: "tenant-a", ClaimID: toolsClaim,
				InvestigationID: toolsInv, RequestID: toolsReq,
				PolicyID: "POL-001",
			})
			requireLiveSentinel(t, err, ports.ErrTenantMismatch)
		})
		t.Run("notfound", func(t *testing.T) {
			var port ports.PolicyPort = httpadapter.NewPolicyClient(liveToolBaseClient("notfound", 5*time.Second))
			_, err := NewPolicyTool(port, &fakePinner{id: "ev-live-t2x"})(ctx, PolicyRequest{
				TenantID: "tenant-a", ClaimID: toolsClaim,
				InvestigationID: toolsInv, RequestID: toolsReq,
				PolicyID: "POL-001",
			})
			requireLiveSentinel(t, err, ports.ErrNotFound)
		})
	})

	t.Run("T8_tpa", func(t *testing.T) {
		t.Run("happy", func(t *testing.T) {
			var port ports.ClaimsPort = httpadapter.NewTPAClient(liveToolBaseClient("", 5*time.Second))
			resp, err := NewTPATool(port, &fakePinner{id: "ev-live-t8"})(ctx, TPARequest{
				TenantID: "tenant-a", ClaimID: toolsClaim,
				InvestigationID: toolsInv, RequestID: toolsReq,
				PolicyID: "POL-001",
			})
			if err != nil {
				t.Fatalf("happy: unexpected error: %v", err)
			}
			found := false
			for _, c := range resp.Claims {
				if c.ClaimID == "CLM-OLD-01" {
					found = true
				}
				if c.TenantID != "tenant-a" {
					t.Fatalf("happy: item %q carries tenant %q, want tenant-a", c.ClaimID, c.TenantID)
				}
			}
			if !found {
				t.Fatalf("happy: want CLM-OLD-01 present, got %+v", resp.Claims)
			}
			if resp.EvidenceID == "" {
				t.Error("happy: blank EvidenceID")
			}
		})
		t.Run("wrongtenant", func(t *testing.T) {
			var port ports.ClaimsPort = httpadapter.NewTPAClient(liveToolBaseClient("wrongtenant", 5*time.Second))
			_, err := NewTPATool(port, &fakePinner{id: "ev-live-t8x"})(ctx, TPARequest{
				TenantID: "tenant-a", ClaimID: toolsClaim,
				InvestigationID: toolsInv, RequestID: toolsReq,
				PolicyID: "POL-001",
			})
			requireLiveSentinel(t, err, ports.ErrTenantMismatch)
		})
	})

	t.Run("T9_provider", func(t *testing.T) {
		t.Run("happy", func(t *testing.T) {
			var port ports.ProviderPort = httpadapter.NewProviderClient(liveToolBaseClient("", 5*time.Second))
			resp, err := NewProviderTool(port, &fakePinner{id: "ev-live-t9"})(ctx, ProviderRequest{
				TenantID: "tenant-a", ClaimID: toolsClaim,
				InvestigationID: toolsInv, RequestID: toolsReq,
				EncounterID: "ENC-100", AllowedEncounters: []string{"ENC-100"},
			})
			if err != nil {
				t.Fatalf("happy: unexpected error: %v", err)
			}
			if resp.Encounter.EncounterID != "ENC-100" || resp.Encounter.PatientRef != "PAT-001" {
				t.Fatalf("happy: want ENC-100/PAT-001, got %q/%q",
					resp.Encounter.EncounterID, resp.Encounter.PatientRef)
			}
			if resp.EvidenceID == "" {
				t.Error("happy: blank EvidenceID")
			}
		})
		t.Run("wrongtenant", func(t *testing.T) {
			var port ports.ProviderPort = httpadapter.NewProviderClient(liveToolBaseClient("wrongtenant", 5*time.Second))
			_, err := NewProviderTool(port, &fakePinner{id: "ev-live-t9x"})(ctx, ProviderRequest{
				TenantID: "tenant-a", ClaimID: toolsClaim,
				InvestigationID: toolsInv, RequestID: toolsReq,
				EncounterID: "ENC-100", AllowedEncounters: []string{"ENC-100"},
			})
			requireLiveSentinel(t, err, ports.ErrTenantMismatch)
		})
	})

	t.Run("T10_risk", func(t *testing.T) {
		t.Run("happy", func(t *testing.T) {
			var port ports.RiskPort = httpadapter.NewRiskClient(liveToolBaseClient("", 5*time.Second))
			resp, err := NewRiskTool(port, &fakePinner{id: "ev-live-t10"})(ctx, RiskRequest{
				TenantID: "tenant-a", ClaimID: toolsClaim,
				InvestigationID: toolsInv, RequestID: toolsReq,
				SubjectID: "SUB-001",
			})
			if err != nil {
				t.Fatalf("happy: unexpected error: %v", err)
			}
			found := false
			for _, s := range resp.Signals {
				if s.SignalType == "RECENT_SIMILAR_CLAIM" {
					found = true
				}
			}
			if !found {
				t.Fatalf("happy: want RECENT_SIMILAR_CLAIM present, got %+v", resp.Signals)
			}
			if resp.EvidenceID == "" {
				t.Error("happy: blank EvidenceID")
			}
		})
		t.Run("wrongtenant", func(t *testing.T) {
			var port ports.RiskPort = httpadapter.NewRiskClient(liveToolBaseClient("wrongtenant", 5*time.Second))
			_, err := NewRiskTool(port, &fakePinner{id: "ev-live-t10x"})(ctx, RiskRequest{
				TenantID: "tenant-a", ClaimID: toolsClaim,
				InvestigationID: toolsInv, RequestID: toolsReq,
				SubjectID: "SUB-001",
			})
			requireLiveSentinel(t, err, ports.ErrTenantMismatch)
		})
	})
}
