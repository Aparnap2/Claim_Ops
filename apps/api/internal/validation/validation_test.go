// Package validation_test covers the six deterministic validators with table
// tests: case-insensitivity, whitespace/case normalization, zero-date BLOCK,
// negative-amount failure, and empty-hash skipping.
//
// Determinism: no randomness, no network, no time.Now. All dates are fixed
// literals parsed with time.Parse; all money is exact int64 paise.
package validation_test

import (
	"testing"
	"time"

	"claimops-api/internal/validation"
)

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse date %q: %v", s, err)
	}
	return tm
}

func TestValidateRequiredDocs(t *testing.T) {
	required := []string{"CLAIM_FORM", "HOSPITAL_FINAL_BILL", "DISCHARGE_SUMMARY"}
	tests := []struct {
		name     string
		present  []string
		wantPass bool
	}{
		{"all present exact", []string{"CLAIM_FORM", "HOSPITAL_FINAL_BILL", "DISCHARGE_SUMMARY"}, true},
		{"case insensitive", []string{"claim_form", "hospital_final_bill", "discharge_summary"}, true},
		{"mixed case", []string{"Claim_Form", "Hospital_Final_Bill", "Discharge_Summary"}, true},
		{"whitespace padded", []string{"  CLAIM_FORM ", "\tHOSPITAL_FINAL_BILL", "DISCHARGE_SUMMARY\n"}, true},
		{"blank entries ignored", []string{"CLAIM_FORM", "", "   ", "HOSPITAL_FINAL_BILL", "DISCHARGE_SUMMARY"}, true},
		{"extra docs tolerated", []string{"CLAIM_FORM", "HOSPITAL_FINAL_BILL", "DISCHARGE_SUMMARY", "EXTRA_XRAY"}, true},
		{"missing one", []string{"CLAIM_FORM", "HOSPITAL_FINAL_BILL"}, false},
		{"missing all", []string{"OTHER_DOC"}, false},
		{"empty", nil, false},
		{"only blanks", []string{"", "   "}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validation.ValidateRequiredDocs(tt.present, required)
			if got.RuleID != validation.RuleRequiredDocs {
				t.Fatalf("rule = %q, want %q", got.RuleID, validation.RuleRequiredDocs)
			}
			if got.Passed != tt.wantPass {
				t.Fatalf("passed = %v, want %v: %s", got.Passed, tt.wantPass, got.Message)
			}
			wantSev := validation.SeverityPass
			if !tt.wantPass {
				wantSev = validation.SeverityFail
			}
			if got.Severity != wantSev {
				t.Fatalf("severity = %s, want %s", got.Severity, wantSev)
			}
		})
	}
}

func TestValidatePolicyActive(t *testing.T) {
	tests := []struct {
		name     string
		incident string // "" means zero time
		from     string // "" means zero time
		to       string // "" means unset (used only when hasTo)
		hasTo    bool
		wantPass bool
	}{
		{"active middle", "2026-08-10", "2026-04-01", "2027-03-31", true, true},
		{"incident on start boundary", "2026-04-01", "2026-04-01", "2027-03-31", true, true},
		{"incident on end boundary", "2027-03-31", "2026-04-01", "2027-03-31", true, true},
		{"incident before start", "2026-03-31", "2026-04-01", "2027-03-31", true, false},
		{"incident after end", "2026-08-10", "2026-04-01", "2026-06-30", true, false},
		{"open ended active", "2026-08-10", "2026-04-01", "", false, true},
		{"open ended before start", "2026-03-01", "2026-04-01", "", false, false},
		{"zero incident blocks", "", "2026-04-01", "2027-03-31", true, false},
		{"zero start blocks", "2026-08-10", "", "2027-03-31", true, false},
		{"both zero block", "", "", "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var incident, from, to time.Time
			if tt.incident != "" {
				incident = mustDate(t, tt.incident)
			}
			if tt.from != "" {
				from = mustDate(t, tt.from)
			}
			if tt.to != "" {
				to = mustDate(t, tt.to)
			}
			got := validation.ValidatePolicyActive(incident, from, to, tt.hasTo)
			if got.RuleID != validation.RulePolicyActive {
				t.Fatalf("rule = %q, want %q", got.RuleID, validation.RulePolicyActive)
			}
			if got.Passed != tt.wantPass {
				t.Fatalf("passed = %v, want %v: %s", got.Passed, tt.wantPass, got.Message)
			}
			// Policy failures are always hard BLOCKs, never mere FAILs.
			wantSev := validation.SeverityPass
			if !tt.wantPass {
				wantSev = validation.SeverityBlock
			}
			if got.Severity != wantSev {
				t.Fatalf("severity = %s, want %s", got.Severity, wantSev)
			}
		})
	}
}

