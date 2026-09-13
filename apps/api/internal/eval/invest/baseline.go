package invest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"claimops-api/internal/investigate/orchestrate"
)

// E0Summary reports the E0 gate pass rate over a corpus run.
// IDs/codes/counts only, never values.
type E0Summary struct {
	TotalCases   int     `json:"total_cases"`
	PassedCases  int     `json:"passed_cases"`
	FailedCases  int     `json:"failed_cases"`
	PassRate     float64 `json:"pass_rate"`
	TotalGates   int     `json:"total_gates"`
	PassedGates  int     `json:"passed_gates"`
	GatePassRate float64 `json:"gate_pass_rate"`
}

// E1Row is one E1 evidence row: observed tool and evidence counts for the
// case. IDs/codes/counts only. Placeholder for future E1 scorer — today it
// carries what can be observed deterministically from EvalResult.
type E1Row struct {
	CaseID            string   `json:"case_id"`
	PermittedTools    []string `json:"permitted_tools"`
	DeclaredTools     []string `json:"declared_tools"`
	ToolCallsUsed     int      `json:"tool_calls_used"`
	KnownUniverseSize int      `json:"known_universe_size"`
	CitedEvidenceSize int      `json:"cited_evidence_size"`
	AttemptCodes      []string `json:"attempt_codes"`
}

// E2Row is one E2 reasoning row: per-case observable dimensions.
// IDs/codes/counts only, no statements or rationales.
type E2Row struct {
	CaseID                string   `json:"case_id"`
	Outcome               string   `json:"outcome"`
	EscalationReason      string   `json:"escalation_reason,omitempty"`
	HypothesisCount       int      `json:"hypothesis_count"`
	FindingCount          int      `json:"finding_count"`
	MissingFalsifierCount int      `json:"missing_falsifier_count"`
	DanglingFindingCount  int      `json:"dangling_finding_count"`
	RecommendationAction  string   `json:"recommendation_action,omitempty"`
	FindingsAcceptable    *bool    `json:"findings_acceptable,omitempty"`
	ActionAcceptable      *bool    `json:"action_acceptable,omitempty"`
	AttemptCodes          []string `json:"attempt_codes"`
}

// DeterministicCase holds the deterministic-only view of one case:
// the verification exception count and its rule codes (IDs/codes only).
type DeterministicCase struct {
	CaseID         string   `json:"case_id"`
	ExceptionCount int      `json:"exception_count"`
	Codes          []string `json:"codes"`
}

// DeterministicOnly is the deterministic-only baseline (documents -> R1-R10
// -> EXCEPTION) aggregated over the corpus.
type DeterministicOnly struct {
	PerCase             map[string]DeterministicCase `json:"per_case"`
	TotalExceptions     int                          `json:"total_exceptions"`
	CasesWithExceptions int                          `json:"cases_with_exceptions"`
}

// BoundedCase holds the bounded investigation view of one case: outcome
// and escalation code only, plus cited-evidence count.
type BoundedCase struct {
	CaseID             string `json:"case_id"`
	Outcome            string `json:"outcome"`
	EscalationReason   string `json:"escalation_reason,omitempty"`
	CitedEvidenceCount int    `json:"cited_evidence_count"`
	ReportReady        bool   `json:"report_ready"`
}

// Bounded is the bounded investigation baseline (evidence-backed
// resolution / escalation) aggregated over the corpus.
type Bounded struct {
	Total     int                    `json:"total"`
	Resolved  int                    `json:"resolved"`
	Escalated int                    `json:"escalated"`
	Reduced   int                    `json:"reduced"`
	PerCase   map[string]BoundedCase `json:"per_case"`
}

// BaselineReport is the v1 comparison artifact for CI: Corpus A-P, E0
// pass rate, E1/E2/E3 tables, deterministic-only exception counts, and
// bounded resolved/escalated counts. All fields are IDs, codes, or counts
// — never values, agreed strings, rationales, or statements.
type BaselineReport struct {
	Corpus            []string          `json:"corpus"`
	E0                E0Summary         `json:"e0"`
	E1                []E1Row           `json:"e1"`
	E2                []E2Row           `json:"e2"`
	E3                E3Report          `json:"e3"`
	DeterministicOnly DeterministicOnly `json:"deterministic_only"`
	Bounded           Bounded           `json:"bounded"`
}

