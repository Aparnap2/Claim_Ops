package bench

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"claimops-api/internal/eval/corpus"
	"claimops-api/internal/parser"
)

// Helpers (score-prefixed so they cannot collide with the sibling
// runner/report tests in this package). All artifacts are hand-built —
// no fixtures, no parser binary.

func scoreBox(y0 float64) *parser.BoundingBox {
	return &parser.BoundingBox{X0: 0, Y0: y0, X1: 1, Y1: y0 + 0.1}
}

func scoreBlock(docID string, page int, id, text string, box *parser.BoundingBox) parser.ContentBlock {
	return parser.ContentBlock{
		ID:   id,
		Type: parser.BlockText,
		Text: text,
		Evidence: parser.EvidenceLocation{
			DocumentID: docID,
			Page:       page,
			BlockID:    id,
			Box:        box,
		},
		Confidence: 0.9,
	}
}

func scoreBlockNoProv(docID string, page int, id, text string) parser.ContentBlock {
	b := scoreBlock(docID, page, id, text, nil)
	b.Evidence.BlockID = ""
	return b
}

func scoreTable(docID string, page int, id string, rows [][]string) parser.Table {
	t := parser.Table{ID: id, Page: page}
	for ri, r := range rows {
		var tr parser.TableRow
		for ci, txt := range r {
			tr.Cells = append(tr.Cells, parser.TableCell{
				Text:     txt,
				Evidence: parser.EvidenceLocation{DocumentID: docID, Page: page, BlockID: fmt.Sprintf("%s-r%d-c%d", id, ri, ci)},
				RowSpan:  1,
				ColSpan:  1,
			})
		}
		t.Rows = append(t.Rows, tr)
	}
	return t
}

func scoreDoc(docID string, blocks []parser.ContentBlock, tables []parser.Table) parser.ParsedDocument {
	pages := []parser.ParsedPage{}
	if blocks != nil || tables != nil {
		pages = append(pages, parser.ParsedPage{Number: 1, Blocks: blocks, Tables: tables})
	}
	if blocks == nil {
		// Normalize nil -> empty so the page still exists when tables do.
		for i := range pages {
			if pages[i].Blocks == nil {
				pages[i].Blocks = []parser.ContentBlock{}
			}
		}
	}
	return parser.ParsedDocument{
		DocumentID: docID,
		Pages:      pages,
		Metadata: parser.DocumentMetadata{
			ParserName:    "test",
			ParserVersion: "0.0.0",
			SourceSHA256:  "abc",
			SourceMedia:   "application/pdf",
			PageCount:     len(pages),
		},
	}
}

func scorePaise(v int64) *int64 { return &v }

func scoreFindField(t *testing.T, res CaseResult, key string) FieldScore {
	t.Helper()
	for _, f := range res.Fields {
		if f.Key == key {
			return f
		}
	}
	t.Fatalf("field %q not scored (fields=%v)", key, res.Fields)
	return FieldScore{}
}

func scoreFailSet(res CaseResult) map[FailureCode]int {
	m := map[FailureCode]int{}
	for _, f := range res.Failures {
		m[f]++
	}
	return m
}

func scoreNoIncorrect(t *testing.T, res CaseResult) {
	t.Helper()
	for _, f := range res.Fields {
		if f.Match == MatchIncorrect {
			t.Errorf("field %q emitted MatchIncorrect (documented limitation: never emitted)", f.Key)
		}
	}
}

func TestScoreFieldExact(t *testing.T) {
	doc := scoreDoc("doc-1",
		[]parser.ContentBlock{scoreBlock("doc-1", 1, "b1", "Claim Number: CLM-2026-0001", scoreBox(0.1))},
		nil)
	g := corpus.Golden{ClaimNumber: "CLM-2026-0001"}
	res := ScoreCase("CASE-001", "claim_form", "easy", doc, g)

	f := scoreFindField(t, res, "claim_number")
	if f.Match != MatchExact {
		t.Errorf("claim_number match = %q, want exact_match", f.Match)
	}
	if !f.HasProvenance {
		t.Errorf("claim_number HasProvenance = false, want true")
	}
	if got := scoreFailSet(res); got[FailFieldMissing] != 0 {
		t.Errorf("unexpected FIELD_MISSING: %v", res.Failures)
	}
	if res.Provenance.HitsTotal != 1 || res.Provenance.HitsWithPage != 1 ||
		res.Provenance.HitsWithBlock != 1 || res.Provenance.HitsWithBox != 1 {
		t.Errorf("provenance = %+v, want all tallies 1", res.Provenance)
	}
	scoreNoIncorrect(t, res)
}

