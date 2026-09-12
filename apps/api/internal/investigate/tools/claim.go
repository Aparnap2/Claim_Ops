// T1 (get_claim) backed by investigate.ClaimReader.
//
// Logging: none (see policy.go). The claim header carries no PII beyond
// upstream IDs, and nothing here is logged regardless.
//
// Envelope: this tool implements investigate.ToolFunc directly — it takes
// the generic investigate.Request, re-validates it standalone (the
// executor validates too, but ToolFuncs must hold their own contract when
// called directly), loads via the reader, and returns the generic
// investigate.Response built ONLY through the tool.go New* constructors
// (never hand-built: NewGetClaimResponse + ToResponse).
//
// Projection: the reader returns investigate.ClaimHeader (claim, status,
// reference, amount paise, version). There are deliberately no
// patient/hospital fields anywhere on this path — the PG query projects
// exactly the safe columns, so such values cannot cross this boundary
// even if the schema later gains them.
package tools

import (
	"context"
	"fmt"
	"strings"

	"claimops-api/internal/claims"
	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/ports"
)

// ClaimRequest is the T1 input: investigation envelope identity only. The
// ToolFunc validates the generic envelope instead; this shape documents
// the identity the tool needs.
type ClaimRequest struct {
	TenantID        string
	ClaimID         string
	InvestigationID string
	RequestID       string
}

// validate rejects blank identity and malformed investigation IDs. Every
// failure wraps ports.ErrContract (fail closed, permanent).
func (r ClaimRequest) validate() error {
	if err := claims.TenantID(r.TenantID).Validate(); err != nil {
		return fmt.Errorf("%w: claim: %v", ports.ErrContract, err)
	}
	if err := claims.ClaimID(r.ClaimID).Validate(); err != nil {
		return fmt.Errorf("%w: claim: %v", ports.ErrContract, err)
	}
	if err := invest.ValidateID(invest.InvestigationIDPrefix, r.InvestigationID); err != nil {
		return fmt.Errorf("%w: claim: %v", ports.ErrContract, err)
	}
	if strings.TrimSpace(r.RequestID) == "" {
		return fmt.Errorf("%w: claim: blank request id", ports.ErrContract)
	}
	return nil
}

// NewClaimTool returns the T1 ToolFunc: re-validate the generic envelope
// (tool echo + identity + knob ownership via Request.Validate), validate
// the investigation echo, LoadClaim via the reader, and respond with the
// single-row claim header. Reader errors (NotFound/TenantMismatch/
// Upstream) propagate untouched so errors.Is classification survives at
// both the ports and investigate layers.
func NewClaimTool(reader investigate.ClaimReader) investigate.ToolFunc {
	return func(ctx context.Context, req investigate.Request) (investigate.Response, error) {
		if req.Tool != invest.ToolGetClaim {
			return investigate.Response{}, fmt.Errorf("%w: claim: dispatched %q, want get_claim", ports.ErrContract, string(req.Tool))
		}
		if err := req.Validate(); err != nil {
			return investigate.Response{}, err
		}
		ident := ClaimRequest{
			TenantID:        req.TenantID,
			ClaimID:         req.ClaimID,
			InvestigationID: req.InvestigationID,
			RequestID:       req.RequestID,
		}
		if err := ident.validate(); err != nil {
			return investigate.Response{}, err
		}
		if reader == nil {
			return investigate.Response{}, fmt.Errorf("%w: claim: nil reader", ports.ErrContract)
		}
		header, err := reader.LoadClaim(ctx, req.TenantID, req.ClaimID)
		if err != nil {
			return investigate.Response{}, err
		}
		resp, err := investigate.NewGetClaimResponse(header)
		if err != nil {
			return investigate.Response{}, err
		}
		return resp.ToResponse(), nil
	}
}
