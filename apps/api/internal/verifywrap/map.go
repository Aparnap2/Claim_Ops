// Package verifywrap maps assemble.CanonicalClaim agreement into
// verify.Input judgment inputs (issue #47).
//
// It is a thin adapter by design: neither assemble nor verify may import
// each other, and neither is modified here. This package imports both plus
// stdlib only (no extract import is needed: assembly statuses come from
// the assemble types; tests import extract to build fixtures).
//
// Binding mapping rules (from the approved spec review):
//
//   - Typed fields are populated ONLY from an AssembledField with
//     Status == assemble.StatusAgreed AND len(NeedsReview) == 0, parsing
//     the Agreed value. AgreedRaw is NEVER read: per the #50 display-only
//     rule it is a first-source raw rendering for humans, not a canonical
//     value for judgment.
//   - ClaimPolicyNumber <- policy_number AGREED verbatim.
//   - ClaimPatientName <- patient_name AGREED verbatim.
//   - ClaimHospitalName <- hospital_name AGREED verbatim.
//   - Admission/Discharge <- admission_date/discharge_date AGREED parsed
//     with time.Parse("2006-01-02", Agreed), setting the Has flags. Agreed
//     dates are extract.NormalizeDate-guaranteed, so a parse failure is a
//     bug signal and returns an error (never a silent zero).
//   - Amounts route by source DocType (F11a; see the F11a block in Map):
//     total_amount_paise AGREED with a HOSPITAL_BILL source feeds
//     BillTotalPaise (+ HasBillTotal); with a CLAIM_FORM / PREAUTH_FORM
//     source it feeds ClaimedPaise. Agreed is paise digits parsed with
//     strconv; a parse failure on a routed value returns an error. An
//     agreed amount with no routable source (e.g. DISCHARGE_SUMMARY- or
//     POLICY_SCHEDULE-only) maps to no amount slot and stays zero.
//   - BillLinePaise is nil ALWAYS (F11b; see the F11b block in Map):
//     bill_lines_N keys carry descriptions only, so R5 has no line inputs
//     on mapped data and never evaluates.
//   - DocsPresent <- claim.DocsPresent verbatim (nil stays nil).
//   - DocEvidence <- the sources of AGREED patient_name and admission_date
//     grouped by DocType: DocEvidence[DocType][key] = Agreed. Only the two
//     keys verify reads (R2 patient_name, R10 admission_date) are carried;
//     hospital_name and amounts are typed-only and never enter DocEvidence.
//   - PolicyNumber / PolicyPatient / PolicyActive / ExternalPolicyOK /
//     ExternalMismatch / Duplicates <- ext verbatim. These are REQUIRED
//     caller-supplied params: zero values intentionally fire R4/R9 by
//     engine design, so callers must supply them.
//   - claim_number, diagnosis, procedure (and any other assembled key with
//     no verify.Input slot, including the bill_lines_N family) are
//     DROPPED from the Input: they remain available in claim.Fields for
//     HITL context only. AGREED instances drop silently; CONFLICT and
//     NEEDS_REVIEW instances surface as Unresolved entries.
//   - MISSING fields map to zero values with no Unresolved entry.
//     CONFLICT / NEEDS_REVIEW fields map to Unresolved entries (Conflict
//     pointer or Review slice) with the typed field left zero. Sentinel
//     values are NEVER emitted.
//   - Terminal errors fire ONLY on internal inconsistency in the assembled
//     claim (blank keys, Agreed set under a non-agreed status, AGREED with
//     an empty Agreed or shadowing review items, CONFLICT without a
//     ConflictEntry, NEEDS_REVIEW without review items, unparseable agreed
//     dates, unparseable routed amounts, unknown statuses). An empty claim
//     maps to the zero Input legitimately and is NOT an error.
//
// Determinism: Unresolved iterates the claim's fields in sorted key order
// (Go map iteration is randomized; the sort closes that). DocEvidence is a
// map keyed by DocType whose values are single agreed strings per key, so
// construction order cannot leak into the result.
package verifywrap

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"claimops-api/internal/assemble"
	"claimops-api/internal/verify"
)