func TestScoreFieldNormalized(t *testing.T) {
	doc := scoreDoc("doc-1", []parser.ContentBlock{
		scoreBlock("doc-1", 1, "b1", "Patient: aarav sharma", scoreBox(0.1)),
		scoreBlock("doc-1", 1, "b2", "Admitted: 2026/03/21", scoreBox(0.2)),
	}, nil)
	g := corpus.Golden{PatientName: "Aarav Sharma", AdmissionDate: "2026-03-21"}
	res := ScoreCase("CASE-001", "claim_form", "easy", doc, g)

	if f := scoreFindField(t, res, "patient_name"); f.Match != MatchNormalized {
		t.Errorf("patient_name match = %q, want normalized_match", f.Match)
	}
	if f := scoreFindField(t, res, "admission_date"); f.Match != MatchNormalized {
		t.Errorf("admission_date match = %q, want normalized_match", f.Match)
	}
	scoreNoIncorrect(t, res)
}

func TestScoreFieldMissing(t *testing.T) {
	doc := scoreDoc("doc-1",
		[]parser.ContentBlock{scoreBlock("doc-1", 1, "b1", "unrelated text here", scoreBox(0.1))},
		nil)
	g := corpus.Golden{ClaimNumber: "CLM-2026-0001", PolicyNumber: "POL-XYZ-42"}
	res := ScoreCase("CASE-001", "claim_form", "easy", doc, g)

	for _, key := range []string{"claim_number", "policy_number"} {
		f := scoreFindField(t, res, key)
		if f.Match != MatchMissing {
			t.Errorf("%s match = %q, want missing", key, f.Match)
		}
		if f.HasProvenance {
			t.Errorf("%s HasProvenance = true on a miss, want false", key)
		}
	}
	if got := scoreFailSet(res); got[FailFieldMissing] != 2 {
		t.Errorf("FIELD_MISSING count = %d, want 2 (once per key): %v", got[FailFieldMissing], res.Failures)
	}
	if res.Provenance.HitsTotal != 0 {
		t.Errorf("provenance HitsTotal = %d, want 0 (tallies over matched hits only)", res.Provenance.HitsTotal)
	}
	scoreNoIncorrect(t, res)
}

func TestScoreNumericPaise(t *testing.T) {
	cases := []struct {
		name       string
		paise      int64
		cand       string
		want       MatchClass
		wantMissCt int // FIELD_MISSING count: claim "CLM-1" always misses (+1 when amount misses)
	}{
		{"rupees_rendered_equals_digits", 14546500, "Total: Rs. 1,45,465.00", MatchNormalized, 1},
		{"rupees_no_paise_suffix", 14546500, "Total Rs 145465", MatchNormalized, 1},
		{"verbatim_paise_is_exact", 14546500, "Total (paise): 14546500", MatchExact, 1},
		{"symmetric_guard", 1454, "Billed Rs. 14.5400", MatchNormalized, 1},
		{"mismatch_is_missing", 14546500, "Total: Rs. 1,45,466.00", MatchMissing, 2},
		{"no_digits_is_missing", 14546500, "Total: not disclosed", MatchMissing, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := scoreDoc("doc-1",
				[]parser.ContentBlock{scoreBlock("doc-1", 1, "b1", tc.cand, scoreBox(0.1))},
				nil)
			g := corpus.Golden{ClaimNumber: "CLM-1", TotalAmountPaise: scorePaise(tc.paise)}
			res := ScoreCase("CASE-001", "hospital_bill", "easy", doc, g)
			f := scoreFindField(t, res, "total_amount_paise")
			if f.Match != tc.want {
				t.Errorf("total_amount_paise match = %q, want %q (candidate %q)", f.Match, tc.want, tc.cand)
			}
			if got := scoreFailSet(res)[FailFieldMissing]; got != tc.wantMissCt {
				t.Errorf("FIELD_MISSING count = %d, want %d: %v", got, tc.wantMissCt, res.Failures)
			}
			scoreNoIncorrect(t, res)
		})
	}
}

