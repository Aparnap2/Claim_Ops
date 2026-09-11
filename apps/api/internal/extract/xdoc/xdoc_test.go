// Tests for the xdoc deterministic extractors (issue #45). All fixtures
// are synthetic canonical artifacts loaded from testdata; no network, no
// I/O beyond fixture reads, no clock reads, no LLM.
package xdoc_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"claimops-api/internal/extract"
	"claimops-api/internal/extract/xdoc"
	"claimops-api/internal/parser"
)

// loadFixture reads one testdata artifact and asserts it is a valid
// canonical document. Fixtures must satisfy parser.Validate: the
// extractors consume validated artifacts only.
func loadFixture(t *testing.T, name string) parser.ParsedDocument {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var doc parser.ParsedDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("fixture %s invalid: %v", name, err)
	}
	return doc
}

// extractorSet binds every extractor to its happy-path fixture for the
// conformance and determinism sweeps.
var extractorSet = []struct {
	ext     extract.Extractor
	docType string
	fixture string
}{
	{xdoc.ClaimForm{}, xdoc.DocClaimForm, "claim_form.json"},
	{xdoc.DischargeSummary{}, xdoc.DocDischargeSummary, "discharge_summary.json"},
	{xdoc.HospitalBill{}, xdoc.DocHospitalBill, "hospital_bill.json"},
	{xdoc.PolicySchedule{}, xdoc.DocPolicySchedule, "policy_schedule.json"},
	{xdoc.PreauthForm{}, xdoc.DocPreauthForm, "preauth_form.json"},
	{xdoc.LabReport{}, xdoc.DocLabReport, "lab_report.json"},
}

// TestConformance runs the #44 contract runner against every extractor over
// its happy-path artifact: identity, status-rule shape, page provenance,
// and the no-fabrication rule.
func TestConformance(t *testing.T) {
	for _, e := range extractorSet {
		t.Run(e.docType, func(t *testing.T) {
			extract.RunConformance(t, e.ext, loadFixture(t, e.fixture))
		})
	}
}

// allowedKey reports whether k is a canonical key or a member of the
// bill_lines_N line-item family.
func allowedKey(k string) bool {
	switch k {
	case xdoc.KeyClaimNumber, xdoc.KeyPolicyNumber, xdoc.KeyPatientName,
		xdoc.KeyHospitalName, xdoc.KeyAdmissionDate, xdoc.KeyDischargeDate,
		xdoc.KeyTotalAmountPaise, xdoc.KeyBillLines, xdoc.KeyDiagnosis,
		xdoc.KeyProcedure:
		return true
	}
	return strings.HasPrefix(k, xdoc.KeyBillLines+"_")
}

// TestKeyAllowlist pins the exact-key rule: no synonyms, no renames, no
// surprise keys on any happy-path extraction.
func TestKeyAllowlist(t *testing.T) {
	for _, e := range extractorSet {
		t.Run(e.docType, func(t *testing.T) {
			facts, err := e.ext.Extract(context.Background(), loadFixture(t, e.fixture))
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if facts.DocType != e.docType {
				t.Fatalf("DocType = %q, want %q", facts.DocType, e.docType)
			}
			for k := range facts.Fields {
				if !allowedKey(k) {
					t.Errorf("key %q is not a canonical key", k)
				}
			}
		})
	}
}

// mustPresent asserts one PRESENT field's raw, normalized, and evidence pin.
func mustPresent(t *testing.T, facts extract.DocumentFacts, key, value, normalized, block string) {
	t.Helper()
	f, ok := facts.Fields[key]
	if !ok {
		t.Fatalf("key %q absent", key)
	}
	if f.Status != extract.StatusPresent {
		t.Fatalf("key %q status = %s, want PRESENT", key, f.Status)
	}
	if f.Value != value {
		t.Errorf("key %q value = %q, want %q", key, f.Value, value)
	}
	if f.Normalized != normalized {
		t.Errorf("key %q normalized = %q, want %q", key, f.Normalized, normalized)
	}
	if f.Evidence.DocumentID != facts.DocumentID || f.Evidence.Page != 1 || f.Evidence.BlockID != block {
		t.Errorf("key %q evidence = %+v, want doc %q page 1 block %q", key, f.Evidence, facts.DocumentID, block)
	}
}

