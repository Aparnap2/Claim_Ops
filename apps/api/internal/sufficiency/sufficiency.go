// Package sufficiency implements the APA-31 deterministic sufficiency
// gate (ADR-007 follow-up): per document class, it decides whether the
// assembled extraction evidence is sufficient to continue the pipeline
// or whether the pipeline must raise an insufficient-evidence
// exception.
//
// The gate is stdlib plus frozen contracts only: it consumes
// extract.DocumentFacts (never bytes, never vendor types), derives the
// required key sets from the frozen xdoc extractor key sets (never
// re-spelled here), and routes insufficiency through the existing
// exception vocabulary only (invest.MissingField items and a
// verify.CodeMissingRequiredDocument finding — the closed R1-R10
// taxonomy gains no code, the invest envelope gains no topology).
//
// Position in the pipeline (wiring is a follow-up; this package is the
// pure rule):
//
//	parser.ParsedDocument -> extract.DocumentFacts -> sufficiency.Evaluate
//	  sufficient   -> assemble -> verifywrap -> verify (unchanged)
//	  insufficient -> HITL via the existing invest envelope vocabulary
//	    (MissingField items + R8 finding + verifywrap Unresolved)
//
// Frozen boundaries (imported and wrapped, never modified): extract
// contract + xdoc key sets, verify R1-R10, assemble statuses,
// verifywrap mapping, invest envelope, worker OCR gate (APA-12/ADR-009).
package sufficiency

import (
	"fmt"
	"slices"
	"strings"

	"claimops-api/internal/extract"
	"claimops-api/internal/extract/xdoc"
	"claimops-api/internal/invest"
	"claimops-api/internal/verify"
)

// Reason codes are the closed, machine-readable insufficiency
// vocabulary. Sorted unique on every insufficient Result.
const (
	// ReasonMissingField marks a required key with no observation
	// (absent key or MISSING status).
	ReasonMissingField = "MISSING_REQUIRED_FIELD"
	// ReasonAmbiguousField marks a required key observed but
	// unparseable (AMBIGUOUS status, raw preserved for HITL).
	ReasonAmbiguousField = "AMBIGUOUS_FIELD"
	// ReasonMultiCandidateField marks a required key with conflicting
	// observations and no silent winner (MULTI_CANDIDATE status).
	ReasonMultiCandidateField = "MULTI_CANDIDATE_FIELD"
	// ReasonMissingBillLines marks a HOSPITAL_BILL with no
	// reconstructed bill_lines_N line item (ADR-007 table demand).
	ReasonMissingBillLines = "MISSING_BILL_LINES"
	// ReasonUnknownDocType marks a document class the gate does not
	// know (fail closed to HITL, never an error).
	ReasonUnknownDocType = "UNKNOWN_DOC_TYPE"
)

// Routing values name the existing channels only: continue the
// deterministic flow, or route to HITL through the existing invest
// envelope vocabulary (MissingField items + R8 finding + the
// verifywrap Unresolved channel). No new topology.
const (
	// RoutingContinue means evidence is sufficient: continue flow.
	RoutingContinue = "continue"
	// RoutingHITL means evidence is insufficient: exception/HITL via
	// the existing envelope, never silent success.
	RoutingHITL = "hitl"
)

// Result is one document's sufficiency verdict. It is pure data:
// Sufficient false always pairs with a non-empty sorted ReasonCodes
// set and RoutingHITL; Missing lists the gapped required keys sorted
// (empty only when the class itself is unknown — there are no known
// required keys to list); Sufficient true always pairs with empty
// gaps and RoutingContinue.
type Result struct {
	// DocumentID echoes the evaluated facts' document.
	DocumentID string
	// DocType echoes the evaluated facts' class.
	DocType string
	// Sufficient reports whether the evidence meets the class bar.
	Sufficient bool
	// Missing lists the required keys with no usable observation,
	// sorted. Empty iff Sufficient or the class is unknown.
	Missing []string
	// ReasonCodes lists the closed insufficiency reasons, sorted
	// unique. Empty iff Sufficient.
	ReasonCodes []string
	// Routing names the existing channel: continue or hitl.
	Routing string
}

