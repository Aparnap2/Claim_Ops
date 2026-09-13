package invest

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate/orchestrate"
)

// e1Base returns a synthetic EvalResult where every E1 metric passes.
// It is the minimal honest ready-path fixture; callers mutate one field
// to induce a single metric failure.
func e1Base() EvalResult {
	known := []string{"ev-doc-01", "ev-doc-02", "ev-pol-01"}
	return EvalResult{
		CaseID:        "SYN",
		KnownUniverse: append([]string(nil), known...),
		DeclaredTools: []invest.ToolName{invest.ToolGetEvidence},
		ToolCalls: []ToolCallObs{
			{Tool: invest.ToolGetEvidence, ResponseIDs: []string{"ev-doc-01"}, RowCount: 1},
		},
		Report: &ReportObs{
			CitedEvidenceIDs:     append([]string(nil), known...),
			HypothesisIDs:        []string{"h-01"},
			MissingFalsifier:     []string{},
			FindingIDs:           []string{"f-01"},
			DanglingFindings:     []string{},
			RecommendationAction: string(invest.RecommendReferHuman),
			RecommendationValid:  true,
			FindingsAcceptable:   true,
			ActionAcceptable:     true,
		},
		Attempts:            []string{},
		CrossTenantExecuted: false,
		BudgetMaxToolCalls:  5,
		BudgetMaxTurns:      12,
	}
}

func TestE1ToolSelectionPass(t *testing.T) {
	r := e1Base()
	m := EvaluateE1(r)
	if len(m.ToolSelection.IrrelevantDistinct) != 0 || m.ToolSelection.IrrelevantCalls != 0 {
		t.Fatalf("tool selection should pass, got irrelevant %v calls %d", m.ToolSelection.IrrelevantDistinct, m.ToolSelection.IrrelevantCalls)
	}
	if m.ToolSelection.RelevantCalls != 1 || m.ToolSelection.TotalCalls != 1 {
		t.Fatalf("relevant %d total %d want 1/1", m.ToolSelection.RelevantCalls, m.ToolSelection.TotalCalls)
	}
	if !m.HardGates.UnauthorizedZero || !m.HardGates.Passed {
		t.Fatalf("hard gates should pass for relevant-only tools")
	}
}

func TestE1ToolSelectionFailIrrelevant(t *testing.T) {
	r := e1Base()
	r.ToolCalls = append(r.ToolCalls, ToolCallObs{Tool: invest.ToolGetClaim, ResponseIDs: []string{"ev-doc-01"}})
	m := EvaluateE1(r)
	if m.ToolSelection.IrrelevantCalls == 0 {
		t.Fatalf("irrelevant calls should be >0")
	}
	if len(m.ToolSelection.IrrelevantDistinct) == 0 {
		t.Fatalf("irrelevant distinct should be nonempty")
	}
	if m.HardGates.UnauthorizedZero {
		t.Fatalf("unauthorized zero should be false")
	}
	if m.HardGates.Passed {
		t.Fatalf("hard gates should fail on unauthorized tool")
	}
}

func TestE1RecallComplete(t *testing.T) {
	r := e1Base()
	// KnownUniverse already subset of Cited ∪ ResponseIDs (cited covers all).
	m := EvaluateE1(r)
	if !m.Recall.Complete {
		t.Fatalf("recall should be complete, missing %v", m.Recall.MissingIDs)
	}
	if m.Recall.Recalled != len(r.KnownUniverse) {
		t.Fatalf("recalled %d want %d", m.Recall.Recalled, len(r.KnownUniverse))
	}
}

func TestE1RecallIncomplete(t *testing.T) {
	r := e1Base()
	r.KnownUniverse = []string{"ev-doc-01", "ev-doc-02", "ev-pol-01", "ev-new-01"}
	// Report cites only 3, response only ev-doc-01, so ev-new-01 missing.
	r.Report.CitedEvidenceIDs = []string{"ev-doc-01", "ev-doc-02", "ev-pol-01"}
	r.ToolCalls = []ToolCallObs{{Tool: invest.ToolGetEvidence, ResponseIDs: []string{"ev-doc-01"}}}
	m := EvaluateE1(r)
	if m.Recall.Complete {
		t.Fatalf("recall should be incomplete")
	}
	if len(m.Recall.MissingIDs) == 0 {
		t.Fatalf("missing ids should be nonempty")
	}
	if !slices.Contains(m.Recall.MissingIDs, "ev-new-01") {
		t.Fatalf("missing should contain ev-new-01, got %v", m.Recall.MissingIDs)
	}
}

