package invest

import (
	"math"
	"reflect"
	"slices"
	"testing"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate/orchestrate"
)

func e3BaseResult(caseID string) EvalResult {
	return EvalResult{
		CaseID:             caseID,
		KnownUniverse:      []string{"ev-doc-01", "ev-doc-02"},
		DeclaredTools:      []invest.ToolName{invest.ToolGetEvidence},
		ToolCalls:          []ToolCallObs{{Tool: invest.ToolGetEvidence, ErrorCode: ""}},
		Report:             &ReportObs{CitedEvidenceIDs: []string{"ev-doc-01"}},
		Outcome:            orchestrate.OutcomeReportReady,
		EscalationReason:   "",
		TurnsUsed:          2,
		ToolCallsUsed:      1,
		BudgetMaxToolCalls: 5,
		BudgetMaxTurns:     12,
		LatencyMs:          10,
		RepeatObserved:     false,
	}
}

func TestE3ValidBudgets(t *testing.T) {
	results := []EvalResult{e3BaseResult("A"), e3BaseResult("B")}
	report, err := ComputeE3(results)
	if err != nil {
		t.Fatalf("valid budgets should not error: %v", err)
	}
	if report.Summary.Total != 2 {
		t.Fatalf("total %d want 2", report.Summary.Total)
	}
	if len(report.PerCase) != 2 {
		t.Fatalf("per-case len %d want 2", len(report.PerCase))
	}
	// Deterministic ordering: PerCase sorted by CaseID.
	if report.PerCase[0].CaseID != "A" || report.PerCase[1].CaseID != "B" {
		t.Fatalf("per-case not sorted by CaseID: %v", report.PerCase)
	}
}

func TestE3InvalidBudgets(t *testing.T) {
	t.Run("empty results", func(t *testing.T) {
		if _, err := ComputeE3(nil); err == nil {
			t.Fatalf("empty results should error")
		}
		if _, err := ComputeE3([]EvalResult{}); err == nil {
			t.Fatalf("empty slice should error")
		}
	})
	t.Run("zero tool budget", func(t *testing.T) {
		r := e3BaseResult("A")
		r.BudgetMaxToolCalls = 0
		if _, err := ComputeE3([]EvalResult{r}); err == nil {
			t.Fatalf("zero tool budget should error")
		}
	})
	t.Run("zero turn budget", func(t *testing.T) {
		r := e3BaseResult("A")
		r.BudgetMaxTurns = 0
		if _, err := ComputeE3([]EvalResult{r}); err == nil {
			t.Fatalf("zero turn budget should error")
		}
	})
	t.Run("negative usage", func(t *testing.T) {
		r := e3BaseResult("A")
		r.TurnsUsed = -1
		if _, err := ComputeE3([]EvalResult{r}); err == nil {
			t.Fatalf("negative turns should error")
		}
		r = e3BaseResult("A")
		r.ToolCallsUsed = -1
		if _, err := ComputeE3([]EvalResult{r}); err == nil {
			t.Fatalf("negative tool calls should error")
		}
		r = e3BaseResult("A")
		r.LatencyMs = -1
		if _, err := ComputeE3([]EvalResult{r}); err == nil {
			t.Fatalf("negative latency should error")
		}
	})
}

func TestE3RepeatedDetection(t *testing.T) {
	a := e3BaseResult("A")
	a.RepeatObserved = true
	b := e3BaseResult("B")
	b.RepeatObserved = false
	c := e3BaseResult("C")
	c.RepeatObserved = true
	report, err := ComputeE3([]EvalResult{a, b, c})
	if err != nil {
		t.Fatalf("ComputeE3 error: %v", err)
	}
	if report.Summary.RepeatedCount != 2 {
		t.Fatalf("repeated count %d want 2", report.Summary.RepeatedCount)
	}
	// Per-case flag preserved and sorted.
	if !report.PerCase[0].Repeated || report.PerCase[1].Repeated || !report.PerCase[2].Repeated {
		t.Fatalf("repeated flags not correctly tracked: %v", report.PerCase)
	}
	// Deterministic: two calls equal.
	report2, _ := ComputeE3([]EvalResult{a, b, c})
	if !reflect.DeepEqual(report, report2) {
		t.Fatalf("repeated detection not deterministic")
	}
}

