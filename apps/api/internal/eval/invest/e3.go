package invest

import (
	"fmt"
	"math"
	"sort"

	"claimops-api/internal/investigate/orchestrate"
)

// E3CaseMetrics is the per-case operational observation over one EvalResult.
// IDs, codes, and counts only — never values or prose.
type E3CaseMetrics struct {
	CaseID             string  `json:"case_id"`
	Turns              int     `json:"turns"`
	ToolCalls          int     `json:"tool_calls"`
	Repeated           bool    `json:"repeated"`
	LatencyMs          int64   `json:"latency_ms"`
	KnownUniverseSize  int     `json:"known_universe_size"`
	CitedEvidenceSize  int     `json:"cited_evidence_size"`
	EvidenceVolume     int     `json:"evidence_volume"`
	BudgetToolUtil     float64 `json:"budget_tool_util"`
	BudgetTurnUtil     float64 `json:"budget_turn_util"`
	FailedToolCalls    int     `json:"failed_tool_calls"`
	IsEscalated        bool    `json:"is_escalated"`
	EscalationReason   string  `json:"escalation_reason,omitempty"`
	IsNoProgress       bool    `json:"is_no_progress"`
	BudgetMaxToolCalls int     `json:"budget_max_tool_calls"`
	BudgetMaxTurns     int     `json:"budget_max_turns"`
}

// E3Quantiles reports distribution over a corpus for one scalar.
// Min/Max/P95 are int64 for the underlying integer metric; Median and P50
// are float64 to allow exact median of even counts, Mean is the arithmetic
// mean. Count is the corpus size used.
type E3Quantiles struct {
	Count  int     `json:"count"`
	Min    int64   `json:"min"`
	Max    int64   `json:"max"`
	Median float64 `json:"median"`
	P50    float64 `json:"p50"`
	P95    int64   `json:"p95"`
	Mean   float64 `json:"mean"`
}

// E3Summary aggregates operational metrics across the corpus.
type E3Summary struct {
	Total                int         `json:"total"`
	Escalated            int         `json:"escalated"`
	EscalationRate       float64     `json:"escalation_rate"`
	RepeatedCount        int         `json:"repeated_count"`
	FailedToolCallsTotal int         `json:"failed_tool_calls_total"`
	NoProgressCount      int         `json:"no_progress_count"`
	Turns                E3Quantiles `json:"turns"`
	ToolCalls            E3Quantiles `json:"tool_calls"`
	LatencyMs            E3Quantiles `json:"latency_ms"`
	EvidenceVolume       E3Quantiles `json:"evidence_volume"`
	BudgetToolUtilMean   float64     `json:"budget_tool_util_mean"`
	BudgetTurnUtilMean   float64     `json:"budget_turn_util_mean"`
}

// E3Report is the full operational report: per-case rows plus corpus summary.
type E3Report struct {
	PerCase []E3CaseMetrics `json:"per_case"`
	Summary E3Summary       `json:"summary"`
}