func TestValidateFieldConsistency(t *testing.T) {
	tests := []struct {
		name     string
		values   map[string]string
		wantPass bool
	}{
		{"exact match", map[string]string{"a": "X", "b": "X"}, true},
		{"case insensitive", map[string]string{"a": "Mumbai", "b": "MUMBAI", "c": "mumbai"}, true},
		{"whitespace and case normalized", map[string]string{"a": "  Fortis  Hospital ", "b": "fortis   hospital"}, true},
		{"empty values skipped", map[string]string{"a": "X", "b": "", "c": "   "}, true},
		{"single source", map[string]string{"a": "X"}, true},
		{"genuine mismatch", map[string]string{"a": "X", "b": "Y"}, false},
		{"mismatch after normalization", map[string]string{"a": "Fortis", "b": "Apollo"}, false},
		{"all empty fails", map[string]string{"a": "", "b": "   "}, false},
		{"no sources fails", map[string]string{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validation.ValidateFieldConsistency("hospital", tt.values)
			if got.RuleID != validation.RuleFieldConsistency {
				t.Fatalf("rule = %q, want %q", got.RuleID, validation.RuleFieldConsistency)
			}
			if got.Passed != tt.wantPass {
				t.Fatalf("passed = %v, want %v: %s", got.Passed, tt.wantPass, got.Message)
			}
		})
	}
}

func TestValidateDateLogic(t *testing.T) {
	admission := mustDate(t, "2026-08-10")
	discharge := mustDate(t, "2026-08-14")
	tests := []struct {
		name         string
		admission    time.Time
		discharge    time.Time
		hasAdmission bool
		hasDischarge bool
		wantPass     bool
	}{
		{"admission before discharge", admission, discharge, true, true, true},
		{"equal dates hold", admission, admission, true, true, true},
		{"admission after discharge", discharge, admission, true, true, false},
		{"missing admission", time.Time{}, discharge, false, true, false},
		{"missing discharge", admission, time.Time{}, true, false, false},
		{"both missing", time.Time{}, time.Time{}, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validation.ValidateDateLogic(tt.admission, tt.discharge, tt.hasAdmission, tt.hasDischarge)
			if got.RuleID != validation.RuleDateLogic {
				t.Fatalf("rule = %q, want %q", got.RuleID, validation.RuleDateLogic)
			}
			if got.Passed != tt.wantPass {
				t.Fatalf("passed = %v, want %v: %s", got.Passed, tt.wantPass, got.Message)
			}
		})
	}
}

func TestValidateAmountPaise(t *testing.T) {
	tests := []struct {
		name            string
		gross, discount int64
		net             int64
		wantPass        bool
	}{
		{"exact reconciliation", 8500000, 550000, 7950000, true},
		{"zero discount", 8500000, 0, 8500000, true},
		{"all zeros", 0, 0, 0, true},
		{"mismatch by full discount", 8500000, 0, 7950000, false},
		{"off by one paise", 8500000, 550000, 7950001, false},
		{"negative gross fails", -8500000, 550000, 7950000, false},
		{"negative discount fails", 8500000, -550000, 7950000, false},
		{"negative net fails", 8500000, 550000, -7950000, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validation.ValidateAmountPaise(tt.gross, tt.discount, tt.net)
			if got.RuleID != validation.RuleAmountReconciliation {
				t.Fatalf("rule = %q, want %q", got.RuleID, validation.RuleAmountReconciliation)
			}
			if got.Passed != tt.wantPass {
				t.Fatalf("passed = %v, want %v: %s", got.Passed, tt.wantPass, got.Message)
			}
			wantSev := validation.SeverityPass
			if !tt.wantPass {
				wantSev = validation.SeverityFail
			}
			if got.Severity != wantSev {
				t.Fatalf("severity = %s, want %s", got.Severity, wantSev)
			}
		})
	}
}

func TestValidateDuplicateSHA256(t *testing.T) {
	tests := []struct {
		name     string
		hashes   []string
		wantPass bool
	}{
		{"unique hashes", []string{"abc123", "def456"}, true},
		{"exact repeat fails", []string{"abc123", "def456", "abc123"}, false},
		{"case insensitive repeat fails", []string{"ABC123", "abc123"}, false},
		{"whitespace trimmed repeat fails", []string{"  abc123 ", "ABC123"}, false},
		{"empty hashes skipped", []string{"", "   ", "abc123"}, true},
		{"only empties pass", []string{"", "  "}, true},
		{"empty list passes", nil, true},
		{"distinct after trim passes", []string{"abc123", "def456", "ghi789"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validation.ValidateDuplicateSHA256(tt.hashes)
			if got.RuleID != validation.RuleDuplicateSHA256 {
				t.Fatalf("rule = %q, want %q", got.RuleID, validation.RuleDuplicateSHA256)
			}
			if got.Passed != tt.wantPass {
				t.Fatalf("passed = %v, want %v: %s", got.Passed, tt.wantPass, got.Message)
			}
		})
	}
}
