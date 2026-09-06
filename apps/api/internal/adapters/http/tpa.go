// TPA HTTP adapter: implements ports.ClaimsPort over JSON HTTP.
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

// TPAClient lists historical claims from an HTTP upstream.
//
// Wire shape: GET {BaseURL}/v1/claims?policy_id={policyID}&tenant={tenant}
// decoding into a bare JSON array of external.PriorClaimResponse, with
// an envelope fallback ({"tenant_id"|"tenant": ..., "claims"|"data"|
// "items": [...]}) for upstreams that wrap the list. Unknown JSON fields
// are ignored: external systems add fields freely. Every item must carry
// a tenant marker matching the request tenant.
type TPAClient struct {
	client *Client
}

// compile-time check that TPAClient satisfies the port.
var _ ports.ClaimsPort = (*TPAClient)(nil)

// NewTPAClient returns a TPAClient backed by c.
func NewTPAClient(c *Client) *TPAClient {
	return &TPAClient{client: c}
}

// ListClaims returns prior claims for policyID scoped to tenant.
//
// Failure contract: transport/timeout/5xx -> ErrUpstream; upstream 404
// -> ErrNotFound; malformed payload or validation failure ->
// ErrContract; envelope tenant differing from tenant ->
// ErrTenantMismatch. Any failure returns nil plus the error (never
// partial items).
func (a *TPAClient) ListClaims(ctx context.Context, tenant string, policyID string) ([]ports.PriorClaim, error) {
	if strings.TrimSpace(tenant) == "" {
		return nil, fmt.Errorf("%w: tpa: blank tenant", ports.ErrContract)
	}
	if strings.TrimSpace(policyID) == "" {
		return nil, fmt.Errorf("%w: tpa: blank policy id", ports.ErrContract)
	}
	q := url.Values{}
	q.Set("tenant", tenant)
	q.Set("policy_id", policyID)
	body, err := a.client.do(ctx, http.MethodGet, "/v1/claims", q)
	if err != nil {
		return nil, err
	}
	// Normal decode: external systems add fields, so never
	// DisallowUnknownFields here.
	items, envelopeTenant, err := decodePriorClaims(body)
	if err != nil {
		return nil, err
	}
	if envelopeTenant != "" && envelopeTenant != tenant {
		return nil, fmt.Errorf("%w: tpa: upstream tenant %q != expected %q", ports.ErrTenantMismatch, envelopeTenant, tenant)
	}
	rawItems, rawErr := rawArrayItems("tpa", body)
	for i := range items {
		if err := validatePriorClaim(items[i]); err != nil {
			return nil, err
		}
		// Per-item tenant + key presence on bare-array payloads (the
		// envelope path already carries the envelope check above).
		if rawErr == nil && i < len(rawItems) {
			if err := checkItemTenant("tpa", tenant, rawItems[i]); err != nil {
				return nil, err
			}
			if err := requireKeys("tpa", rawItems[i],
				[]string{"claim_id", "ClaimID"},
				[]string{"status", "Status"},
				[]string{"approved_paise", "ApprovedPaise"},
			); err != nil {
				return nil, err
			}
		}
	}
	out := make([]ports.PriorClaim, len(items))
	copy(out, items)
	return out, nil
}

// tpaEnvelope is the wrapped-list fallback shape.
type tpaEnvelope struct {
	TenantID string                        `json:"tenant_id"`
	Tenant   string                        `json:"tenant"`
	Claims   []external.PriorClaimResponse `json:"claims"`
	Data     []external.PriorClaimResponse `json:"data"`
	Items    []external.PriorClaimResponse `json:"items"`
}

// decodePriorClaims decodes body as a bare array first, then as a wrapped
// envelope. It returns the items plus any envelope tenant for the caller
// to compare against the expected tenant.
func decodePriorClaims(body []byte) ([]ports.PriorClaim, string, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || trimmed == "null" {
		return nil, "", fmt.Errorf("%w: tpa: empty body", ports.ErrContract)
	}
	var items []external.PriorClaimResponse
	if err := json.Unmarshal(body, &items); err == nil {
		// Bare array decoded. A JSON "null" unmarshals to nil with no
		// error; treat an explicit null as empty rather than success
		// with nil — but an empty array is a valid zero-claims result.
		if items == nil && strings.TrimSpace(trimmed) != "[]" {
			// Body was JSON null: fall through to envelope attempt so
			// the error path reports a contract failure, not success.
		} else {
			if items == nil {
				items = []external.PriorClaimResponse{}
			}
			return []ports.PriorClaim(items), "", nil
		}
	}
	var env tpaEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, "", fmt.Errorf("%w: tpa: decode: %v", ports.ErrContract, err)
	}
	merged := append(append(env.Claims, env.Data...), env.Items...)
	if merged == nil {
		merged = []external.PriorClaimResponse{}
	}
	tenant := env.TenantID
	if tenant == "" {
		tenant = env.Tenant
	}
	return []ports.PriorClaim(merged), tenant, nil
}

// validatePriorClaim enforces post-decode invariants, including the
// per-item tenant marker (blank markers are contract failures).
func validatePriorClaim(p ports.PriorClaim) error {
	if strings.TrimSpace(p.ClaimID) == "" {
		return fmt.Errorf("%w: tpa: blank claim id", ports.ErrContract)
	}
	if strings.TrimSpace(p.TenantID) == "" {
		return fmt.Errorf("%w: tpa: item missing tenant marker for claim %q", ports.ErrContract, p.ClaimID)
	}
	if p.ApprovedPaise < 0 {
		return fmt.Errorf("%w: tpa: negative approved paise %d for claim %q", ports.ErrContract, p.ApprovedPaise, p.ClaimID)
	}
	return nil
}