func TestE3MedianP95KnownFixture(t *testing.T) {
	// Fixture 1: odd n=5 turns [10,20,30,40,50] -> median 30, p95 is last (50), min 10 max 50 mean 30.
	vals := []int64{10, 20, 30, 40, 50}
	var results []EvalResult
	for i, v := range vals {
		r := e3BaseResult(string(rune('A' + i)))
		r.TurnsUsed = int(v)
		r.ToolCallsUsed = int(v)
		r.LatencyMs = v * 10
		results = append(results, r)
	}
	report, err := ComputeE3(results)
	if err != nil {
		t.Fatalf("ComputeE3 error: %v", err)
	}
	if report.Summary.Turns.Median != 30 {
		t.Fatalf("median %v want 30", report.Summary.Turns.Median)
	}
	if report.Summary.Turns.P50 != 30 {
		t.Fatalf("p50 %v want 30", report.Summary.Turns.P50)
	}
	if report.Summary.Turns.P95 != 50 {
		t.Fatalf("p95 %d want 50", report.Summary.Turns.P95)
	}
	if report.Summary.Turns.Min != 10 || report.Summary.Turns.Max != 50 {
		t.Fatalf("min %d max %d want 10/50", report.Summary.Turns.Min, report.Summary.Turns.Max)
	}
	if math.Abs(report.Summary.Turns.Mean-30) > 1e-9 {
		t.Fatalf("mean %v want 30", report.Summary.Turns.Mean)
	}
	if report.Summary.ToolCalls.Median != 30 {
		t.Fatalf("tool calls median %v want 30", report.Summary.ToolCalls.Median)
	}

	// Fixture 2: even n=4 [10,20,30,40] -> median (20+30)/2=25, p95 ceil(0.95*4)=ceil(3.8)=4 idx3 => 40.
	even := []int64{10, 20, 30, 40}
	results = nil
	for i, v := range even {
		r := e3BaseResult(string(rune('A' + i)))
		r.TurnsUsed = int(v)
		r.ToolCallsUsed = int(v)
		r.LatencyMs = v
		results = append(results, r)
	}
	report, _ = ComputeE3(results)
	if report.Summary.Turns.Median != 25 {
		t.Fatalf("even median %v want 25", report.Summary.Turns.Median)
	}
	if report.Summary.Turns.P95 != 40 {
		t.Fatalf("even p95 %d want 40", report.Summary.Turns.P95)
	}

	// Fixture 3: n=20 p95 is ceil(0.95*20)=19 idx18 => 19th smallest (1-index 19).
	// Values 1..20 sorted => p95 =19.
	results = nil
	for i := 1; i <= 20; i++ {
		r := e3BaseResult(string(rune('A'+i%26)) + string(rune('0'+i/26)))
		r.CaseID = string(rune('A'+i)) + string(rune('0'+i)) // unique
		r.TurnsUsed = i
		r.ToolCallsUsed = i
		r.LatencyMs = int64(i)
		results = append(results, r)
	}
	report, _ = ComputeE3(results)
	if report.Summary.Turns.P95 != 19 {
		t.Fatalf("n=20 p95 %d want 19", report.Summary.Turns.P95)
	}
	if report.Summary.Turns.Median != 10.5 {
		t.Fatalf("n=20 median %v want 10.5", report.Summary.Turns.Median)
	}

	// Deterministic: second run matches.
	report2, _ := ComputeE3(results)
	if !reflect.DeepEqual(report.Summary.Turns, report2.Summary.Turns) {
		t.Fatalf("quantiles not deterministic")
	}
	// Ensure PerCase sorted regardless of input order shuffled.
	shuffled := append([]EvalResult(nil), results...)
	slices.Reverse(shuffled)
	reportShuffled, _ := ComputeE3(shuffled)
	if !reflect.DeepEqual(report.Summary, reportShuffled.Summary) {
		t.Fatalf("summary should be order-independent")
	}
	if !slices.IsSortedFunc(reportShuffled.PerCase, func(a, b E3CaseMetrics) int {
		if a.CaseID < b.CaseID {
			return -1
		}
		if a.CaseID > b.CaseID {
			return 1
		}
		return 0
	}) {
		t.Fatalf("per-case not sorted")
	}
}

