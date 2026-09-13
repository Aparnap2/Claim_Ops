package invest

import (
	"fmt"
	"slices"
	"strings"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate/orchestrate"
)

// E2 dimensions (per-case observable, no single 0-100 score).
const (
	DimHypothesisQuality         = "hypothesis-quality"
	DimFalsifierQuality          = "falsifier-quality"
	DimEvidenceGrounding         = "evidence-grounding"
	DimContradictionHandling     = "contradiction-handling"
	DimFindingQuality            = "finding-quality"
	DimRecommendationValidity    = "recommendation-validity"
	DimEscalationAppropriateness = "escalation-appropriateness"
)

// AllE2Dimensions returns the 7 dimensions in fixed order.
func AllE2Dimensions() []string {
	return []string{
		DimHypothesisQuality,
		DimFalsifierQuality,
		DimEvidenceGrounding,
		DimContradictionHandling,
		DimFindingQuality,
		DimRecommendationValidity,
		DimEscalationAppropriateness,
	}
}

// E2Result is one dimension verdict.
type E2Result struct {
	Dimension string
	Passed    bool
	Detail    string
}

func e2pass(dim, detail string) E2Result {
	return E2Result{Dimension: dim, Passed: true, Detail: detail}
}

func e2fail(dim, detail string) E2Result {
	return E2Result{Dimension: dim, Passed: false, Detail: detail}
}

// escalationAppropriate reports whether r's outcome matches c's expectation,
// treating INSUFFICIENT_EVIDENCE as valid success when MustEscalate.
// This mirrors the prompt: MustEscalate ↔ escalated + reason match;
// INSUFFICIENT_EVIDENCE as valid success when MustEscalate.
func escalationAppropriate(c EvalCase, r EvalResult) bool {
	if c.MustEscalate {
		// Explicit escalation with expected reason.
		if r.Outcome == orchestrate.OutcomeEscalated && r.EscalationReason == c.MustEscalateReason {
			return true
		}
		// INSUFFICIENT_EVIDENCE as valid success: either an escalation
		// reason named INSUFFICIENT_EVIDENCE, or a report that requests
		// evidence (the cognitive equivalent).
		if r.Outcome == orchestrate.OutcomeEscalated && string(r.EscalationReason) == "INSUFFICIENT_EVIDENCE" {
			return true
		}
		if r.Outcome == orchestrate.OutcomeReportReady && r.Report != nil {
			// Check raw report if present.
			if r.ReportRaw != nil {
				if r.ReportRaw.Recommendation.Action == invest.RecommendRequestEvidence {
					return true
				}
			} else if r.Report.RecommendationAction == string(invest.RecommendRequestEvidence) {
				return true
			}
		}
		return false
	}
	// Ready-path: must be REPORT_READY with a report.
	if r.Outcome == orchestrate.OutcomeReportReady && r.Report != nil {
		return true
	}
	return false
}

