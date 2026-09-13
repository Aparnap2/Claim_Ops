package invest

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"claimops-api/internal/invest"
)

func TestBaselineDeterministicVsBoundedFixture(t *testing.T) {
	h := Harness{}
	cases := AllCases()
	var results []EvalResult
	for _, c := range cases {
		res, err := h.Run(context.Background(), c)
		if err != nil {
			t.Fatalf("case %s harness error: %v", c.ID, err)
		}
		results = append(results, res)
	}
	report, err := BuildBaseline(cases, results)
	if err != nil {
		t.Fatalf("BuildBaseline error: %v", err)
	}

	// Corpus ordering: sorted A-P.
	if !slices.IsSorted(report.Corpus) {
		t.Fatalf("corpus not sorted: %v", report.Corpus)
	}
	expectedCorpus := make([]string, 0, len(cases))
	for _, c := range cases {
		expectedCorpus = append(expectedCorpus, c.ID)
	}
	slices.Sort(expectedCorpus)
	if !reflect.DeepEqual(report.Corpus, expectedCorpus) {
		t.Fatalf("corpus mismatch want %v got %v", expectedCorpus, report.Corpus)
	}
	if len(report.Corpus) != 16 {
		t.Fatalf("corpus len %d want 16 (A-P)", len(report.Corpus))
	}

	// E1/E2/E3 tables sorted by CaseID.
	if !slices.IsSortedFunc(report.E1, func(a, b E1Row) int { return strings.Compare(a.CaseID, b.CaseID) }) {
		t.Fatalf("E1 not sorted by case")
	}
	if !slices.IsSortedFunc(report.E2, func(a, b E2Row) int { return strings.Compare(a.CaseID, b.CaseID) }) {
		t.Fatalf("E2 not sorted by case")
	}
	// Verify E3 sorted via loop.
	for i := 1; i < len(report.E3.PerCase); i++ {
		if report.E3.PerCase[i].CaseID < report.E3.PerCase[i-1].CaseID {
			t.Fatalf("E3 not sorted at %d: %s before %s", i, report.E3.PerCase[i].CaseID, report.E3.PerCase[i-1].CaseID)
		}
	}

	// Deterministic-only counts: each case has exactly one exception code.
	if report.DeterministicOnly.TotalExceptions != len(cases) {
		t.Fatalf("total exceptions %d want %d", report.DeterministicOnly.TotalExceptions, len(cases))
	}
	if report.DeterministicOnly.CasesWithExceptions != len(cases) {
		t.Fatalf("cases with exceptions %d want %d", report.DeterministicOnly.CasesWithExceptions, len(cases))
	}
	for _, id := range report.Corpus {
		dc, ok := report.DeterministicOnly.PerCase[id]
		if !ok {
			t.Fatalf("missing deterministic per-case %s", id)
		}
		if dc.ExceptionCount != 1 {
			t.Fatalf("case %s exception count %d want 1", id, dc.ExceptionCount)
		}
		if len(dc.Codes) != 1 {
			t.Fatalf("case %s codes len %d want 1", id, len(dc.Codes))
		}
		if !slices.IsSorted(dc.Codes) {
			t.Fatalf("case %s codes not sorted", id)
		}
	}

	// Bounded counts: resolved + escalated == total, sorted corpus.
	if report.Bounded.Total != len(cases) {
		t.Fatalf("bounded total %d want %d", report.Bounded.Total, len(cases))
	}
	// Ready-path A-H are 8 resolved, I-P are 8 escalated (spec matrix).
	if report.Bounded.Resolved != 8 {
		t.Fatalf("bounded resolved %d want 8", report.Bounded.Resolved)
	}
	if report.Bounded.Escalated != 8 {
		t.Fatalf("bounded escalated %d want 8", report.Bounded.Escalated)
	}
	if report.Bounded.Resolved+report.Bounded.Escalated != report.Bounded.Total {
		t.Fatalf("bounded resolved+escalated != total")
	}
	for _, id := range report.Corpus {
		bc, ok := report.Bounded.PerCase[id]
		if !ok {
			t.Fatalf("missing bounded per-case %s", id)
		}
		if bc.ReportReady && bc.Outcome != string(orchestrateOutcomeReportReady()) {
			t.Fatalf("case %s report ready but outcome %q", id, bc.Outcome)
		}
	}

	// E1/E2 contain only IDs/codes/counts: no values.
	for _, row := range report.E1 {
		if len(row.PermittedTools) == 0 {
			t.Fatalf("case %s permitted tools empty", row.CaseID)
		}
	}

	// BuildBaseline validation: duplicate case handled elsewhere but corpus ordering holds.
}