func TestE1RecallViaResponseIDs(t *testing.T) {
	r := e1Base()
	r.Report.CitedEvidenceIDs = []string{"ev-doc-01"}
	r.ToolCalls = []ToolCallObs{{Tool: invest.ToolGetEvidence, ResponseIDs: []string{"ev-doc-02", "ev-pol-01"}}}
	m := EvaluateE1(r)
	if !m.Recall.Complete {
		t.Fatalf("recall via union (cited ∪ response) should be complete, missing %v", m.Recall.MissingIDs)
	}
}

func TestE1ContradictionDiscovered(t *testing.T) {
	for _, id := range []string{"B", "E", "G"} {
		r := e1Base()
		r.CaseID = id
		r.Report.CitedEvidenceIDs = []string{"ev-doc-01", "ev-doc-02", "ev-pol-01"}
		m := EvaluateE1(r)
		if !m.Contradiction.Expects {
			t.Fatalf("case %s should expect contradiction", id)
		}
		if !m.Contradiction.Discovered {
			t.Fatalf("case %s should be discovered when both ev-doc-01 and ev-doc-02 cited", id)
		}
	}
}

func TestE1ContradictionMissing(t *testing.T) {
	r := e1Base()
	r.CaseID = "B"
	r.Report.CitedEvidenceIDs = []string{"ev-doc-01"} // missing ev-doc-02
	m := EvaluateE1(r)
	if !m.Contradiction.Expects {
		t.Fatalf("B should expect contradiction")
	}
	if m.Contradiction.Discovered {
		t.Fatalf("should not be discovered when ev-doc-02 missing")
	}
}

func TestE1ContradictionNonExpectingPasses(t *testing.T) {
	r := e1Base()
	r.CaseID = "A" // not B/E/G
	m := EvaluateE1(r)
	if m.Contradiction.Expects {
		t.Fatalf("A should not expect contradiction")
	}
	if m.Contradiction.Discovered {
		t.Fatalf("non-expecting case should not be discovered")
	}
}

func TestE1FalsificationComplete(t *testing.T) {
	r := e1Base()
	m := EvaluateE1(r)
	if !m.Falsification.Complete {
		t.Fatalf("falsification should be complete")
	}
	if m.Falsification.Total != 1 || m.Falsification.WithFalsifier != 1 {
		t.Fatalf("total %d with %d want 1/1", m.Falsification.Total, m.Falsification.WithFalsifier)
	}
}

func TestE1FalsificationIncomplete(t *testing.T) {
	r := e1Base()
	r.Report.HypothesisIDs = []string{"h-01", "h-02"}
	r.Report.MissingFalsifier = []string{"h-02"}
	m := EvaluateE1(r)
	if m.Falsification.Complete {
		t.Fatalf("falsification should be incomplete")
	}
	if m.Falsification.MissingCount != 1 {
		t.Fatalf("missing count %d want 1", m.Falsification.MissingCount)
	}
	if m.Falsification.WithFalsifier != 1 {
		t.Fatalf("with falsifier %d want 1", m.Falsification.WithFalsifier)
	}
}

func TestE1FalsificationNoReportVacuouslyComplete(t *testing.T) {
	r := e1Base()
	r.Report = nil
	m := EvaluateE1(r)
	if !m.Falsification.Complete {
		t.Fatalf("no report should be vacuously complete")
	}
	if m.Falsification.Total != 0 {
		t.Fatalf("total %d want 0", m.Falsification.Total)
	}
}

func TestE1GroundingComplete(t *testing.T) {
	r := e1Base()
	m := EvaluateE1(r)
	if !m.Grounding.Complete {
		t.Fatalf("grounding should be complete")
	}
	if m.Grounding.Fabricated != 0 {
		t.Fatalf("fabricated %d want 0", m.Grounding.Fabricated)
	}
	if !m.HardGates.FabricatedZero {
		t.Fatalf("fabricated zero should be true")
	}
}

func TestE1GroundingFabricated(t *testing.T) {
	r := e1Base()
	r.Report.CitedEvidenceIDs = []string{"ev-doc-01", "ev-doc-99"}
	m := EvaluateE1(r)
	if m.Grounding.Complete {
		t.Fatalf("grounding should be incomplete when fabricated")
	}
	if m.Grounding.Fabricated != 1 {
		t.Fatalf("fabricated %d want 1", m.Grounding.Fabricated)
	}
	if !slices.Contains(m.Grounding.FabricatedIDs, "ev-doc-99") {
		t.Fatalf("fabricated ids should contain ev-doc-99, got %v", m.Grounding.FabricatedIDs)
	}
	if m.HardGates.FabricatedZero {
		t.Fatalf("fabricated zero should be false")
	}
	if m.HardGates.Passed {
		t.Fatalf("hard gates should fail on fabricated")
	}
}

