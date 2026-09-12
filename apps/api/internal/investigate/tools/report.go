// T11 (create_investigation_report): the sole writer. Validates one
// canonical report (hypotheses via invest.ValidateHypothesis, findings
// via invest.ValidateFinding, recommendations via
// invest.ValidateRecommendation, missing evidence shape plus
// additive-only against the envelope baseline, and evidence resolution
// against the Resolver) and stores it write-once keyed by
// InvestigationID. Repeat inserts for the same ID replay the stored
// report (replayed=true) instead of writing a second row.
//
// Row identity: ReportRow.ID IS the InvestigationID (mirroring the
// investigation_reports table, whose PRIMARY KEY on id enforces
// UNIQUE(investigation_id) with no second column that could diverge).
//
// Store pg implementation: pg_store.go (PGReportStore, live-tested).
// This file depends only on the Store/Resolver seams below.
//
// Logging: none (see policy.go). Report content must never reach logs;
// there is deliberately no logger in this package.
package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"claimops-api/internal/claims"
	"claimops-api/internal/invest"
	"claimops-api/internal/ports"
)

// MaxReportBytes caps the T11 canonical report payload (1 MiB),
// mirroring investigate.MaxReportBytes without importing the parent
// package (tools is consumed by the executor, never the reverse).
const MaxReportBytes = 1 << 20

// externalSources is the closed external-evidence vocabulary for
// MissingExternal keys, mirroring the invest derivation set.
var externalSources = []string{"policy", "tpa", "provider", "risk"}

// Store persists one canonical report row. Insert is write-once keyed
// by row.ID (= InvestigationID): the first insert stores the row and
// returns replayed=false; a repeat insert for the same ID stores
// nothing and returns replayed=true.
type Store interface {
	Insert(ctx context.Context, row ReportRow) (replayed bool, err error)
}

// Resolver maps a cited evidence ID to its owning tenant/claim so the
// tool can prove every citation traces to this investigation's scope.
// Unknown IDs return an error (mapped to ErrContract by the tool);
// tenant drift maps to ErrTenantMismatch and claim drift to
// ErrContract.
type Resolver func(ctx context.Context, evidenceID string) (tenant, claim string, err error)

// ReportRequest is the T11 input: investigation envelope identity plus
// the report body. Exception is the united envelope the report
// resolves; MissingEvidence is the investigator's EXTENDED open
// list, which must still contain every baseline item (additive only).
type ReportRequest struct {
	TenantID        string
	ClaimID         string
	InvestigationID string
	RequestID       string
	ExceptionID     string
	Exception       invest.UnresolvedException
	Hypotheses      []invest.Hypothesis
	Findings        []invest.Finding
	Recommendations []invest.Recommendation
	MissingEvidence []invest.MissingItem
}

// ReportRow is the T11 stored row. ID is always the InvestigationID.
type ReportRow struct {
	ID              string
	TenantID        string
	ClaimID         string
	ExceptionID     string
	InvestigationID string
	Hypotheses      []invest.Hypothesis
	Findings        []invest.Finding
	Recommendations []invest.Recommendation
	MissingEvidence []invest.MissingItem
	ContentHash     string
}

// ReportResponse is the T11 receipt: the stored report identity, its
// content hash, and whether this call replayed an existing row.
type ReportResponse struct {
	ReportID    string
	ContentHash string
	Replayed    bool
	Version     int
}

// canonicalReport is the pinned shape: fixed field order (struct
// marshal), sorted copies everywhere, no maps, no floats, no
// timestamps minted here.
type canonicalReport struct {
	TenantID        string                  `json:"tenant_id"`
	ClaimID         string                  `json:"claim_id"`
	ExceptionID     string                  `json:"exception_id"`
	InvestigationID string                  `json:"investigation_id"`
	Hypotheses      []invest.Hypothesis     `json:"hypotheses"`
	Findings        []invest.Finding        `json:"findings"`
	Recommendations []invest.Recommendation `json:"recommendations"`
	MissingEvidence []invest.MissingItem    `json:"missing_evidence"`
}

