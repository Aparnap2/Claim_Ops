// Pure unit tests for the Tier-1 document intelligence foundation: no
// network, no I/O, no clock reads. Covers the Classify matrix (keywords,
// priority order, fallback), ExtractFields on sample texts (full vs
// partial-line confidence, anchors, unknown-type nil), and the
// NormalizeName/NormalizeDate/NormalizePaise edge cases.
package documents_test

import (
	"strings"
	"testing"
	"time"

	"claimops-api/internal/documents"
)

func TestClassifyMatrix(t *testing.T) {
	cases := []struct {
		name     string
		fileName string
		wantType documents.DocType
		wantConf float64
	}{
		{name: "claim form underscore", fileName: "claim_form_scan.pdf", wantType: documents.DocClaimForm, wantConf: 0.9},
		{name: "claim form joined", fileName: "ClaimForm-2026.jpg", wantType: documents.DocClaimForm, wantConf: 0.9},
		{name: "claim form uppercase", fileName: "CLAIM_FORM.PDF", wantType: documents.DocClaimForm, wantConf: 0.9},
		{name: "discharge summary", fileName: "discharge_summary.pdf", wantType: documents.DocDischargeSummary, wantConf: 0.9},
		{name: "discharge mixed case", fileName: "Discharge-Summary-final.PDF", wantType: documents.DocDischargeSummary, wantConf: 0.9},
		{name: "hospital bill", fileName: "hospital_bill_final.pdf", wantType: documents.DocHospitalBill, wantConf: 0.85},
		{name: "invoice", fileName: "invoice_2024-118.pdf", wantType: documents.DocHospitalBill, wantConf: 0.85},
		{name: "policy", fileName: "policy_schedule.pdf", wantType: documents.DocPolicyDocument, wantConf: 0.9},
		{name: "aadhaar", fileName: "aadhaar_card.png", wantType: documents.DocIdentityDocument, wantConf: 0.8},
		{name: "pan token", fileName: "pan_card.jpg", wantType: documents.DocIdentityDocument, wantConf: 0.8},
		{name: "pan hyphen", fileName: "my-pan-doc.pdf", wantType: documents.DocIdentityDocument, wantConf: 0.8},
		{name: "passport", fileName: "passport_scan.pdf", wantType: documents.DocIdentityDocument, wantConf: 0.8},
		{name: "identity", fileName: "identity_proof.pdf", wantType: documents.DocIdentityDocument, wantConf: 0.8},
		{name: "unknown", fileName: "random_notes.txt", wantType: documents.DocUnknown, wantConf: 0.0},
		{name: "unknown empty", fileName: "", wantType: documents.DocUnknown, wantConf: 0.0},
		{name: "pan substring rejected", fileName: "company_panel_report.pdf", wantType: documents.DocUnknown, wantConf: 0.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotType, gotConf := documents.Classify(tc.fileName, "")
			if gotType != tc.wantType {
				t.Fatalf("Classify(%q): want type %q, got %q", tc.fileName, tc.wantType, gotType)
			}
			if gotConf != tc.wantConf {
				t.Fatalf("Classify(%q): want confidence %v, got %v", tc.fileName, tc.wantConf, gotConf)
			}
		})
	}
}

func TestClassifyPriorityOrder(t *testing.T) {
	cases := []struct {
		name     string
		fileName string
		wantType documents.DocType
	}{
		{name: "claim form beats discharge", fileName: "claim_form_discharge.pdf", wantType: documents.DocClaimForm},
		{name: "discharge beats bill", fileName: "discharge_bill.pdf", wantType: documents.DocDischargeSummary},
		{name: "bill beats policy", fileName: "policy_bill.pdf", wantType: documents.DocHospitalBill},
		{name: "policy beats identity", fileName: "policy_passport.pdf", wantType: documents.DocPolicyDocument},
		{name: "bill beats identity", fileName: "invoice_aadhaar.pdf", wantType: documents.DocHospitalBill},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotType, _ := documents.Classify(tc.fileName, "")
			if gotType != tc.wantType {
				t.Fatalf("Classify(%q): want %q, got %q", tc.fileName, tc.wantType, gotType)
			}
		})
	}
}

func TestExtractFieldsSampleDischarge(t *testing.T) {
	content := "Patient Name: Aparna Pradhan\n" +
		"Policy Number = POL-12345\n" +
		"Claim No: CLM-987\n" +
		"Date of Admission: 10/02/2026\n" +
		"Discharge Date: 2026-02-15\n" +
		"Hospital Name : City Care Hospital\n" +
		"Net Payable: Rs. 45250.50\n"
	got := documents.ExtractFields(documents.DocDischargeSummary, content)
	byName := make(map[string]documents.Field, len(got))
	for _, f := range got {
		byName[f.Name] = f
	}
	want := map[string]string{
		"patient_name":   "Aparna Pradhan",
		"policy_number":  "POL-12345",
		"claim_number":   "CLM-987",
		"admission_date": "10/02/2026",
		"discharge_date": "2026-02-15",
		"hospital_name":  "City Care Hospital",
		"total_bill":     "Rs. 45250.50",
	}
	if len(byName) != len(want) {
		t.Fatalf("want %d fields, got %d (%v)", len(want), len(byName), byName)
	}
	for name, value := range want {
		f, ok := byName[name]
		if !ok {
			t.Fatalf("missing field %q", name)
		}
		if f.Value != value {
			t.Fatalf("field %q: want value %q, got %q", name, value, f.Value)
		}
		if f.Page != 1 {
			t.Fatalf("field %q: want page 1, got %d", name, f.Page)
		}
		if f.Confidence != 0.9 {
			t.Fatalf("field %q: want confidence 0.9, got %v", name, f.Confidence)
		}
		if f.Anchor == "" || len([]rune(f.Anchor)) > 200 {
			t.Fatalf("field %q: anchor must be non-blank and <=200 chars, got %q", name, f.Anchor)
		}
	}
}

