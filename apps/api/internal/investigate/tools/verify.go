// T6 (get_verification_findings) envelope replay over already-loaded
// findings. There is deliberately no reader, no store, and no database
// access in this file.
//
// Why replay-only (the downgrade): T6 was scoped as a read of the stored
// deterministic envelope, but the stored-envelope read path (verify /
// verifywrap rows in PG) is not wired to this package yet. Recomputing
// verification inside the tool would double-judge the claim: the
// deterministic verdict is authoritative precisely because exactly one
// stage computes it, and the executor contract forbids tools from
// invoking verify. So the caller loads findings through the deterministic
// Build path and hands them to NewVerifyTool; the ToolFunc validates the
// envelope, validates the findings fail-closed, and replays them
// verbatim — the typed response carries the findings in emission order,
// the generic envelope cites their sorted-unique evidence IDs plus the
// finding count. No DB, no recompute, no model.
//
// Logging: none (see policy.go). Finding messages are human display
// only and nothing here is logged regardless.
package tools

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"claimops-api/internal/claims"
	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/ports"
)

// Finding is the local T6 typed finding: the deterministic code, its
// severity label, the human display message, and the cited
// stored-evidence IDs. It mirrors invest.RuleFinding minus the derived
// AffectedFields projection (derived, never asserted, so never replayed).
type Finding struct {
	Code        string
	Severity    string
	Message     string
	EvidenceIDs []string
}

// validate rejects blank code/severity/message and evidence pins that are
// not sorted, unique, and non-blank. Every failure wraps
// ports.ErrContract (fail closed, permanent).
func (f Finding) validate() error {
	if strings.TrimSpace(f.Code) == "" {
		return fmt.Errorf("%w: verify: finding has blank code", ports.ErrContract)
	}
	if strings.TrimSpace(f.Severity) == "" {
		return fmt.Errorf("%w: verify: finding %q has blank severity", ports.ErrContract, f.Code)
	}
	if strings.TrimSpace(f.Message) == "" {
		return fmt.Errorf("%w: verify: finding %q has blank message", ports.ErrContract, f.Code)
	}
	for i := range f.EvidenceIDs {
		if strings.TrimSpace(f.EvidenceIDs[i]) == "" {
			return fmt.Errorf("%w: verify: finding %q has blank evidence id at index %d", ports.ErrContract, f.Code, i)
		}
	}
	if !slices.IsSorted(f.EvidenceIDs) {
		return fmt.Errorf("%w: verify: finding %q evidence ids not sorted", ports.ErrContract, f.Code)
	}
	for i := 1; i < len(f.EvidenceIDs); i++ {
		if f.EvidenceIDs[i] == f.EvidenceIDs[i-1] {
			return fmt.Errorf("%w: verify: finding %q duplicates evidence id %q", ports.ErrContract, f.Code, f.EvidenceIDs[i])
		}
	}
	return nil
}

// VerifyResponse is the T6 typed output: the loaded findings verbatim, in
// deterministic emission order (never re-sorted here). An empty replay is
// valid: it states that verification raised nothing.
type VerifyResponse struct {
	Findings []Finding
}

// NewVerifyResponse validates every finding and deep-copies the list so
// later caller mutations cannot rewrite the replay.
func NewVerifyResponse(findings []Finding) (VerifyResponse, error) {
	for i := range findings {
		if err := findings[i].validate(); err != nil {
			return VerifyResponse{}, err
		}
	}
	out := make([]Finding, 0, len(findings))
	for i := range findings {
		f := findings[i]
		f.EvidenceIDs = append([]string(nil), f.EvidenceIDs...)
		out = append(out, f)
	}
	return VerifyResponse{Findings: out}, nil
}

// evidenceUnion returns the sorted-unique evidence IDs cited across all
// findings: the envelope-citable provenance of the replay.
func (r VerifyResponse) evidenceUnion() []string {
	var ids []string
	for i := range r.Findings {
		ids = append(ids, r.Findings[i].EvidenceIDs...)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// ToResponse converts to the generic envelope via the tool.go
// constructors only: the replayed evidence IDs plus the finding count.
// More than MaxResponseIDs distinct evidence IDs fails closed — the
// envelope cannot cite what it cannot carry.
func (r VerifyResponse) ToResponse() (investigate.Response, error) {
	ids := r.evidenceUnion()
	if len(ids) > investigate.MaxResponseIDs {
		return investigate.Response{}, fmt.Errorf("%w: verify: replay cites %d evidence ids, cap %d", ports.ErrContract, len(ids), investigate.MaxResponseIDs)
	}
	resp, err := investigate.NewGetVerificationFindingsResponse(ids, len(r.Findings))
	if err != nil {
		return investigate.Response{}, err
	}
	return resp.ToResponse(), nil
}

// NewVerifyTool returns the T6 ToolFunc: re-validate the generic envelope
// (tool echo + identity + knob ownership via Request.Validate), validate
// the investigation echo, and replay the already-loaded findings
// verbatim. findings are snapshot-copied at construction so later caller
// mutations cannot rewrite the replay; every call re-validates, so a bad
// snapshot fails every call identically (deterministic, permanent).
func NewVerifyTool(findings []Finding) investigate.ToolFunc {
	snap := append([]Finding(nil), findings...)
	for i := range snap {
		snap[i].EvidenceIDs = append([]string(nil), snap[i].EvidenceIDs...)
	}
	return func(_ context.Context, req investigate.Request) (investigate.Response, error) {
		if req.Tool != invest.ToolGetVerificationFindings {
			return investigate.Response{}, fmt.Errorf("%w: verify: dispatched %q, want get_verification_findings", ports.ErrContract, string(req.Tool))
		}
		if err := req.Validate(); err != nil {
			return investigate.Response{}, err
		}
		if err := claims.TenantID(req.TenantID).Validate(); err != nil {
			return investigate.Response{}, fmt.Errorf("%w: verify: %v", ports.ErrContract, err)
		}
		if err := claims.ClaimID(req.ClaimID).Validate(); err != nil {
			return investigate.Response{}, fmt.Errorf("%w: verify: %v", ports.ErrContract, err)
		}
		if err := invest.ValidateID(invest.InvestigationIDPrefix, req.InvestigationID); err != nil {
			return investigate.Response{}, fmt.Errorf("%w: verify: %v", ports.ErrContract, err)
		}
		if strings.TrimSpace(req.RequestID) == "" {
			return investigate.Response{}, fmt.Errorf("%w: verify: blank request id", ports.ErrContract)
		}
		typed, err := NewVerifyResponse(snap)
		if err != nil {
			return investigate.Response{}, err
		}
		return typed.ToResponse()
	}
}