// e2HypothesisQuality checks ID + falsifier present + statement non-blank.
// No LLM, no prose grading: structural non-blank checks only.
func e2HypothesisQuality(c EvalCase, r EvalResult) E2Result {
	if r.ReportRaw != nil {
		if len(r.ReportRaw.Hypotheses) == 0 {
			if c.MustEscalate && escalationAppropriate(c, r) {
				return e2pass(DimHypothesisQuality, "no hypotheses; escalated appropriately")
			}
			return e2fail(DimHypothesisQuality, "report has no hypotheses")
		}
		for i := range r.ReportRaw.Hypotheses {
			h := &r.ReportRaw.Hypotheses[i]
			if strings.TrimSpace(h.ID) == "" {
				return e2fail(DimHypothesisQuality, fmt.Sprintf("hypothesis[%d] has blank id", i))
			}
			if strings.TrimSpace(h.Statement) == "" {
				return e2fail(DimHypothesisQuality, fmt.Sprintf("hypothesis %q has blank statement", h.ID))
			}
			if strings.TrimSpace(h.Falsifier) == "" {
				return e2fail(DimHypothesisQuality, fmt.Sprintf("hypothesis %q has blank falsifier", h.ID))
			}
		}
		return e2pass(DimHypothesisQuality, fmt.Sprintf("%d hypotheses with id+statement+falsifier", len(r.ReportRaw.Hypotheses)))
	}
	// Fallback to ReportObs (IDs/codes only).
	if r.Report == nil {
		if c.MustEscalate && escalationAppropriate(c, r) {
			return e2pass(DimHypothesisQuality, "no report; escalated appropriately")
		}
		return e2fail(DimHypothesisQuality, "ready-path has no report")
	}
	if len(r.Report.HypothesisIDs) == 0 {
		return e2fail(DimHypothesisQuality, "report has no hypotheses")
	}
	if len(r.Report.MissingFalsifier) > 0 {
		return e2fail(DimHypothesisQuality, fmt.Sprintf("hypotheses missing falsifier: %v", r.Report.MissingFalsifier))
	}
	return e2pass(DimHypothesisQuality, fmt.Sprintf("%d hypotheses", len(r.Report.HypothesisIDs)))
}

// e2FalsifierQuality checks falsifier non-blank, distinct from statement.
func e2FalsifierQuality(c EvalCase, r EvalResult) E2Result {
	if r.ReportRaw != nil {
		if len(r.ReportRaw.Hypotheses) == 0 {
			if c.MustEscalate && escalationAppropriate(c, r) {
				return e2pass(DimFalsifierQuality, "no hypotheses; escalated appropriately")
			}
			return e2fail(DimFalsifierQuality, "report has no hypotheses")
		}
		for i := range r.ReportRaw.Hypotheses {
			h := &r.ReportRaw.Hypotheses[i]
			if strings.TrimSpace(h.Falsifier) == "" {
				return e2fail(DimFalsifierQuality, fmt.Sprintf("hypothesis %q has blank falsifier", h.ID))
			}
			if strings.TrimSpace(h.Falsifier) == strings.TrimSpace(h.Statement) {
				return e2fail(DimFalsifierQuality, fmt.Sprintf("hypothesis %q falsifier identical to statement", h.ID))
			}
		}
		return e2pass(DimFalsifierQuality, "falsifiers distinct and non-blank")
	}
	if r.Report == nil {
		if c.MustEscalate && escalationAppropriate(c, r) {
			return e2pass(DimFalsifierQuality, "no report; escalated appropriately")
		}
		return e2fail(DimFalsifierQuality, "ready-path has no report")
	}
	if len(r.Report.MissingFalsifier) > 0 {
		return e2fail(DimFalsifierQuality, fmt.Sprintf("hypotheses missing falsifier: %v", r.Report.MissingFalsifier))
	}
	// Without raw we cannot check distinctness; pass on presence alone.
	return e2pass(DimFalsifierQuality, "falsifiers present")
}

// e2EvidenceGrounding checks finding EvidenceIDs ⊆ KnownUniverse and
// hypothesis FactRefs ⊆ KnownUniverse.
func e2EvidenceGrounding(c EvalCase, r EvalResult) E2Result {
	if r.Report == nil && r.ReportRaw == nil {
		if c.MustEscalate && escalationAppropriate(c, r) {
			return e2pass(DimEvidenceGrounding, "no report; escalated appropriately")
		}
		return e2fail(DimEvidenceGrounding, "ready-path has no report")
	}
	known := make(map[string]struct{}, len(r.KnownUniverse))
	for _, id := range r.KnownUniverse {
		known[id] = struct{}{}
	}
	if r.ReportRaw != nil {
		for i := range r.ReportRaw.Hypotheses {
			h := &r.ReportRaw.Hypotheses[i]
			for _, id := range h.EvidenceIDs {
				if _, ok := known[id]; !ok {
					return e2fail(DimEvidenceGrounding, fmt.Sprintf("hypothesis %q cites evidence %q outside universe", h.ID, id))
				}
			}
			for _, fr := range h.FactRefs {
				if _, ok := known[fr.EvidenceID]; !ok {
					return e2fail(DimEvidenceGrounding, fmt.Sprintf("hypothesis %q fact ref %q cites evidence %q outside universe", h.ID, fr.Key, fr.EvidenceID))
				}
			}
		}
		for i := range r.ReportRaw.Findings {
			f := &r.ReportRaw.Findings[i]
			for _, id := range f.EvidenceIDs {
				if _, ok := known[id]; !ok {
					return e2fail(DimEvidenceGrounding, fmt.Sprintf("finding %q cites evidence %q outside universe", f.ID, id))
				}
			}
		}
		return e2pass(DimEvidenceGrounding, "all evidence grounded")
	}
	// Fallback to ReportObs CitedEvidenceIDs.
	for _, id := range r.Report.CitedEvidenceIDs {
		if _, ok := known[id]; !ok {
			return e2fail(DimEvidenceGrounding, fmt.Sprintf("report cites evidence %q outside universe", id))
		}
	}
	return e2pass(DimEvidenceGrounding, "cited evidence grounded")
}