// Canonical field keys mirrored from the xdoc emitter (xdoc.Key*). They
// are redeclared here — not imported — because the #47 constraints allow
// only stdlib + assemble + verify (+ extract) imports.
const (
	keyPolicyNumber     = "policy_number"
	keyPatientName      = "patient_name"
	keyHospitalName     = "hospital_name"
	keyAdmissionDate    = "admission_date"
	keyDischargeDate    = "discharge_date"
	keyTotalAmountPaise = "total_amount_paise"
)

// PREAUTH_FORM mirrored from the xdoc emitter. The CLAIM_FORM /
// HOSPITAL_BILL / DISCHARGE_SUMMARY DocTypes are taken from the verify
// constants (verify.DocClaimForm and kin) instead.
const docPreauthForm = "PREAUTH_FORM"

// dateLayout is the canonical date form. Agreed dates are
// extract.NormalizeDate-guaranteed to this layout.
const dateLayout = "2006-01-02"

// Externals carries the caller-supplied non-document inputs. Every field
// is copied verbatim into verify.Input, including zero values: a zero
// PolicyActive fires R4 and a zero ExternalPolicyOK fires R9 by engine
// design, so callers MUST supply these (there is no "unknown" state in
// verify.Input — unset IS false).
type Externals struct {
	PolicyNumber     string
	PolicyPatient    string
	PolicyActive     bool
	ExternalPolicyOK bool
	ExternalMismatch string
	Duplicates       []string
}

// Unresolved is one assembled field that cannot feed clean typed input:
// a CONFLICT (Conflict set, carrying the ConflictEntry) or a
// NEEDS_REVIEW (Review set, carrying the review items verbatim). The
// typed Input field for Key is always left zero; human review owns it.
type Unresolved struct {
	Key      string
	Status   assemble.Status
	Conflict *assemble.ConflictEntry
	Review   []assemble.ReviewItem
}