func TestBaselineMarkdownNoValues(t *testing.T) {
	h := Harness{}
	cases := AllCases()
	var results []EvalResult
	for _, c := range cases {
		res, err := h.Run(context.Background(), c)
		if err != nil {
			t.Fatalf("harness: %v", err)
		}
		results = append(results, res)
	}
	report, err := BuildBaseline(cases, results)
	if err != nil {
		t.Fatalf("BuildBaseline: %v", err)
	}
	md := report.Markdown()
	// Must not leak agreed values or conflict distinct values.
	for _, forbidden := range []string{"City Hospital", "POL-X", "POL-Y", "125000", "130000"} {
		if strings.Contains(md, forbidden) {
			t.Fatalf("markdown contains forbidden value %q", forbidden)
		}
	}
	// Must contain IDs/codes/counts.
	for _, want := range []string{"A", "B", "REPORT_READY"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown should contain %q", want)
		}
	}
	// Must contain header tables.
	if !strings.Contains(md, "## E1 Evidence") {
		t.Fatalf("markdown missing E1 section")
	}
	if !strings.Contains(md, "## E2 Reasoning") {
		t.Fatalf("markdown missing E2 section")
	}
	if !strings.Contains(md, "## E3 Operational") {
		t.Fatalf("markdown missing E3 section")
	}
	if !strings.Contains(md, "## Deterministic-only") {
		t.Fatalf("markdown missing deterministic section")
	}
	if !strings.Contains(md, "## Bounded") {
		t.Fatalf("markdown missing bounded section")
	}
}

func TestBaselineJSONRoundTrip(t *testing.T) {
	h := Harness{}
	cases := AllCases()
	var results []EvalResult
	for _, c := range cases {
		res, err := h.Run(context.Background(), c)
		if err != nil {
			t.Fatalf("harness: %v", err)
		}
		results = append(results, res)
	}
	report, err := BuildBaseline(cases, results)
	if err != nil {
		t.Fatalf("BuildBaseline: %v", err)
	}
	data, err := report.JSON()
	if err != nil {
		t.Fatalf("JSON(): %v", err)
	}
	// JSON must be valid and round-trip.
	var decoded BaselineReport
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Re-encode and compare structs via DeepEqual on corpus/E0 etc. JSON encode
	// may normalize but struct equality should hold for corpus and counts.
	if !reflect.DeepEqual(report.Corpus, decoded.Corpus) {
		t.Fatalf("corpus round-trip mismatch")
	}
	if report.DeterministicOnly.TotalExceptions != decoded.DeterministicOnly.TotalExceptions {
		t.Fatalf("total exceptions round-trip mismatch")
	}
	if report.Bounded.Total != decoded.Bounded.Total {
		t.Fatalf("bounded total round-trip mismatch")
	}
	if len(report.E1) != len(decoded.E1) || len(report.E2) != len(decoded.E2) {
		t.Fatalf("E1/E2 len mismatch after round-trip")
	}
	// JSON must contain only IDs/codes/counts, never values.
	jsonStr := string(data)
	for _, forbidden := range []string{"City Hospital", "POL-X", "POL-Y"} {
		if strings.Contains(jsonStr, forbidden) {
			t.Fatalf("json contains forbidden value %q", forbidden)
		}
	}
	// Must contain IDs/codes/counts.
	for _, want := range []string{"\"total_cases\"", "\"corpus\""} {
		if !strings.Contains(jsonStr, want) {
			t.Fatalf("json should contain %q", want)
		}
	}
	// Ensure deterministic JSON: second call identical.
	data2, _ := report.JSON()
	if string(data) != string(data2) {
		t.Fatalf("JSON not deterministic across calls")
	}
}

func TestBaselineCorpusOrderingEdge(t *testing.T) {
	h := Harness{}
	cases := AllCases()
	// Shuffle input order: reverse.
	slices.Reverse(cases)
	var results []EvalResult
	for _, c := range cases {
		res, err := h.Run(context.Background(), c)
		if err != nil {
			t.Fatalf("harness: %v", err)
		}
		results = append(results, res)
	}
	report, err := BuildBaseline(cases, results)
	if err != nil {
		t.Fatalf("BuildBaseline with shuffled order: %v", err)
	}
	if !slices.IsSorted(report.Corpus) {
		t.Fatalf("corpus should be sorted even when input shuffled")
	}
	// E1/E2/E3 should be sorted regardless.
	for i := 1; i < len(report.E1); i++ {
		if report.E1[i].CaseID < report.E1[i-1].CaseID {
			t.Fatalf("E1 not sorted at %d", i)
		}
	}
}

// helper to avoid importing orchestrate directly for outcome string.
func orchestrateOutcomeReportReady() string {
	// Avoid circular import complexity: REPORT_READY literal.
	return "REPORT_READY"
}

// Ensure baseline_test imports invest at least once for per-case counts.
var _ = invest.ToolGetEvidence
