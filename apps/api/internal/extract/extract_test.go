// External contract tests: status vocabulary, RefFrom projection, the
// normalizer tables, the public RunConformance entry, and the
// compile-level proof that verify.Input is constructible from
// DocumentFacts (the full mapper is #47's job). Deterministic,
// hand-built values only: no network, no I/O, no clock reads.
package extract_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"claimops-api/internal/extract"
	"claimops-api/internal/parser"
	"claimops-api/internal/verify"
)

func TestFieldStatusValues(t *testing.T) {
	cases := map[extract.FieldStatus]string{
		extract.StatusPresent:        "PRESENT",
		extract.StatusMissing:        "MISSING",
		extract.StatusAmbiguous:      "AMBIGUOUS",
		extract.StatusMultiCandidate: "MULTI_CANDIDATE",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("status = %q, want %q", string(got), want)
		}
	}
}

func TestRefFromDropsBox(t *testing.T) {
	loc := parser.EvidenceLocation{
		DocumentID: "doc-1",
		Page:       2,
		BlockID:    "b7",
		Box:        &parser.BoundingBox{X0: 0.1, Y0: 0.2, X1: 0.3, Y1: 0.4},
	}
	got := extract.RefFrom(loc)
	want := extract.EvidenceRef{DocumentID: "doc-1", Page: 2, BlockID: "b7"}
	if got != want {
		t.Fatalf("RefFrom = %+v, want %+v (box must not cross the layer)", got, want)
	}
}

// TestStatusMatrix pins the status rules as executable documentation:
// absent -> MISSING with nothing carried; conflicting -> MULTI_CANDIDATE
// with no top-level value; present-but-unparseable -> AMBIGUOUS with raw
// preserved; single stated value -> PRESENT with value plus page pin.
func TestStatusMatrix(t *testing.T) {
	ev := extract.EvidenceRef{DocumentID: "doc-1", Page: 1, BlockID: "b1"}
	rows := []struct {
		name  string
		field extract.ExtractedField
		check func(t *testing.T, f extract.ExtractedField)
	}{
		{
			name:  "present",
			field: extract.ExtractedField{Key: "k", Value: "v", Normalized: "v", Evidence: ev, Status: extract.StatusPresent},
			check: func(t *testing.T, f extract.ExtractedField) {
				t.Helper()
				if f.Value == "" || f.Evidence.Page < 1 {
					t.Errorf("PRESENT must carry value + page>=1: %+v", f)
				}
			},
		},
		{
			name:  "missing",
			field: extract.ExtractedField{Key: "k", Status: extract.StatusMissing},
			check: func(t *testing.T, f extract.ExtractedField) {
				t.Helper()
				if f.Value != "" || f.Normalized != "" || len(f.Candidates) != 0 || f.Evidence != (extract.EvidenceRef{}) {
					t.Errorf("MISSING must carry nothing: %+v", f)
				}
			},
		},
		{
			name:  "ambiguous",
			field: extract.ExtractedField{Key: "k", Value: "sometime last week", Evidence: ev, Status: extract.StatusAmbiguous},
			check: func(t *testing.T, f extract.ExtractedField) {
				t.Helper()
				if f.Value == "" || f.Normalized != "" {
					t.Errorf("AMBIGUOUS must preserve raw and normalize nothing: %+v", f)
				}
			},
		},
		{
			name: "multi",
			field: extract.ExtractedField{Key: "k", Status: extract.StatusMultiCandidate, Candidates: []extract.Candidate{
				{Value: "a", Evidence: ev},
				{Value: "b", Evidence: ev},
			}},
			check: func(t *testing.T, f extract.ExtractedField) {
				t.Helper()
				if f.Value != "" || len(f.Candidates) < 2 {
					t.Errorf("MULTI_CANDIDATE must pick no winner and keep >=2: %+v", f)
				}
			},
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) { r.check(t, r.field) })
	}
}