// ComputeE3 builds the operational metrics over a set of EvalResults.
// It validates budgets (BudgetMaxToolCalls > 0, BudgetMaxTurns > 0) and
// returns an error on invalid budgets or an empty slice. No values or
// prose cross this boundary — only IDs, codes, and counts are observed.
func ComputeE3(results []EvalResult) (E3Report, error) {
	if len(results) == 0 {
		return E3Report{}, fmt.Errorf("eval: e3: empty results")
	}
	per := make([]E3CaseMetrics, 0, len(results))
	var (
		turnsVals    []int64
		callsVals    []int64
		latencyVals  []int64
		evidenceVals []int64
	)
	var (
		toolUtilSum float64
		turnUtilSum float64
	)
	var (
		escalatedCount  int
		repeatedCount   int
		failedTotal     int
		noProgressCount int
	)
	for _, r := range results {
		if r.BudgetMaxToolCalls <= 0 {
			return E3Report{}, fmt.Errorf("eval: e3: case %q has invalid BudgetMaxToolCalls %d", r.CaseID, r.BudgetMaxToolCalls)
		}
		if r.BudgetMaxTurns <= 0 {
			return E3Report{}, fmt.Errorf("eval: e3: case %q has invalid BudgetMaxTurns %d", r.CaseID, r.BudgetMaxTurns)
		}
		if r.TurnsUsed < 0 || r.ToolCallsUsed < 0 || r.LatencyMs < 0 {
			return E3Report{}, fmt.Errorf("eval: e3: case %q has negative usage", r.CaseID)
		}
		if r.ToolCallsUsed > r.BudgetMaxToolCalls && r.ToolCallsUsed > 0 && r.BudgetMaxToolCalls > 0 {
			// Not an error — quantiles should reflect over-budget as >1 util —
			// but keep monotonic detail for the gate.
		}
		knownSize := len(r.KnownUniverse)
		citedSize := 0
		if r.Report != nil {
			citedSize = len(r.Report.CitedEvidenceIDs)
		}
		evidenceVolume := knownSize + citedSize

		failed := 0
		for i := range r.ToolCalls {
			if r.ToolCalls[i].ErrorCode != "" {
				failed++
			}
		}

		isEscalated := r.Outcome == orchestrate.OutcomeEscalated
		if isEscalated {
			escalatedCount++
		}
		if r.RepeatObserved {
			repeatedCount++
		}
		isNoProgress := r.EscalationReason == orchestrate.EscalationNoProgress
		if isNoProgress {
			noProgressCount++
		}
		failedTotal += failed

		toolUtil := float64(r.ToolCallsUsed) / float64(r.BudgetMaxToolCalls)
		turnUtil := float64(r.TurnsUsed) / float64(r.BudgetMaxTurns)
		toolUtilSum += toolUtil
		turnUtilSum += turnUtil

		m := E3CaseMetrics{
			CaseID:             r.CaseID,
			Turns:              r.TurnsUsed,
			ToolCalls:          r.ToolCallsUsed,
			Repeated:           r.RepeatObserved,
			LatencyMs:          r.LatencyMs,
			KnownUniverseSize:  knownSize,
			CitedEvidenceSize:  citedSize,
			EvidenceVolume:     evidenceVolume,
			BudgetToolUtil:     toolUtil,
			BudgetTurnUtil:     turnUtil,
			FailedToolCalls:    failed,
			IsEscalated:        isEscalated,
			EscalationReason:   string(r.EscalationReason),
			IsNoProgress:       isNoProgress,
			BudgetMaxToolCalls: r.BudgetMaxToolCalls,
			BudgetMaxTurns:     r.BudgetMaxTurns,
		}
		per = append(per, m)
		turnsVals = append(turnsVals, int64(r.TurnsUsed))
		callsVals = append(callsVals, int64(r.ToolCallsUsed))
		latencyVals = append(latencyVals, r.LatencyMs)
		evidenceVals = append(evidenceVals, int64(evidenceVolume))
	}

	// Sort per-case by CaseID for determinism (corpus A-P invariant).
	sort.Slice(per, func(i, j int) bool { return per[i].CaseID < per[j].CaseID })

	// Recompute means deterministically from sorted PerCase (avoids
	// floating-point order dependence of input-order accumulation).
	var sortedToolUtilSum, sortedTurnUtilSum float64
	for _, m := range per {
		sortedToolUtilSum += m.BudgetToolUtil
		sortedTurnUtilSum += m.BudgetTurnUtil
	}

	summary := E3Summary{
		Total:                len(results),
		Escalated:            escalatedCount,
		EscalationRate:       float64(escalatedCount) / float64(len(results)),
		RepeatedCount:        repeatedCount,
		FailedToolCallsTotal: failedTotal,
		NoProgressCount:      noProgressCount,
		Turns:                quantiles(turnsVals),
		ToolCalls:            quantiles(callsVals),
		LatencyMs:            quantiles(latencyVals),
		EvidenceVolume:       quantiles(evidenceVals),
		BudgetToolUtilMean:   sortedToolUtilSum / float64(len(results)),
		BudgetTurnUtilMean:   sortedTurnUtilSum / float64(len(results)),
	}
	return E3Report{PerCase: per, Summary: summary}, nil
}

// quantiles computes Min, Max, Median, P50, P95, Mean over int64 values.
// Empty yields zero-valued quantiles. Median and P50 are identical; for
// even n the median is the average of the two middle values. P95 is the
// ceil(0.95*n)-th element (1-indexed) in sorted order.
func quantiles(vals []int64) E3Quantiles {
	n := len(vals)
	if n == 0 {
		return E3Quantiles{}
	}
	sorted := append([]int64(nil), vals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var sum int64
	for _, v := range sorted {
		sum += v
	}
	mean := float64(sum) / float64(n)
	min := sorted[0]
	max := sorted[n-1]
	median := medianFloat(sorted)
	p95Idx := int(math.Ceil(0.95*float64(n))) - 1
	if p95Idx < 0 {
		p95Idx = 0
	}
	if p95Idx >= n {
		p95Idx = n - 1
	}
	p95 := sorted[p95Idx]
	return E3Quantiles{
		Count:  n,
		Min:    min,
		Max:    max,
		Median: median,
		P50:    median,
		P95:    p95,
		Mean:   mean,
	}
}

func medianFloat(sorted []int64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return float64(sorted[n/2])
	}
	a := sorted[n/2-1]
	b := sorted[n/2]
	return float64(a+b) / 2.0
}