func TestE3EscalationRate(t *testing.T) {
	a := e3BaseResult("A")
	a.Outcome = orchestrate.OutcomeReportReady
	b := e3BaseResult("B")
	b.Outcome = orchestrate.OutcomeEscalated
	b.EscalationReason = orchestrate.EscalationInvalidOutput
	c := e3BaseResult("C")
	c.Outcome = orchestrate.OutcomeEscalated
	c.EscalationReason = orchestrate.EscalationRepetition
	d := e3BaseResult("D")
	d.Outcome = orchestrate.OutcomeReportReady
	report, _ := ComputeE3([]EvalResult{a, b, c, d})
	if report.Summary.Escalated != 2 {
		t.Fatalf("escalated %d want 2", report.Summary.Escalated)
	}
	if math.Abs(report.Summary.EscalationRate-0.5) > 1e-9 {
		t.Fatalf("escalation rate %v want 0.5", report.Summary.EscalationRate)
	}
	if report.PerCase[1].IsEscalated != true || report.PerCase[0].IsEscalated != false {
		t.Fatalf("is escalated flags incorrect")
	}
}

func TestE3FailedCalls(t *testing.T) {
	a := e3BaseResult("A")
	a.ToolCalls = []ToolCallObs{
		{Tool: invest.ToolGetEvidence, ErrorCode: ""},
		{Tool: invest.ToolGetEvidence, ErrorCode: "CONTRACT"},
		{Tool: invest.ToolGetEvidence, ErrorCode: ""},
	}
	b := e3BaseResult("B")
	b.ToolCalls = []ToolCallObs{
		{Tool: invest.ToolGetEvidence, ErrorCode: "UPSTREAM"},
	}
	report, _ := ComputeE3([]EvalResult{a, b})
	if report.Summary.FailedToolCallsTotal != 2 {
		t.Fatalf("failed total %d want 2", report.Summary.FailedToolCallsTotal)
	}
	if report.PerCase[0].FailedToolCalls != 1 {
		t.Fatalf("case A failed %d want 1", report.PerCase[0].FailedToolCalls)
	}
	if report.PerCase[1].FailedToolCalls != 1 {
		t.Fatalf("case B failed %d want 1", report.PerCase[1].FailedToolCalls)
	}
}

func TestE3NoProgressTermination(t *testing.T) {
	a := e3BaseResult("A")
	a.Outcome = orchestrate.OutcomeEscalated
	a.EscalationReason = orchestrate.EscalationNoProgress
	b := e3BaseResult("B")
	b.Outcome = orchestrate.OutcomeEscalated
	b.EscalationReason = orchestrate.EscalationCallsExhausted
	c := e3BaseResult("C")
	c.Outcome = orchestrate.OutcomeReportReady
	report, _ := ComputeE3([]EvalResult{a, b, c})
	if report.Summary.NoProgressCount != 1 {
		t.Fatalf("no-progress count %d want 1", report.Summary.NoProgressCount)
	}
	// Per-case IsNoProgress.
	if !report.PerCase[0].IsNoProgress {
		t.Fatalf("case A should be no-progress")
	}
	if report.PerCase[1].IsNoProgress {
		t.Fatalf("case B should not be no-progress")
	}
}