func TestScoreLineItems(t *testing.T) {
	doc := scoreDoc("doc-1", []parser.ContentBlock{
		scoreBlock("doc-1", 1, "b1", "Charges include Room Rent for 3 days", scoreBox(0.1)),
	}, nil)
	g := corpus.Golden{
		ClaimNumber: "CLM-1",
		LineItems: []corpus.GoldenLineItem{
			{Description: "Room Rent", AmountPaise: 30000},
			{Description: "Pharmacy", AmountPaise: 12500},
		},
	}
	res := ScoreCase("CASE-001", "claim_form", "easy", doc, g)

	if f := scoreFindField(t, res, "line_item[0]"); f.Match != MatchExact {
		t.Errorf("line_item[0] match = %q, want exact_match (verbatim substring)", f.Match)
	}
	if f := scoreFindField(t, res, "line_item[1]"); f.Match != MatchMissing {
		t.Errorf("line_item[1] match = %q, want missing", f.Match)
	}
	// Item amounts must not be scored as fields (total-signal duplication).
	for _, f := range res.Fields {
		if strings.Contains(f.Key, "amount") && f.Key != "total_amount_paise" {
			t.Errorf("unexpected amount field scored: %q", f.Key)
		}
	}
	scoreNoIncorrect(t, res)
}

func TestScoreConflictDatesIndependent(t *testing.T) {
	doc := scoreDoc("doc-1", []parser.ContentBlock{
		scoreBlock("doc-1", 1, "b1", "Admitted 2026-03-21, discharged 2026-03-25", scoreBox(0.1)),
	}, nil)
	g := corpus.Golden{
		ClaimNumber:      "CLM-1",
		AdmissionDate:    "2026-03-21",
		DischargeDate:    "2026-03-25",
		ExpectedConflict: "DATE_CONFLICT",
	}
	res := ScoreCase("CASE-001", "claim_form", "hard", doc, g)
	if f := scoreFindField(t, res, "admission_date"); f.Match != MatchExact {
		t.Errorf("admission_date match = %q, want exact_match", f.Match)
	}
	if f := scoreFindField(t, res, "discharge_date"); f.Match != MatchExact {
		t.Errorf("discharge_date match = %q, want exact_match", f.Match)
	}
	scoreNoIncorrect(t, res)
}

func TestScoreSkippedKeys(t *testing.T) {
	doc := scoreDoc("doc-1",
		[]parser.ContentBlock{scoreBlock("doc-1", 1, "b1", "CLM-9", scoreBox(0.1))},
		nil)
	g := corpus.Golden{
		ClaimNumber:      "CLM-9",
		PatientName:      "", // empty golden: skipped
		ExpectedConflict: "DATE_CONFLICT",
		Extra: map[string]json.RawMessage{
			"bundle_id":   json.RawMessage(`"B-1"`),
			"lab_results": json.RawMessage(`"neg"`),
		},
	}
	res := ScoreCase("CASE-001", "claim_form", "easy", doc, g)
	if len(res.Fields) != 1 || res.Fields[0].Key != "claim_number" {
		t.Errorf("fields = %v, want only [claim_number]", res.Fields)
	}
	scoreNoIncorrect(t, res)
}

