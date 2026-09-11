package bench

import (
	"strings"
	"testing"

	"claimops-api/internal/eval/corpus"
	"claimops-api/internal/parser"
)

// reportGenSample builds a small deterministic report covering parse,
// field, table, provenance and order signals. Aggregates are derived via
// aggregate() exactly as Run produces them.
func reportGenSample() Report {
	rep := Report{
		Meta: RunMeta{CorpusVersion: "vtest", CorpusCases: 3, ParserName: "liteparse", ParserVersion: "0.0-test", AdapterName: "liteparse"},
		Cases: []CaseResult{
			{CaseID: "CASE-001", DocumentType: "discharge_summary", Difficulty: "D0",
				ParseOK: true, ArtifactValid: true,
				Fields: []FieldScore{
					{Key: "claim_number", Match: MatchExact, HasProvenance: true},
					{Key: "patient_name", Match: MatchNormalized, HasProvenance: true},
				},
				Tables:     TableScore{Verdict: "TABLE_NA"},
				Provenance: ProvenanceScore{HitsTotal: 2, HitsWithPage: 2, HitsWithBlock: 2, HitsWithBox: 1}},
			{CaseID: "CASE-002", DocumentType: "hospital_bill", Difficulty: "D1",
				ParseOK: true, ArtifactValid: true,
				Fields: []FieldScore{
					{Key: "claim_number", Match: MatchExact, HasProvenance: true},
					{Key: "total_amount_paise", Match: MatchMissing},
				},
				Tables:     TableScore{ExpectedTables: 1, Verdict: "TABLE_MISSED"},
				Provenance: ProvenanceScore{HitsTotal: 1, HitsWithPage: 1, HitsWithBlock: 1, HitsWithBox: 1},
				Failures:   []FailureCode{FailFieldMissing, FailTableMissing}},
			{CaseID: "CASE-003", DocumentType: "claim_form", Difficulty: "D0",
				ParseOK: false, ArtifactValid: false,
				Failures: []FailureCode{FailParseFailure}},
		},
	}
	aggregate(&rep)
	return rep
}

func TestReportGenDeterministic(t *testing.T) {
	rep := reportGenSample()
	first := RenderMarkdown(rep)
	second := RenderMarkdown(rep)
	if first != second {
		t.Fatal("RenderMarkdown twice must be identical")
	}
	rev := rep
	rev.Cases = append([]CaseResult(nil), rep.Cases...)
	for i, j := 0, len(rev.Cases)-1; i < j; i, j = i+1, j-1 {
		rev.Cases[i], rev.Cases[j] = rev.Cases[j], rev.Cases[i]
	}
	aggregate(&rev)
	if got := RenderMarkdown(rev); got != first {
		t.Fatalf("different case order must render identically:\n--- first ---\n%s\n--- reordered ---\n%s", first, got)
	}
	if got := FieldTable(rev); got != FieldTable(rep) {
		t.Fatalf("FieldTable must be order-independent:\n%s\nvs\n%s", FieldTable(rep), got)
	}
}

func TestReportGenTriageExhaustive(t *testing.T) {
	all := []FailureCode{
		FailParseFailure, FailUnsupportedMedia, FailEmptyArtifact,
		FailMissingText, FailTextMismatch, FailPageStructure, FailReadingOrder,
		FailFieldMissing, FailFieldIncorrect,
		FailTableMissing, FailTablePartial, FailTableStructure,
		FailProvenanceMissing, FailProvenanceIncorrect,
	}
	rep := Report{Cases: []CaseResult{{CaseID: "CASE-ALL", Failures: all}}}
	got := Triage(rep)
	if len(got) != len(all) {
		t.Fatalf("triage covered %d of %d codes: %v", len(got), len(all), got)
	}
	valid := map[string]bool{
		"parser-limitation": true, "adapter-mapping": true,
		"corpus-golden": true, "contract-design": true, "investigate": true,
	}
	for _, code := range all {
		v, ok := got[code]
		if !ok || strings.TrimSpace(v) == "" {
			t.Fatalf("code %q missing or empty rationale", code)
		}
		class := strings.SplitN(v, ":", 2)[0]
		if !valid[class] {
			t.Fatalf("code %q has invalid class %q in %q", code, class, v)
		}
	}
	// Pinned classifications from the issue evidence.
	for code, wantClass := range map[FailureCode]string{
		FailTableMissing:      "parser-limitation",
		FailProvenanceMissing: "contract-design",
		FailFieldMissing:      "investigate",
		FailEmptyArtifact:     "corpus-golden",
	} {
		if got := Triage(Report{Cases: []CaseResult{{CaseID: "X", Failures: []FailureCode{code}}}})[code]; !strings.HasPrefix(got, wantClass) {
			t.Fatalf("code %q triage = %q, want class %q", code, got, wantClass)
		}
	}
	// Unknown codes default to investigate.
	unknown := Triage(Report{Cases: []CaseResult{{CaseID: "X", Failures: []FailureCode{"FUTURE_CODE"}}}})
	if v := unknown["FUTURE_CODE"]; !strings.HasPrefix(v, "investigate") {
		t.Fatalf("unknown code triage = %q, want investigate", v)
	}
	// Determinism: triage depends on code presence only.
	a := Triage(reportGenSample())
	b := Triage(reportGenSample())
	if len(a) != len(b) {
		t.Fatal("triage not deterministic")
	}
	for k, v := range a {
		if b[k] != v {
			t.Fatalf("triage mismatch on %q", k)
		}
	}
}