// requiredKeys is the per-class bar, derived from the frozen xdoc
// observed scalar sets (claim_form.go, discharge_summary.go,
// hospital_bill.go, policy_schedule.go, preauth_form.go, lab_report.go)
// minus the clinical keys (diagnosis, procedure) that no verify rule
// or amount route consumes and that stay HITL context only. Keys are
// referenced, never re-spelled, so an upstream rename is a compile
// break here, never a silent fork (the invest-aliases-verify pattern).
var requiredKeys = map[string][]string{
	xdoc.DocClaimForm: {
		xdoc.KeyClaimNumber, xdoc.KeyPolicyNumber, xdoc.KeyPatientName,
		xdoc.KeyHospitalName, xdoc.KeyAdmissionDate, xdoc.KeyDischargeDate,
		xdoc.KeyTotalAmountPaise,
	},
	xdoc.DocDischargeSummary: {
		xdoc.KeyPatientName, xdoc.KeyHospitalName,
		xdoc.KeyAdmissionDate, xdoc.KeyDischargeDate,
	},
	xdoc.DocHospitalBill: {
		xdoc.KeyPatientName, xdoc.KeyHospitalName,
		xdoc.KeyAdmissionDate, xdoc.KeyDischargeDate,
		xdoc.KeyTotalAmountPaise,
	},
	xdoc.DocPolicySchedule: {xdoc.KeyPolicyNumber, xdoc.KeyPatientName},
	xdoc.DocPreauthForm: {
		xdoc.KeyClaimNumber, xdoc.KeyPolicyNumber, xdoc.KeyPatientName,
		xdoc.KeyHospitalName, xdoc.KeyAdmissionDate,
		xdoc.KeyTotalAmountPaise,
	},
	xdoc.DocLabReport: {xdoc.KeyPatientName, xdoc.KeyHospitalName},
}

// billLinePrefix marks the reconstructed line-item family: the xdoc
// billLineKey(i) rendering is KeyBillLines + "_" + N. The bare
// KeyBillLines key alone (emitted MISSING when no table qualifies) is
// never a line item.
var billLinePrefix = xdoc.KeyBillLines + "_"

// MissingItems bridges an insufficient Result into the existing
// invest envelope vocabulary: one MissingField item per missing key
// with display-only detail, plus a single class-level item when the
// class itself is unknown (there the open question is the class, so
// the item is keyed by it). Same kind throughout and Missing-sorted
// input keep the output in invest.Validate canonical order. Empty on
// sufficient results.
func (r Result) MissingItems() []invest.MissingItem {
	if r.Sufficient {
		return nil
	}
	if len(r.Missing) == 0 {
		return []invest.MissingItem{{
			Kind:   invest.MissingField,
			Key:    r.DocType,
			Detail: "APA-31 " + ReasonUnknownDocType + ": class " + r.DocType + " fails closed to HITL",
		}}
	}
	out := make([]invest.MissingItem, 0, len(r.Missing))
	for _, k := range r.Missing {
		out = append(out, invest.MissingItem{
			Kind:   invest.MissingField,
			Key:    k,
			Detail: "APA-31: " + k + " lacks a usable PRESENT observation (insufficient evidence)",
		})
	}
	return out
}

// SynthesizeFinding bridges an insufficient Result into the closed
// verify R1-R10 taxonomy: an R8 (MISSING_REQUIRED_DOCUMENT, the
// evidence-sufficiency signal) finding carrying the document as the
// evidence pointer, so invest.Build accepts it without taxonomy or
// topology changes. Severity mirrors the existing low-OCR synth
// precedent (worker runNewPipeline emits its evidence-sufficiency R8
// at MEDIUM, not the engine R8 HIGH). The second return is false on
// sufficient results.
func (r Result) SynthesizeFinding() (verify.Exception, bool) {
	if r.Sufficient {
		return verify.Exception{}, false
	}
	return verify.Exception{
		Code:     verify.CodeMissingRequiredDocument,
		Severity: verify.SeverityMedium,
		Message: fmt.Sprintf("insufficient evidence for %s: %s; keys: %s.",
			r.DocType, strings.Join(r.ReasonCodes, ","), strings.Join(r.Missing, ",")),
		EvidenceIDs: []string{r.DocumentID},
	}, true
}