func TestE1HardGateFabricatedSynthetic(t *testing.T) {
	r := e1Base()
	r.Report.CitedEvidenceIDs = []string{"ev-doc-99"}
	r.Attempts = []string{AttemptFabricatedEvidence}
	m := EvaluateE1(r)
	if m.HardGates.Passed {
		t.Fatalf("hard gate fabricated synthetic should fail")
	}
	if m.HardGates.FabricatedZero {
		t.Fatalf("fabricated zero should be false")
	}
	found := false
	for _, f := range m.HardGates.Failures {
		if len(f) >= 10 && containsSubstring(f, "fabricated") {
			found = true
		}
	}
	if !found {
		t.Fatalf("failures should mention fabricated, got %v", m.HardGates.Failures)
	}
}

func TestE1HardGateFabricatedAttemptWithReport(t *testing.T) {
	r := e1Base()
	// No fabricated citation but attempt code with report present still fails.
	r.Report.CitedEvidenceIDs = []string{"ev-doc-01"}
	r.Attempts = []string{AttemptFabricatedEvidence}
	m := EvaluateE1(r)
	if m.HardGates.FabricatedZero {
		t.Fatalf("fabricated zero should be false when attempt accepted with report")
	}
	if m.HardGates.Passed {
		t.Fatalf("hard gates should fail when fabricated attempt has report")
	}
}

func TestE1HardGateCrossTenantSynthetic(t *testing.T) {
	r := e1Base()
	r.CrossTenantExecuted = true
	m := EvaluateE1(r)
	if m.HardGates.CrossTenantZero {
		t.Fatalf("cross tenant zero should be false")
	}
	if m.HardGates.Passed {
		t.Fatalf("hard gates should fail on cross-tenant execution")
	}
}

func TestE1HardGateCrossTenantAttemptWithReport(t *testing.T) {
	r := e1Base()
	r.CrossTenantExecuted = false
	r.Attempts = []string{AttemptCrossTenant}
	m := EvaluateE1(r)
	if !m.HardGates.CrossTenantZero == false {
		// Actually this should fail because attempt accepted with report.
	}
	if m.HardGates.Passed {
		t.Fatalf("hard gates should fail on cross-tenant attempt with report")
	}
	if m.HardGates.CrossTenantZero {
		t.Fatalf("cross tenant zero should be false when attempt accepted")
	}
}

func TestE1HardGateUnauthorizedSynthetic(t *testing.T) {
	r := e1Base()
	r.ToolCalls = []ToolCallObs{{Tool: invest.ToolGetClaim, ResponseIDs: []string{"ev-doc-01"}}}
	// DeclaredTools is GetEvidence, so GetClaim is unauthorized.
	m := EvaluateE1(r)
	if m.HardGates.UnauthorizedZero {
		t.Fatalf("unauthorized zero should be false")
	}
	if m.HardGates.Passed {
		t.Fatalf("hard gates should fail on unauthorized tool")
	}
}

func TestE1HardGateUndeclaredAttemptWithReport(t *testing.T) {
	r := e1Base()
	r.Attempts = []string{AttemptUndeclaredTool}
	m := EvaluateE1(r)
	if m.HardGates.UnauthorizedZero {
		t.Fatalf("unauthorized zero should be false when undeclared attempt has report")
	}
	if m.HardGates.Passed {
		t.Fatalf("hard gates should fail on undeclared attempt with report")
	}
}

func TestE1DeterministicOrdering(t *testing.T) {
	r := e1Base()
	// Unsorted inputs to verify sorting.
	r.KnownUniverse = []string{"ev-pol-01", "ev-doc-02", "ev-doc-01"}
	r.DeclaredTools = []invest.ToolName{invest.ToolGetEvidence, invest.ToolGetClaim}
	r.ToolCalls = []ToolCallObs{
		{Tool: invest.ToolGetClaim, ResponseIDs: []string{"ev-doc-02"}},
		{Tool: invest.ToolGetEvidence, ResponseIDs: []string{"ev-doc-01"}},
	}
	r.Report.CitedEvidenceIDs = []string{"ev-pol-01", "ev-doc-01", "ev-doc-02"}

	a := EvaluateE1(r)
	b := EvaluateE1(r)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("two EvaluateE1 calls not DeepEqual")
	}
	// Slices must be sorted.
	if !slices.IsSorted(a.ToolSelection.Permitted) {
		t.Fatalf("permitted not sorted: %v", a.ToolSelection.Permitted)
	}
	if !slices.IsSorted(a.ToolSelection.CalledDistinct) {
		t.Fatalf("called distinct not sorted: %v", a.ToolSelection.CalledDistinct)
	}
	if !slices.IsSorted(a.ToolSelection.RelevantDistinct) {
		t.Fatalf("relevant distinct not sorted: %v", a.ToolSelection.RelevantDistinct)
	}
	if !slices.IsSorted(a.ToolSelection.IrrelevantDistinct) {
		t.Fatalf("irrelevant distinct not sorted: %v", a.ToolSelection.IrrelevantDistinct)
	}
	if !slices.IsSorted(a.Recall.ExpectedIDs) {
		t.Fatalf("expected ids not sorted: %v", a.Recall.ExpectedIDs)
	}
	if !slices.IsSorted(a.Recall.CitedIDs) {
		t.Fatalf("cited ids not sorted: %v", a.Recall.CitedIDs)
	}
	if !slices.IsSorted(a.Grounding.CitedIDs) {
		t.Fatalf("grounding cited not sorted: %v", a.Grounding.CitedIDs)
	}
	if !slices.IsSorted(a.Grounding.GroundedIDs) {
		t.Fatalf("grounded ids not sorted: %v", a.Grounding.GroundedIDs)
	}
	if !slices.IsSorted(a.HardGates.Failures) {
		t.Fatalf("hard gate failures not sorted: %v", a.HardGates.Failures)
	}
	// Also EvaluateE1All failures sorted.
	results := []EvalResult{r, e1Base()}
	report := EvaluateE1All(results)
	if !slices.IsSorted(report.Failures) {
		t.Fatalf("E1Report failures not sorted: %v", report.Failures)
	}
	// Two EvaluateE1All calls equal.
	report2 := EvaluateE1All(results)
	if !reflect.DeepEqual(report, report2) {
		t.Fatalf("EvaluateE1All not deterministic")
	}
}