func TestNormalizeDate(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		want  string
		wantE error
	}{
		{name: "iso", in: "2026-01-05", want: "2026-01-05"},
		{name: "leap day", in: "2024-02-29", want: "2024-02-29"},
		{name: "padded spaces", in: "  2026-01-05\t", want: "2026-01-05"},
		{name: "empty maps to missing", in: "", wantE: extract.ErrBlankDate},
		{name: "blank maps to missing", in: "   ", wantE: extract.ErrBlankDate},
		{name: "slash locale rejected", in: "05/01/2026", wantE: extract.ErrInvalidDate},
		{name: "dash locale rejected", in: "05-01-2026", wantE: extract.ErrInvalidDate},
		{name: "non-padded rejected", in: "2026-1-5", wantE: extract.ErrInvalidDate},
		{name: "month 13 rejected", in: "2026-13-01", wantE: extract.ErrInvalidDate},
		{name: "feb 30 rejected", in: "2026-02-30", wantE: extract.ErrInvalidDate},
		{name: "non-leap feb 29 rejected", in: "2026-02-29", wantE: extract.ErrInvalidDate},
		{name: "words rejected", in: "sometime last week", wantE: extract.ErrInvalidDate},
		{name: "trailing junk rejected", in: "2026-01-05T00:00:00Z", wantE: extract.ErrInvalidDate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extract.NormalizeDate(tc.in)
			if tc.wantE != nil {
				if err != tc.wantE {
					t.Fatalf("NormalizeDate(%q) = %q, %v; want error %v", tc.in, got, err, tc.wantE)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeDate(%q): unexpected error %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeDate(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizePaise(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int64
	}{
		// Both directions of the rupees/paise convention converge: an
		// explicit decimal rendering and a bare rupees integer reach the
		// same minor units (the scorer's c+"00" == g arm accepts both).
		{name: "decimal rupees", in: "Rs. 1,45,465.00", want: 14546500},
		{name: "bare rupees", in: "Rs 145465", want: 14546500},
		{name: "indian grouping", in: "1,45,465.00", want: 14546500},
		{name: "rupee sign", in: "₹500.50", want: 50050},
		{name: "inr prefix", in: "INR 1,000", want: 100000},
		{name: "single decimal", in: "10.5", want: 1050},
		{name: "zero", in: "0", want: 0},
		{name: "zero decimal", in: "0.00", want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extract.NormalizePaise(tc.in)
			if err != nil {
				t.Fatalf("NormalizePaise(%q): unexpected error %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizePaise(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
	rejects := []struct {
		name string
		in   string
		want error
	}{
		{name: "empty maps to missing", in: "", want: extract.ErrBlankAmount},
		{name: "blank maps to missing", in: "   ", want: extract.ErrBlankAmount},
		{name: "words", in: "not disclosed", want: extract.ErrInvalidAmount},
		{name: "negative", in: "-5", want: extract.ErrInvalidAmount},
		{name: "explicit plus", in: "+5", want: extract.ErrInvalidAmount},
		{name: "three places", in: "10.123", want: extract.ErrInvalidAmount},
		{name: "double dot", in: "12.3.4", want: extract.ErrInvalidAmount},
		{name: "bare dot", in: ".", want: extract.ErrInvalidAmount},
	}
	for _, tc := range rejects {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := extract.NormalizePaise(tc.in); err != tc.want {
				t.Fatalf("NormalizePaise(%q) = %d, %v; want error %v", tc.in, got, err, tc.want)
			}
		})
	}
}

func TestNormalizeName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Lowercase-collapsed: mirrors verify.normalizeName so facts
		// arrive pre-folded for R2 identity comparison (documents
		// Title-cases instead — display, not identity).
		{name: "fold", in: "  Aparna   PRADHAN ", want: "aparna pradhan"},
		{name: "tabs", in: "\tRavi\nKumar\t", want: "ravi kumar"},
		{name: "empty", in: "", want: ""},
		{name: "blank", in: "   ", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extract.NormalizeName(tc.in); got != tc.want {
				t.Fatalf("NormalizeName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Trim only: case policy stays with verify R1, so extraction
		// never destroys information the judge may need.
		{name: "trim", in: "  POL-123  ", want: "POL-123"},
		{name: "case kept", in: "pol-123", want: "pol-123"},
		{name: "empty", in: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extract.NormalizeID(tc.in); got != tc.want {
				t.Fatalf("NormalizeID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// publicStub is a minimal Extractor proving the public RunConformance
// entry accepts conforming facts over a hand-built artifact.
type publicStub struct{ facts extract.DocumentFacts }

func (s publicStub) Name() string    { return "stub-extract" }
func (s publicStub) Version() string { return "0.0.1" }
func (s publicStub) Extract(ctx context.Context, doc parser.ParsedDocument) (extract.DocumentFacts, error) {
	if err := ctx.Err(); err != nil {
		return extract.DocumentFacts{}, err
	}
	return s.facts, nil
}

func TestRunConformancePublicEntry(t *testing.T) {
	const docID = "doc-public-1"
	mkBlock := func(id, text string) parser.ContentBlock {
		return parser.ContentBlock{
			ID:         id,
			Type:       parser.BlockText,
			Text:       text,
			Evidence:   parser.EvidenceLocation{DocumentID: docID, Page: 1, BlockID: id},
			Confidence: 0.9,
		}
	}
	fixture := parser.ParsedDocument{
		DocumentID: docID,
		Pages: []parser.ParsedPage{{Number: 1, Blocks: []parser.ContentBlock{
			mkBlock("b1", "Claim Number: CLM-1001"),
		}}},
		Metadata: parser.DocumentMetadata{
			ParserName: "stub-parser", ParserVersion: "0.0.1",
			SourceSHA256: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a8",
			SourceMedia:  "text/plain", PageCount: 1,
		},
	}
	if err := fixture.Validate(); err != nil {
		t.Fatalf("hand-built fixture invalid: %v", err)
	}
	facts := extract.DocumentFacts{
		DocumentID: docID,
		DocType:    "CLAIM_FORM",
		Fields: map[string]extract.ExtractedField{
			"claim_number": {
				Key: "claim_number", Value: "CLM-1001", Normalized: "CLM-1001",
				Evidence:         extract.EvidenceRef{DocumentID: docID, Page: 1, BlockID: "b1"},
				Status:           extract.StatusPresent,
				Extractor:        "stub-extract",
				ExtractorVersion: "0.0.1",
			},
			"policy_number": {
				Key: "policy_number", Status: extract.StatusMissing,
				Extractor: "stub-extract", ExtractorVersion: "0.0.1",
			},
		},
	}
	extract.RunConformance(t, publicStub{facts: facts}, fixture)
}

// TestVerifyInputConstructible is the compile-level proof that
// verify.Input can be built from DocumentFacts for one claim. The
// assembly below is illustrative only — the full mapper is #47's job.
func TestVerifyInputConstructible(t *testing.T) {
	const docID = "doc-verify-1"
	ev := extract.EvidenceRef{DocumentID: docID, Page: 1, BlockID: "b1"}
	totalPaise, err := extract.NormalizePaise("Rs. 1,45,465.00")
	if err != nil {
		t.Fatalf("NormalizePaise: %v", err)
	}
	facts := extract.DocumentFacts{
		DocumentID: docID,
		DocType:    verify.DocClaimForm,
		Fields: map[string]extract.ExtractedField{
			"policy_number": {
				Key: "policy_number", Value: "POL-123",
				Normalized:       extract.NormalizeID("POL-123"),
				Evidence:         ev,
				Status:           extract.StatusPresent,
				Extractor:        "stub",
				ExtractorVersion: "0.0.1",
			},
			"patient_name": {
				Key: "patient_name", Value: "Aparna Pradhan",
				Normalized:       extract.NormalizeName("Aparna Pradhan"),
				Evidence:         ev,
				Status:           extract.StatusPresent,
				Extractor:        "stub",
				ExtractorVersion: "0.0.1",
			},
			"admission_date": {
				Key: "admission_date", Value: "2026-01-05", Normalized: "2026-01-05",
				Evidence: ev, Status: extract.StatusPresent,
				Extractor: "stub", ExtractorVersion: "0.0.1",
			},
			"discharge_date": {
				Key: "discharge_date", Value: "2026-01-09", Normalized: "2026-01-09",
				Evidence: ev, Status: extract.StatusPresent,
				Extractor: "stub", ExtractorVersion: "0.0.1",
			},
			"total_bill": {
				Key: "total_bill", Value: "Rs. 1,45,465.00",
				Normalized: strconv.FormatInt(totalPaise, 10),
				Evidence:   ev, Status: extract.StatusPresent,
				Extractor: "stub", ExtractorVersion: "0.0.1",
			},
		},
	}

	admission, err := time.Parse("2006-01-02", facts.Fields["admission_date"].Normalized)
	if err != nil {
		t.Fatalf("admission Normalized not a date: %v", err)
	}
	discharge, err := time.Parse("2006-01-02", facts.Fields["discharge_date"].Normalized)
	if err != nil {
		t.Fatalf("discharge Normalized not a date: %v", err)
	}
	in := verify.Input{
		ClaimPolicyNumber: facts.Fields["policy_number"].Normalized,
		ClaimPatientName:  facts.Fields["patient_name"].Normalized,
		ClaimedPaise:      totalPaise,
		Admission:         admission,
		Discharge:         discharge,
		HasAdmission:      true,
		HasDischarge:      true,
		PolicyNumber:      "POL-123",
		PolicyPatient:     "aparna pradhan",
		PolicyActive:      true,
		DocsPresent: map[string]bool{
			verify.DocClaimForm:        true,
			verify.DocDischargeSummary: true,
			verify.DocHospitalBill:     true,
		},
		DocEvidence: map[string]map[string]string{
			facts.DocType: {
				"patient_name":   facts.Fields["patient_name"].Normalized,
				"admission_date": facts.Fields["admission_date"].Normalized,
			},
		},
		BillLinePaise:    []int64{10000000, 4546500},
		BillTotalPaise:   totalPaise,
		HasBillTotal:     true,
		ExternalPolicyOK: true,
	}
	res := verify.Verify(in)
	if !res.Passed {
		t.Fatalf("verify.Input built from facts did not pass: %+v", res.Exceptions)
	}
}
