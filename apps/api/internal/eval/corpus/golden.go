package corpus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// expectedFileName is the ground-truth sidecar file in each case directory.
const expectedFileName = "expected.json"

// dateLayout is the only accepted date shape in expected.json.
const dateLayout = "2006-01-02"

// GoldenLineItem is one ground-truth bill line. Amounts are INR paise.
type GoldenLineItem struct {
	Description string `json:"description"`
	AmountPaise int64  `json:"amount_paise"`
}

// UnmarshalJSON accepts the contract keys (description, amount_paise) plus
// common aliases (desc/item/particulars, amount/total_paise/value_paise) so
// minor generator drift surfaces as data rather than a hard decode failure.
// Unknown keys are ignored.
func (l *GoldenLineItem) UnmarshalJSON(data []byte) error {
	type plain GoldenLineItem
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	if p.Description != "" && p.AmountPaise != 0 {
		*l = GoldenLineItem(p)
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	if p.Description == "" {
		for _, k := range []string{"desc", "item", "particulars"} {
			if v, ok := m[k]; ok {
				var s string
				if err := json.Unmarshal(v, &s); err == nil {
					p.Description = s
					break
				}
			}
		}
	}
	if p.AmountPaise == 0 {
		for _, k := range []string{"amount", "total_paise", "value_paise"} {
			if v, ok := m[k]; ok {
				var n int64
				if err := json.Unmarshal(v, &n); err == nil {
					p.AmountPaise = n
					break
				}
			}
		}
	}
	*l = GoldenLineItem(p)
	return nil
}

// Golden is the ground-truth field map from expected.json. TotalAmountPaise
// is a pointer so "key absent" (nil) is distinguishable from an explicit
// zero. ExpectedConflict records the cross-document conflict label for
// claim bundles; verification of such conflicts is a later phase and is
// NOT implemented here. Extra carries unknown top-level keys for forward
// compatibility with the benchmark harness (#31).
type Golden struct {
	ClaimNumber      string           `json:"claim_number"`
	PolicyNumber     string           `json:"policy_number"`
	PatientName      string           `json:"patient_name"`
	Hospital         string           `json:"hospital"`
	AdmissionDate    string           `json:"admission_date"`
	DischargeDate    string           `json:"discharge_date"`
	TotalAmountPaise *int64           `json:"total_amount_paise"`
	LineItems        []GoldenLineItem `json:"line_items"`
	ExpectedConflict string           `json:"expected_conflict"`

	Extra map[string]json.RawMessage `json:"-"`
}

// goldenKnownKeys partitions decoded top-level keys into Golden fields
// (above) versus Extra (everything else).
var goldenKnownKeys = map[string]bool{
	"claim_number":       true,
	"policy_number":      true,
	"patient_name":       true,
	"hospital":           true,
	"admission_date":     true,
	"discharge_date":     true,
	"total_amount_paise": true,
	"line_items":         true,
	"expected_conflict":  true,
}

// LoadGolden reads and decodes <caseDir>/expected.json. It does not
// validate: call Validate with the case document type explicitly.
func LoadGolden(caseDir string) (Golden, error) {
	data, err := os.ReadFile(filepath.Join(caseDir, expectedFileName))
	if err != nil {
		return Golden{}, fmt.Errorf("golden: read expected.json in %s: %w", caseDir, err)
	}
	var g Golden
	if err := json.Unmarshal(data, &g); err != nil {
		return Golden{}, fmt.Errorf("golden: decode expected.json in %s: %w", caseDir, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Golden{}, fmt.Errorf("golden: decode expected.json map in %s: %w", caseDir, err)
	}
	for k, v := range raw {
		if !goldenKnownKeys[k] {
			if g.Extra == nil {
				g.Extra = make(map[string]json.RawMessage)
			}
			g.Extra[k] = v
		}
	}
	return g, nil
}

// Validate enforces the golden invariants: claim_number is required
// except for policy_schedule (claim lives at bundle level, not on the
// schedule) and unknown (pathological fixtures carry no truth);
// total_amount_paise is required (and >= 0) for hospital bills, a present
// total is never negative, and present dates parse as YYYY-MM-DD. Date
// ordering is deliberately NOT checked: conflict fixtures may carry
// intentionally inconsistent dates with ExpectedConflict set.
func (g Golden) Validate(documentType string) error {
	t := strings.ToLower(strings.TrimSpace(documentType))
	if t != "policy_schedule" && t != "unknown" && strings.TrimSpace(g.ClaimNumber) == "" {
		return fmt.Errorf("golden: missing required key %q", "claim_number")
	}
	if (t == "hospital_bill" || t == "bill") && g.TotalAmountPaise == nil {
		return fmt.Errorf("golden: document_type %s requires %q", documentType, "total_amount_paise")
	}
	if g.TotalAmountPaise != nil && *g.TotalAmountPaise < 0 {
		return fmt.Errorf("golden: total_amount_paise must be >= 0, got %d", *g.TotalAmountPaise)
	}
	if err := checkGoldenDate("admission_date", g.AdmissionDate); err != nil {
		return err
	}
	if err := checkGoldenDate("discharge_date", g.DischargeDate); err != nil {
		return err
	}
	return nil
}

func checkGoldenDate(key, value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	if _, err := time.Parse(dateLayout, strings.TrimSpace(value)); err != nil {
		return fmt.Errorf("golden: %s must be YYYY-MM-DD, got %q", key, value)
	}
	return nil
}
