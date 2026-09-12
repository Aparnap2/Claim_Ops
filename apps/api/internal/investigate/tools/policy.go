// Package tools implements the typed allowlisted evidence tools for
// bounded investigation (issue #54, Chunk C).
//
// Seam: every tool is a registry func(ctx, req Request) (Response, error).
// Each Request carries TenantID/ClaimID/InvestigationID/RequestID plus
// tool params; each Response carries a typed payload plus a Truncated
// flag. Constructors inject port dependencies and a Pinner; tests inject
// fakes, production injects the HTTP adapters.
//
// This file owns T2 (get_policy_context) and T7
// (get_external_policy_status), both backed by ports.PolicyPort.
//
// Logging: none. Tools never log request or response fields. Encounter
// patient_ref/diagnosis and all other upstream content must never reach
// logs; there is deliberately no logger in this package.
//
// Pinner duplication is intentional seam-duplication: every tool file in
// this package declares its own minimal Pinner interface so each file
// compiles independently of sibling files (in particular the future
// pin.go). The interfaces are identical by design and are to be unified
// at merge.
package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"claimops-api/internal/claims"
	"claimops-api/internal/invest"
	"claimops-api/internal/ports"
)

// Pinner pins one canonical upstream payload as an evidence row and
// returns its stable row ID. The pg implementation lands later; tools
// only depend on this seam.
type Pinner interface {
	Pin(ctx context.Context, tenant, claim, sourceType, sourceID string, canonicalBytes []byte) (evidenceID string, err error)
}

// ---------------------------------------------------------------------------
// T2: get_policy_context
// ---------------------------------------------------------------------------

// PolicyRequest is the T2 input: investigation envelope identity plus the
// upstream policy key.
type PolicyRequest struct {
	TenantID        string
	ClaimID         string
	InvestigationID string
	RequestID       string
	PolicyID        string
}

// validate rejects blank identity, malformed investigation IDs, and blank
// params. Every failure wraps ports.ErrContract (fail closed, permanent).
func (r PolicyRequest) validate() error {
	if err := claims.TenantID(r.TenantID).Validate(); err != nil {
		return fmt.Errorf("%w: policy: %v", ports.ErrContract, err)
	}
	if err := claims.ClaimID(r.ClaimID).Validate(); err != nil {
		return fmt.Errorf("%w: policy: %v", ports.ErrContract, err)
	}
	if err := invest.ValidateID(invest.InvestigationIDPrefix, r.InvestigationID); err != nil {
		return fmt.Errorf("%w: policy: %v", ports.ErrContract, err)
	}
	if strings.TrimSpace(r.RequestID) == "" {
		return fmt.Errorf("%w: policy: blank request id", ports.ErrContract)
	}
	if strings.TrimSpace(r.PolicyID) == "" {
		return fmt.Errorf("%w: policy: blank policy id", ports.ErrContract)
	}
	return nil
}

// PolicyResponse is the T2 output: the full upstream projection plus the
// pinned evidence row. Single-row fetch, so Truncated is always false.
type PolicyResponse struct {
	Policy      ports.PolicyInfo
	EvidenceID  string
	ContentHash string
	Truncated   bool
}

// canonicalPolicy renders the projection deterministically for pinning.
// encoding/json v1 over a struct is field-order stable; no maps, no
// floats, no timestamps minted here.
func canonicalPolicy(p ports.PolicyInfo) ([]byte, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("%w: policy: canonicalize: %v", ports.ErrContract, err)
	}
	return raw, nil
}

// checkPolicyProjection enforces the typed equivalent of the adapter's
// requireKeys plus post-decode invariants, so fake ports face the same
// gate as the HTTP adapter: blank required fields fail closed with
// ErrContract, upstream tenant drift fails the whole call with
// ErrTenantMismatch. (Raw key-presence lives in the HTTP adapter, which
// sees wire bytes; the port DTO is already decoded, so the tool checks
// the decoded shape instead.)
func checkPolicyProjection(scopeTenant string, p ports.PolicyInfo) error {
	if strings.TrimSpace(p.PolicyID) == "" {
		return fmt.Errorf("%w: policy: blank policy id in projection", ports.ErrContract)
	}
	if strings.TrimSpace(p.TenantID) == "" {
		return fmt.Errorf("%w: policy: blank tenant id in projection", ports.ErrContract)
	}
	if p.TenantID != scopeTenant {
		return fmt.Errorf("%w: policy: upstream tenant %q != scope tenant %q", ports.ErrTenantMismatch, p.TenantID, scopeTenant)
	}
	if strings.TrimSpace(p.Status) == "" {
		return fmt.Errorf("%w: policy: blank status in projection", ports.ErrContract)
	}
	if p.EffectiveFrom.IsZero() {
		return fmt.Errorf("%w: policy: zero effective_from in projection", ports.ErrContract)
	}
	if p.SumInsuredPaise < 0 {
		return fmt.Errorf("%w: policy: negative sum insured paise %d", ports.ErrContract, p.SumInsuredPaise)
	}
	if p.AvailablePaise < 0 {
		return fmt.Errorf("%w: policy: negative available paise %d", ports.ErrContract, p.AvailablePaise)
	}
	if !p.EffectiveTo.IsZero() && p.EffectiveTo.Before(p.EffectiveFrom) {
		return fmt.Errorf("%w: policy: effective_to before effective_from", ports.ErrContract)
	}
	return nil
}