// BuildBaseline constructs the BaselineReport from cases and their harness
// results. It validates that every case ID has a matching result, counts
// deterministic exceptions via envelope RuleFindings (the verification
// EXCEPTION count), and counts bounded outcomes via EvalResult ReportReady
// vs Escalated. E1/E2 rows are derived deterministically from the results;
// E3 is computed via ComputeE3. Deterministic-only exceptions never carry
// values — only codes.
func BuildBaseline(cases []EvalCase, results []EvalResult) (*BaselineReport, error) {
	if len(cases) == 0 {
		return nil, fmt.Errorf("eval: baseline: empty cases")
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("eval: baseline: empty results")
	}
	if len(cases) != len(results) {
		return nil, fmt.Errorf("eval: baseline: cases %d != results %d", len(cases), len(results))
	}
	// Index results by CaseID.
	byResult := make(map[string]EvalResult, len(results))
	for _, r := range results {
		if _, dup := byResult[r.CaseID]; dup {
			return nil, fmt.Errorf("eval: baseline: duplicate result case id %q", r.CaseID)
		}
		byResult[r.CaseID] = r
	}
	// Ensure every case has a result.
	for _, c := range cases {
		if _, ok := byResult[c.ID]; !ok {
			return nil, fmt.Errorf("eval: baseline: missing result for case %q", c.ID)
		}
	}

	// Corpus: sorted case IDs A-P.
	corpus := make([]string, 0, len(cases))
	for _, c := range cases {
		corpus = append(corpus, c.ID)
	}
	sort.Strings(corpus)

	// E0 pass rate via gate evaluation.
	var (
		totalCases  = len(cases)
		passedCases int
		totalGates  int
		passedGates int
	)
	for _, c := range cases {
		r := byResult[c.ID]
		gates := EvaluateAll(r)
		totalGates += len(gates)
		allPass := true
		for _, g := range gates {
			if g.Passed {
				passedGates++
			} else {
				allPass = false
			}
		}
		// Also check escalation contract via RunAll per-case: use gates only here
		// for E0 rate; the overall contract is available via RunAll if needed.
		if allPass {
			// For ready-path cases, also require outcome match; for MustEscalate
			// require escalated. This aligns E0 rate with RunAll semantics
			// without double-counting gates.
			if c.MustEscalate {
				if r.Outcome == orchestrate.OutcomeEscalated && r.EscalationReason == c.MustEscalateReason {
					passedCases++
				}
			} else {
				if r.Outcome == orchestrate.OutcomeReportReady && r.Report != nil {
					passedCases++
				}
			}
		}
	}
	e0 := E0Summary{
		TotalCases:  totalCases,
		PassedCases: passedCases,
		FailedCases: totalCases - passedCases,
		PassRate:    float64(passedCases) / float64(totalCases),
		TotalGates:  totalGates,
		PassedGates: passedGates,
	}
	if totalGates > 0 {
		e0.GatePassRate = float64(passedGates) / float64(totalGates)
	}

	// E1 rows.
	e1 := make([]E1Row, 0, len(cases))
	for _, c := range cases {
		r := byResult[c.ID]
		cited := 0
		if r.Report != nil {
			cited = len(r.Report.CitedEvidenceIDs)
		}
		perm := make([]string, 0, len(c.PermittedTools))
		for _, t := range c.PermittedTools {
			perm = append(perm, string(t))
		}
		sort.Strings(perm)
		decl := make([]string, 0, len(r.DeclaredTools))
		for _, t := range r.DeclaredTools {
			decl = append(decl, string(t))
		}
		sort.Strings(decl)
		attempts := append([]string(nil), r.Attempts...)
		sort.Strings(attempts)
		if attempts == nil {
			attempts = []string{}
		}
		e1 = append(e1, E1Row{
			CaseID:            c.ID,
			PermittedTools:    perm,
			DeclaredTools:     decl,
			ToolCallsUsed:     r.ToolCallsUsed,
			KnownUniverseSize: len(r.KnownUniverse),
			CitedEvidenceSize: cited,
			AttemptCodes:      attempts,
		})
	}
	sort.Slice(e1, func(i, j int) bool { return e1[i].CaseID < e1[j].CaseID })

	// E2 rows.
	e2 := make([]E2Row, 0, len(cases))
	for _, c := range cases {
		r := byResult[c.ID]
		row := E2Row{
			CaseID:           c.ID,
			Outcome:          string(r.Outcome),
			EscalationReason: string(r.EscalationReason),
			AttemptCodes:     append([]string(nil), r.Attempts...),
		}
		sort.Strings(row.AttemptCodes)
		if row.AttemptCodes == nil {
			row.AttemptCodes = []string{}
		}
		if r.Report != nil {
			row.HypothesisCount = len(r.Report.HypothesisIDs)
			row.FindingCount = len(r.Report.FindingIDs)
			row.MissingFalsifierCount = len(r.Report.MissingFalsifier)
			row.DanglingFindingCount = len(r.Report.DanglingFindings)
			row.RecommendationAction = r.Report.RecommendationAction
			fa := r.Report.FindingsAcceptable
			aa := r.Report.ActionAcceptable
			row.FindingsAcceptable = &fa
			row.ActionAcceptable = &aa
		}
		e2 = append(e2, row)
	}
	sort.Slice(e2, func(i, j int) bool { return e2[i].CaseID < e2[j].CaseID })

	// E3 via ComputeE3.
	orderedResults := make([]EvalResult, 0, len(results))
	for _, id := range corpus {
		orderedResults = append(orderedResults, byResult[id])
	}
	e3, err := ComputeE3(orderedResults)
	if err != nil {
		return nil, fmt.Errorf("eval: baseline: e3: %w", err)
	}

	// Deterministic-only: envelope RuleFindings count (verification EXCEPTION count).
	detPer := make(map[string]DeterministicCase, len(cases))
	totalExceptions := 0
	casesWithExceptions := 0
	for _, c := range cases {
		codes := make([]string, 0, len(c.Envelope.RuleFindings))
		for _, f := range c.Envelope.RuleFindings {
			codes = append(codes, string(f.Code))
		}
		sort.Strings(codes)
		if codes == nil {
			codes = []string{}
		}
		cnt := len(codes)
		totalExceptions += cnt
		if cnt > 0 {
			casesWithExceptions++
		}
		detPer[c.ID] = DeterministicCase{
			CaseID:         c.ID,
			ExceptionCount: cnt,
			Codes:          codes,
		}
	}

	// Bounded: ReportReady vs Escalated.
	boundedPer := make(map[string]BoundedCase, len(results))
	resolved := 0
	escalated := 0
	for _, c := range cases {
		r := byResult[c.ID]
		cited := 0
		if r.Report != nil {
			cited = len(r.Report.CitedEvidenceIDs)
		}
		bc := BoundedCase{
			CaseID:             c.ID,
			Outcome:            string(r.Outcome),
			EscalationReason:   string(r.EscalationReason),
			CitedEvidenceCount: cited,
			ReportReady:        r.Outcome == orchestrate.OutcomeReportReady,
		}
		if bc.ReportReady {
			resolved++
		} else if r.Outcome == orchestrate.OutcomeEscalated {
			escalated++
		}
		boundedPer[c.ID] = bc
	}
	// Reduced is the bounded improvement over deterministic-only where a
	// case moved from exception to report-ready. For v1 we report 0 when
	// not defined, keeping the field present for CI without inventing a
	// value. Callers may compute reduced as CasesWithExceptions - escalated
	// if needed, but we expose the raw counts only.
	reduced := 0

	report := &BaselineReport{
		Corpus: corpus,
		E0:     e0,
		E1:     e1,
		E2:     e2,
		E3:     e3,
		DeterministicOnly: DeterministicOnly{
			PerCase:             detPer,
			TotalExceptions:     totalExceptions,
			CasesWithExceptions: casesWithExceptions,
		},
		Bounded: Bounded{
			Total:     len(cases),
			Resolved:  resolved,
			Escalated: escalated,
			Reduced:   reduced,
			PerCase:   boundedPer,
		},
	}
	return report, nil
}

