package orchestrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
)

// DecodeModelAction parses one raw model turn with strict field checking:
// unknown fields (agreed_raw, approval vocabulary, or any stage-foreign
// key) are rejected, trailing data is rejected, blank output classifies
// as I7, and output past the byte cap classifies as I8. Structural shape
// (act discriminator, tool/request/report presence) is ValidateModelAction.
func DecodeModelAction(data []byte, maxOutputBytes int) (ModelAction, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return ModelAction{}, newInvalidError(invalidEmpty, ErrModelEmpty, nil)
	}
	if len(data) > maxOutputBytes {
		return ModelAction{}, newInvalidError(invalidOversize, ErrModelContract,
			fmt.Errorf("payload %d bytes exceeds %d", len(data), maxOutputBytes))
	}
	var a ModelAction
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return ModelAction{}, newInvalidError(invalidMalformed, ErrModelContract, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return ModelAction{}, newInvalidError(invalidMalformed, ErrModelContract,
			fmt.Errorf("trailing data"))
	}
	normalizeModelAction(&a)
	return a, nil
}

// normalizeModelAction nil-normalizes slices without reordering content.
// Decode path only: ValidateModelAction must see wire order verbatim.
func normalizeModelAction(a *ModelAction) {
	if a.Report == nil {
		return
	}
	r := a.Report
	if r.Hypotheses == nil {
		r.Hypotheses = []invest.Hypothesis{}
	}
	if r.Findings == nil {
		r.Findings = []invest.Finding{}
	}
	if r.MissingAdditive == nil {
		r.MissingAdditive = []invest.MissingItem{}
	}
	for i := range r.Hypotheses {
		h := &r.Hypotheses[i]
		if h.FactRefs == nil {
			h.FactRefs = []invest.FactRef{}
		}
		if h.EvidenceIDs == nil {
			h.EvidenceIDs = []string{}
		}
	}
	for i := range r.Findings {
		if r.Findings[i].EvidenceIDs == nil {
			r.Findings[i].EvidenceIDs = []string{}
		}
	}
	if r.Recommendation.FindingIDs == nil {
		r.Recommendation.FindingIDs = []string{}
	}
}

// ValidateModelAction checks one decoded act: Gate A for CALL_TOOL
// (allowlist, scope subset, T11 denial, validated Request with identity
// echoing scope) and structural shape for SUBMIT_REPORT (grounding Gate B
// lives in grounding.go and runs in the loop after this passes).
func ValidateModelAction(a ModelAction, scope investigate.Scope, investigationID string) error {
	switch a.Action {
	case ActionCallTool:
		return validateCallTool(a, scope, investigationID)
	case ActionSubmitReport:
		return validateSubmitStructural(a)
	default:
		return newInvalidError(invalidAction, ErrModelContract,
			fmt.Errorf("unknown action %q (closed 2-act vocabulary)", a.Action))
	}
}

// validateCallTool enforces Gate A.
func validateCallTool(a ModelAction, scope investigate.Scope, investigationID string) error {
	if strings.TrimSpace(string(a.Tool)) == "" {
		return newInvalidError(invalidAction, ErrModelContract,
			fmt.Errorf("call_tool without a tool"))
	}
	if !invest.IsAllowlisted(a.Tool) {
		return newInvalidError(invalidDenied, ErrToolDenied,
			fmt.Errorf("tool %q not in allowlist", string(a.Tool)))
	}
	if !scope.IsAllowed(a.Tool) {
		return newInvalidError(invalidDenied, ErrToolDenied,
			fmt.Errorf("tool %q not declared in scope", string(a.Tool)))
	}
	if a.Tool == invest.ToolCreateInvestigationReport {
		return newInvalidError(invalidDenied, ErrToolDenied,
			fmt.Errorf("tool %q is never callable by the loop", string(a.Tool)))
	}
	if a.Report != nil {
		return newInvalidError(invalidRequest, ErrModelContract,
			fmt.Errorf("call_tool must not carry a report"))
	}
	if a.Request == nil {
		return newInvalidError(invalidRequest, ErrModelContract,
			fmt.Errorf("call_tool without a request"))
	}
	req := *a.Request
	if err := req.Validate(); err != nil {
		return newInvalidError(invalidRequest, ErrModelContract, err)
	}
	if req.Tool != a.Tool {
		return newInvalidError(invalidRequest, ErrModelContract,
			fmt.Errorf("request tool %q != act tool %q", string(req.Tool), string(a.Tool)))
	}
	if req.TenantID != scope.TenantID || req.ClaimID != scope.ClaimID || req.RequestID != scope.RequestID {
		return newInvalidError(invalidRequest, ErrModelContract,
			fmt.Errorf("request identity must echo scope"))
	}
	if req.InvestigationID != investigationID {
		return newInvalidError(invalidRequest, ErrModelContract,
			fmt.Errorf("request investigation must echo the envelope"))
	}
	return nil
}