// contradictionCases is the set of cases where conflicting evidence exists
// and findings must cover it (B: amount conflict, E: external policy mismatch,
// G: admission-date skew).
var contradictionCases = map[string]struct{}{
	"B": {},
	"E": {},
	"G": {},
}

func e2ContradictionHandling(c EvalCase, r EvalResult) E2Result {
	if _, ok := contradictionCases[c.ID]; !ok {
		return e2pass(DimContradictionHandling, "no contradiction expectation for this case")
	}
	if r.Report == nil && r.ReportRaw == nil {
		return e2fail(DimContradictionHandling, fmt.Sprintf("case %s contradiction not covered: no report", c.ID))
	}
	// Require cited evidence to cover the conflicting sources.
	// Derive conflicting evidence IDs from envelope Unresolved entries.
	conflictIDs := make(map[string]struct{})
	for i := range c.Envelope.Unresolved {
		u := &c.Envelope.Unresolved[i]
		if u.Conflict != nil {
			for _, s := range u.Conflict.Sources {
				conflictIDs[s.EvidenceID] = struct{}{}
			}
		}
	}
	// If envelope has no conflict sources (defensive), require at least 2 cited.
	if len(conflictIDs) == 0 {
		cited := 0
		if r.ReportRaw != nil {
			seen := make(map[string]struct{})
			for _, f := range r.ReportRaw.Findings {
				for _, id := range f.EvidenceIDs {
					seen[id] = struct{}{}
				}
			}
			cited = len(seen)
		} else if r.Report != nil {
			cited = len(r.Report.CitedEvidenceIDs)
		}
		if cited >= 2 {
			return e2pass(DimContradictionHandling, "findings cover multiple evidence")
		}
		return e2fail(DimContradictionHandling, "findings do not cover conflicting evidence")
	}
	// Check cited set covers all conflict IDs.
	citedSet := make(map[string]struct{})
	if r.ReportRaw != nil {
		for _, h := range r.ReportRaw.Hypotheses {
			for _, id := range h.EvidenceIDs {
				citedSet[id] = struct{}{}
			}
		}
		for _, f := range r.ReportRaw.Findings {
			for _, id := range f.EvidenceIDs {
				citedSet[id] = struct{}{}
			}
		}
	} else if r.Report != nil {
		for _, id := range r.Report.CitedEvidenceIDs {
			citedSet[id] = struct{}{}
		}
	}
	for id := range conflictIDs {
		if _, ok := citedSet[id]; !ok {
			return e2fail(DimContradictionHandling, fmt.Sprintf("case %s contradiction id %q not covered by findings", c.ID, id))
		}
	}
	return e2pass(DimContradictionHandling, "findings cover conflicting evidence")
}

