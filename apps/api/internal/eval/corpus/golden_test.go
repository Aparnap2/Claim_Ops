package corpus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func paise(n int64) *int64 { return &n }

func TestGoldenValidate(t *testing.T) {
	tests := []struct {
		name    string
		golden  Golden
		docType string
		wantErr string // "" means valid
	}{
		{"valid_bill", Golden{ClaimNumber: "CLM-1", TotalAmountPaise: paise(15000), AdmissionDate: "2026-01-05", DischargeDate: "2026-01-09"}, "bill", ""},
		{"valid_zero_total", Golden{ClaimNumber: "CLM-1", TotalAmountPaise: paise(0)}, "bill", ""},
		{"valid_nonbill_without_total", Golden{ClaimNumber: "CLM-1", PatientName: "Asha"}, "discharge_summary", ""},
		{"conflict_dates_allowed", Golden{ClaimNumber: "CLM-1", TotalAmountPaise: paise(100), AdmissionDate: "2026-02-10", DischargeDate: "2026-02-01", ExpectedConflict: "date_conflict"}, "bill", ""},
		{"missing_claim_number", Golden{TotalAmountPaise: paise(100)}, "bill", "claim_number"},
		{"blank_claim_number", Golden{ClaimNumber: "  ", TotalAmountPaise: paise(100)}, "bill", "claim_number"},
		{"bill_missing_total", Golden{ClaimNumber: "CLM-1"}, "bill", "total_amount_paise"},
		{"negative_total", Golden{ClaimNumber: "CLM-1", TotalAmountPaise: paise(-50)}, "bill", ">= 0"},
		{"negative_total_nonbill", Golden{ClaimNumber: "CLM-1", TotalAmountPaise: paise(-1)}, "receipt", ">= 0"},
		{"bad_admission_date", Golden{ClaimNumber: "CLM-1", TotalAmountPaise: paise(1), AdmissionDate: "05-01-2026"}, "bill", "admission_date"},
		{"bad_discharge_date", Golden{ClaimNumber: "CLM-1", DischargeDate: "Jan 9 2026"}, "discharge_summary", "discharge_date"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.golden.Validate(tt.docType)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestLoadGolden_RoundTripAndExtra(t *testing.T) {
	dir := t.TempDir()
	raw := `{
		"claim_number": "CLM-42",
		"policy_number": "POL-7",
		"patient_name": "Meera",
		"hospital": "City Care",
		"admission_date": "2026-03-01",
		"discharge_date": "2026-03-04",
		"total_amount_paise": 25000,
		"line_items": [{"description": "Room", "amount_paise": 20000}],
		"expected_conflict": "amount_mismatch",
		"future_field": "kept"
	}`
	if err := os.WriteFile(filepath.Join(dir, "expected.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := LoadGolden(dir)
	if err != nil {
		t.Fatalf("LoadGolden: %v", err)
	}
	if g.ClaimNumber != "CLM-42" || g.TotalAmountPaise == nil || *g.TotalAmountPaise != 25000 {
		t.Fatalf("decoded golden = %+v", g)
	}
	if len(g.LineItems) != 1 || g.LineItems[0].Description != "Room" || g.LineItems[0].AmountPaise != 20000 {
		t.Fatalf("line items = %+v", g.LineItems)
	}
	if g.ExpectedConflict != "amount_mismatch" {
		t.Fatalf("conflict = %q", g.ExpectedConflict)
	}
	if g.Extra["future_field"] == nil {
		t.Fatalf("expected future_field in Extra, got %v", g.Extra)
	}
	if err := g.Validate("bill"); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestLoadGolden_AliasLineItem(t *testing.T) {
	dir := t.TempDir()
	raw := `{"claim_number": "CLM-1", "total_amount_paise": 500, "line_items": [{"desc": "X-ray", "amount": 500}]}`
	if err := os.WriteFile(filepath.Join(dir, "expected.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := LoadGolden(dir)
	if err != nil {
		t.Fatalf("LoadGolden: %v", err)
	}
	if len(g.LineItems) != 1 || g.LineItems[0].Description != "X-ray" || g.LineItems[0].AmountPaise != 500 {
		t.Fatalf("aliased line item = %+v", g.LineItems)
	}
}

func TestLoadGolden_MissingFile(t *testing.T) {
	if _, err := LoadGolden(t.TempDir()); err == nil {
		t.Fatal("expected error for missing expected.json, got nil")
	}
}

func TestLoadGolden_BadJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "expected.json"), []byte(`{oops`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGolden(dir); err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}
