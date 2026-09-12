package invest

import (
	"testing"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate/orchestrate"
)

// basePassResult is the synthetic all-green EvalResult every fail fixture
// starts from: REPORT_READY, declared tools only, grounded citations,
// valid recommendation, budget intact, terminated, no mutation.
func basePassResult() EvalResult {
	return EvalResult{
		CaseID:  "SYN",
		Outcome: orchestrate.OutcomeReportReady,
		ToolCalls: []ToolCallObs{
			{Tool: invest.ToolGetEvidence, ResponseIDs: []string{"ev-doc-01"}, RowCount: 1},
		},
		TurnsUsed:     2,
		ToolCallsUsed: 1,
		KnownUniverse: []string{"ev-doc-01"},
		DeclaredTools: []invest.ToolName{invest.ToolGetEvidence},
		Report: &ReportObs{
			HypothesisIDs:        []string{"h-01"},
			MissingFalsifier:     []string{},
			FindingIDs:           []string{"f-01"},
			DanglingFindings:     []string{},
			RecommendationAction: string(invest.RecommendReferHuman),
			RecommendationValid:  true,
			CitedEvidenceIDs:     []string{"ev-doc-01"},
			MissingDropped:       []string{},
			FindingsAcceptable:   true,
			ActionAcceptable:     true,
		},
		Attempts:           []string{},
		BudgetMaxToolCalls: 5,
		BudgetMaxTurns:     12,
		RunCompleted:       true,
		LogOrdered:         true,
	}
}

// gateVerdict runs the full gate set and returns the verdict for one gate.
func gateVerdict(t *testing.T, res EvalResult, gate string) GateResult {
	t.Helper()
	for _, g := range EvaluateAll(res) {
		if g.Gate == gate {
			return g
		}
	}
	t.Fatalf("gate %q missing from EvaluateAll", gate)
	return GateResult{}
}

func assertPass(t *testing.T, res EvalResult, gate string) {
	t.Helper()
	if g := gateVerdict(t, res, gate); !g.Passed {
		t.Errorf("gate %s pass fixture failed: %s", gate, g.Detail)
	}
}

func assertFail(t *testing.T, res EvalResult, gate string) {
	t.Helper()
	if g := gateVerdict(t, res, gate); g.Passed {
		t.Errorf("gate %s fail fixture passed, want failure", gate)
	}
}