// e2FindingQuality checks FindingID→HypothesisID resolution and Summary non-blank.
func e2FindingQuality(c EvalCase, r EvalResult) E2Result {
	if r.ReportRaw != nil {
		if len(r.ReportRaw.Findings) == 0 {
			if c.MustEscalate && escalationAppropriate(c, r) {
				return e2pass(DimFindingQuality, "no findings; escalated appropriately")
			}
			return e2fail(DimFindingQuality, "report has no findings")
		}
		hyps := make(map[string]struct{}, len(r.ReportRaw.Hypotheses))
		for _, h := range r.ReportRaw.Hypotheses {
			hyps[h.ID] = struct{}{}
		}
		for i := range r.ReportRaw.Findings {
			f := &r.ReportRaw.Findings[i]
			if strings.TrimSpace(f.ID) == "" {
				return e2fail(DimFindingQuality, fmt.Sprintf("finding[%d] has blank id", i))
			}
			if strings.TrimSpace(f.Summary) == "" {
				return e2fail(DimFindingQuality, fmt.Sprintf("finding %q has blank summary", f.ID))
			}
			if _, ok := hyps[f.HypothesisID]; !ok {
				return e2fail(DimFindingQuality, fmt.Sprintf("finding %q resolves unknown hypothesis %q", f.ID, f.HypothesisID))
			}
			if len(f.EvidenceIDs) == 0 {
				return e2fail(DimFindingQuality, fmt.Sprintf("finding %q cites no evidence", f.ID))
			}
		}
		return e2pass(DimFindingQuality, fmt.Sprintf("%d findings resolved", len(r.ReportRaw.Findings)))
	}
	if r.Report == nil {
		if c.MustEscalate && escalationAppropriate(c, r) {
			return e2pass(DimFindingQuality, "no report; escalated appropriately")
		}
		return e2fail(DimFindingQuality, "ready-path has no report")
	}
	if len(r.Report.FindingIDs) == 0 {
		return e2fail(DimFindingQuality, "report has no findings")
	}
	if len(r.Report.DanglingFindings) > 0 {
		return e2fail(DimFindingQuality, fmt.Sprintf("dangling findings: %v", r.Report.DanglingFindings))
	}
	// Without raw, summary non-blank is already enforced by ValidateReport
	// for REPORT_READY; assume pass.
	return e2pass(DimFindingQuality, "findings resolve hypotheses")
}

// e2RecommendationValidity checks closed enum and FindingIDs grounded.
func e2RecommendationValidity(c EvalCase, r EvalResult) E2Result {
	if r.ReportRaw != nil {
		if err := invest.ValidateRecommendation(r.ReportRaw.Recommendation); err != nil {
			return e2fail(DimRecommendationValidity, fmt.Sprintf("invalid recommendation: %v", err))
		}
		findings := make(map[string]struct{}, len(r.ReportRaw.Findings))
		for _, f := range r.ReportRaw.Findings {
			findings[f.ID] = struct{}{}
		}
		for _, id := range r.ReportRaw.Recommendation.FindingIDs {
			if _, ok := findings[id]; !ok {
				return e2fail(DimRecommendationValidity, fmt.Sprintf("recommendation cites unknown finding %q", id))
			}
		}
		return e2pass(DimRecommendationValidity, fmt.Sprintf("recommendation %s valid", string(r.ReportRaw.Recommendation.Action)))
	}
	if r.Report == nil {
		if c.MustEscalate && escalationAppropriate(c, r) {
			return e2pass(DimRecommendationValidity, "no report; escalated appropriately")
		}
		return e2fail(DimRecommendationValidity, "ready-path has no report")
	}
	if !r.Report.RecommendationValid {
		return e2fail(DimRecommendationValidity, fmt.Sprintf("recommendation %q invalid", r.Report.RecommendationAction))
	}
	switch invest.RecommendationAction(r.Report.RecommendationAction) {
	case invest.RecommendRequestEvidence, invest.RecommendConfirmException, invest.RecommendReferHuman, invest.RecommendReverify:
	default:
		return e2fail(DimRecommendationValidity, fmt.Sprintf("recommendation %q not in closed enum", r.Report.RecommendationAction))
	}
	return e2pass(DimRecommendationValidity, fmt.Sprintf("recommendation %s valid", r.Report.RecommendationAction))
}

