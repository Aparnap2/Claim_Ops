package invest

import (
	"context"
	"testing"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate/orchestrate"
)

// e2BaseCase returns a minimal ready-path case.
func e2BaseCase() EvalCase {
	return EvalCase{
		ID:           "SYN",
		Name:         "synthetic",
		MustEscalate: false,
	}
}

// e2BasePassResult is a synthetic all-green EvalResult for E2.
// It carries both ReportObs and ReportRaw so each dimension can be checked
// with structure, not prose.
func e2BasePassResult() EvalResult {
	raw := orchestrate.Report{
		Hypotheses: []invest.Hypothesis{
			{
				ID:          "h-01",
				Statement:   "The exception is explained by the cited evidence.",
				Falsifier:   "Evidence showing the exception does not hold.",
				Status:      invest.HypothesisOpen,
				EvidenceIDs: []string{"ev-doc-01"},
				FactRefs: []invest.FactRef{
					{Key: "hospital_name", Agreed: "City Hospital", EvidenceID: "ev-doc-01"},
				},
			},
		},
		Findings: []invest.Finding{
			{ID: "f-01", HypothesisID: "h-01", Summary: "Evidence shows exception.", EvidenceIDs: []string{"ev-doc-01"}},
		},
		Recommendation: invest.Recommendation{
			Action:     invest.RecommendReferHuman,
			Rationale:  "Rationale.",
			FindingIDs: []string{"f-01"},
		},
		MissingAdditive: []invest.MissingItem{},
	}
	obs := &ReportObs{
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
	}
	return EvalResult{
		CaseID:             "SYN",
		Outcome:            orchestrate.OutcomeReportReady,
		KnownUniverse:      []string{"ev-doc-01"},
		DeclaredTools:      []invest.ToolName{invest.ToolGetEvidence},
		Report:             obs,
		ReportRaw:          &raw,
		Attempts:           []string{},
		BudgetMaxToolCalls: 5,
		BudgetMaxTurns:     12,
		RunCompleted:       true,
		LogOrdered:         true,
	}
}

func e2Verdict(t *testing.T, c EvalCase, r EvalResult, dim string) E2Result {
	t.Helper()
	for _, g := range EvaluateE2(c, r) {
		if g.Dimension == dim {
			return g
		}
	}
	t.Fatalf("dimension %q missing", dim)
	return E2Result{}
}

func assertE2Pass(t *testing.T, c EvalCase, r EvalResult, dim string) {
	t.Helper()
	if g := e2Verdict(t, c, r, dim); !g.Passed {
		t.Errorf("dimension %s pass fixture failed: %s", dim, g.Detail)
	}
}

func assertE2Fail(t *testing.T, c EvalCase, r EvalResult, dim string) {
	t.Helper()
	if g := e2Verdict(t, c, r, dim); g.Passed {
		t.Errorf("dimension %s fail fixture passed, want failure", dim)
	}
}

