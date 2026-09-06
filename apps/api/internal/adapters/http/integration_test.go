// Live upstream-contract tests for the four outbound HTTP adapters.
//
// These tests run against the Mockoon LIVE container at
// http://localhost:3001 (container claimops-mockoon, env
// mocks/mockoon/claims-systems.json) and skip when Mockoon is unreachable:
// the gate dials GET /v1/policies/POL-001 with a 3s timeout and calls
// t.Skip on any error or non-200 status.
//
// Adapter wire paths match the Mockoon routes directly (see the wire-shape
// notes on each client), so no proxy sits between the adapters and the
// mock: every assertion exercises real adapter decode/validate logic
// against real Mockoon bodies.
//
// One test-only bridge remains: scenario injection. The adapters send
// only tenant/subject query params, so ?scenario=<case> (the selector
// Mockoon rules match on) is appended by a test RoundTripper wrapping
// the client's transport.
//
// Strict contract: truncated payloads map to ErrContract via required-key
// presence checks, and every list item must carry a tenant marker matching
// the request tenant (drift maps to ErrTenantMismatch).
package httpadapter_test

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

// mockoonOrigin is the live Mockoon container origin under test.
const mockoonOrigin = "http://localhost:3001"

// requireMockoon gates the live suite: dial GET /v1/policies/POL-001 with
// a 3s timeout and skip when Mockoon is unreachable or unhealthy.
func requireMockoon(t *testing.T) {
	t.Helper()
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(mockoonOrigin + "/v1/policies/POL-001")
	if err != nil {
		t.Skipf("Mockoon unreachable at %s: %v", mockoonOrigin, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Skipf("Mockoon gate %s returned status %d, skipping live contract tests", mockoonOrigin, resp.StatusCode)
	}
}

// scenarioTransport is a test RoundTripper that appends ?scenario=<case>
// to every outgoing request so Mockoon rule-based responses are selectable.
// The adapters themselves never send this param.
type scenarioTransport struct {
	scenario string
	base     http.RoundTripper
}

func (s scenarioTransport) RoundTrip(r *http.Request) (*http.Response, error) {
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

// testClient builds an adapter Client pointed at base, injecting scenario
// when non-empty. Slow-case callers pass a 10s timeout (Mockoon `slow`
// responses carry 3s latency, which the 3s default would race).
func testClient(base string, scenario string, timeout time.Duration) *httpadapter.Client {
	c := httpadapter.New(base, timeout)
	if scenario != "" {
		c.HTTP.Transport = scenarioTransport{scenario: scenario}
	}
	return c
}

// requireSentinel fails the test unless err wraps sentinel via errors.Is.
func requireSentinel(t *testing.T, err error, sentinel error) {
	t.Helper()
	if !errors.Is(err, sentinel) {
		t.Fatalf("want error wrapping %v, got %v", sentinel, err)
	}
}

func TestLiveContracts(t *testing.T) {
	requireMockoon(t)
	base := mockoonOrigin
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	t.Run("policy", func(t *testing.T) {
		newPolicy := func(scenario string, timeout time.Duration) *httpadapter.PolicyClient {
			return httpadapter.NewPolicyClient(testClient(base, scenario, timeout))
		}
		t.Run("happy", func(t *testing.T) {
			got, err := newPolicy("", 5*time.Second).GetPolicy(ctx, "tenant-a", "POL-001")
			if err != nil {
				t.Fatalf("happy: unexpected error: %v", err)
			}
			if got.PolicyID != "POL-001" || got.TenantID != "tenant-a" {
				t.Fatalf("happy: want POL-001/tenant-a, got %q/%q", got.PolicyID, got.TenantID)
			}
		})
		t.Run("notfound", func(t *testing.T) {
			_, err := newPolicy("notfound", 5*time.Second).GetPolicy(ctx, "tenant-a", "POL-001")
			requireSentinel(t, err, ports.ErrNotFound)
			if httpadapter.Retryable(err) {
				t.Fatalf("notfound: ErrNotFound must not be retryable")
			}
		})
		t.Run("error", func(t *testing.T) {
			_, err := newPolicy("error", 5*time.Second).GetPolicy(ctx, "tenant-a", "POL-001")
			requireSentinel(t, err, ports.ErrUpstream)
			if !httpadapter.Retryable(err) {
				t.Fatalf("error: ErrUpstream must be retryable")
			}
		})
		t.Run("malformed", func(t *testing.T) {
			_, err := newPolicy("malformed", 5*time.Second).GetPolicy(ctx, "tenant-a", "POL-001")
			requireSentinel(t, err, ports.ErrContract)
			if httpadapter.Retryable(err) {
				t.Fatalf("malformed: ErrContract must not be retryable")
			}
		})
		t.Run("wrongtenant", func(t *testing.T) {
			_, err := newPolicy("wrongtenant", 5*time.Second).GetPolicy(ctx, "tenant-a", "POL-001")
			requireSentinel(t, err, ports.ErrTenantMismatch)
			if httpadapter.Retryable(err) {
				t.Fatalf("wrongtenant: ErrTenantMismatch must not be retryable")
			}
		})
		t.Run("partial", func(t *testing.T) {
			// The partial body omits sum_insured_paise: required-key
			// presence maps the truncation to ErrContract.
			_, err := newPolicy("partial", 5*time.Second).GetPolicy(ctx, "tenant-a", "POL-001")
			requireSentinel(t, err, ports.ErrContract)
			if httpadapter.Retryable(err) {
				t.Fatalf("partial: ErrContract must not be retryable")
			}
		})
		t.Run("slow", func(t *testing.T) {
			// 3s Mockoon latency: needs the 10s client timeout to succeed.
			got, err := newPolicy("slow", 10*time.Second).GetPolicy(ctx, "tenant-a", "POL-001")
			if err != nil {
				t.Fatalf("slow: unexpected error: %v", err)
			}
			if got.PolicyID != "POL-001" || got.TenantID != "tenant-a" {
				t.Fatalf("slow: want POL-001/tenant-a, got %q/%q", got.PolicyID, got.TenantID)
			}
		})
	})

	t.Run("tpa", func(t *testing.T) {
		newTPA := func(scenario string, timeout time.Duration) *httpadapter.TPAClient {
			return httpadapter.NewTPAClient(testClient(base, scenario, timeout))
		}
		contains := func(items []ports.PriorClaim, id string) bool {
			for _, c := range items {
				if c.ClaimID == id {
					return true
				}
			}
			return false
		}
		t.Run("happy", func(t *testing.T) {
			got, err := newTPA("", 5*time.Second).ListClaims(ctx, "tenant-a", "POL-001")
			if err != nil {
				t.Fatalf("happy: unexpected error: %v", err)
			}
			if !contains(got, "CLM-OLD-01") {
				t.Fatalf("happy: want CLM-OLD-01 present, got %+v", got)
			}
		})
		t.Run("notfound", func(t *testing.T) {
			_, err := newTPA("notfound", 5*time.Second).ListClaims(ctx, "tenant-a", "POL-001")
			requireSentinel(t, err, ports.ErrNotFound)
			if httpadapter.Retryable(err) {
				t.Fatalf("notfound: ErrNotFound must not be retryable")
			}
		})
		t.Run("error", func(t *testing.T) {
			_, err := newTPA("error", 5*time.Second).ListClaims(ctx, "tenant-a", "POL-001")
			requireSentinel(t, err, ports.ErrUpstream)
			if !httpadapter.Retryable(err) {
				t.Fatalf("error: ErrUpstream must be retryable")
			}
		})
		t.Run("malformed", func(t *testing.T) {
			_, err := newTPA("malformed", 5*time.Second).ListClaims(ctx, "tenant-a", "POL-001")
			requireSentinel(t, err, ports.ErrContract)
			if httpadapter.Retryable(err) {
				t.Fatalf("malformed: ErrContract must not be retryable")
			}
		})
		t.Run("wrongtenant", func(t *testing.T) {
			// Per-item tenant-b drift maps to ErrTenantMismatch.
			_, err := newTPA("wrongtenant", 5*time.Second).ListClaims(ctx, "tenant-a", "POL-001")
			requireSentinel(t, err, ports.ErrTenantMismatch)
			if httpadapter.Retryable(err) {
				t.Fatalf("wrongtenant: ErrTenantMismatch must not be retryable")
			}
		})
		t.Run("partial", func(t *testing.T) {
			// The partial body omits approved_paise: required-key
			// presence maps the truncation to ErrContract.
			_, err := newTPA("partial", 5*time.Second).ListClaims(ctx, "tenant-a", "POL-001")
			requireSentinel(t, err, ports.ErrContract)
			if httpadapter.Retryable(err) {
				t.Fatalf("partial: ErrContract must not be retryable")
			}
		})
		t.Run("slow", func(t *testing.T) {
			// 3s Mockoon latency: needs the 10s client timeout to succeed.
			got, err := newTPA("slow", 10*time.Second).ListClaims(ctx, "tenant-a", "POL-001")
			if err != nil {
				t.Fatalf("slow: unexpected error: %v", err)
			}
			if !contains(got, "CLM-OLD-01") {
				t.Fatalf("slow: want CLM-OLD-01 present, got %+v", got)
			}
		})
	})

	t.Run("provider", func(t *testing.T) {
		newProvider := func(scenario string, timeout time.Duration) *httpadapter.ProviderClient {
			return httpadapter.NewProviderClient(testClient(base, scenario, timeout))
		}
		t.Run("happy", func(t *testing.T) {
			got, err := newProvider("", 5*time.Second).GetEncounter(ctx, "tenant-a", "ENC-100")
			if err != nil {
				t.Fatalf("happy: unexpected error: %v", err)
			}
			if got.EncounterID != "ENC-100" || got.PatientRef != "PAT-001" {
				t.Fatalf("happy: want ENC-100/PAT-001, got %q/%q", got.EncounterID, got.PatientRef)
			}
		})
		t.Run("notfound", func(t *testing.T) {
			_, err := newProvider("notfound", 5*time.Second).GetEncounter(ctx, "tenant-a", "ENC-100")
			requireSentinel(t, err, ports.ErrNotFound)
			if httpadapter.Retryable(err) {
				t.Fatalf("notfound: ErrNotFound must not be retryable")
			}
		})
		t.Run("error", func(t *testing.T) {
			_, err := newProvider("error", 5*time.Second).GetEncounter(ctx, "tenant-a", "ENC-100")
			requireSentinel(t, err, ports.ErrUpstream)
			if !httpadapter.Retryable(err) {
				t.Fatalf("error: ErrUpstream must be retryable")
			}
		})
		t.Run("malformed", func(t *testing.T) {
			_, err := newProvider("malformed", 5*time.Second).GetEncounter(ctx, "tenant-a", "ENC-100")
			requireSentinel(t, err, ports.ErrContract)
			if httpadapter.Retryable(err) {
				t.Fatalf("malformed: ErrContract must not be retryable")
			}
		})
		t.Run("wrongtenant", func(t *testing.T) {
			// The Encounter DTO has no tenant field; the adapter enforces
			// tenant opportunistically via the envelope tenant_id, which
			// the mock includes, so this is ErrTenantMismatch.
			_, err := newProvider("wrongtenant", 5*time.Second).GetEncounter(ctx, "tenant-a", "ENC-100")
			requireSentinel(t, err, ports.ErrTenantMismatch)
			if httpadapter.Retryable(err) {
				t.Fatalf("wrongtenant: ErrTenantMismatch must not be retryable")
			}
		})
		t.Run("partial", func(t *testing.T) {
			// The partial body omits admission_at: required-key
			// presence maps the truncation to ErrContract.
			_, err := newProvider("partial", 5*time.Second).GetEncounter(ctx, "tenant-a", "ENC-100")
			requireSentinel(t, err, ports.ErrContract)
			if httpadapter.Retryable(err) {
				t.Fatalf("partial: ErrContract must not be retryable")
			}
		})
		t.Run("slow", func(t *testing.T) {
			// 3s Mockoon latency: needs the 10s client timeout to succeed.
			got, err := newProvider("slow", 10*time.Second).GetEncounter(ctx, "tenant-a", "ENC-100")
			if err != nil {
				t.Fatalf("slow: unexpected error: %v", err)
			}
			if got.EncounterID != "ENC-100" || got.PatientRef != "PAT-001" {
				t.Fatalf("slow: want ENC-100/PAT-001, got %q/%q", got.EncounterID, got.PatientRef)
			}
		})
	})

	t.Run("risk", func(t *testing.T) {
		newRisk := func(scenario string, timeout time.Duration) *httpadapter.RiskClient {
			return httpadapter.NewRiskClient(testClient(base, scenario, timeout))
		}
		contains := func(items []ports.RiskSignal, sig string) bool {
			for _, s := range items {
				if s.SignalType == sig {
					return true
				}
			}
			return false
		}
		t.Run("happy", func(t *testing.T) {
			got, err := newRisk("", 5*time.Second).GetSignals(ctx, "tenant-a", "SUB-001")
			if err != nil {
				t.Fatalf("happy: unexpected error: %v", err)
			}
			if !contains(got, "RECENT_SIMILAR_CLAIM") {
				t.Fatalf("happy: want RECENT_SIMILAR_CLAIM present, got %+v", got)
			}
		})
		t.Run("notfound", func(t *testing.T) {
			_, err := newRisk("notfound", 5*time.Second).GetSignals(ctx, "tenant-a", "SUB-001")
			requireSentinel(t, err, ports.ErrNotFound)
			if httpadapter.Retryable(err) {
				t.Fatalf("notfound: ErrNotFound must not be retryable")
			}
		})
		t.Run("error", func(t *testing.T) {
			_, err := newRisk("error", 5*time.Second).GetSignals(ctx, "tenant-a", "SUB-001")
			requireSentinel(t, err, ports.ErrUpstream)
			if !httpadapter.Retryable(err) {
				t.Fatalf("error: ErrUpstream must be retryable")
			}
		})
		t.Run("malformed", func(t *testing.T) {
			_, err := newRisk("malformed", 5*time.Second).GetSignals(ctx, "tenant-a", "SUB-001")
			requireSentinel(t, err, ports.ErrContract)
			if httpadapter.Retryable(err) {
				t.Fatalf("malformed: ErrContract must not be retryable")
			}
		})
		t.Run("wrongtenant", func(t *testing.T) {
			// Per-item tenant-b drift maps to ErrTenantMismatch.
			_, err := newRisk("wrongtenant", 5*time.Second).GetSignals(ctx, "tenant-a", "SUB-001")
			requireSentinel(t, err, ports.ErrTenantMismatch)
			if httpadapter.Retryable(err) {
				t.Fatalf("wrongtenant: ErrTenantMismatch must not be retryable")
			}
		})
		t.Run("partial", func(t *testing.T) {
			// The partial body omits severity, which validation requires.
			_, err := newRisk("partial", 5*time.Second).GetSignals(ctx, "tenant-a", "SUB-001")
			requireSentinel(t, err, ports.ErrContract)
			if httpadapter.Retryable(err) {
				t.Fatalf("partial: ErrContract must not be retryable")
			}
		})
		t.Run("slow", func(t *testing.T) {
			// 3s Mockoon latency: needs the 10s client timeout to succeed.
			got, err := newRisk("slow", 10*time.Second).GetSignals(ctx, "tenant-a", "SUB-001")
			if err != nil {
				t.Fatalf("slow: unexpected error: %v", err)
			}
			if !contains(got, "RECENT_SIMILAR_CLAIM") {
				t.Fatalf("slow: want RECENT_SIMILAR_CLAIM present, got %+v", got)
			}
		})
	})
}