// NewPolicyTool returns the T2 registry func: validate, GET via the port,
// typed requireKeys equivalent, tenant post-decode check, pin the
// evidence row, and respond with the full projection plus evidence
// identity. Port errors (NotFound/Upstream/Contract/TenantMismatch)
// propagate untouched so errors.Is classification survives.
func NewPolicyTool(port ports.PolicyPort, pin Pinner) func(ctx context.Context, req PolicyRequest) (PolicyResponse, error) {
	return func(ctx context.Context, req PolicyRequest) (PolicyResponse, error) {
		if err := req.validate(); err != nil {
			return PolicyResponse{}, err
		}
		if port == nil {
			return PolicyResponse{}, fmt.Errorf("%w: policy: nil port", ports.ErrContract)
		}
		if pin == nil {
			return PolicyResponse{}, fmt.Errorf("%w: policy: nil pinner", ports.ErrContract)
		}
		got, err := port.GetPolicy(ctx, req.TenantID, req.PolicyID)
		if err != nil {
			return PolicyResponse{}, err
		}
		if err := checkPolicyProjection(req.TenantID, got); err != nil {
			return PolicyResponse{}, err
		}
		canonical, err := canonicalPolicy(got)
		if err != nil {
			return PolicyResponse{}, err
		}
		evID, err := pin.Pin(ctx, req.TenantID, req.ClaimID, "policy", got.PolicyID, canonical)
		if err != nil {
			return PolicyResponse{}, err
		}
		if strings.TrimSpace(evID) == "" {
			return PolicyResponse{}, fmt.Errorf("%w: policy: pinner returned blank evidence id", ports.ErrContract)
		}
		sum := sha256.Sum256(canonical)
		return PolicyResponse{
			Policy:      got,
			EvidenceID:  evID,
			ContentHash: hex.EncodeToString(sum[:]),
			Truncated:   false,
		}, nil
	}
}

// ---------------------------------------------------------------------------
// T7: get_external_policy_status
// ---------------------------------------------------------------------------

// PolicyStatusRequest is the T7 input: same identity shape as T2, same
// upstream key. T7 shares the PolicyPort but never pins.
type PolicyStatusRequest struct {
	TenantID        string
	ClaimID         string
	InvestigationID string
	RequestID       string
	PolicyID        string
}

// validate mirrors PolicyRequest validation (fail closed, ErrContract).
func (r PolicyStatusRequest) validate() error {
	if err := claims.TenantID(r.TenantID).Validate(); err != nil {
		return fmt.Errorf("%w: policy_status: %v", ports.ErrContract, err)
	}
	if err := claims.ClaimID(r.ClaimID).Validate(); err != nil {
		return fmt.Errorf("%w: policy_status: %v", ports.ErrContract, err)
	}
	if err := invest.ValidateID(invest.InvestigationIDPrefix, r.InvestigationID); err != nil {
		return fmt.Errorf("%w: policy_status: %v", ports.ErrContract, err)
	}
	if strings.TrimSpace(r.RequestID) == "" {
		return fmt.Errorf("%w: policy_status: blank request id", ports.ErrContract)
	}
	if strings.TrimSpace(r.PolicyID) == "" {
		return fmt.Errorf("%w: policy_status: blank policy id", ports.ErrContract)
	}
	return nil
}

// PolicyStatus is the T7 status-only projection. Exactly these five JSON
// keys exist: policy_id, status, effective_from, effective_to,
// source_ref. Sums, waiting periods, and tenant echoes are structurally
// absent, so they cannot leak through this tool. The JSON-key-allowlist
// test in policy_test.go proves it.
type PolicyStatus struct {
	PolicyID      string    `json:"policy_id"`
	Status        string    `json:"status"`
	EffectiveFrom time.Time `json:"effective_from"`
	EffectiveTo   time.Time `json:"effective_to"`
	SourceRef     string    `json:"source_ref"`
}

// projectStatus narrows a full projection to the status subset. No sums,
// no waiting periods, no tenant echo cross this boundary.
func projectStatus(p ports.PolicyInfo) PolicyStatus {
	return PolicyStatus{
		PolicyID:      p.PolicyID,
		Status:        p.Status,
		EffectiveFrom: p.EffectiveFrom,
		EffectiveTo:   p.EffectiveTo,
		SourceRef:     p.SourceRef,
	}
}

// PolicyStatusResponse is the T7 output: the status subset only. There
// are deliberately no evidence fields: T7 MUST NOT pin.
type PolicyStatusResponse struct {
	Status    PolicyStatus
	Truncated bool
}

// NewPolicyStatusTool returns the T7 registry func: validate, GET via the
// same PolicyPort, tenant post-decode check, and respond with the
// status-only subset. It takes no Pinner by construction, so pinning is
// impossible, not merely avoided.
func NewPolicyStatusTool(port ports.PolicyPort) func(ctx context.Context, req PolicyStatusRequest) (PolicyStatusResponse, error) {
	return func(ctx context.Context, req PolicyStatusRequest) (PolicyStatusResponse, error) {
		if err := req.validate(); err != nil {
			return PolicyStatusResponse{}, err
		}
		if port == nil {
			return PolicyStatusResponse{}, fmt.Errorf("%w: policy_status: nil port", ports.ErrContract)
		}
		got, err := port.GetPolicy(ctx, req.TenantID, req.PolicyID)
		if err != nil {
			return PolicyStatusResponse{}, err
		}
		if err := checkPolicyProjection(req.TenantID, got); err != nil {
			return PolicyStatusResponse{}, err
		}
		return PolicyStatusResponse{Status: projectStatus(got), Truncated: false}, nil
	}
}
