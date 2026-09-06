// Risk HTTP adapter: implements ports.RiskPort over JSON HTTP.
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

// RiskClient fetches risk signals from an HTTP upstream.
//
// Wire shape: GET {BaseURL}/v1/signals/{subjectID}?tenant={tenant}
// decoding into a bare JSON array of external.RiskSignalResponse, with an
// envelope fallback ({"tenant_id"|"tenant": ..., "signals"|"data"|"items":
// [...]}) for upstreams that wrap the list. Unknown JSON fields are
// ignored: external systems add fields freely. Every item must carry a
// tenant marker matching the request tenant.
type RiskClient struct {
	client *Client
}

// compile-time check that RiskClient satisfies the port.
var _ ports.RiskPort = (*RiskClient)(nil)

// NewRiskClient returns a RiskClient backed by c.
func NewRiskClient(c *Client) *RiskClient {
	return &RiskClient{client: c}
}

// GetSignals returns risk signals for subjectID scoped to tenant.
//
// Failure contract: transport/timeout/5xx -> ErrUpstream; upstream 404 ->
// ErrNotFound; malformed payload or validation failure -> ErrContract;
// envelope tenant differing from tenant -> ErrTenantMismatch. Any failure
// returns nil plus the error (never partial items).
func (a *RiskClient) GetSignals(ctx context.Context, tenant string, subjectID string) ([]ports.RiskSignal, error) {
	if strings.TrimSpace(tenant) == "" {
		return nil, fmt.Errorf("%w: risk: blank tenant", ports.ErrContract)
	}
	if strings.TrimSpace(subjectID) == "" {
		return nil, fmt.Errorf("%w: risk: blank subject id", ports.ErrContract)
	}
	q := url.Values{}
	q.Set("tenant", tenant)
	body, err := a.client.do(ctx, http.MethodGet, "/v1/signals/"+url.PathEscape(subjectID), q)
	if err != nil {
		return nil, err
	}
	// Normal decode: external systems add fields, so never
	// DisallowUnknownFields here.
	items, envelopeTenant, err := decodeRiskSignals(body)
	if err != nil {
		return nil, err
	}
	if envelopeTenant != "" && envelopeTenant != tenant {
		return nil, fmt.Errorf("%w: risk: upstream tenant %q != expected %q", ports.ErrTenantMismatch, envelopeTenant, tenant)
	}
	rawItems, rawErr := rawArrayItems("risk", body)
	for i := range items {
		if err := validateRiskSignal(items[i]); err != nil {
			return nil, err
		}
		if rawErr == nil && i < len(rawItems) {
			if err := checkItemTenant("risk", tenant, rawItems[i]); err != nil {
				return nil, err
			}
			if err := requireKeys("risk", rawItems[i],
				[]string{"signal_type", "SignalType"},
				[]string{"severity", "Severity"},
			); err != nil {
				return nil, err
			}
		}
	}
	out := make([]ports.RiskSignal, len(items))
	copy(out, items)
	return out, nil
}

// riskEnvelope is the wrapped-list fallback shape.
type riskEnvelope struct {
	TenantID string                        `json:"tenant_id"`
	Tenant   string                        `json:"tenant"`
	Signals  []external.RiskSignalResponse `json:"signals"`
	Data     []external.RiskSignalResponse `json:"data"`
	Items    []external.RiskSignalResponse `json:"items"`
}

// decodeRiskSignals decodes body as a bare array first, then as a wrapped
// envelope. It returns the items plus any envelope tenant for the caller
// to compare against the expected tenant.
func decodeRiskSignals(body []byte) ([]ports.RiskSignal, string, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || trimmed == "null" {
		return nil, "", fmt.Errorf("%w: risk: empty body", ports.ErrContract)
	}
	var items []external.RiskSignalResponse
	if err := json.Unmarshal(body, &items); err == nil {
		if items == nil && strings.TrimSpace(trimmed) != "[]" {
			// JSON null decoded without error; fall through to the
			// envelope attempt so the failure reports a contract error.
		} else {
			if items == nil {
				items = []external.RiskSignalResponse{}
			}
			return []ports.RiskSignal(items), "", nil
		}
	}
	var env riskEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, "", fmt.Errorf("%w: risk: decode: %v", ports.ErrContract, err)
	}
	merged := append(append(env.Signals, env.Data...), env.Items...)
	if merged == nil {
		merged = []external.RiskSignalResponse{}
	}
	tenant := env.TenantID
	if tenant == "" {
		tenant = env.Tenant
	}
	return []ports.RiskSignal(merged), tenant, nil
}

// validateRiskSignal enforces post-decode invariants, including the
// per-item tenant marker.
func validateRiskSignal(s ports.RiskSignal) error {
	if strings.TrimSpace(s.SignalType) == "" {
		return fmt.Errorf("%w: risk: blank signal type", ports.ErrContract)
	}
	if strings.TrimSpace(s.TenantID) == "" {
		return fmt.Errorf("%w: risk: item missing tenant marker for signal %q", ports.ErrContract, s.SignalType)
	}
	if strings.TrimSpace(s.Severity) == "" {
		return fmt.Errorf("%w: risk: blank severity", ports.ErrContract)
	}
	return nil
}