func TestE2DimensionsPassFail(t *testing.T) {
	// hypothesis quality: ID + falsifier present + statement non-blank
	t.Run(DimHypothesisQuality, func(t *testing.T) {
		c := e2BaseCase()
		base := e2BasePassResult()
		assertE2Pass(t, c, base, DimHypothesisQuality)

		failBlankStatement := e2BasePassResult()
		failBlankStatement.ReportRaw.Hypotheses[0].Statement = "   "
		assertE2Fail(t, c, failBlankStatement, DimHypothesisQuality)

		failBlankFalsifier := e2BasePassResult()
		failBlankFalsifier.ReportRaw.Hypotheses[0].Falsifier = ""
		assertE2Fail(t, c, failBlankFalsifier, DimHypothesisQuality)

		failBlankID := e2BasePassResult()
		failBlankID.ReportRaw.Hypotheses[0].ID = ""
		assertE2Fail(t, c, failBlankID, DimHypothesisQuality)
	})

	// falsifier quality: falsifier non-blank, distinct from statement
	t.Run(DimFalsifierQuality, func(t *testing.T) {
		c := e2BaseCase()
		base := e2BasePassResult()
		assertE2Pass(t, c, base, DimFalsifierQuality)

		failIdentical := e2BasePassResult()
		failIdentical.ReportRaw.Hypotheses[0].Falsifier = failIdentical.ReportRaw.Hypotheses[0].Statement
		assertE2Fail(t, c, failIdentical, DimFalsifierQuality)

		failBlank := e2BasePassResult()
		failBlank.ReportRaw.Hypotheses[0].Falsifier = "   "
		assertE2Fail(t, c, failBlank, DimFalsifierQuality)
	})

	// evidence grounding: finding EvidenceIDs ⊆ KnownUniverse, hypothesis FactRefs ⊆ KnownUniverse
	t.Run(DimEvidenceGrounding, func(t *testing.T) {
		c := e2BaseCase()
		base := e2BasePassResult()
		assertE2Pass(t, c, base, DimEvidenceGrounding)

		failFinding := e2BasePassResult()
		failFinding.ReportRaw.Findings[0].EvidenceIDs = []string{"ev-ghost-99"}
		assertE2Fail(t, c, failFinding, DimEvidenceGrounding)

		failFactRef := e2BasePassResult()
		failFactRef.ReportRaw.Hypotheses[0].FactRefs[0].EvidenceID = "ev-ghost-99"
		assertE2Fail(t, c, failFactRef, DimEvidenceGrounding)

		failHypEvidence := e2BasePassResult()
		failHypEvidence.ReportRaw.Hypotheses[0].EvidenceIDs = []string{"ev-ghost-99"}
		assertE2Fail(t, c, failHypEvidence, DimEvidenceGrounding)
	})

	// contradiction handling: cases B/E/G must cover conflicting evidence
	t.Run(DimContradictionHandling, func(t *testing.T) {
		// Non-contradiction case vacuously passes even with minimal report.
		cSyn := e2BaseCase()
		base := e2BasePassResult()
		assertE2Pass(t, cSyn, base, DimContradictionHandling)

		// Real contradiction case B: require covering conflict IDs.
		// Use the corpus case B envelope to get conflict IDs.
		corpusB := caseB()
		// Build a passing result via harness? Instead synthetic pass that covers.
		pass := e2BasePassResult()
		// Need to set CaseID to B and KnownUniverse to match corpusB universe.
		pass.CaseID = corpusB.ID
		pass.KnownUniverse = append([]string(nil), corpusB.ExpectedEvidenceIDs...)
		// Make cited set cover all ExpectedEvidenceIDs (honest report).
		raw := orchestrate.Report{
			Hypotheses: []invest.Hypothesis{
				{ID: "h-01", Statement: "s", Falsifier: "f distinct", Status: invest.HypothesisOpen, EvidenceIDs: pass.KnownUniverse},
			},
			Findings: []invest.Finding{
				{ID: "f-01", HypothesisID: "h-01", Summary: "summ", EvidenceIDs: pass.KnownUniverse},
			},
			Recommendation: invest.Recommendation{Action: invest.RecommendReferHuman, Rationale: "r", FindingIDs: []string{"f-01"}},
		}
		obs := &ReportObs{
			HypothesisIDs:        []string{"h-01"},
			MissingFalsifier:     []string{},
			FindingIDs:           []string{"f-01"},
			DanglingFindings:     []string{},
			RecommendationAction: string(invest.RecommendReferHuman),
			RecommendationValid:  true,
			CitedEvidenceIDs:     append([]string(nil), pass.KnownUniverse...),
		}
		pass.Report = obs
		pass.ReportRaw = &raw
		pass.Outcome = orchestrate.OutcomeReportReady
		assertE2Pass(t, corpusB, pass, DimContradictionHandling)

		// Fail: missing conflicting evidence (only ev-doc-01, but envelope expects ev-doc-02 as well)
		fail := pass
		// Deep copy raw to avoid alias
		rawFail := raw
		rawFail.Hypotheses[0].EvidenceIDs = []string{"ev-doc-01"}
		rawFail.Findings[0].EvidenceIDs = []string{"ev-doc-01"}
		obsFail := *obs
		obsFail.CitedEvidenceIDs = []string{"ev-doc-01"}
		fail.ReportRaw = &rawFail
		fail.Report = &obsFail
		assertE2Fail(t, corpusB, fail, DimContradictionHandling)

		// Also test E and G similarly: they should require coverage.
		for _, letter := range []string{"E", "G"} {
			var cc EvalCase
			switch letter {
			case "E":
				cc = caseE()
			case "G":
				cc = caseG()
			}
			// Build a passing report whose evidence covers this case's
			// actual conflict sources (B's pass covers B, but E/G have
			// distinct conflicts after the C1 honesty fix).
			passG := pass
			passG.KnownUniverse = append([]string(nil), cc.ExpectedEvidenceIDs...)
			passG.Report.CitedEvidenceIDs = append([]string(nil), cc.ExpectedEvidenceIDs...)
			passG.ReportRaw = &orchestrate.Report{
				Hypotheses: []invest.Hypothesis{
					{ID: "h-01", Statement: "s", Falsifier: "f distinct", Status: invest.HypothesisOpen, EvidenceIDs: cc.ExpectedEvidenceIDs},
				},
				Findings: []invest.Finding{
					{ID: "f-01", HypothesisID: "h-01", Summary: "summ", EvidenceIDs: cc.ExpectedEvidenceIDs},
				},
				Recommendation: invest.Recommendation{Action: invest.RecommendReferHuman, Rationale: "r", FindingIDs: []string{"f-01"}},
			}
			assertE2Pass(t, cc, passG, DimContradictionHandling)
			assertE2Fail(t, cc, fail, DimContradictionHandling)
		}

		// No-report for contradiction case should fail.
		noReport := e2BasePassResult()
		noReport.CaseID = corpusB.ID
		noReport.Report = nil
		noReport.ReportRaw = nil
		noReport.Outcome = orchestrate.OutcomeEscalated
		noReport.EscalationReason = orchestrate.EscalationInvalidOutput
		// B is not MustEscalate, so no-report should fail contradiction handling.
		assertE2Fail(t, corpusB, noReport, DimContradictionHandling)
	})

	// finding quality: FindingID→HypothesisID resolution, Summary non-blank
	t.Run(DimFindingQuality, func(t *testing.T) {
		c := e2BaseCase()
		base := e2BasePassResult()
		assertE2Pass(t, c, base, DimFindingQuality)

		failDangling := e2BasePassResult()
		failDangling.ReportRaw.Findings[0].HypothesisID = "h-99"
		assertE2Fail(t, c, failDangling, DimFindingQuality)

		failBlankSummary := e2BasePassResult()
		failBlankSummary.ReportRaw.Findings[0].Summary = "   "
		assertE2Fail(t, c, failBlankSummary, DimFindingQuality)

		failNoEvidence := e2BasePassResult()
		failNoEvidence.ReportRaw.Findings[0].EvidenceIDs = []string{}
		assertE2Fail(t, c, failNoEvidence, DimFindingQuality)
	})

	// recommendation validity: closed enum, FindingIDs grounded
	t.Run(DimRecommendationValidity, func(t *testing.T) {
		c := e2BaseCase()
		base := e2BasePassResult()
		assertE2Pass(t, c, base, DimRecommendationValidity)

		failEnum := e2BasePassResult()
		failEnum.ReportRaw.Recommendation.Action = invest.RecommendationAction("APPROVE")
		assertE2Fail(t, c, failEnum, DimRecommendationValidity)

		failFindingRef := e2BasePassResult()
		failFindingRef.ReportRaw.Recommendation.FindingIDs = []string{"f-99"}
		assertE2Fail(t, c, failFindingRef, DimRecommendationValidity)

		// Also test via ReportObs fallback.
		failObs := e2BasePassResult()
		failObs.ReportRaw = nil
		failObs.Report.RecommendationValid = false
		assertE2Fail(t, c, failObs, DimRecommendationValidity)
	})

	// escalation appropriateness: MustEscalate ↔ escalated + reason match; INSUFFICIENT_EVIDENCE as valid success
	t.Run(DimEscalationAppropriateness, func(t *testing.T) {
		// Ready-path passes when REPORT_READY.
		cReady := e2BaseCase()
		cReady.MustEscalate = false
		base := e2BasePassResult()
		base.Outcome = orchestrate.OutcomeReportReady
		assertE2Pass(t, cReady, base, DimEscalationAppropriateness)

		// Ready-path fails when escalated.
		failReadyEscalated := e2BasePassResult()
		failReadyEscalated.Outcome = orchestrate.OutcomeEscalated
		failReadyEscalated.EscalationReason = orchestrate.EscalationInvalidOutput
		failReadyEscalated.Report = nil
		failReadyEscalated.ReportRaw = nil
		assertE2Fail(t, cReady, failReadyEscalated, DimEscalationAppropriateness)

		// MustEscalate passes when escalated with correct reason.
		cEsc := e2BaseCase()
		cEsc.MustEscalate = true
		cEsc.MustEscalateReason = orchestrate.EscalationInvalidOutput
		esc := e2BasePassResult()
		esc.Outcome = orchestrate.OutcomeEscalated
		esc.EscalationReason = orchestrate.EscalationInvalidOutput
		esc.Report = nil
		esc.ReportRaw = nil
		assertE2Pass(t, cEsc, esc, DimEscalationAppropriateness)

		// MustEscalate fails when reason mismatches.
		escWrong := esc
		escWrong.EscalationReason = orchestrate.EscalationNoProgress
		assertE2Fail(t, cEsc, escWrong, DimEscalationAppropriateness)

		// MustEscalate fails when REPORT_READY without INSUFFICIENT_EVIDENCE.
		escReport := e2BasePassResult()
		escReport.Outcome = orchestrate.OutcomeReportReady
		// Recommendation is REFER_HUMAN, not REQUEST_EVIDENCE, so should fail.
		assertE2Fail(t, cEsc, escReport, DimEscalationAppropriateness)

		// INSUFFICIENT_EVIDENCE via REQUEST_EVIDENCE as valid success.
		insufficient := e2BasePassResult()
		insufficient.Outcome = orchestrate.OutcomeReportReady
		insufficient.ReportRaw.Recommendation.Action = invest.RecommendRequestEvidence
		insufficient.Report.RecommendationAction = string(invest.RecommendRequestEvidence)
		assertE2Pass(t, cEsc, insufficient, DimEscalationAppropriateness)

		// INSUFFICIENT_EVIDENCE escalation reason as valid success.
		insufficientEsc := esc
		insufficientEsc.EscalationReason = orchestrate.EscalationReason("INSUFFICIENT_EVIDENCE")
		assertE2Pass(t, cEsc, insufficientEsc, DimEscalationAppropriateness)
	})
}