// validateSubmitStructural checks the report shape (I5). Citation
// membership (I6) is CheckReportGrounding, called by the loop.
func validateSubmitStructural(a ModelAction) error {
	if a.Tool != "" || a.Request != nil {
		return newInvalidError(invalidReport, ErrModelContract,
			fmt.Errorf("submit_report must not carry tool fields"))
	}
	if a.Report == nil {
		return newInvalidError(invalidReport, ErrModelContract,
			fmt.Errorf("submit_report without a report"))
	}
	if err := ValidateReport(*a.Report); err != nil {
		return newInvalidError(invalidReport, ErrModelContract, err)
	}
	return nil
}

// ValidateReport checks the report shape standalone: at least one
// hypothesis and one finding, unique IDs, per-item invest validation,
// a validated recommendation, and sorted unique additive items with
// non-blank detail. Grounding against KnownEvidence is separate.
func ValidateReport(r Report) error {
	if len(r.Hypotheses) == 0 {
		return fmt.Errorf("orchestrate: report needs at least one hypothesis: %w", ErrModelContract)
	}
	if len(r.Findings) == 0 {
		return fmt.Errorf("orchestrate: report needs at least one finding: %w", ErrModelContract)
	}
	seenHyp := make(map[string]struct{}, len(r.Hypotheses))
	for i := range r.Hypotheses {
		h := &r.Hypotheses[i]
		if err := invest.ValidateHypothesis(*h); err != nil {
			return fmt.Errorf("orchestrate: report hypothesis[%d]: %v: %w", i, err, ErrModelContract)
		}
		if _, dup := seenHyp[h.ID]; dup {
			return fmt.Errorf("orchestrate: report duplicates hypothesis id %q: %w", h.ID, ErrModelContract)
		}
		seenHyp[h.ID] = struct{}{}
	}
	seenFind := make(map[string]struct{}, len(r.Findings))
	for i := range r.Findings {
		f := &r.Findings[i]
		if err := invest.ValidateFinding(*f); err != nil {
			return fmt.Errorf("orchestrate: report finding[%d]: %v: %w", i, err, ErrModelContract)
		}
		if _, dup := seenFind[f.ID]; dup {
			return fmt.Errorf("orchestrate: report duplicates finding id %q: %w", f.ID, ErrModelContract)
		}
		seenFind[f.ID] = struct{}{}
	}
	if err := invest.ValidateRecommendation(r.Recommendation); err != nil {
		return fmt.Errorf("orchestrate: report recommendation: %v: %w", err, ErrModelContract)
	}
	if err := validateMissingAdditive(r.MissingAdditive); err != nil {
		return err
	}
	return nil
}

// validateMissingAdditive checks additive items standalone: closed kind,
// non-blank key/detail, closed external keys, sorted order, no exact
// duplicates. Additive-vs-envelope is a grounding check, not here.
func validateMissingAdditive(items []invest.MissingItem) error {
	lastKind, lastKey, lastDetail := "", "", ""
	seen := make(map[invest.MissingItem]struct{}, len(items))
	for i := range items {
		m := &items[i]
		switch m.Kind {
		case invest.MissingRequiredDocument, invest.MissingField, invest.MissingExternal:
		default:
			return fmt.Errorf("orchestrate: missing_additive[%d] has unknown kind %q: %w", i, string(m.Kind), ErrModelContract)
		}
		if strings.TrimSpace(m.Key) == "" {
			return fmt.Errorf("orchestrate: missing_additive[%d] has blank key: %w", i, ErrModelContract)
		}
		if strings.TrimSpace(m.Detail) == "" {
			return fmt.Errorf("orchestrate: missing_additive[%d] has blank detail: %w", i, ErrModelContract)
		}
		if m.Kind == invest.MissingExternal {
			switch m.Key {
			case "policy", "tpa", "provider", "risk":
			default:
				return fmt.Errorf("orchestrate: missing_additive[%d] unknown external source %q: %w", i, m.Key, ErrModelContract)
			}
		}
		cur := string(m.Kind) + "\x00" + m.Key + "\x00" + m.Detail
		prev := lastKind + "\x00" + lastKey + "\x00" + lastDetail
		if i > 0 && cur < prev {
			return fmt.Errorf("orchestrate: missing_additive not sorted at index %d: %w", i, ErrModelContract)
		}
		lastKind, lastKey, lastDetail = string(m.Kind), m.Key, m.Detail
		if _, dup := seen[*m]; dup {
			return fmt.Errorf("orchestrate: missing_additive duplicates item at index %d: %w", i, ErrModelContract)
		}
		seen[*m] = struct{}{}
	}
	return nil
}

// sortMissingAdditive returns a sorted copy (canonical output path).
func sortMissingAdditive(items []invest.MissingItem) []invest.MissingItem {
	out := append([]invest.MissingItem(nil), items...)
	slices.SortFunc(out, func(a, b invest.MissingItem) int {
		if c := strings.Compare(string(a.Kind), string(b.Kind)); c != 0 {
			return c
		}
		if c := strings.Compare(a.Key, b.Key); c != 0 {
			return c
		}
		return strings.Compare(a.Detail, b.Detail)
	})
	if out == nil {
		out = []invest.MissingItem{}
	}
	return out
}