// checkMissingItem enforces the missing-evidence shape: closed kind,
// non-blank key, closed external key for external kind, non-blank
// detail (display-only context is required, mirroring invest.Validate).
func checkMissingItem(m invest.MissingItem) error {
	switch m.Kind {
	case invest.MissingRequiredDocument, invest.MissingField, invest.MissingExternal:
	default:
		return fmt.Errorf("%w: report: missing_evidence has unknown kind %q", ports.ErrContract, string(m.Kind))
	}
	if strings.TrimSpace(m.Key) == "" {
		return fmt.Errorf("%w: report: missing_evidence has blank key", ports.ErrContract)
	}
	if m.Kind == invest.MissingExternal && !slices.Contains(externalSources, m.Key) {
		return fmt.Errorf("%w: report: missing_evidence has unknown external source %q", ports.ErrContract, m.Key)
	}
	if strings.TrimSpace(m.Detail) == "" {
		return fmt.Errorf("%w: report: missing_evidence %q has blank detail", ports.ErrContract, m.Key)
	}
	return nil
}

// NewReportTool returns the T11 registry func: identity validation,
// envelope validation via invest.Validate plus tenant/claim/exception
// echo, per-item invest validation, referential checks
// (finding→hypothesis, recommendation→finding), missing shape plus
// additive-only against the envelope baseline, evidence resolution for
// every cited ID, canonical marshal under the byte cap, and one
// write-once store insert. Store errors propagate untouched so
// errors.Is classification survives.
func NewReportTool(store Store, resolve Resolver) func(ctx context.Context, req ReportRequest) (ReportResponse, error) {
	return func(ctx context.Context, req ReportRequest) (ReportResponse, error) {
		if err := claims.TenantID(req.TenantID).Validate(); err != nil {
			return ReportResponse{}, fmt.Errorf("%w: report: %v", ports.ErrContract, err)
		}
		if err := claims.ClaimID(req.ClaimID).Validate(); err != nil {
			return ReportResponse{}, fmt.Errorf("%w: report: %v", ports.ErrContract, err)
		}
		if err := invest.ValidateID(invest.InvestigationIDPrefix, req.InvestigationID); err != nil {
			return ReportResponse{}, fmt.Errorf("%w: report: %v", ports.ErrContract, err)
		}
		if strings.TrimSpace(req.RequestID) == "" {
			return ReportResponse{}, fmt.Errorf("%w: report: blank request id", ports.ErrContract)
		}
		if err := invest.ValidateID(invest.ExceptionIDPrefix, req.ExceptionID); err != nil {
			return ReportResponse{}, fmt.Errorf("%w: report: %v", ports.ErrContract, err)
		}
		if store == nil {
			return ReportResponse{}, fmt.Errorf("%w: report: nil store", ports.ErrContract)
		}
		if resolve == nil {
			return ReportResponse{}, fmt.Errorf("%w: report: nil resolver", ports.ErrContract)
		}
		if err := invest.Validate(req.Exception); err != nil {
			return ReportResponse{}, fmt.Errorf("%w: report: exception: %v", ports.ErrContract, err)
		}
		if req.Exception.TenantID != req.TenantID {
			return ReportResponse{}, fmt.Errorf("%w: report: exception tenant %q != request tenant %q", ports.ErrTenantMismatch, req.Exception.TenantID, req.TenantID)
		}
		if req.Exception.ClaimID != req.ClaimID {
			return ReportResponse{}, fmt.Errorf("%w: report: exception claim %q != request claim %q", ports.ErrContract, req.Exception.ClaimID, req.ClaimID)
		}
		if req.Exception.ExceptionID != req.ExceptionID {
			return ReportResponse{}, fmt.Errorf("%w: report: exception id %q != request exception %q", ports.ErrContract, req.Exception.ExceptionID, req.ExceptionID)
		}
		if req.Exception.InvestigationID != req.InvestigationID {
			return ReportResponse{}, fmt.Errorf("%w: report: exception investigation %q != request investigation %q", ports.ErrContract, req.Exception.InvestigationID, req.InvestigationID)
		}
		if len(req.Hypotheses) == 0 {
			return ReportResponse{}, fmt.Errorf("%w: report: needs at least one hypothesis", ports.ErrContract)
		}
		if len(req.Findings) == 0 {
			return ReportResponse{}, fmt.Errorf("%w: report: needs at least one finding", ports.ErrContract)
		}
		if len(req.Recommendations) == 0 {
			return ReportResponse{}, fmt.Errorf("%w: report: needs at least one recommendation", ports.ErrContract)
		}
		for i := range req.Hypotheses {
			if err := invest.ValidateHypothesis(req.Hypotheses[i]); err != nil {
				return ReportResponse{}, fmt.Errorf("%w: report: %v", ports.ErrContract, err)
			}
		}
		for i := range req.Findings {
			if err := invest.ValidateFinding(req.Findings[i]); err != nil {
				return ReportResponse{}, fmt.Errorf("%w: report: %v", ports.ErrContract, err)
			}
		}
		for i := range req.Recommendations {
			if err := invest.ValidateRecommendation(req.Recommendations[i]); err != nil {
				return ReportResponse{}, fmt.Errorf("%w: report: %v", ports.ErrContract, err)
			}
		}
		seenHyp := map[string]struct{}{}
		for i := range req.Hypotheses {
			id := req.Hypotheses[i].ID
			if _, dup := seenHyp[id]; dup {
				return ReportResponse{}, fmt.Errorf("%w: report: duplicate hypothesis id %q", ports.ErrContract, id)
			}
			seenHyp[id] = struct{}{}
		}
		for i := range req.Findings {
			if _, ok := seenHyp[req.Findings[i].HypothesisID]; !ok {
				return ReportResponse{}, fmt.Errorf("%w: report: finding %q cites unknown hypothesis %q", ports.ErrContract, req.Findings[i].ID, req.Findings[i].HypothesisID)
			}
		}
		seenFind := map[string]struct{}{}
		for i := range req.Findings {
			id := req.Findings[i].ID
			if _, dup := seenFind[id]; dup {
				return ReportResponse{}, fmt.Errorf("%w: report: duplicate finding id %q", ports.ErrContract, id)
			}
			seenFind[id] = struct{}{}
		}
		for i := range req.Recommendations {
			for _, fid := range req.Recommendations[i].FindingIDs {
				if _, ok := seenFind[fid]; !ok {
					return ReportResponse{}, fmt.Errorf("%w: report: recommendation %s cites unknown finding %q", ports.ErrContract, string(req.Recommendations[i].Action), fid)
				}
			}
		}
		for i := range req.MissingEvidence {
			if err := checkMissingItem(req.MissingEvidence[i]); err != nil {
				return ReportResponse{}, err
			}
		}
		seenMissing := map[invest.MissingItem]struct{}{}
		for i := range req.MissingEvidence {
			if _, dup := seenMissing[req.MissingEvidence[i]]; dup {
				return ReportResponse{}, fmt.Errorf("%w: report: duplicate missing_evidence entry %+v", ports.ErrContract, req.MissingEvidence[i])
			}
			seenMissing[req.MissingEvidence[i]] = struct{}{}
		}
		for i := range req.Exception.MissingEvidence {
			if _, ok := seenMissing[req.Exception.MissingEvidence[i]]; !ok {
				return ReportResponse{}, fmt.Errorf("%w: report: drops baseline missing_evidence %+v (additive only)", ports.ErrContract, req.Exception.MissingEvidence[i])
			}
		}
		cited := map[string]struct{}{}
		for i := range req.Hypotheses {
			for _, id := range req.Hypotheses[i].EvidenceIDs {
				cited[id] = struct{}{}
			}
			for _, fr := range req.Hypotheses[i].FactRefs {
				cited[fr.EvidenceID] = struct{}{}
			}
		}
		for i := range req.Findings {
			for _, id := range req.Findings[i].EvidenceIDs {
				cited[id] = struct{}{}
			}
		}
		ids := mapsKeys(cited)
		slices.Sort(ids)
		for _, id := range ids {
			tenant, claim, err := resolve(ctx, id)
			if err != nil {
				return ReportResponse{}, fmt.Errorf("%w: report: unknown evidence %q", ports.ErrContract, id)
			}
			if tenant != req.TenantID {
				return ReportResponse{}, fmt.Errorf("%w: report: evidence %q tenant %q != request tenant %q", ports.ErrTenantMismatch, id, tenant, req.TenantID)
			}
			if claim != req.ClaimID {
				return ReportResponse{}, fmt.Errorf("%w: report: evidence %q claim %q != request claim %q", ports.ErrContract, id, claim, req.ClaimID)
			}
		}
		hyps := append([]invest.Hypothesis(nil), req.Hypotheses...)
		for i := range hyps {
			hyps[i].FactRefs = append([]invest.FactRef(nil), hyps[i].FactRefs...)
			slices.SortFunc(hyps[i].FactRefs, func(a, b invest.FactRef) int {
				if c := strings.Compare(a.Key, b.Key); c != 0 {
					return c
				}
				return strings.Compare(a.EvidenceID, b.EvidenceID)
			})
		}
		slices.SortFunc(hyps, func(a, b invest.Hypothesis) int {
			return strings.Compare(a.ID, b.ID)
		})
		finds := append([]invest.Finding(nil), req.Findings...)
		slices.SortFunc(finds, func(a, b invest.Finding) int {
			return strings.Compare(a.ID, b.ID)
		})
		recs := append([]invest.Recommendation(nil), req.Recommendations...)
		slices.SortFunc(recs, func(a, b invest.Recommendation) int {
			if c := strings.Compare(string(a.Action), string(b.Action)); c != 0 {
				return c
			}
			return strings.Compare(a.Rationale, b.Rationale)
		})
		missing := append([]invest.MissingItem(nil), req.MissingEvidence...)
		slices.SortFunc(missing, func(a, b invest.MissingItem) int {
			if c := strings.Compare(string(a.Kind), string(b.Kind)); c != 0 {
				return c
			}
			if c := strings.Compare(a.Key, b.Key); c != 0 {
				return c
			}
			return strings.Compare(a.Detail, b.Detail)
		})
		canonical, err := json.Marshal(canonicalReport{
			TenantID:        req.TenantID,
			ClaimID:         req.ClaimID,
			ExceptionID:     req.ExceptionID,
			InvestigationID: req.InvestigationID,
			Hypotheses:      hyps,
			Findings:        finds,
			Recommendations: recs,
			MissingEvidence: missing,
		})
		if err != nil {
			return ReportResponse{}, fmt.Errorf("%w: report: canonicalize: %v", ports.ErrContract, err)
		}
		if len(canonical) > MaxReportBytes {
			return ReportResponse{}, fmt.Errorf("%w: report: canonical %d bytes exceeds %d", ports.ErrContract, len(canonical), MaxReportBytes)
		}
		sum := sha256.Sum256(canonical)
		hash := hex.EncodeToString(sum[:])
		row := ReportRow{
			ID:              req.InvestigationID,
			TenantID:        req.TenantID,
			ClaimID:         req.ClaimID,
			ExceptionID:     req.ExceptionID,
			InvestigationID: req.InvestigationID,
			Hypotheses:      hyps,
			Findings:        finds,
			Recommendations: recs,
			MissingEvidence: missing,
			ContentHash:     hash,
		}
		replayed, err := store.Insert(ctx, row)
		if err != nil {
			return ReportResponse{}, err
		}
		return ReportResponse{
			ReportID:    row.ID,
			ContentHash: hash,
			Replayed:    replayed,
			Version:     1,
		}, nil
	}
}

// mapsKeys returns the keys of a string set. The caller sorts the result
// so evidence resolution runs in deterministic order.
func mapsKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}