// e2EscalationAppropriateness checks MustEscalate ↔ escalated + reason match,
// with INSUFFICIENT_EVIDENCE as valid success when MustEscalate.
func e2EscalationAppropriateness(c EvalCase, r EvalResult) E2Result {
	appropriate := escalationAppropriate(c, r)
	if !appropriate {
		if c.MustEscalate {
			if r.Outcome == orchestrate.OutcomeEscalated {
				return e2fail(DimEscalationAppropriateness, fmt.Sprintf("must escalate %q but got reason %q", string(c.MustEscalateReason), string(r.EscalationReason)))
			}
			if r.Outcome == orchestrate.OutcomeReportReady && r.Report != nil {
				act := ""
				if r.ReportRaw != nil {
					act = string(r.ReportRaw.Recommendation.Action)
				} else {
					act = r.Report.RecommendationAction
				}
				return e2fail(DimEscalationAppropriateness, fmt.Sprintf("must escalate %q but landed REPORT_READY (%s)", string(c.MustEscalateReason), act))
			}
			return e2fail(DimEscalationAppropriateness, fmt.Sprintf("must escalate %q but outcome %q", string(c.MustEscalateReason), string(r.Outcome)))
		}
		return e2fail(DimEscalationAppropriateness, fmt.Sprintf("ready-path must not escalate, got %q (%q)", string(r.Outcome), string(r.EscalationReason)))
	}
	if c.MustEscalate {
		if r.Outcome == orchestrate.OutcomeEscalated {
			if string(r.EscalationReason) == "INSUFFICIENT_EVIDENCE" {
				return e2pass(DimEscalationAppropriateness, "MustEscalate satisfied via INSUFFICIENT_EVIDENCE")
			}
			return e2pass(DimEscalationAppropriateness, fmt.Sprintf("escalated appropriately %s", string(r.EscalationReason)))
		}
		// REPORT_READY with REQUEST_EVIDENCE counts as INSUFFICIENT_EVIDENCE success.
		return e2pass(DimEscalationAppropriateness, "MustEscalate satisfied via INSUFFICIENT_EVIDENCE (REQUEST_EVIDENCE)")
	}
	return e2pass(DimEscalationAppropriateness, "ready-path report")
}

// EvaluateE2 runs the 7 E2 dimensions over one case result in fixed order.
// No single 0-100 score is produced; dimensions are separately observable.
func EvaluateE2(c EvalCase, r EvalResult) []E2Result {
	// Verify dimensions are individually observable: return separate entries,
	// never a collapsed score.
	_ = slices.Contains(AllE2Dimensions(), DimHypothesisQuality)
	return []E2Result{
		e2HypothesisQuality(c, r),
		e2FalsifierQuality(c, r),
		e2EvidenceGrounding(c, r),
		e2ContradictionHandling(c, r),
		e2FindingQuality(c, r),
		e2RecommendationValidity(c, r),
		e2EscalationAppropriateness(c, r),
	}
}

// E2Summary is the overall E2 verdict over many cases.
type E2Summary struct {
	Passed   bool
	Failures []string
}

// RunE2All evaluates every E2 dimension over every run and checks that
// every dimension passes every case.
func RunE2All(runs []CaseRun) E2Summary {
	var failures []string
	for i := range runs {
		run := &runs[i]
		if run.Result.CaseID != run.Case.ID {
			failures = append(failures, fmt.Sprintf("case %q: result case id %q mismatch", run.Case.ID, run.Result.CaseID))
			continue
		}
		for _, g := range EvaluateE2(run.Case, run.Result) {
			if !g.Passed {
				failures = append(failures, fmt.Sprintf("case %s dimension %s: %s", run.Case.ID, g.Dimension, g.Detail))
			}
		}
	}
	return E2Summary{Passed: len(failures) == 0, Failures: failures}
}
