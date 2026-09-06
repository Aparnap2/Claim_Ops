// Package verify_test covers the ten deterministic verification rules with
// table tests: one happy pass, one failing case per rule, and a
// multi-exception case.
//
// Determinism: no randomness, no network, no time.Now. Dates are fixed
// literals; money is exact int64 paise. DocEvidence admission_date values
// are already normalized YYYY-MM-DD.
package verify_test

import (
	"testing"
	"time"

	"claimops-api/internal/verify"
)

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse date %q: %v", s, err)
	}
	return tm
}

// happyInput returns a fully passing input; each failing case mutates a copy.
func happyInput(t *testing.T) verify.Input {
	t.Helper()
	admission := mustDate(t, "2026-08-10")
	discharge := mustDate(t, "2026-08-14")
	return verify.Input{
		ClaimPolicyNumber: "POL-123",
		ClaimPatientName:  "Aarav Sharma",
		ClaimHospitalName: "Fortis Hospital",
		ClaimedPaise:      7950000,
		Admission:         admission,
		Discharge:         discharge,
		HasAdmission:      true,
		HasDischarge:      true,
		PolicyNumber:      "POL-123",
		PolicyPatient:     "aarav  sharma",
		PolicyActive:      true,
		DocsPresent: map[string]bool{
			"CLAIM_FORM":        true,
			"DISCHARGE_SUMMARY": true,
			"HOSPITAL_BILL":     true,
		},
		DocEvidence: map[string]map[string]string{
			"CLAIM_FORM": {
				"patient_name":   "AARAV SHARMA",
				"admission_date": "2026-08-10",
				"hospital_name":  "fortis hospital",
			},
			"DISCHARGE_SUMMARY": {
				"patient_name":   "aarav sharma",
				"admission_date": "2026-08-10",
				"hospital_name":  "Fortis Hospital",
			},
			"HOSPITAL_BILL": {
				"patient_name":   "aarav  sharma",
				"admission_date": "2026-08-10",
				"hospital_name":  "FORTIS HOSPITAL",
			},
		},
		BillLinePaise:    []int64{5000000, 2950000},
		BillTotalPaise:   7950000,
		HasBillTotal:     true,
		ExternalPolicyOK: true,
		Duplicates:       nil,
	}
}

func codes(ex []verify.Exception) []string {
	got := make([]string, 0, len(ex))
	for _, e := range ex {
		got = append(got, e.Code)
	}
	return got
}

func codeSet(ex []verify.Exception) map[string]int {
	m := make(map[string]int, len(ex))
	for _, e := range ex {
		m[e.Code]++
	}
	return m
}

func severityByCode(ex []verify.Exception) map[string]string {
	m := make(map[string]string, len(ex))
	for _, e := range ex {
		if _, ok := m[e.Code]; !ok {
			m[e.Code] = e.Severity
		}
	}
	return m
}

