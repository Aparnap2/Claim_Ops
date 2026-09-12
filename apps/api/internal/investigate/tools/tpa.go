// T8 (get_tpa_case) backed by ports.ClaimsPort.
//
// Logging: none (see policy.go). Prior-claim rows carry no PII beyond
// upstream IDs, and nothing here is logged regardless.
//
// Pinner is re-declared locally per file on purpose: each tool file
// compiles independently of sibling files (in particular the future
// pin.go). The declarations are identical by design and are to be
// unified at merge.
package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"claimops-api/internal/claims"
	"claimops-api/internal/invest"
	"claimops-api/internal/ports"
)

// MaxTPARows caps T8 list output. Longer upstream lists are truncated
// with Truncated set; the tool never fails closed on count.
const MaxTPARows = 20

// TPARequest is the T8 input: investigation envelope identity plus the
// upstream policy key whose history is read.
type TPARequest struct {
	TenantID        string
	ClaimID         string
	InvestigationID string
	RequestID       string
	PolicyID        string
}

// validate rejects blank identity, malformed investigation IDs, and blank
// params. Every failure wraps ports.ErrContract (fail closed, permanent).
func (r TPARequest) validate() error {
	if err := claims.TenantID(r.TenantID).Validate(); err != nil {
		return fmt.Errorf("%w: tpa: %v", ports.ErrContract, err)
	}
	if err := claims.ClaimID(r.ClaimID).Validate(); err != nil {
		return fmt.Errorf("%w: tpa: %v", ports.ErrContract, err)
	}
	if err := invest.ValidateID(invest.InvestigationIDPrefix, r.InvestigationID); err != nil {
		return fmt.Errorf("%w: tpa: %v", ports.ErrContract, err)
	}
	if strings.TrimSpace(r.RequestID) == "" {
		return fmt.Errorf("%w: tpa: blank request id", ports.ErrContract)
	}
	if strings.TrimSpace(r.PolicyID) == "" {
		return fmt.Errorf("%w: tpa: blank policy id", ports.ErrContract)
	}
	return nil
}

// TPAResponse is the T8 output: prior-claim projections in verbatim
// upstream order plus the pinned evidence row. Truncated reports whether
// the upstream list exceeded MaxTPARows.
type TPAResponse struct {
	Claims      []ports.PriorClaim
	EvidenceID  string
	ContentHash string
	Truncated   bool
}

// checkPriorClaim enforces per-item invariants: non-blank claim id,
// present tenant marker, and marker equality with the scope tenant.
// Drift fails the WHOLE call with ErrTenantMismatch (never partial
// items); blank markers or ids fail closed with ErrContract.
func checkPriorClaim(scopeTenant string, item ports.PriorClaim) error {
	if strings.TrimSpace(item.ClaimID) == "" {
		return fmt.Errorf("%w: tpa: blank claim id in item", ports.ErrContract)
	}
	if strings.TrimSpace(item.TenantID) == "" {
		return fmt.Errorf("%w: tpa: item missing tenant marker for claim %q", ports.ErrContract, item.ClaimID)
	}
	if item.TenantID != scopeTenant {
		return fmt.Errorf("%w: tpa: upstream tenant %q != scope tenant %q", ports.ErrTenantMismatch, item.TenantID, scopeTenant)
	}
	if item.ApprovedPaise < 0 {
		return fmt.Errorf("%w: tpa: negative approved paise %d for claim %q", ports.ErrContract, item.ApprovedPaise, item.ClaimID)
	}
	return nil
}

// NewTPATool returns the T8 registry func: validate, LIST via the port,
// per-item tenant re-check (whole call fails on drift), truncate with
// flag at MaxTPARows (never fail-closed on count, order verbatim), pin
// the FULL upstream payload as the evidence row, and respond. Port
// errors propagate untouched so errors.Is classification survives.
//
// The pinned bytes are the full pre-truncation list: the evidence row
// records what the upstream said, while Truncated records that the
// response carries a prefix of it.
func NewTPATool(port ports.ClaimsPort, pin Pinner) func(ctx context.Context, req TPARequest) (TPAResponse, error) {
	return func(ctx context.Context, req TPARequest) (TPAResponse, error) {
		if err := req.validate(); err != nil {
			return TPAResponse{}, err
		}
		if port == nil {
			return TPAResponse{}, fmt.Errorf("%w: tpa: nil port", ports.ErrContract)
		}
		if pin == nil {
			return TPAResponse{}, fmt.Errorf("%w: tpa: nil pinner", ports.ErrContract)
		}
		items, err := port.ListClaims(ctx, req.TenantID, req.PolicyID)
		if err != nil {
			return TPAResponse{}, err
		}
		for i := range items {
			if err := checkPriorClaim(req.TenantID, items[i]); err != nil {
				return TPAResponse{}, err
			}
		}
		canonical, err := json.Marshal(items)
		if err != nil {
			return TPAResponse{}, fmt.Errorf("%w: tpa: canonicalize: %v", ports.ErrContract, err)
		}
		if canonical == nil {
			canonical = []byte("[]")
		}
		evID, err := pin.Pin(ctx, req.TenantID, req.ClaimID, "tpa", req.PolicyID, canonical)
		if err != nil {
			return TPAResponse{}, err
		}
		if strings.TrimSpace(evID) == "" {
			return TPAResponse{}, fmt.Errorf("%w: tpa: pinner returned blank evidence id", ports.ErrContract)
		}
		out := items
		truncated := false
		if len(out) > MaxTPARows {
			out = out[:MaxTPARows]
			truncated = true
		}
		if out == nil {
			out = []ports.PriorClaim{}
		}
		sum := sha256.Sum256(canonical)
		return TPAResponse{
			Claims:      out,
			EvidenceID:  evID,
			ContentHash: hex.EncodeToString(sum[:]),
			Truncated:   truncated,
		}, nil
	}
}