func TestScoreTableVerdicts(t *testing.T) {
	items := []corpus.GoldenLineItem{
		{Description: "Room Rent", AmountPaise: 30000},
		{Description: "Pharmacy", AmountPaise: 12500},
	}
	cases := []struct {
		name        string
		docType     string
		golden      corpus.Golden
		tables      []parser.Table
		blocks      []parser.ContentBlock
		wantVerdict string
		wantFails   []FailureCode
		wantRows    int
	}{
		{
			name: "pass_all_rows", docType: "hospital_bill",
			golden:      corpus.Golden{ClaimNumber: "CLM-1", LineItems: items},
			tables:      []parser.Table{scoreTable("doc-1", 1, "t1", [][]string{{"Room Rent", "300"}, {"Pharmacy", "125"}})},
			wantVerdict: "TABLE_PASS", wantRows: 2,
		},
		{
			name: "partial_one_row_short", docType: "hospital_bill",
			golden:      corpus.Golden{ClaimNumber: "CLM-1", LineItems: items},
			tables:      []parser.Table{scoreTable("doc-1", 1, "t1", [][]string{{"Room Rent", "300"}, {"Misc", "9"}})},
			wantVerdict: "TABLE_PARTIAL", wantFails: []FailureCode{FailTablePartial}, wantRows: 1,
		},
		{
			name: "missed_bill_no_tables", docType: "hospital_bill",
			golden:      corpus.Golden{ClaimNumber: "CLM-1", LineItems: items},
			wantVerdict: "TABLE_MISSED", wantFails: []FailureCode{FailTableMissing},
		},
		{
			name: "missed_bill_flattened_to_paragraphs", docType: "bill",
			golden:      corpus.Golden{ClaimNumber: "CLM-1", LineItems: items},
			blocks:      []parser.ContentBlock{scoreBlock("doc-1", 1, "b1", "Room Rent 300, Pharmacy 125", scoreBox(0.1))},
			wantVerdict: "TABLE_MISSED", wantFails: []FailureCode{FailTableMissing},
		},
		{
			name: "na_no_expectation_no_tables", docType: "claim_form",
			golden:      corpus.Golden{ClaimNumber: "CLM-1"},
			wantVerdict: "TABLE_NA",
		},
		{
			name: "pass_extra_tables_without_expectation", docType: "claim_form",
			golden:      corpus.Golden{ClaimNumber: "CLM-1"},
			tables:      []parser.Table{scoreTable("doc-1", 1, "t1", [][]string{{"a", "b"}})},
			wantVerdict: "TABLE_PASS",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := scoreDoc("doc-1", tc.blocks, tc.tables)
			res := ScoreCase("CASE-001", tc.docType, "easy", doc, tc.golden)
			if res.Tables.Verdict != tc.wantVerdict {
				t.Errorf("verdict = %q, want %q", res.Tables.Verdict, tc.wantVerdict)
			}
			if res.Tables.RowsMatched != tc.wantRows {
				t.Errorf("rows_matched = %d, want %d", res.Tables.RowsMatched, tc.wantRows)
			}
			fails := scoreFailSet(res)
			for _, w := range tc.wantFails {
				if fails[w] != 1 {
					t.Errorf("failure %q count = %d, want 1 (failures=%v)", w, fails[w], res.Failures)
				}
			}
			if len(tc.wantFails) == 0 && (fails[FailTableMissing] != 0 || fails[FailTablePartial] != 0) {
				t.Errorf("unexpected table failure: %v", res.Failures)
			}
			scoreNoIncorrect(t, res)
		})
	}
}

func TestScoreTableCellsMatched(t *testing.T) {
	doc := scoreDoc("doc-1", nil,
		[]parser.Table{scoreTable("doc-1", 1, "t1", [][]string{{"Room Rent", "300"}, {"Other", "1"}})})
	g := corpus.Golden{LineItems: []corpus.GoldenLineItem{{Description: "Room Rent", AmountPaise: 1}}}
	res := ScoreCase("CASE-001", "hospital_bill", "easy", doc, g)
	if res.Tables.CellsMatched != 1 {
		t.Errorf("cells_matched = %d, want 1", res.Tables.CellsMatched)
	}
	if res.Tables.RowsMatched != 1 {
		t.Errorf("rows_matched = %d, want 1", res.Tables.RowsMatched)
	}
	scoreNoIncorrect(t, res)
}