// Evaluate applies the per-class sufficiency invariant to one
// document's facts: sufficient iff the class is known, every required
// key is observed PRESENT (exactly-at-threshold suffices), and, for
// HOSPITAL_BILL, at least one bill_lines_N line item is PRESENT
// (ADR-007: tables reconstructed where the golden class demands
// them). Only StatusPresent counts: MISSING/absent observations,
// AMBIGUOUS raw values, and MULTI_CANDIDATE conflict sets (never a
// silent winner, per the #44 contract) are all unusable, each with
// its own reason code. Unknown statuses fail closed as missing.
//
// It is pure: no I/O, no clock, no network. Terminal errors fire
// only on unattributable input (blank DocumentID); unknown or blank
// classes fail closed to insufficient (HITL), never to an error —
// invalid confidence is evidence-quality precedent (APA-12) for
// routing doubt to humans rather than crashing the pipeline.
func Evaluate(facts extract.DocumentFacts) (Result, error) {
	if strings.TrimSpace(facts.DocumentID) == "" {
		return Result{}, fmt.Errorf("sufficiency: blank DocumentID (evidence unattributable)")
	}
	res := Result{DocumentID: facts.DocumentID, DocType: facts.DocType}
	required, known := requiredKeys[facts.DocType]
	if !known {
		res.ReasonCodes = []string{ReasonUnknownDocType}
		res.Routing = RoutingHITL
		return res, nil
	}
	reasons := make(map[string]struct{})
	for _, key := range required {
		f, ok := facts.Fields[key]
		if !ok || f.Status == extract.StatusMissing {
			res.Missing = append(res.Missing, key)
			reasons[ReasonMissingField] = struct{}{}
			continue
		}
		switch f.Status {
		case extract.StatusPresent:
		case extract.StatusAmbiguous:
			res.Missing = append(res.Missing, key)
			reasons[ReasonAmbiguousField] = struct{}{}
		case extract.StatusMultiCandidate:
			res.Missing = append(res.Missing, key)
			reasons[ReasonMultiCandidateField] = struct{}{}
		default:
			// Unknown status: no usable observation by definition
			// (the #44 vocabulary is closed); fail closed as missing.
			res.Missing = append(res.Missing, key)
			reasons[ReasonMissingField] = struct{}{}
		}
	}
	if facts.DocType == xdoc.DocHospitalBill && !hasBillLine(facts) {
		res.Missing = append(res.Missing, xdoc.KeyBillLines)
		reasons[ReasonMissingBillLines] = struct{}{}
	}
	slices.Sort(res.Missing)
	for reason := range reasons {
		res.ReasonCodes = append(res.ReasonCodes, reason)
	}
	slices.Sort(res.ReasonCodes)
	res.Sufficient = len(res.Missing) == 0
	if res.Sufficient {
		res.Routing = RoutingContinue
	} else {
		res.Routing = RoutingHITL
	}
	return res, nil
}

// hasBillLine reports whether facts carry at least one reconstructed
// bill line item: a bill_lines_N key with StatusPresent. The bare
// bill_lines marker (MISSING when no table row qualifies) never
// counts, nor do non-PRESENT line keys.
func hasBillLine(facts extract.DocumentFacts) bool {
	for key, f := range facts.Fields {
		if !strings.HasPrefix(key, billLinePrefix) {
			continue
		}
		if f.Status == extract.StatusPresent {
			return true
		}
	}
	return false
}