// TestE2NoSingleScoreCollapse asserts dimensions are separately observable
// and no collapsed 0-100 score exists.
func TestE2NoSingleScoreCollapse(t *testing.T) {
	c := e2BaseCase()
	r := e2BasePassResult()
	results := EvaluateE2(c, r)
	if len(results) != 7 {
		t.Fatalf("EvaluateE2 returned %d dimensions, want 7", len(results))
	}
	if len(AllE2Dimensions()) != 7 {
		t.Fatalf("AllE2Dimensions len = %d, want 7", len(AllE2Dimensions()))
	}
	// Ensure each dimension is distinct.
	seen := make(map[string]struct{}, len(results))
	for _, g := range results {
		if _, dup := seen[g.Dimension]; dup {
			t.Fatalf("duplicate dimension %q", g.Dimension)
		}
		seen[g.Dimension] = struct{}{}
	}
	// Ensure no single score field exists: E2Result must not have Score.
	// We verify by checking that EvaluateE2 does not collapse: mutating one
	// dimension failure must not affect other dimensions' Passed values in a
	// way that suggests a shared score.
	failOne := e2BasePassResult()
	failOne.ReportRaw.Hypotheses[0].Falsifier = failOne.ReportRaw.Hypotheses[0].Statement // only falsifier dimension fails
	resOne := EvaluateE2(c, failOne)
	var falsifierFailed, hypothesisPassed bool
	for _, g := range resOne {
		if g.Dimension == DimFalsifierQuality && !g.Passed {
			falsifierFailed = true
		}
		if g.Dimension == DimHypothesisQuality && g.Passed {
			hypothesisPassed = true
		}
	}
	if !falsifierFailed {
		t.Errorf("expected falsifier quality to fail")
	}
	if !hypothesisPassed {
		t.Errorf("hypothesis quality should still pass when only falsifier distinctness fails; dimensions must be independent")
	}
}