func TestScoreProvenance(t *testing.T) {
	t.Run("missing_block_id_is_failure", func(t *testing.T) {
		doc := scoreDoc("doc-1",
			[]parser.ContentBlock{scoreBlockNoProv("doc-1", 1, "b1", "CLM-7")},
			nil)
		res := ScoreCase("CASE-001", "claim_form", "easy", doc, corpus.Golden{ClaimNumber: "CLM-7"})
		f := scoreFindField(t, res, "claim_number")
		if f.Match != MatchExact {
			t.Errorf("match = %q, want exact_match", f.Match)
		}
		if f.HasProvenance {
			t.Errorf("HasProvenance = true with blank block id, want false")
		}
		if res.Provenance.HitsTotal != 1 || res.Provenance.HitsWithBlock != 0 {
			t.Errorf("provenance = %+v, want total 1, with_block 0", res.Provenance)
		}
		if got := scoreFailSet(res); got[FailProvenanceMissing] != 1 {
			t.Errorf("PROVENANCE_MISSING count = %d, want 1: %v", got[FailProvenanceMissing], res.Failures)
		}
		scoreNoIncorrect(t, res)
	})
	t.Run("nil_box_is_tally_only", func(t *testing.T) {
		doc := scoreDoc("doc-1",
			[]parser.ContentBlock{scoreBlock("doc-1", 1, "b1", "CLM-7", nil)},
			nil)
		res := ScoreCase("CASE-001", "claim_form", "easy", doc, corpus.Golden{ClaimNumber: "CLM-7"})
		if res.Provenance.HitsTotal != 1 || res.Provenance.HitsWithBox != 0 || res.Provenance.HitsWithBlock != 1 {
			t.Errorf("provenance = %+v, want total 1, with_box 0, with_block 1", res.Provenance)
		}
		if got := scoreFailSet(res); got[FailProvenanceMissing] != 0 {
			t.Errorf("nil box must not raise PROVENANCE_MISSING: %v", res.Failures)
		}
		scoreNoIncorrect(t, res)
	})
}

func TestScoreReadingOrder(t *testing.T) {
	t.Run("violation", func(t *testing.T) {
		doc := scoreDoc("doc-1", []parser.ContentBlock{
			scoreBlock("doc-1", 1, "b1", "first", scoreBox(0.5)),
			scoreBlock("doc-1", 1, "b2", "second", scoreBox(0.2)),
		}, nil)
		res := ScoreCase("CASE-001", "claim_form", "easy", doc, corpus.Golden{})
		if got := scoreFailSet(res); got[FailReadingOrder] != 1 {
			t.Errorf("READING_ORDER_MISMATCH count = %d, want 1: %v", got[FailReadingOrder], res.Failures)
		}
	})
	t.Run("ordered_boxes_ok", func(t *testing.T) {
		doc := scoreDoc("doc-1", []parser.ContentBlock{
			scoreBlock("doc-1", 1, "b1", "first", scoreBox(0.1)),
			scoreBlock("doc-1", 1, "b2", "second", scoreBox(0.1)),
		}, nil)
		res := ScoreCase("CASE-001", "claim_form", "easy", doc, corpus.Golden{})
		if got := scoreFailSet(res); got[FailReadingOrder] != 0 {
			t.Errorf("unexpected READING_ORDER_MISMATCH: %v", res.Failures)
		}
	})
	t.Run("no_boxes_silent_skip", func(t *testing.T) {
		doc := scoreDoc("doc-1", []parser.ContentBlock{
			scoreBlock("doc-1", 1, "b1", "zzz second", nil),
			scoreBlock("doc-1", 1, "b2", "aaa first", nil),
		}, nil)
		res := ScoreCase("CASE-001", "claim_form", "easy", doc, corpus.Golden{})
		if got := scoreFailSet(res); got[FailReadingOrder] != 0 {
			t.Errorf("boxless pages must skip silently: %v", res.Failures)
		}
	})
}

func TestScoreEmptyArtifact(t *testing.T) {
	t.Run("zero_pages", func(t *testing.T) {
		doc := parser.ParsedDocument{DocumentID: "doc-1"}
		res := ScoreCase("CASE-001", "claim_form", "easy", doc, corpus.Golden{ClaimNumber: "CLM-1"})
		if !res.ParseOK {
			t.Errorf("ParseOK = false, want true (parse succeeded, content absent)")
		}
		fails := scoreFailSet(res)
		if fails[FailEmptyArtifact] != 1 {
			t.Errorf("EMPTY_ARTIFACT count = %d, want 1: %v", fails[FailEmptyArtifact], res.Failures)
		}
		if fails[FailPageStructure] != 1 {
			t.Errorf("PAGE_STRUCTURE_MISMATCH count = %d, want 1: %v", fails[FailPageStructure], res.Failures)
		}
		scoreNoIncorrect(t, res)
	})
	t.Run("blank_blocks_only", func(t *testing.T) {
		doc := scoreDoc("doc-1",
			[]parser.ContentBlock{scoreBlock("doc-1", 1, "b1", "   ", scoreBox(0.1))},
			nil)
		res := ScoreCase("CASE-001", "claim_form", "easy", doc, corpus.Golden{})
		if !res.ParseOK {
			t.Errorf("ParseOK = false, want true")
		}
		if got := scoreFailSet(res); got[FailEmptyArtifact] != 1 {
			t.Errorf("EMPTY_ARTIFACT count = %d, want 1: %v", got[FailEmptyArtifact], res.Failures)
		}
	})
}

