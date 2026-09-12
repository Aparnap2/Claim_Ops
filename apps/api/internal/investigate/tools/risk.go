// T10 (get_risk_signals) backed by ports.RiskPort.
//
// Logging: none (see policy.go). Signal content must never reach logs;
// there is deliberately no logger in this package.
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

// MaxRiskRows caps T10 list output. Longer upstream lists are truncated
// with Truncated set; the tool never fails closed on count and preserves
// verbatim upstream order in the prefix it returns.
const MaxRiskRows = 20

// RiskRequest is the T10 input: investigation envelope identity plus the
// upstream subject key whose signals are read.
type RiskRequest struct {
	TenantID        string
	ClaimID         string
	InvestigationID string
	RequestID       string
	SubjectID       string
}

// validate rejects blank identity, malformed investigation IDs, and blank
// params. Every failure wraps ports.ErrContract (fail closed, permanent).
func (r RiskRequest) validate() error {
	if err := claims.TenantID(r.TenantID).Validate(); err != nil {
		return fmt.Errorf("%w: risk: %v", ports.ErrContract, err)
	}
	if err := claims.ClaimID(r.ClaimID).Validate(); err != nil {
		return fmt.Errorf("%w: risk: %v", ports.ErrContract, err)
	}
	if err := invest.ValidateID(invest.InvestigationIDPrefix, r.InvestigationID); err != nil {
		return fmt.Errorf("%w: risk: %v", ports.ErrContract, err)
	}
	if strings.TrimSpace(r.RequestID) == "" {
		return fmt.Errorf("%w: risk: blank request id", ports.ErrContract)
	}
	if strings.TrimSpace(r.SubjectID) == "" {
		return fmt.Errorf("%w: risk: blank subject id", ports.ErrContract)
	}
	return nil
}

// RiskSignal is the T10 output projection: the upstream signal carried
// verbatim, minus the tenant marker (enforced at the gate, never echoed
// back out). Severity is a plain string by design: it is an upstream
// label, never an invest.Severity judgment, and must not be confused
// with one.
type RiskSignal struct {
	SignalType string
	Severity   string
	SourceRef  string
}

// projectRisk narrows one upstream signal to the output projection. The
// tenant marker is consumed by the gate and does not cross this
// boundary.
func projectRisk(s ports.RiskSignal) RiskSignal {
	return RiskSignal{
		SignalType: s.SignalType,
		Severity:   s.Severity,
		SourceRef:  s.SourceRef,
	}
}

// RiskResponse is the T10 output: signal projections in verbatim
// upstream order plus the pinned evidence row. Truncated reports whether
// the upstream list exceeded MaxRiskRows.
type RiskResponse struct {
	Signals     []RiskSignal
	EvidenceID  string
	ContentHash string
	Truncated   bool
}

// checkRiskSignal enforces per-item invariants: non-blank signal type
// and severity, a present tenant marker, and marker equality with the
// scope tenant. Drift fails the WHOLE call with ErrTenantMismatch
// (never partial items); blank markers, types, or severities fail
// closed with ErrContract.
func checkRiskSignal(scopeTenant string, item ports.RiskSignal) error {
	if strings.TrimSpace(item.SignalType) == "" {
		return fmt.Errorf("%w: risk: blank signal type in item", ports.ErrContract)
	}
	if strings.TrimSpace(item.TenantID) == "" {
		return fmt.Errorf("%w: risk: item missing tenant marker for signal %q", ports.ErrContract, item.SignalType)
	}
	if item.TenantID != scopeTenant {
		return fmt.Errorf("%w: risk: upstream tenant %q != scope tenant %q", ports.ErrTenantMismatch, item.TenantID, scopeTenant)
	}
	if strings.TrimSpace(item.Severity) == "" {
		return fmt.Errorf("%w: risk: blank severity for signal %q", ports.ErrContract, item.SignalType)
	}
	return nil
}

// NewRiskTool returns the T10 registry func: validate, LIST via the
// port, per-item tenant re-check (whole call fails on drift), truncate
// with flag at MaxRiskRows (never fail-closed on count, order
// verbatim), pin the FULL upstream payload as the evidence row, and
// respond. Port errors propagate untouched so errors.Is classification
// survives.
//
// The pinned bytes are the full pre-truncation list: the evidence row
// records what the upstream said, while Truncated records that the
// response carries a prefix of it.
func NewRiskTool(port ports.RiskPort, pin Pinner) func(ctx context.Context, req RiskRequest) (RiskResponse, error) {
	return func(ctx context.Context, req RiskRequest) (RiskResponse, error) {
		if err := req.validate(); err != nil {
			return RiskResponse{}, err
		}
		if port == nil {
			return RiskResponse{}, fmt.Errorf("%w: risk: nil port", ports.ErrContract)
		}
		if pin == nil {
			return RiskResponse{}, fmt.Errorf("%w: risk: nil pinner", ports.ErrContract)
		}
		items, err := port.GetSignals(ctx, req.TenantID, req.SubjectID)
		if err != nil {
			return RiskResponse{}, err
		}
		for i := range items {
			if err := checkRiskSignal(req.TenantID, items[i]); err != nil {
				return RiskResponse{}, err
			}
		}
		canonical, err := json.Marshal(items)
		if err != nil {
			return RiskResponse{}, fmt.Errorf("%w: risk: canonicalize: %v", ports.ErrContract, err)
		}
		if canonical == nil {
			canonical = []byte("[]")
		}
		evID, err := pin.Pin(ctx, req.TenantID, req.ClaimID, "risk", req.SubjectID, canonical)
		if err != nil {
			return RiskResponse{}, err
		}
		if strings.TrimSpace(evID) == "" {
			return RiskResponse{}, fmt.Errorf("%w: risk: pinner returned blank evidence id", ports.ErrContract)
		}
		out := make([]RiskSignal, 0, len(items))
		for i := range items {
			out = append(out, projectRisk(items[i]))
		}
		truncated := false
		if len(out) > MaxRiskRows {
			out = out[:MaxRiskRows]
			truncated = true
		}
		if out == nil {
			out = []RiskSignal{}
		}
		sum := sha256.Sum256(canonical)
		return RiskResponse{
			Signals:     out,
			EvidenceID:  evID,
			ContentHash: hex.EncodeToString(sum[:]),
			Truncated:   truncated,
		}, nil
	}
}