func TestE3EvidenceVolume(t *testing.T) {
	a := e3BaseResult("A")
	a.KnownUniverse = []string{"ev-doc-01", "ev-doc-02", "ev-pol-01"} // size 3
	a.Report.CitedEvidenceIDs = []string{"ev-doc-01", "ev-doc-02"}    // size 2 => volume 5
	b := e3BaseResult("B")
	b.KnownUniverse = []string{"ev-doc-01"} // size1
	b.Report = nil                          // cited 0 => volume1
	report, _ := ComputeE3([]EvalResult{a, b})
	if report.PerCase[0].EvidenceVolume != 5 {
		t.Fatalf("case A volume %d want 5", report.PerCase[0].EvidenceVolume)
	}
	if report.PerCase[1].EvidenceVolume != 1 {
		t.Fatalf("case B volume %d want 1", report.PerCase[1].EvidenceVolume)
	}
	if report.Summary.EvidenceVolume.Count != 2 {
		t.Fatalf("volume count %d want 2", report.Summary.EvidenceVolume.Count)
	}
	if report.Summary.EvidenceVolume.Min != 1 || report.Summary.EvidenceVolume.Max != 5 {
		t.Fatalf("volume min %d max %d want 1/5", report.Summary.EvidenceVolume.Min, report.Summary.EvidenceVolume.Max)
	}
}

func TestE3BudgetUtil(t *testing.T) {
	a := e3BaseResult("A")
	a.ToolCallsUsed = 2
	a.TurnsUsed = 6
	a.BudgetMaxToolCalls = 5 // util 0.4
	a.BudgetMaxTurns = 12    // util 0.5
	b := e3BaseResult("B")
	b.ToolCallsUsed = 4
	b.TurnsUsed = 3
	b.BudgetMaxToolCalls = 5 // 0.8
	b.BudgetMaxTurns = 12    // 0.25
	report, _ := ComputeE3([]EvalResult{a, b})
	if math.Abs(report.PerCase[0].BudgetToolUtil-0.4) > 1e-9 {
		t.Fatalf("case A tool util %v want 0.4", report.PerCase[0].BudgetToolUtil)
	}
	if math.Abs(report.PerCase[0].BudgetTurnUtil-0.5) > 1e-9 {
		t.Fatalf("case A turn util %v want 0.5", report.PerCase[0].BudgetTurnUtil)
	}
	if math.Abs(report.Summary.BudgetToolUtilMean-0.6) > 1e-9 {
		t.Fatalf("tool util mean %v want 0.6", report.Summary.BudgetToolUtilMean)
	}
	if math.Abs(report.Summary.BudgetTurnUtilMean-0.375) > 1e-9 {
		t.Fatalf("turn util mean %v want 0.375", report.Summary.BudgetTurnUtilMean)
	}
	// Over-budget allowed: util >1.
	c := e3BaseResult("C")
	c.ToolCallsUsed = 10
	c.BudgetMaxToolCalls = 5 // 2.0
	report2, err := ComputeE3([]EvalResult{c})
	if err != nil {
		t.Fatalf("over-budget should not error: %v", err)
	}
	if math.Abs(report2.PerCase[0].BudgetToolUtil-2.0) > 1e-9 {
		t.Fatalf("over-budget util %v want 2.0", report2.PerCase[0].BudgetToolUtil)
	}
}

func TestE3OverBudgetAndDeterminism(t *testing.T) {
	results := []EvalResult{
		func() EvalResult { r := e3BaseResult("A"); r.ToolCallsUsed = 5; r.BudgetMaxToolCalls = 5; return r }(),
		func() EvalResult { r := e3BaseResult("B"); r.ToolCallsUsed = 6; r.BudgetMaxToolCalls = 5; return r }(),
	}
	report, err := ComputeE3(results)
	if err != nil {
		t.Fatalf("over-budget fixture should not error: %v", err)
	}
	if report.Summary.ToolCalls.Max != 6 {
		t.Fatalf("max %d want 6", report.Summary.ToolCalls.Max)
	}
	report2, _ := ComputeE3(results)
	if !reflect.DeepEqual(report, report2) {
		t.Fatalf("determinism failed")
	}
}