func TestScoreIncorrectNeverEmitted(t *testing.T) {
	// Battery across exact / normalized / missing / numeric / tables /
	// provenance / order: pins the documented limitation that text search
	// cannot distinguish incorrect from missing.
	docs := []parser.ParsedDocument{
		scoreDoc("doc-1", []parser.ContentBlock{scoreBlock("doc-1", 1, "b1", "CLM-1 POL-2", scoreBox(0.1))}, nil),
		scoreDoc("doc-1", []parser.ContentBlock{scoreBlock("doc-1", 1, "b1", "nothing relevant", nil)}, nil),
		scoreDoc("doc-1", nil, []parser.Table{scoreTable("doc-1", 1, "t1", [][]string{{"Room Rent"}})}),
		scoreDoc("doc-1", []parser.ContentBlock{scoreBlockNoProv("doc-1", 1, "b1", "CLM-1")}, nil),
	}
	goldens := []corpus.Golden{
		{ClaimNumber: "CLM-1", PolicyNumber: "POL-2", PatientName: "Nobody Here", TotalAmountPaise: scorePaise(999)},
		{LineItems: []corpus.GoldenLineItem{{Description: "Room Rent", AmountPaise: 5}}},
		{},
	}
	for i, d := range docs {
		for j, g := range goldens {
			res := ScoreCase("CASE-BATT", "hospital_bill", "hard", d, g)
			for _, f := range res.Fields {
				if f.Match == MatchIncorrect {
					t.Errorf("doc %d golden %d field %q emitted incorrect", i, j, f.Key)
				}
			}
		}
	}
}

func TestScoreNoValueEcho(t *testing.T) {
	// PII rule: output carries keys/counts/codes, never golden VALUES.
	g := corpus.Golden{
		ClaimNumber:      "CLM-ZZQZXK-9999",
		PolicyNumber:     "POL-QWERTY-1234",
		PatientName:      "Zzxqxk Wqwert",
		Hospital:         "Zzxq General Wqert",
		AdmissionDate:    "2026-03-21",
		TotalAmountPaise: scorePaise(987654321),
		LineItems:        []corpus.GoldenLineItem{{Description: "Zzxq Special Wqert", AmountPaise: 111}},
	}
	doc := scoreDoc("doc-1", []parser.ContentBlock{
		scoreBlock("doc-1", 1, "b1", "CLM-ZZQZXK-9999 admitted 2026-03-21, total Rs. 98,76,543.21 for Zzxq Special Wqert", scoreBox(0.1)),
	}, nil)
	res := ScoreCase("CASE-001", "hospital_bill", "easy", doc, g)
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"CLM-ZZQZXK-9999", "POL-QWERTY-1234", "Zzxkx", "Zzxqxk Wqwert",
		"Zzxq General Wqert", "987654321", "Zzxq Special Wqert",
	} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("scored output echoes golden value %q", secret)
		}
	}
	// Keys must still be present.
	for _, key := range []string{"claim_number", "policy_number", "patient_name", "line_item[0]"} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("scored output missing field key %q", key)
		}
	}
}

func TestScoreRunnerOwnedFieldsZero(t *testing.T) {
	doc := scoreDoc("doc-1",
		[]parser.ContentBlock{scoreBlock("doc-1", 1, "b1", "CLM-1", scoreBox(0.1))},
		nil)
	res := ScoreCase("CASE-001", "claim_form", "easy", doc, corpus.Golden{ClaimNumber: "CLM-1"})
	if res.LatencyMs != 0 || res.InputBytes != 0 {
		t.Errorf("LatencyMs=%d InputBytes=%d, want 0 (runner-owned)", res.LatencyMs, res.InputBytes)
	}
	if res.CaseID != "CASE-001" || res.DocumentType != "claim_form" || res.Difficulty != "easy" {
		t.Errorf("identity fields not passed through: %+v", res)
	}
	if !res.ArtifactValid {
		t.Errorf("ArtifactValid = false for a Validate()-clean artifact")
	}
}