func TestVerify(t *testing.T) {
	admission := mustDate(t, "2026-08-10")
	discharge := mustDate(t, "2026-08-14")

	tests := []struct {
		name      string
		mutate    func(*verify.Input)
		wantPass  bool
		wantCodes []string
		wantSev   map[string]string
	}{
		{
			name:      "happy pass",
			mutate:    func(in *verify.Input) {},
			wantPass:  true,
			wantCodes: nil,
			wantSev:   nil,
		},
		{
			name: "R1 policy number conflict",
			mutate: func(in *verify.Input) {
				in.PolicyNumber = "POL-999"
			},
			wantPass:  false,
			wantCodes: []string{"POLICY_NUMBER_CONFLICT"},
			wantSev:   map[string]string{"POLICY_NUMBER_CONFLICT": "HIGH"},
		},
		{
			name: "R2 patient identity conflict claim vs policy",
			mutate: func(in *verify.Input) {
				in.PolicyPatient = "Vivaan Rao"
			},
			wantPass:  false,
			wantCodes: []string{"PATIENT_IDENTITY_CONFLICT"},
			wantSev:   map[string]string{"PATIENT_IDENTITY_CONFLICT": "HIGH"},
		},
		{
			name: "R2 patient identity conflict via document evidence",
			mutate: func(in *verify.Input) {
				in.DocEvidence["DISCHARGE_SUMMARY"]["patient_name"] = "Vivaan Rao"
			},
			wantPass:  false,
			wantCodes: []string{"PATIENT_IDENTITY_CONFLICT"},
			wantSev:   map[string]string{"PATIENT_IDENTITY_CONFLICT": "HIGH"},
		},
		{
			name: "R3 invalid date range",
			mutate: func(in *verify.Input) {
				in.Admission, in.Discharge = discharge, admission
			},
			wantPass:  false,
			wantCodes: []string{"INVALID_DATE_RANGE"},
			wantSev:   map[string]string{"INVALID_DATE_RANGE": "HIGH"},
		},
		{
			name: "R4 policy not active",
			mutate: func(in *verify.Input) {
				in.PolicyActive = false
			},
			wantPass:  false,
			wantCodes: []string{"POLICY_NOT_ACTIVE"},
			wantSev:   map[string]string{"POLICY_NOT_ACTIVE": "HIGH"},
		},
		{
			name: "R5 amount reconciliation failure",
			mutate: func(in *verify.Input) {
				in.BillTotalPaise = 7950001
				in.ClaimedPaise = 1000 // keep R6 passing in isolation
			},
			wantPass:  false,
			wantCodes: []string{"AMOUNT_RECONCILIATION_FAILURE"},
			wantSev:   map[string]string{"AMOUNT_RECONCILIATION_FAILURE": "HIGH"},
		},
		{
			name: "R6 claim exceeds bill",
			mutate: func(in *verify.Input) {
				in.ClaimedPaise = 8000000
			},
			wantPass:  false,
			wantCodes: []string{"CLAIM_EXCEEDS_BILL"},
			wantSev:   map[string]string{"CLAIM_EXCEEDS_BILL": "MEDIUM"},
		},
		{
			name: "R7 duplicate documents one per id",
			mutate: func(in *verify.Input) {
				in.Duplicates = []string{"doc-1", "doc-2"}
			},
			wantPass:  false,
			wantCodes: []string{"DUPLICATE_DOCUMENT", "DUPLICATE_DOCUMENT"},
			wantSev:   map[string]string{"DUPLICATE_DOCUMENT": "MEDIUM"},
		},
		{
			name: "R8 missing required document",
			mutate: func(in *verify.Input) {
				in.DocsPresent["HOSPITAL_BILL"] = false
			},
			wantPass:  false,
			wantCodes: []string{"MISSING_REQUIRED_DOCUMENT"},
			wantSev:   map[string]string{"MISSING_REQUIRED_DOCUMENT": "HIGH"},
		},
		{
			name: "R9 external policy mismatch",
			mutate: func(in *verify.Input) {
				in.ExternalPolicyOK = false
				in.ExternalMismatch = "plan tier mismatch"
			},
			wantPass:  false,
			wantCodes: []string{"EXTERNAL_POLICY_MISMATCH"},
			wantSev:   map[string]string{"EXTERNAL_POLICY_MISMATCH": "HIGH"},
		},
		{
			name: "R10 admission date conflict",
			mutate: func(in *verify.Input) {
				in.DocEvidence["DISCHARGE_SUMMARY"]["admission_date"] = "2026-08-11"
			},
			wantPass:  false,
			wantCodes: []string{"DATE_CONFLICT"},
			wantSev:   map[string]string{"DATE_CONFLICT": "HIGH"},
		},
		{
			name: "multi-exception R1+R4+R7+R8",
			mutate: func(in *verify.Input) {
				in.PolicyNumber = "POL-999"
				in.PolicyActive = false
				in.DocsPresent["CLAIM_FORM"] = false
				in.Duplicates = []string{"doc-9"}
			},
			wantPass: false,
			wantCodes: []string{
				"POLICY_NUMBER_CONFLICT",
				"POLICY_NOT_ACTIVE",
				"DUPLICATE_DOCUMENT",
				"MISSING_REQUIRED_DOCUMENT",
			},
			wantSev: map[string]string{
				"POLICY_NUMBER_CONFLICT":    "HIGH",
				"POLICY_NOT_ACTIVE":         "HIGH",
				"DUPLICATE_DOCUMENT":        "MEDIUM",
				"MISSING_REQUIRED_DOCUMENT": "HIGH",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := happyInput(t)
			tt.mutate(&in)
			got := verify.Verify(in)
			if got.Passed != tt.wantPass {
				t.Fatalf("Passed = %v, want %v: %+v", got.Passed, tt.wantPass, got.Exceptions)
			}
			gotCodes := codes(got.Exceptions)
			if len(gotCodes) != len(tt.wantCodes) {
				t.Fatalf("codes = %v, want %v", gotCodes, tt.wantCodes)
			}
			for i := range gotCodes {
				if gotCodes[i] != tt.wantCodes[i] {
					t.Fatalf("codes = %v, want %v", gotCodes, tt.wantCodes)
				}
			}
			if tt.wantSev != nil {
				sevs := severityByCode(got.Exceptions)
				for code, want := range tt.wantSev {
					if sevs[code] != want {
						t.Fatalf("severity[%s] = %q, want %q", code, sevs[code], want)
					}
				}
			}
			// Every exception must carry a code, severity, and message.
			for _, e := range got.Exceptions {
				if e.Code == "" || e.Severity == "" || e.Message == "" {
					t.Fatalf("exception missing code/severity/message: %+v", e)
				}
			}
			// R7 must attach the duplicate doc id as evidence.
			if want := codeSet(got.Exceptions)["DUPLICATE_DOCUMENT"]; want > 0 {
				n := 0
				for _, e := range got.Exceptions {
					if e.Code == "DUPLICATE_DOCUMENT" {
						n++
						if len(e.EvidenceIDs) == 0 {
							t.Fatalf("DUPLICATE_DOCUMENT without EvidenceIDs: %+v", e)
						}
					}
				}
				if n != want {
					t.Fatalf("duplicate count = %d, want %d", n, want)
				}
			}
		})
	}
}

// TestVerifyR9MessageCarriesMismatch ensures the external mismatch detail is
// surfaced in the exception message.
func TestVerifyR9MessageCarriesMismatch(t *testing.T) {
	in := happyInput(t)
	in.ExternalPolicyOK = false
	in.ExternalMismatch = "plan tier mismatch"
	got := verify.Verify(in)
	if got.Passed {
		t.Fatal("Passed = true, want false")
	}
	if len(got.Exceptions) != 1 {
		t.Fatalf("exceptions = %+v, want exactly one", got.Exceptions)
	}
	e := got.Exceptions[0]
	if e.Code != "EXTERNAL_POLICY_MISMATCH" {
		t.Fatalf("code = %q, want EXTERNAL_POLICY_MISMATCH", e.Code)
	}
	if got := e.Message; len(got) == 0 {
		t.Fatal("empty message")
	} else {
		found := false
		for i := 0; i+len("plan tier mismatch") <= len(got); i++ {
			if got[i:i+len("plan tier mismatch")] == "plan tier mismatch" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("message %q does not carry mismatch detail", got)
		}
	}
}
