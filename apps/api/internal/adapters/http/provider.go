// Provider HTTP adapter: implements ports.ProviderPort over JSON HTTP.
package httpadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"claimops-api/internal/contracts/external"
	"claimops-api/internal/ports"
)

// ProviderClient fetches provider encounters from an HTTP upstream.
//
// Wire shape: GET {BaseURL}/v1/encounters/{encounterID}?tenant={tenant}
// decoding into external.EncounterResponse (alias of ports.Encounter).
// Unknown JSON fields are ignored: external systems add fields freely.
// Required keys are presence-checked first so truncated payloads map to
// ErrContract. The Encounter DTO carries no tenant field, so tenant
// consistency is enforced opportunistically: when the payload carries an
// envelope-level tenant (tenant_id/tenant), a mismatch maps to
// ErrTenantMismatch.
type ProviderClient struct {
	client *Client
}

// compile-time check that ProviderClient satisfies the port.
var _ ports.ProviderPort = (*ProviderClient)(nil)

// NewProviderClient returns a ProviderClient backed by c.
func NewProviderClient(c *Client) *ProviderClient {
	return &ProviderClient{client: c}
}

// GetEncounter returns the encounter for encounterID scoped to tenant.
//
// Failure contract: transport/timeout/5xx -> ErrUpstream; missing record
// (upstream 404) -> ErrNotFound; malformed payload or validation failure
// -> ErrContract; envelope tenant differing from tenant ->
// ErrTenantMismatch. Any failure returns the zero Encounter (never a
// partial struct).
func (a *ProviderClient) GetEncounter(ctx context.Context, tenant string, encounterID string) (ports.Encounter, error) {
	if strings.TrimSpace(tenant) == "" {
		return ports.Encounter{}, fmt.Errorf("%w: provider: blank tenant", ports.ErrContract)
	}
	if strings.TrimSpace(encounterID) == "" {
		return ports.Encounter{}, fmt.Errorf("%w: provider: blank encounter id", ports.ErrContract)
	}
	q := url.Values{}
	q.Set("tenant", tenant)
	body, err := a.client.do(ctx, http.MethodGet, "/v1/encounters/"+url.PathEscape(encounterID), q)
	if err != nil {
		return ports.Encounter{}, err
	}
	raw, err := rawObject("provider", body)
	if err != nil {
		return ports.Encounter{}, err
	}
	if err := requireKeys("provider", raw,
		[]string{"encounter_id", "EncounterID"},
		[]string{"patient_ref", "PatientRef"},
		[]string{"hospital_id", "HospitalID"},
		[]string{"admission_at", "AdmissionAt"},
	); err != nil {
		return ports.Encounter{}, err
	}
	// Normal decode: external systems add fields, so never
	// DisallowUnknownFields here.
	var resp external.EncounterResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return ports.Encounter{}, fmt.Errorf("%w: provider: decode: %v", ports.ErrContract, err)
	}
	if envTenant := peekTenant(body); envTenant != "" && envTenant != tenant {
		return ports.Encounter{}, fmt.Errorf("%w: provider: upstream tenant %q != expected %q", ports.ErrTenantMismatch, envTenant, tenant)
	}
	if err := validateEncounter(resp); err != nil {
		return ports.Encounter{}, err
	}
	return ports.Encounter(resp), nil
}

// validateEncounter enforces post-decode invariants.
func validateEncounter(e ports.Encounter) error {
	if strings.TrimSpace(e.EncounterID) == "" {
		return fmt.Errorf("%w: provider: blank encounter id", ports.ErrContract)
	}
	if strings.TrimSpace(e.PatientRef) == "" {
		return fmt.Errorf("%w: provider: blank patient ref", ports.ErrContract)
	}
	if strings.TrimSpace(e.HospitalID) == "" {
		return fmt.Errorf("%w: provider: blank hospital id", ports.ErrContract)
	}
	if !e.AdmissionAt.IsZero() && !e.DischargeAt.IsZero() && e.DischargeAt.Before(e.AdmissionAt) {
		return fmt.Errorf("%w: provider: dischargeAt %v before admissionAt %v", ports.ErrContract, e.DischargeAt, e.AdmissionAt)
	}
	return nil
}

// peekTenant extracts an envelope-level tenant from raw JSON when the
// upstream includes one alongside the DTO fields. It looks for common
// key spellings and returns "" when absent, so DTOs without a tenant
// field skip the check instead of failing.
func peekTenant(body []byte) string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return ""
	}
	for _, key := range []string{"tenant_id", "tenantId", "tenantID", "tenant"} {
		if v, ok := raw[key]; ok {
			var s string
			if err := json.Unmarshal(v, &s); err == nil && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}