// JSON returns the canonical JSON artifact for CI: IDs, codes, and counts
// only, stable ordering, no values.
func (r BaselineReport) JSON() ([]byte, error) {
	// Ensure deterministic ordering via sorted corpus.
	cpy := r
	// Deep copy E1/E2 sorted already; ensure JSON ordering stable.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(cpy); err != nil {
		return nil, fmt.Errorf("eval: baseline json: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Markdown renders the v1 comparison report as Markdown with tables
// containing only IDs, codes, and counts — never values, rationales, or
// statements.
func (r BaselineReport) Markdown() string {
	var b strings.Builder
	b.WriteString("# ClaimOps Baseline v1 — Deterministic-only vs Bounded Investigation\n\n")
	b.WriteString(fmt.Sprintf("Corpus: %s (%d cases A-P)\n\n", strings.Join(r.Corpus, ", "), len(r.Corpus)))

	b.WriteString("## E0 Gate Summary\n\n")
	b.WriteString(fmt.Sprintf("Pass rate: %d/%d (%.1f%%) cases; gates %d/%d (%.1f%%)\n\n",
		r.E0.PassedCases, r.E0.TotalCases, r.E0.PassRate*100,
		r.E0.PassedGates, r.E0.TotalGates, r.E0.GatePassRate*100))
	b.WriteString("| Case | Gates passed | Total gates | Pass |\n")
	b.WriteString("|------|--------------|-------------|------|\n")
	// Per-case gate detail is derived from E2 outcome vs E0, but we report
	// the aggregate pass/fail per case via Bounded outcome as IDs only.
	// For a full per-gate table, gates are enumerated from E0 counts.
	b.WriteString(fmt.Sprintf("| corpus | %d | %d | %v |\n\n", r.E0.PassedGates, r.E0.TotalGates, r.E0.PassedCases == r.E0.TotalCases))

	b.WriteString("## E1 Evidence (deterministic)\n\n")
	b.WriteString("| Case | Tool calls | Known | Cited | Permitted tools | Declared tools | Attempt codes |\n")
	b.WriteString("|------|------------|-------|-------|-----------------|----------------|---------------|\n")
	for _, row := range r.E1 {
		b.WriteString(fmt.Sprintf("| %s | %d | %d | %d | %s | %s | %s |\n",
			row.CaseID,
			row.ToolCallsUsed,
			row.KnownUniverseSize,
			row.CitedEvidenceSize,
			strings.Join(row.PermittedTools, ","),
			strings.Join(row.DeclaredTools, ","),
			strings.Join(row.AttemptCodes, ","),
		))
	}
	b.WriteString("\n")

	b.WriteString("## E2 Reasoning (G9-derived, no single score)\n\n")
	b.WriteString("| Case | Outcome | Reason | Hypotheses | Findings | MissingFalsifier | Dangling | Recommendation | FindingsAcceptable | ActionAcceptable | Attempt codes |\n")
	b.WriteString("|------|---------|--------|------------|----------|------------------|----------|----------------|--------------------|------------------|---------------|\n")
	for _, row := range r.E2 {
		fa := ""
		aa := ""
		if row.FindingsAcceptable != nil {
			fa = fmt.Sprintf("%v", *row.FindingsAcceptable)
		}
		if row.ActionAcceptable != nil {
			aa = fmt.Sprintf("%v", *row.ActionAcceptable)
		}
		b.WriteString(fmt.Sprintf("| %s | %s | %s | %d | %d | %d | %d | %s | %s | %s | %s |\n",
			row.CaseID,
			row.Outcome,
			row.EscalationReason,
			row.HypothesisCount,
			row.FindingCount,
			row.MissingFalsifierCount,
			row.DanglingFindingCount,
			row.RecommendationAction,
			fa, aa,
			strings.Join(row.AttemptCodes, ","),
		))
	}
	b.WriteString("\n")

	b.WriteString("## E3 Operational\n\n")
	b.WriteString(fmt.Sprintf("Total %d | Escalated %d (%.1f%%) | Repeated %d | Failed tool calls %d | No-progress %d\n\n",
		r.E3.Summary.Total,
		r.E3.Summary.Escalated, r.E3.Summary.EscalationRate*100,
		r.E3.Summary.RepeatedCount,
		r.E3.Summary.FailedToolCallsTotal,
		r.E3.Summary.NoProgressCount))
	b.WriteString("| Metric | Count | Min | Max | Median | P50 | P95 | Mean |\n")
	b.WriteString("|--------|-------|-----|-----|--------|-----|-----|------|\n")
	q := r.E3.Summary
	b.WriteString(fmt.Sprintf("| Turns | %d | %d | %d | %.1f | %.1f | %d | %.1f |\n", q.Turns.Count, q.Turns.Min, q.Turns.Max, q.Turns.Median, q.Turns.P50, q.Turns.P95, q.Turns.Mean))
	b.WriteString(fmt.Sprintf("| Tool calls | %d | %d | %d | %.1f | %.1f | %d | %.1f |\n", q.ToolCalls.Count, q.ToolCalls.Min, q.ToolCalls.Max, q.ToolCalls.Median, q.ToolCalls.P50, q.ToolCalls.P95, q.ToolCalls.Mean))
	b.WriteString(fmt.Sprintf("| Latency ms | %d | %d | %d | %.1f | %.1f | %d | %.1f |\n", q.LatencyMs.Count, q.LatencyMs.Min, q.LatencyMs.Max, q.LatencyMs.Median, q.LatencyMs.P50, q.LatencyMs.P95, q.LatencyMs.Mean))
	b.WriteString(fmt.Sprintf("| Evidence volume | %d | %d | %d | %.1f | %.1f | %d | %.1f |\n", q.EvidenceVolume.Count, q.EvidenceVolume.Min, q.EvidenceVolume.Max, q.EvidenceVolume.Median, q.EvidenceVolume.P50, q.EvidenceVolume.P95, q.EvidenceVolume.Mean))
	b.WriteString(fmt.Sprintf("\nBudget tool util mean: %.2f | Budget turn util mean: %.2f\n\n", q.BudgetToolUtilMean, q.BudgetTurnUtilMean))
	b.WriteString("| Case | Turns | Tool calls | Repeated | Latency ms | Known | Cited | Volume | Tool util | Turn util | Failed | Outcome | Reason | No-progress |\n")
	b.WriteString("|------|-------|------------|----------|------------|-------|-------|--------|-----------|-----------|--------|---------|--------|-------------|\n")
	for _, pc := range r.E3.PerCase {
		b.WriteString(fmt.Sprintf("| %s | %d | %d | %v | %d | %d | %d | %d | %.2f | %.2f | %d | %s | %s | %v |\n",
			pc.CaseID, pc.Turns, pc.ToolCalls, pc.Repeated, pc.LatencyMs,
			pc.KnownUniverseSize, pc.CitedEvidenceSize, pc.EvidenceVolume,
			pc.BudgetToolUtil, pc.BudgetTurnUtil, pc.FailedToolCalls,
			func() string {
				if pc.IsEscalated {
					return "ESCALATED"
				}
				return "REPORT_READY"
			}(),
			pc.EscalationReason,
			pc.IsNoProgress,
		))
	}
	b.WriteString("\n")

	b.WriteString("## Deterministic-only (documents -> R1-R10 -> EXCEPTION)\n\n")
	b.WriteString(fmt.Sprintf("Total exceptions: %d | Cases with exceptions: %d\n\n", r.DeterministicOnly.TotalExceptions, r.DeterministicOnly.CasesWithExceptions))
	b.WriteString("| Case | Exception count | Codes |\n")
	b.WriteString("|------|-----------------|-------|\n")
	for _, id := range r.Corpus {
		dc := r.DeterministicOnly.PerCase[id]
		b.WriteString(fmt.Sprintf("| %s | %d | %s |\n", dc.CaseID, dc.ExceptionCount, strings.Join(dc.Codes, ",")))
	}
	b.WriteString("\n")

	b.WriteString("## Bounded (deterministic + investigation)\n\n")
	b.WriteString(fmt.Sprintf("Total %d | Resolved %d | Escalated %d | Reduced %d\n\n", r.Bounded.Total, r.Bounded.Resolved, r.Bounded.Escalated, r.Bounded.Reduced))
	b.WriteString("| Case | Outcome | Reason | Cited evidence count | Report ready |\n")
	b.WriteString("|------|---------|--------|----------------------|----------------|\n")
	for _, id := range r.Corpus {
		bc := r.Bounded.PerCase[id]
		b.WriteString(fmt.Sprintf("| %s | %s | %s | %d | %v |\n", bc.CaseID, bc.Outcome, bc.EscalationReason, bc.CitedEvidenceCount, bc.ReportReady))
	}
	b.WriteString("\n")

	b.WriteString("Notes: All tables carry IDs, codes, and counts only. No field values, agreed strings, statements, rationales, or summaries are rendered. No single accuracy number is reported; interpret E1/E2/E3 dimensions separately.\n")
	return b.String()
}
