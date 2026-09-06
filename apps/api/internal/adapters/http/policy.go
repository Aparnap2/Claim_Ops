// Policy HTTP adapter: implements ports.PolicyPort over JSON HTTP.
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

// PolicyClient fetches policy details from an HTTP upstream.
//
// Wire shape: GET {BaseURL}/v1/policies/{policyID}?tenant={tenant}
// decoding into external.PolicyResponse (alias of ports.PolicyInfo).
// Unknown JSON fields are ignored: external systems add fields freely.
// Required keys are presence-checked first so truncated payloads map to
// ErrContract instead of silently zero-filled structs.
type PolicyClient struct {
	client *Client
}

// compile-time check that PolicyClient satisfies the port.
var _ ports.PolicyPort = (*PolicyClient)(nil)

// NewPolicyClient returns a PolicyClient backed by c.
func NewPolicyClient(c *Client) *PolicyClient {
	return &PolicyClient{client: c}
}

// GetPolicy returns the policy for policyID scoped to tenant.
//
// Failure contract: transport/timeout/5xx -> ErrUpstream; missing record
// (upstream 404) -> ErrNotFound; malformed payload or validation failure
// -> ErrContract; upstream tenant differing from tenant ->
// ErrTenantMismatch. Any failure returns the zero PolicyInfo (never a
// partial struct).
func (a *PolicyClient) GetPolicy(ctx context.Context, tenant string, policyID string) (ports.PolicyInfo, error) {
	if strings.TrimSpace(tenant) == "" {
		return ports.PolicyInfo{}, fmt.Errorf("%w: policy: blank tenant", ports.ErrContract)
	}
	if strings.TrimSpace(policyID) == "" {
		return ports.PolicyInfo{}, fmt.Errorf("%w: policy: blank policy id", ports.ErrContract)
	}
	q := url.Values{}
	q.Set("tenant", tenant)
	body, err := a.client.do(ctx, http.MethodGet, "/v1/policies/"+url.PathEscape(policyID), q)
	if err != nil {
		return ports.PolicyInfo{}, err
	}
	raw, err := rawObject("policy", body)
	if err != nil {
		return ports.PolicyInfo{}, err
	}
	if err := requireKeys("policy", raw,
		[]string{"policy_id", "PolicyID"},
		[]string{"tenant_id", "TenantID", "tenant"},
		[]string{"status", "Status"},
		[]string{"effective_from", "EffectiveFrom"},
		[]string{"sum_insured_paise", "SumInsuredPaise"},
	); err != nil {
		return ports.PolicyInfo{}, err
	}
	// Normal decode: external systems add fields, so never
	// DisallowUnknownFields here.
	var resp external.PolicyResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return ports.PolicyInfo{}, fmt.Errorf("%w: policy: decode: %v", ports.ErrContract, err)
	}
	if err := validatePolicy(tenant, resp); err != nil {
		return ports.PolicyInfo{}, err
	}
	return ports.PolicyInfo(resp), nil
}

// validatePolicy enforces post-decode invariants, mapping violations to
// ErrTenantMismatch (tenant drift) or ErrContract (shape violations).
func validatePolicy(expectedTenant string, p ports.PolicyInfo) error {
	if strings.TrimSpace(p.TenantID) == "" {
		return fmt.Errorf("%w: policy: blank tenant id", ports.ErrContract)
	}
	if p.TenantID != expectedTenant {
		return fmt.Errorf("%w: policy: upstream tenant %q != expected %q", ports.ErrTenantMismatch, p.TenantID, expectedTenant)
	}
	if strings.TrimSpace(p.PolicyID) == "" {
		return fmt.Errorf("%w: policy: blank policy id", ports.ErrContract)
	}
	if p.SumInsuredPaise < 0 {
		return fmt.Errorf("%w: policy: negative sum insured paise %d", ports.ErrContract, p.SumInsuredPaise)
	}
	if p.AvailablePaise < 0 {
		return fmt.Errorf("%w: policy: negative available paise %d", ports.ErrContract, p.AvailablePaise)
	}
	if !p.EffectiveFrom.IsZero() && !p.EffectiveTo.IsZero() && p.EffectiveTo.Before(p.EffectiveFrom) {
		return fmt.Errorf("%w: policy: effectiveTo %v before effectiveFrom %v", ports.ErrContract, p.EffectiveTo, p.EffectiveFrom)
	}
	return nil
}