func TestExtractFieldsPartialLineConfidence(t *testing.T) {
	content := "Admitted on priority, Patient Name: Ravi Kumar, ward 3\n"
	got := documents.ExtractFields(documents.DocClaimForm, content)
	if len(got) != 1 {
		t.Fatalf("want 1 field, got %v", got)
	}
	if got[0].Name != "patient_name" {
		t.Fatalf("want patient_name, got %q", got[0].Name)
	}
	if got[0].Confidence != 0.6 {
		t.Fatalf("want confidence 0.6 for embedded key, got %v", got[0].Confidence)
	}
}

func TestExtractFieldsUnknownTypeNil(t *testing.T) {
	if got := documents.ExtractFields(documents.DocUnknown, "Patient Name: X\n"); got != nil {
		t.Fatalf("want nil for unknown docType, got %v", got)
	}
	if got := documents.ExtractFields("FORGED_TYPE", "Patient Name: X\n"); got != nil {
		t.Fatalf("want nil for forged docType, got %v", got)
	}
}

func TestExtractFieldsAnchorTruncated(t *testing.T) {
	line := "Patient Name: " + strings.Repeat("A", 300)
	got := documents.ExtractFields(documents.DocHospitalBill, line)
	if len(got) != 1 {
		t.Fatalf("want 1 field, got %v", got)
	}
	if n := len([]rune(got[0].Anchor)); n != 200 {
		t.Fatalf("want anchor truncated to 200 chars, got %d", n)
	}
}

func TestNormalizeName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "double space", in: "Aparna  Pradhan", want: "Aparna Pradhan"},
		{name: "pad and case", in: "  aparna   PRADHAN ", want: "Aparna Pradhan"},
		{name: "single", in: "arjun", want: "Arjun"},
		{name: "empty", in: "", want: ""},
		{name: "blanks", in: "   ", want: ""},
		{name: "multi word", in: "ram kumar  SHARMA", want: "Ram Kumar Sharma"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := documents.NormalizeName(tc.in); got != tc.want {
				t.Fatalf("NormalizeName(%q): want %q, got %q", tc.in, tc.want, got)
			}
		})
	}
}

func TestNormalizeDate(t *testing.T) {
	want := time.Date(2026, time.February, 10, 0, 0, 0, 0, time.UTC)
	for _, in := range []string{"2026-02-10", "10/02/2026", "10-02-2026"} {
		got, err := documents.NormalizeDate(in)
		if err != nil {
			t.Fatalf("NormalizeDate(%q): unexpected error: %v", in, err)
		}
		if !got.Equal(want) {
			t.Fatalf("NormalizeDate(%q): want %v, got %v", in, want, got)
		}
	}
	for _, in := range []string{"", "10-13-2026", "2026/02/10", "Feb 10 2026", "32/01/2026", "not-a-date"} {
		if _, err := documents.NormalizeDate(in); err == nil {
			t.Fatalf("NormalizeDate(%q): want error, got nil", in)
		}
	}
}

func TestNormalizePaise(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int64
	}{
		{name: "rupee comma decimals", in: "₹1,23,456.78", want: 12345678},
		{name: "rs prefix", in: "Rs. 500", want: 50000},
		{name: "rs no dot", in: "Rs 1,000", want: 100000},
		{name: "inr prefix", in: "INR 250.75", want: 25075},
		{name: "plain", in: "1000", want: 100000},
		{name: "one decimal", in: "99.9", want: 9990},
		{name: "two decimals", in: "10.00", want: 1000},
		{name: "zero", in: "0", want: 0},
		{name: "spaces", in: "  2 500.25  ", want: 250025},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := documents.NormalizePaise(tc.in)
			if err != nil {
				t.Fatalf("NormalizePaise(%q): unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizePaise(%q): want %d, got %d", tc.in, tc.want, got)
			}
		})
	}
}

func TestNormalizePaiseRejects(t *testing.T) {
	for _, in := range []string{"", "   ", "abc", "-50", "-₹100", "10.123", "12.3.4", ".50", "50.", "Rs. -5", "(100)", "10,0a.00"} {
		if _, err := documents.NormalizePaise(in); err == nil {
			t.Fatalf("NormalizePaise(%q): want error, got nil", in)
		}
	}
}

func TestNoopOCRErrors(t *testing.T) {
	var ocr documents.NoopOCR
	if _, err := ocr.ExtractText(t.Context(), documents.Document{}, []byte("blob")); err == nil {
		t.Fatalf("want error from NoopOCR, got nil")
	}
}