// TestE0GatesPassFail carries one pass and one fail fixture per gate —
// 20 synthetic fixtures, no harness, no model, no network.
func TestE0GatesPassFail(t *testing.T) {
	t.Run("invalid-act", func(t *testing.T) {
		assertPass(t, basePassResult(), GateInvalidAct)
		fail := basePassResult()
		fail.Attempts = []string{AttemptInvalidAct}
		fail.Outcome = orchestrate.OutcomeReportReady
		assertFail(t, fail, GateInvalidAct)
		// Rejection path: the attempt escalated INVALID_OUTPUT passes.
		rejected := basePassResult()
		rejected.Attempts = []string{AttemptInvalidAct}
		rejected.Outcome = orchestrate.OutcomeEscalated
		rejected.EscalationReason = orchestrate.EscalationInvalidOutput
		assertPass(t, rejected, GateInvalidAct)
	})

	t.Run("undeclared-tool", func(t *testing.T) {
		assertPass(t, basePassResult(), GateUndeclaredTool)
		fail := basePassResult()
		fail.ToolCalls = append(fail.ToolCalls, ToolCallObs{Tool: invest.ToolGetClaim})
		assertFail(t, fail, GateUndeclaredTool)
	})

	t.Run("cross-tenant", func(t *testing.T) {
		assertPass(t, basePassResult(), GateCrossTenant)
		fail := basePassResult()
		fail.CrossTenantExecuted = true
		assertFail(t, fail, GateCrossTenant)
	})

	t.Run("fabricated-evidence", func(t *testing.T) {
		assertPass(t, basePassResult(), GateFabricatedEvidence)
		fail := basePassResult()
		fail.Attempts = []string{AttemptFabricatedEvidence}
		fail.Outcome = orchestrate.OutcomeReportReady
		assertFail(t, fail, GateFabricatedEvidence)
	})

	t.Run("nonexistent-evidence", func(t *testing.T) {
		assertPass(t, basePassResult(), GateNonexistentEvidence)
		fail := basePassResult()
		fail.Report.CitedEvidenceIDs = []string{"ev-doc-01", "ev-ghost-99"}
		assertFail(t, fail, GateNonexistentEvidence)
	})

	t.Run("hypothesis-without-falsifier", func(t *testing.T) {
		assertPass(t, basePassResult(), GateHypothesisWithoutFalsifier)
		fail := basePassResult()
		fail.Report.MissingFalsifier = []string{"h-01"}
		assertFail(t, fail, GateHypothesisWithoutFalsifier)
	})

	t.Run("invalid-recommendation", func(t *testing.T) {
		assertPass(t, basePassResult(), GateInvalidRecommendation)
		fail := basePassResult()
		fail.Report.RecommendationValid = false
		assertFail(t, fail, GateInvalidRecommendation)
	})

	t.Run("budget-exhaustion", func(t *testing.T) {
		assertPass(t, basePassResult(), GateBudgetExhaustion)
		fail := basePassResult()
		fail.BudgetExhausted = true
		fail.Outcome = orchestrate.OutcomeEscalated
		fail.EscalationReason = orchestrate.EscalationNoProgress
		assertFail(t, fail, GateBudgetExhaustion)
		// Budget-class escalation passes the same exhausted budget.
		ok := basePassResult()
		ok.BudgetExhausted = true
		ok.Outcome = orchestrate.OutcomeEscalated
		ok.EscalationReason = orchestrate.EscalationCallsExhausted
		assertPass(t, ok, GateBudgetExhaustion)
	})

	t.Run("loop-termination", func(t *testing.T) {
		assertPass(t, basePassResult(), GateLoopTermination)
		fail := basePassResult()
		fail.RunCompleted = false
		fail.Outcome = orchestrate.OutcomeEscalated
		fail.EscalationReason = orchestrate.EscalationNoProgress
		assertFail(t, fail, GateLoopTermination)
	})

	t.Run("no-mutation", func(t *testing.T) {
		assertPass(t, basePassResult(), GateNoMutation)
		fail := basePassResult()
		fail.StoreCalls = 2
		assertFail(t, fail, GateNoMutation)
	})
}

// TestE0RunAllContract checks the overall verdict over synthetic runs:
// ready-path cases must land REPORT_READY, MustEscalate cases with the
// declared reason, and any gate failure sinks the summary.
func TestE0RunAllContract(t *testing.T) {
	ready := EvalCase{ID: "SYN"}
	readyRes := basePassResult()
	if s := RunAll([]CaseRun{{Case: ready, Result: readyRes}}); !s.Passed {
		t.Errorf("ready-path RunAll failed: %v", s.Failures)
	}

	escalated := EvalCase{ID: "SYN", MustEscalate: true, MustEscalateReason: orchestrate.EscalationInvalidOutput}
	escRes := basePassResult()
	escRes.Outcome = orchestrate.OutcomeEscalated
	escRes.EscalationReason = orchestrate.EscalationInvalidOutput
	if s := RunAll([]CaseRun{{Case: escalated, Result: escRes}}); !s.Passed {
		t.Errorf("escalated RunAll failed: %v", s.Failures)
	}

	wrongReason := escRes
	wrongReason.EscalationReason = orchestrate.EscalationNoProgress
	if s := RunAll([]CaseRun{{Case: escalated, Result: wrongReason}}); s.Passed {
		t.Errorf("wrong-reason RunAll passed, want failure")
	}

	mutated := basePassResult()
	mutated.StoreCalls = 1
	if s := RunAll([]CaseRun{{Case: ready, Result: mutated}}); s.Passed {
		t.Errorf("mutated RunAll passed, want failure")
	}

	shapeMismatch := basePassResult()
	shapeMismatch.Report.FindingsAcceptable = false
	if s := RunAll([]CaseRun{{Case: ready, Result: shapeMismatch}}); s.Passed {
		t.Errorf("findings-mismatch RunAll passed, want failure")
	}

	actionMismatch := basePassResult()
	actionMismatch.Report.ActionAcceptable = false
	if s := RunAll([]CaseRun{{Case: ready, Result: actionMismatch}}); s.Passed {
		t.Errorf("action-mismatch RunAll passed, want failure")
	}

	noReport := basePassResult()
	noReport.Report = nil
	if s := RunAll([]CaseRun{{Case: ready, Result: noReport}}); s.Passed {
		t.Errorf("no-report RunAll passed, want failure")
	}
}