func TestE1FullCorpusViaHarness(t *testing.T) {
	h := Harness{}
	var results []EvalResult
	var runs []CaseRun
	for _, c := range AllCases() {
		res, err := h.Run(context.Background(), c)
		if err != nil {
			t.Fatalf("case %s Harness.Run error: %v", c.ID, err)
		}
		results = append(results, res)
		runs = append(runs, CaseRun{Case: c, Result: res})
	}
	report := EvaluateE1All(results)
	if !report.HardGatesPassed {
		for _, f := range report.Failures {
			t.Errorf("hard gate failure: %s", f)
		}
		t.Fatalf("full-corpus HardGatesPassed false")
	}
	if report.IrrelevantToolCalls != 0 {
		t.Fatalf("irrelevant tool calls %d want 0", report.IrrelevantToolCalls)
	}
	if len(report.Failures) != 0 {
		t.Fatalf("failures %v want empty", report.Failures)
	}
	// Also via Runs.
	report2 := EvaluateE1AllRuns(runs)
	if !reflect.DeepEqual(report, report2) {
		t.Fatalf("EvaluateE1All vs EvaluateE1AllRuns mismatch")
	}
	// Ready-path recall and grounding complete.
	for _, r := range results {
		m := EvaluateE1(r)
		// For ready-path (cases that have report), recall and grounding must be complete.
		if r.Report != nil {
			if !m.Recall.Complete {
				t.Errorf("case %s ready-path recall incomplete missing %v", r.CaseID, m.Recall.MissingIDs)
			}
			if !m.Grounding.Complete {
				t.Errorf("case %s ready-path grounding incomplete fabricated %v", r.CaseID, m.Grounding.FabricatedIDs)
			}
		}
		// Every result should have HardGates passed in full harness.
		if !m.HardGates.Passed {
			t.Errorf("case %s hard gates should pass in harness, failures %v", r.CaseID, m.HardGates.Failures)
		}
	}
	// Stress determinism: two full runs DeepEqual.
	report3 := EvaluateE1All(results)
	if !reflect.DeepEqual(report, report3) {
		t.Fatalf("full-corpus report not deterministic")
	}
}

func TestE1EscalatedRecallNotCompleteButHardGatePasses(t *testing.T) {
	// Escalated case with no report and no tool growth: recall incomplete but hard gates pass.
	r := EvalResult{
		CaseID:             "I",
		KnownUniverse:      []string{"ev-doc-01", "ev-doc-02", "ev-pol-01"},
		DeclaredTools:      []invest.ToolName{invest.ToolGetEvidence},
		ToolCalls:          []ToolCallObs{},
		Report:             nil,
		Attempts:           []string{},
		BudgetMaxToolCalls: 5,
		BudgetMaxTurns:     12,
		Outcome:            orchestrate.OutcomeEscalated,
		EscalationReason:   orchestrate.EscalationCallsExhausted,
	}
	m := EvaluateE1(r)
	// Recall should be incomplete because union is empty.
	if m.Recall.Complete {
		t.Fatalf("escalated recall should be incomplete")
	}
	// But grounding vacuously complete and hard gates pass.
	if !m.Grounding.Complete {
		t.Fatalf("grounding should be vacuously complete when no report")
	}
	if !m.HardGates.Passed {
		t.Fatalf("hard gates should pass for escalated no-report, got %v", m.HardGates.Failures)
	}
}

func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