// TestE2FullCorpusViaHarness runs E2 over all real harness cases.
// Every honest case should pass all 7 dimensions; every MustEscalate case
// should pass escalation appropriateness and vacuously pass other dims.
func TestE2FullCorpusViaHarness(t *testing.T) {
	h := Harness{}
	var runs []CaseRun
	for _, c := range AllCases() {
		res, err := h.Run(context.Background(), c)
		if err != nil {
			t.Fatalf("case %s Harness.Run error: %v", c.ID, err)
		}
		runs = append(runs, CaseRun{Case: c, Result: res})
		// Per-case: every E2 dimension must pass (honest reports are grounded,
		// esca cases are appropriate escalations).
		for _, g := range EvaluateE2(c, res) {
			if !g.Passed {
				t.Errorf("case %s dimension %s: %s (outcome %q reason %q)", c.ID, g.Dimension, g.Detail, string(res.Outcome), string(res.EscalationReason))
			}
		}
	}
	summary := RunE2All(runs)
	if !summary.Passed {
		for _, f := range summary.Failures {
			t.Errorf("RunE2All: %s", f)
		}
	}
}

// TestE2InsufficientEvidenceKeptAsSuccess ensures INSUFFICIENT_EVIDENCE
// is not treated as failure.
func TestE2InsufficientEvidenceKeptAsSuccess(t *testing.T) {
	c := e2BaseCase()
	c.MustEscalate = true
	c.MustEscalateReason = orchestrate.EscalationCallsExhausted
	// Simulate a model that returned REQUEST_EVIDENCE instead of escalating.
	r := e2BasePassResult()
	r.CaseID = c.ID
	r.Outcome = orchestrate.OutcomeReportReady
	r.ReportRaw.Recommendation.Action = invest.RecommendRequestEvidence
	r.Report.RecommendationAction = string(invest.RecommendRequestEvidence)
	results := EvaluateE2(c, r)
	for _, g := range results {
		if g.Dimension == DimEscalationAppropriateness && !g.Passed {
			t.Fatalf("INSUFFICIENT_EVIDENCE via REQUEST_EVIDENCE should be success, got failure: %s", g.Detail)
		}
	}
}