// mustMissing asserts one key carries the MISSING shape (nothing carried).
func mustMissing(t *testing.T, facts extract.DocumentFacts, key string) {
	t.Helper()
	f, ok := facts.Fields[key]
	if !ok {
		t.Fatalf("key %q absent (declared keys must be explicit MISSING)", key)
	}
	if f.Status != extract.StatusMissing {
		t.Fatalf("key %q status = %s, want MISSING", key, f.Status)
	}
}

func TestClaimFormGolden(t *testing.T) {
	facts, err := xdoc.ClaimForm{}.Extract(context.Background(), loadFixture(t, "claim_form.json"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	mustPresent(t, facts, xdoc.KeyClaimNumber, "CLM-2026-0042", "CLM-2026-0042", "b2")
	mustPresent(t, facts, xdoc.KeyPolicyNumber, "POL-HS-88123", "POL-HS-88123", "b3")
	mustPresent(t, facts, xdoc.KeyPatientName, "Aarav Sharma", "aarav sharma", "b4")
	mustPresent(t, facts, xdoc.KeyHospitalName, "CityCare Hospital", "citycare hospital", "b5")
	mustPresent(t, facts, xdoc.KeyAdmissionDate, "2026-01-05", "2026-01-05", "b6")
	mustPresent(t, facts, xdoc.KeyDischargeDate, "2026-01-09", "2026-01-09", "b7")
	mustPresent(t, facts, xdoc.KeyTotalAmountPaise, "Rs. 1,45,465.00", "14546500", "b8")
	mustPresent(t, facts, xdoc.KeyDiagnosis, "Acute appendicitis", "acute appendicitis", "b9")
	mustPresent(t, facts, xdoc.KeyProcedure, "Laparoscopic appendectomy", "laparoscopic appendectomy", "b10")
	if len(facts.Fields) != 9 {
		t.Errorf("fields = %d keys, want 9 (no extras, no bill lines)", len(facts.Fields))
	}
}

func TestDischargeSummaryGolden(t *testing.T) {
	facts, err := xdoc.DischargeSummary{}.Extract(context.Background(), loadFixture(t, "discharge_summary.json"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	mustPresent(t, facts, xdoc.KeyPatientName, "Aarav Sharma", "aarav sharma", "b2")
	mustPresent(t, facts, xdoc.KeyHospitalName, "CityCare Hospital", "citycare hospital", "b3")
	mustPresent(t, facts, xdoc.KeyAdmissionDate, "2026-01-05", "2026-01-05", "b4")
	mustPresent(t, facts, xdoc.KeyDischargeDate, "2026-01-09", "2026-01-09", "b5")
	mustPresent(t, facts, xdoc.KeyDiagnosis, "Acute appendicitis", "acute appendicitis", "b6")
	mustPresent(t, facts, xdoc.KeyProcedure, "Laparoscopic appendectomy", "laparoscopic appendectomy", "b7")
	if len(facts.Fields) != 6 {
		t.Errorf("fields = %d keys, want 6", len(facts.Fields))
	}
}

func TestHospitalBillGolden(t *testing.T) {
	facts, err := xdoc.HospitalBill{}.Extract(context.Background(), loadFixture(t, "hospital_bill.json"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	mustPresent(t, facts, xdoc.KeyPatientName, "Aarav Sharma", "aarav sharma", "b2")
	mustPresent(t, facts, xdoc.KeyHospitalName, "CityCare Hospital", "citycare hospital", "b3")
	mustPresent(t, facts, xdoc.KeyAdmissionDate, "2026-01-05", "2026-01-05", "b4")
	mustPresent(t, facts, xdoc.KeyDischargeDate, "2026-01-09", "2026-01-09", "b5")
	mustPresent(t, facts, xdoc.KeyTotalAmountPaise, "Rs. 1,45,465.00", "14546500", "b6")
	// Line items come from the canonical table in reading order; the
	// header row and the totals row must not leak into the family.
	wantLines := []string{"Room rent - 4 days", "Surgery charges", "Pharmacy"}
	for i, want := range wantLines {
		key := xdoc.KeyBillLines + "_" + strconv.Itoa(i)
		f, ok := facts.Fields[key]
		if !ok {
			t.Fatalf("key %q absent", key)
		}
		if f.Status != extract.StatusPresent {
			t.Fatalf("key %q status = %s, want PRESENT", key, f.Status)
		}
		if f.Value != want {
			t.Errorf("key %q value = %q, want %q", key, f.Value, want)
		}
		if f.Evidence.DocumentID != facts.DocumentID || f.Evidence.Page != 1 {
			t.Errorf("key %q evidence = %+v, want doc page 1 (table cells carry no block-id)", key, f.Evidence)
		}
	}
	if _, ok := facts.Fields[xdoc.KeyBillLines]; ok {
		t.Errorf("bare %q must be absent when indexed lines exist", xdoc.KeyBillLines)
	}
	if len(facts.Fields) != 8 {
		t.Errorf("fields = %d keys, want 8 (5 scalar + 3 lines)", len(facts.Fields))
	}
}

func TestPolicyScheduleGolden(t *testing.T) {
	facts, err := xdoc.PolicySchedule{}.Extract(context.Background(), loadFixture(t, "policy_schedule.json"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	mustPresent(t, facts, xdoc.KeyPolicyNumber, "POL-HS-88123", "POL-HS-88123", "b2")
	mustPresent(t, facts, xdoc.KeyPatientName, "Aarav Sharma", "aarav sharma", "b3")
	if len(facts.Fields) != 2 {
		t.Errorf("fields = %d keys, want 2", len(facts.Fields))
	}
}

func TestPreauthFormGolden(t *testing.T) {
	facts, err := xdoc.PreauthForm{}.Extract(context.Background(), loadFixture(t, "preauth_form.json"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	mustPresent(t, facts, xdoc.KeyClaimNumber, "CLM-2026-0042", "CLM-2026-0042", "b2")
	mustPresent(t, facts, xdoc.KeyPolicyNumber, "POL-HS-88123", "POL-HS-88123", "b3")
	mustPresent(t, facts, xdoc.KeyPatientName, "Aarav Sharma", "aarav sharma", "b4")
	mustPresent(t, facts, xdoc.KeyHospitalName, "CityCare Hospital", "citycare hospital", "b5")
	mustPresent(t, facts, xdoc.KeyAdmissionDate, "2026-01-05", "2026-01-05", "b6")
	mustPresent(t, facts, xdoc.KeyDiagnosis, "Acute appendicitis", "acute appendicitis", "b7")
	mustPresent(t, facts, xdoc.KeyProcedure, "Laparoscopic appendectomy", "laparoscopic appendectomy", "b8")
	mustPresent(t, facts, xdoc.KeyTotalAmountPaise, "Rs. 1,50,000.00", "15000000", "b9")
	if _, ok := facts.Fields[xdoc.KeyDischargeDate]; ok {
		t.Errorf("key %q must be absent: no discharge date at pre-authorization time", xdoc.KeyDischargeDate)
	}
}

func TestLabReportGolden(t *testing.T) {
	facts, err := xdoc.LabReport{}.Extract(context.Background(), loadFixture(t, "lab_report.json"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	mustPresent(t, facts, xdoc.KeyPatientName, "Aarav Sharma", "aarav sharma", "b2")
	mustPresent(t, facts, xdoc.KeyHospitalName, "CityCare Diagnostics", "citycare diagnostics", "b3")
	mustPresent(t, facts, xdoc.KeyDiagnosis, "Findings suggestive of acute appendicitis", "findings suggestive of acute appendicitis", "b4")
	if len(facts.Fields) != 3 {
		t.Errorf("fields = %d keys, want 3", len(facts.Fields))
	}
}

// TestAdversarialMissing pins the sparse-artifact rule: labels absent, so
// every declared key is explicit MISSING.
func TestAdversarialMissing(t *testing.T) {
	facts, err := xdoc.ClaimForm{}.Extract(context.Background(), loadFixture(t, "adversarial_missing.json"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, k := range []string{
		xdoc.KeyClaimNumber, xdoc.KeyPolicyNumber, xdoc.KeyPatientName,
		xdoc.KeyHospitalName, xdoc.KeyAdmissionDate, xdoc.KeyDischargeDate,
		xdoc.KeyTotalAmountPaise, xdoc.KeyDiagnosis, xdoc.KeyProcedure,
	} {
		mustMissing(t, facts, k)
	}
}

// TestAdversarialMulti pins the no-silent-winner rule: two conflicting
// claim numbers surface both candidates, and the untouched patient name
// stays PRESENT.
func TestAdversarialMulti(t *testing.T) {
	facts, err := xdoc.ClaimForm{}.Extract(context.Background(), loadFixture(t, "adversarial_multi.json"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	f := facts.Fields[xdoc.KeyClaimNumber]
	if f.Status != extract.StatusMultiCandidate {
		t.Fatalf("claim_number status = %s, want MULTI_CANDIDATE", f.Status)
	}
	if f.Value != "" || f.Normalized != "" {
		t.Fatalf("MULTI_CANDIDATE carries top-level value %q/%q (never a silent winner)", f.Value, f.Normalized)
	}
	if len(f.Candidates) != 2 {
		t.Fatalf("candidates = %d, want 2", len(f.Candidates))
	}
	got := map[string]bool{}
	for _, c := range f.Candidates {
		got[c.Value] = true
	}
	if !got["CLM-2026-0042"] || !got["CLM-2026-0099"] {
		t.Fatalf("candidates = %v, want both conflicting values", got)
	}
	mustPresent(t, facts, xdoc.KeyPatientName, "Aarav Sharma", "aarav sharma", "b3")
}

// TestAdversarialAmbiguous pins the unparseable rule: locale dates and
// non-numeric amounts keep their raw text with no normalized form.
func TestAdversarialAmbiguous(t *testing.T) {
	facts, err := xdoc.ClaimForm{}.Extract(context.Background(), loadFixture(t, "adversarial_ambiguous.json"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	admission := facts.Fields[xdoc.KeyAdmissionDate]
	if admission.Status != extract.StatusAmbiguous || admission.Value != "05/01/2026" || admission.Normalized != "" {
		t.Errorf("admission_date = %+v, want AMBIGUOUS raw 05/01/2026", admission)
	}
	total := facts.Fields[xdoc.KeyTotalAmountPaise]
	if total.Status != extract.StatusAmbiguous || total.Value != "not disclosed" || total.Normalized != "" {
		t.Errorf("total_amount_paise = %+v, want AMBIGUOUS raw not disclosed", total)
	}
}

// TestAdversarialEmpty pins the empty-artifact rule: zero pages still
// validate, and every declared key is MISSING.
func TestAdversarialEmpty(t *testing.T) {
	doc := loadFixture(t, "adversarial_empty.json")
	for _, e := range extractorSet {
		t.Run(e.docType, func(t *testing.T) {
			facts, err := e.ext.Extract(context.Background(), doc)
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if facts.DocumentID != doc.DocumentID || facts.DocType != e.docType {
				t.Fatalf("identity = %q/%q, want %q/%q", facts.DocumentID, facts.DocType, doc.DocumentID, e.docType)
			}
			for k, f := range facts.Fields {
				if f.Status != extract.StatusMissing {
					t.Errorf("key %q status = %s on empty artifact, want MISSING", k, f.Status)
				}
			}
		})
	}
}

// TestAdversarialBillNoTable pins the no-hallucination rule: line-like
// block prose without canonical tables yields bill_lines MISSING while the
// labeled total still extracts.
func TestAdversarialBillNoTable(t *testing.T) {
	facts, err := xdoc.HospitalBill{}.Extract(context.Background(), loadFixture(t, "adversarial_bill_no_table.json"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	mustMissing(t, facts, xdoc.KeyBillLines)
	mustPresent(t, facts, xdoc.KeyTotalAmountPaise, "Rs. 1,45,465.00", "14546500", "b4")
	for k := range facts.Fields {
		if strings.HasPrefix(k, xdoc.KeyBillLines+"_") {
			t.Errorf("key %q hallucinated from prose without tables", k)
		}
	}
}

// TestDeterminism runs every extractor twice over its fixture and requires
// byte-identical observations.
func TestDeterminism(t *testing.T) {
	for _, e := range extractorSet {
		t.Run(e.docType, func(t *testing.T) {
			doc := loadFixture(t, e.fixture)
			first, err := e.ext.Extract(context.Background(), doc)
			if err != nil {
				t.Fatalf("first Extract: %v", err)
			}
			second, err := e.ext.Extract(context.Background(), doc)
			if err != nil {
				t.Fatalf("second Extract: %v", err)
			}
			if !reflect.DeepEqual(first, second) {
				t.Fatalf("non-deterministic extraction:\nfirst=%#v\nsecond=%#v", first, second)
			}
		})
	}
}

// inlineDoc builds a single-block canonical artifact for engine-path tests.
func inlineDoc(t *testing.T, docID, text string) parser.ParsedDocument {
	t.Helper()
	doc := parser.ParsedDocument{
		DocumentID: docID,
		Pages: []parser.ParsedPage{{Number: 1, Blocks: []parser.ContentBlock{{
			ID: "b1", Type: parser.BlockText, Text: text,
			Evidence:   parser.EvidenceLocation{DocumentID: docID, Page: 1, BlockID: "b1"},
			Confidence: 1.0,
		}}}},
		Metadata: parser.DocumentMetadata{ParserName: "test", ParserVersion: "0",
			SourceSHA256: "abc", SourceMedia: "application/pdf", PageCount: 1},
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("inline doc invalid: %v", err)
	}
	return doc
}

// TestContinuationWindow pins the two-line continuation bound: an adjacent
// (or one-blank-gap) value is PRESENT; prose two blanks down is MISSING.
func TestContinuationWindow(t *testing.T) {
	ext := xdoc.DischargeSummary{}
	near := inlineDoc(t, "doc-near", "Diagnosis:\n\nAcute appendicitis")
	facts, err := ext.Extract(context.Background(), near)
	if err != nil {
		t.Fatal(err)
	}
	if f := facts.Fields["diagnosis"]; f.Status != extract.StatusPresent || f.Value != "Acute appendicitis" {
		t.Fatalf("adjacent continuation = %+v, want PRESENT Acute appendicitis", f)
	}
	far := inlineDoc(t, "doc-far", "Diagnosis:\n\n\nDistant boilerplate prose")
	facts, err = ext.Extract(context.Background(), far)
	if err != nil {
		t.Fatal(err)
	}
	if f := facts.Fields["diagnosis"]; f.Status != extract.StatusMissing {
		t.Fatalf("distant prose = %+v, want MISSING", f)
	}
}

// TestLongestAliasFirst pins the headline regex property: a longer alias
// wins over its shorter prefix on the same line.
func TestLongestAliasFirst(t *testing.T) {
	doc := inlineDoc(t, "doc-alias", "Total Amount Payable: Rs. 500")
	facts, err := xdoc.ClaimForm{}.Extract(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if f := facts.Fields["total_amount_paise"]; f.Status != extract.StatusPresent {
		t.Fatalf("longest alias = %+v, want PRESENT total_amount_paise", f)
	}
}

// TestExactDuplicateAgreement pins repeats-as-agreement: the same value
// twice is PRESENT, not MULTI_CANDIDATE.
func TestExactDuplicateAgreement(t *testing.T) {
	doc := inlineDoc(t, "doc-dup", "Patient Name: Aarav Sharma\nPatient Name: Aarav Sharma")
	facts, err := xdoc.ClaimForm{}.Extract(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if f := facts.Fields["patient_name"]; f.Status != extract.StatusPresent {
		t.Fatalf("exact repeat = %+v, want PRESENT (agreement, not conflict)", f)
	}
}

// TestSerialRowRejected pins the money-shape gate: a table row with only
// a serial number and no amount must not fabricate a bill line.
func TestSerialRowRejected(t *testing.T) {
	cell := func(text string) parser.TableCell {
		return parser.TableCell{Text: text,
			Evidence: parser.EvidenceLocation{DocumentID: "doc-serial", Page: 1},
			RowSpan:  1, ColSpan: 1}
	}
	doc := parser.ParsedDocument{
		DocumentID: "doc-serial",
		Pages: []parser.ParsedPage{{Number: 1,
			Blocks: []parser.ContentBlock{{
				ID: "b1", Type: parser.BlockText, Text: "Bill",
				Evidence:   parser.EvidenceLocation{DocumentID: "doc-serial", Page: 1, BlockID: "b1"},
				Confidence: 1.0,
			}},
			Tables: []parser.Table{{ID: "t1", Page: 1, Rows: []parser.TableRow{
				{Cells: []parser.TableCell{cell("S.No"), cell("Description"), cell("Amount")}},
				{Cells: []parser.TableCell{cell("4"), cell("Stationery")}},
				{Cells: []parser.TableCell{cell("1"), cell("X-ray"), cell("Rs. 850")}},
			}}},
		}},
		Metadata: parser.DocumentMetadata{ParserName: "test", ParserVersion: "0",
			SourceSHA256: "abc", SourceMedia: "application/pdf", PageCount: 1},
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("inline doc invalid: %v", err)
	}
	facts, err := xdoc.HospitalBill{}.Extract(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := facts.Fields["bill_lines_1"]; ok {
		t.Fatalf("serial-only row fabricated a line: %+v", facts.Fields)
	}
	f0, ok := facts.Fields["bill_lines_0"]
	if !ok || f0.Status != extract.StatusPresent || f0.Value != "X-ray" {
		t.Fatalf("real row = %+v, want PRESENT X-ray", facts.Fields)
	}
}