func TestReportGenNoEcho(t *testing.T) {
	// End-to-end no-echo proof through the real scorer: the golden carries
	// a sentinel VALUE; neither FieldTable nor the full markdown may echo
	// it (output is keys/counts/codes only by construction).
	const sentinel = "ZZZ_SENTINEL_X"
	doc := scoreDoc("doc-1",
		[]parser.ContentBlock{scoreBlock("doc-1", 1, "b1", "Claim "+sentinel+" filed", scoreBox(0.1))},
		nil)
	res := ScoreCase("CASE-S1", "claim_form", "D0", doc, corpus.Golden{ClaimNumber: sentinel})
	rep := Report{
		Meta:  RunMeta{CorpusVersion: "vtest", CorpusCases: 1, ParserName: "liteparse", ParserVersion: "0.0-test", AdapterName: "liteparse"},
		Cases: []CaseResult{res},
	}
	aggregate(&rep)
	if md := RenderMarkdown(rep); strings.Contains(md, sentinel) {
		t.Fatalf("markdown echoes golden value:\n%s", md)
	}
	if ft := FieldTable(rep); strings.Contains(ft, sentinel) {
		t.Fatalf("field table echoes golden value:\n%s", ft)
	}
	// Keys must still be present (evidence without values).
	if md := RenderMarkdown(rep); !strings.Contains(md, "claim_number") {
		t.Fatalf("markdown missing field key:\n%s", md)
	}
}

func TestReportGenTableMissedVisible(t *testing.T) {
	md := RenderMarkdown(reportGenSample())
	if !strings.Contains(md, "TABLE_MISSED") {
		t.Fatalf("report with a MISSED case must contain TABLE_MISSED:\n%s", md)
	}
	if !strings.Contains(md, "TABLE_MISSING") {
		t.Fatalf("report with a MISSED case must name the TABLE_MISSING failure code:\n%s", md)
	}
	if !strings.Contains(md, "flattened to paragraphs is TABLE_MISSED") {
		t.Fatalf("report must state flattened-to-paragraphs = MISSED:\n%s", md)
	}
	for _, bad := range []string{"all tables passed", "tables fully recovered", "table success", "overall table success"} {
		if strings.Contains(strings.ToLower(md), bad) {
			t.Fatalf("report claims table success without the miss count (%q):\n%s", bad, md)
		}
	}
}

func TestReportGenProvenanceHonesty(t *testing.T) {
	t.Run("nil_box_not_failure", func(t *testing.T) {
		// Block carries a block id but no box: matched hit, tallied
		// without box, and crucially no PROVENANCE_MISSING failure.
		doc := scoreDoc("doc-1",
			[]parser.ContentBlock{scoreBlock("doc-1", 1, "b1", "CLM-7", nil)},
			nil)
		res := ScoreCase("CASE-P1", "claim_form", "D0", doc, corpus.Golden{ClaimNumber: "CLM-7"})
		rep := Report{Cases: []CaseResult{res}}
		aggregate(&rep)
		if _, ok := Triage(rep)[FailProvenanceMissing]; ok {
			t.Fatalf("nil-box hit triaged as PROVENANCE_MISSING: %v", res.Failures)
		}
		if md := RenderMarkdown(rep); strings.Contains(md, "| PROVENANCE_MISSING |") {
			t.Fatalf("nil-box case labeled with PROVENANCE_MISSING row:\n%s", md)
		}
	})
	t.Run("block_id_gap_is_contract_design", func(t *testing.T) {
		// Block without a block id (the contract shape of a table cell
		// hit): PROVENANCE_MISSING must triage as contract-design.
		doc := scoreDoc("doc-1",
			[]parser.ContentBlock{scoreBlockNoProv("doc-1", 1, "b1", "CLM-7")},
			nil)
		res := ScoreCase("CASE-P2", "claim_form", "D0", doc, corpus.Golden{ClaimNumber: "CLM-7"})
		rep := Report{Cases: []CaseResult{res}}
		aggregate(&rep)
		v, ok := Triage(rep)[FailProvenanceMissing]
		if !ok {
			t.Fatalf("expected PROVENANCE_MISSING, got %v", res.Failures)
		}
		if !strings.HasPrefix(v, "contract-design") {
			t.Fatalf("PROVENANCE_MISSING triage = %q, want contract-design", v)
		}
		if md := RenderMarkdown(rep); !strings.Contains(md, "| PROVENANCE_MISSING |") {
			t.Fatalf("taxonomy must list PROVENANCE_MISSING:\n%s", md)
		}
	})
}

func TestReportGenZeroCase(t *testing.T) {
	var rep Report // nil maps, no cases: must render without panic/div-zero.
	md := RenderMarkdown(rep)
	for _, want := range []string{"n/a", "(none)", "(no cases)", "report_version", "vendor-silent 1.0"} {
		if !strings.Contains(md, want) {
			t.Fatalf("zero-case report missing %q:\n%s", want, md)
		}
	}
	if ft := FieldTable(rep); !strings.Contains(ft, "no fields scored") {
		t.Fatalf("zero-case field table must note absence:\n%s", ft)
	}
	if v := ReportVersion(); v != "v1" {
		t.Fatalf("ReportVersion = %q, want v1", v)
	}
}

func TestReportGenConfidence(t *testing.T) {
	md := RenderMarkdown(reportGenSample())
	if !strings.Contains(md, "vendor-silent 1.0") {
		t.Fatalf("report must carry the vendor-silent-1.0 disclaimer:\n%s", md)
	}
	lower := strings.ToLower(md)
	for _, bad := range []string{"confident", "high confidence", "guaranteed", "well-calibrated"} {
		if strings.Contains(lower, bad) {
			t.Fatalf("report claims parser confidence (%q):\n%s", bad, md)
		}
	}
}