// Map converts an assembled claim plus caller externals into verify
// judgment inputs and the HITL-owned unresolved set. It never mutates its
// inputs. See the package comment for the binding rules.
func Map(claim assemble.CanonicalClaim, ext Externals) (verify.Input, []Unresolved, error) {
	var in verify.Input
	var unresolved []Unresolved

	// DocsPresent verbatim (nil stays nil so an empty claim maps to the
	// zero Input; verify treats a nil map as no documents present).
	if claim.DocsPresent != nil {
		in.DocsPresent = make(map[string]bool, len(claim.DocsPresent))
		for k, v := range claim.DocsPresent {
			in.DocsPresent[k] = v
		}
	}

	// Index ConflictEntries by key for the CONFLICT lookups below.
	conflicts := make(map[string]assemble.ConflictEntry, len(claim.Conflicts))
	for i := range claim.Conflicts {
		c := &claim.Conflicts[i]
		if c.Key == "" {
			return verify.Input{}, nil, fmt.Errorf("verifywrap: conflicts[%d] has blank key", i)
		}
		if _, dup := conflicts[c.Key]; dup {
			return verify.Input{}, nil, fmt.Errorf("verifywrap: duplicate conflict entry for key %q", c.Key)
		}
		conflicts[c.Key] = *c
	}

	// agreedClean holds the fields eligible for typed mapping: AGREED
	// with no shadowing review items. Everything else either maps to
	// zero (MISSING), to Unresolved (CONFLICT / NEEDS_REVIEW), or is
	// dropped (AGREED keys with no Input slot).
	agreedClean := make(map[string]assemble.AssembledField)
	for _, key := range slices.Sorted(maps.Keys(claim.Fields)) {
		f := claim.Fields[key]
		if key == "" || f.Key == "" {
			return verify.Input{}, nil, fmt.Errorf("verifywrap: blank field key")
		}
		if f.Key != key {
			return verify.Input{}, nil, fmt.Errorf("verifywrap: field key %q stored under map key %q", f.Key, key)
		}
		switch f.Status {
		case assemble.StatusAgreed:
			if f.Agreed == "" {
				return verify.Input{}, nil, fmt.Errorf("verifywrap: key %q AGREED with empty Agreed value", key)
			}
			if len(f.NeedsReview) != 0 {
				return verify.Input{}, nil, fmt.Errorf("verifywrap: key %q AGREED shadowed by %d review items (assembler must downgrade to NEEDS_REVIEW)", key, len(f.NeedsReview))
			}
			agreedClean[key] = f
		case assemble.StatusMissing:
			if f.Agreed != "" || f.AgreedRaw != "" || len(f.Sources) != 0 || len(f.NeedsReview) != 0 {
				return verify.Input{}, nil, fmt.Errorf("verifywrap: key %q MISSING carries votes or agreement", key)
			}
			// Zero values; no Unresolved entry.
		case assemble.StatusConflict:
			if f.Agreed != "" || f.AgreedRaw != "" {
				return verify.Input{}, nil, fmt.Errorf("verifywrap: key %q CONFLICT carries Agreed value", key)
			}
			c, ok := conflicts[key]
			if !ok {
				return verify.Input{}, nil, fmt.Errorf("verifywrap: key %q CONFLICT without ConflictEntry", key)
			}
			cp := c
			cp.Distinct = append([]string(nil), c.Distinct...)
			cp.Sources = append([]assemble.FieldSource(nil), c.Sources...)
			unresolved = append(unresolved, Unresolved{Key: key, Status: f.Status, Conflict: &cp})
		case assemble.StatusNeedsReview:
			if len(f.NeedsReview) == 0 {
				return verify.Input{}, nil, fmt.Errorf("verifywrap: key %q NEEDS_REVIEW without review items", key)
			}
			if len(f.Sources) == 0 && (f.Agreed != "" || f.AgreedRaw != "") {
				return verify.Input{}, nil, fmt.Errorf("verifywrap: key %q NEEDS_REVIEW without votes carries Agreed value", key)
			}
			// Shadowed agreement (Agreed set with votes + review) and
			// voteless review both land here; the typed field stays
			// zero either way. NEVER a sentinel value.
			unresolved = append(unresolved, Unresolved{Key: key, Status: f.Status, Review: append([]assemble.ReviewItem(nil), f.NeedsReview...)})
		default:
			return verify.Input{}, nil, fmt.Errorf("verifywrap: key %q has unknown status %q", key, string(f.Status))
		}
	}

	// Identity: verbatim Agreed (never AgreedRaw — #50 display-only rule).
	if f, ok := agreedClean[keyPolicyNumber]; ok {
		in.ClaimPolicyNumber = f.Agreed
	}
	if f, ok := agreedClean[keyPatientName]; ok {
		in.ClaimPatientName = f.Agreed
	}
	if f, ok := agreedClean[keyHospitalName]; ok {
		in.ClaimHospitalName = f.Agreed
	}

	// Dates: Agreed is NormalizeDate-guaranteed YYYY-MM-DD; a parse
	// failure is a bug signal and returns an error, never a silent zero.
	if f, ok := agreedClean[keyAdmissionDate]; ok {
		t, err := time.Parse(dateLayout, f.Agreed)
		if err != nil {
			return verify.Input{}, nil, fmt.Errorf("verifywrap: key %q AGREED value %q is not a parseable date: %w", keyAdmissionDate, f.Agreed, err)
		}
		in.Admission, in.HasAdmission = t, true
	}
	if f, ok := agreedClean[keyDischargeDate]; ok {
		t, err := time.Parse(dateLayout, f.Agreed)
		if err != nil {
			return verify.Input{}, nil, fmt.Errorf("verifywrap: key %q AGREED value %q is not a parseable date: %w", keyDischargeDate, f.Agreed, err)
		}
		in.Discharge, in.HasDischarge = t, true
	}

	// F11a amount routing by source DocType. The assembler guarantees
	// every FieldSource preserves its producing document's DocType
	// verbatim, so the route reads the AGREED total_amount_paise field's
	// sources: any HOSPITAL_BILL source routes the value to
	// BillTotalPaise (+ HasBillTotal); any CLAIM_FORM / PREAUTH_FORM
	// source routes the same value to ClaimedPaise. Both routes may fire
	// at once (multi-DocType agreement on one value); neither fires when
	// the agreement comes from an unroutable DocType (e.g.
	// DISCHARGE_SUMMARY- or POLICY_SCHEDULE-only), leaving both amount
	// slots zero. Agreed is paise digits; a parse failure on a routed
	// value is a bug signal and returns an error.
	if f, ok := agreedClean[keyTotalAmountPaise]; ok {
		var hasBill, hasClaim bool
		for _, s := range f.Sources {
			switch s.DocType {
			case verify.DocHospitalBill:
				hasBill = true
			case verify.DocClaimForm, docPreauthForm:
				hasClaim = true
			}
		}
		if hasBill || hasClaim {
			paise, err := strconv.ParseInt(f.Agreed, 10, 64)
			if err != nil {
				return verify.Input{}, nil, fmt.Errorf("verifywrap: key %q AGREED value %q is not paise digits: %w", keyTotalAmountPaise, f.Agreed, err)
			}
			if hasBill {
				in.BillTotalPaise, in.HasBillTotal = paise, true
			}
			if hasClaim {
				in.ClaimedPaise = paise
			}
		}
	}

	// F11b bill lines: BillLinePaise stays nil ALWAYS. The bill_lines_N
	// assembled keys carry descriptions ONLY (amounts were discarded
	// upstream), so there is no line amount to map. R5
	// (AMOUNT_RECONCILIATION_FAILURE) requires len(BillLinePaise) > 0 and
	// therefore NEVER evaluates on mapped input — skipped loudly by
	// leaving the slice nil rather than by special-casing the engine.
	// Any future line-amount patch must change this block explicitly.
	in.BillLinePaise = nil

	// DocEvidence: the sources of AGREED patient_name and admission_date
	// grouped by DocType, each holding the single Agreed value. These are
	// the only two keys verify reads cross-source (R2 patient_name, R10
	// admission_date); every DocType entry for one key holds the same
	// Agreed string, so mapped DocEvidence can never fork R10 by itself
	// — cross-source date disagreement surfaces as CONFLICT -> Unresolved
	// (HITL), never as R10 input. Blank-DocType sources mark no presence
	// and are skipped: there is no DocType to group under.
	for _, key := range []string{keyPatientName, keyAdmissionDate} {
		f, ok := agreedClean[key]
		if !ok {
			continue
		}
		for _, s := range f.Sources {
			if s.DocType == "" {
				continue
			}
			if in.DocEvidence == nil {
				in.DocEvidence = make(map[string]map[string]string)
			}
			m, ok := in.DocEvidence[s.DocType]
			if !ok {
				m = make(map[string]string)
				in.DocEvidence[s.DocType] = m
			}
			m[key] = f.Agreed
		}
	}

	// Dropped keys (documented, not mapped): claim_number, diagnosis,
	// procedure, the bill_lines_N family, and any other assembled key
	// with no verify.Input slot have no judgment input to feed. Their
	// AGREED values drop silently here and stay available in
	// claim.Fields for HITL context; their CONFLICT / NEEDS_REVIEW
	// states already produced Unresolved entries above.

	// Externals verbatim (required params — zero values fire R4/R9 by
	// engine design; see the Externals type comment).
	in.PolicyNumber = ext.PolicyNumber
	in.PolicyPatient = ext.PolicyPatient
	in.PolicyActive = ext.PolicyActive
	in.ExternalPolicyOK = ext.ExternalPolicyOK
	in.ExternalMismatch = ext.ExternalMismatch
	if ext.Duplicates != nil {
		in.Duplicates = append([]string(nil), ext.Duplicates...)
	}

	// Sorted by construction (fields iterated in sorted key order); sort
	// explicitly so the guarantee does not depend on loop history.
	slices.SortFunc(unresolved, func(a, b Unresolved) int {
		switch {
		case a.Key < b.Key:
			return -1
		case a.Key > b.Key:
			return 1
		default:
			return 0
		}
	})
	return in, unresolved, nil
}
