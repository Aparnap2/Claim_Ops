package orchestrate

// APA-67 — the bounded, content-free record of a REJECTED report.
//
// Finding: on the validator rejection path the loop hands submitPartial to
// the escalation, and submitPartial returns nil whenever ValidateReport
// fails. A wrong-kind (and several other) denials therefore reached the
// escalation with Partial == nil and no in-band trace of what the model
// emitted: the denial could not be diagnosed from the run at all. The
// grounding path does not lose the report, so it needs no record.
//
// This file adds RejectionRecord: what the model structurally submitted,
// reduced to closed vocabulary and counts. It is diagnostic, not a payload.
//
// What is deliberately NOT retained, ever:
//   - raw model bytes, prompts, or model output text;
//   - report free text (hypothesis statement/falsifier, finding summary,
//     recommendation rationale);
//   - missing_additive Detail, which is free text by construction;
//   - missing_additive Key unless it is a member of closedExternalKeys.
//
// That last rule is the load-bearing one. Missing additive keys are NOT
// closed vocabulary: for required_document and field the key is model-
// authored text, and for external the key is closed ONLY for items that
// PASSED validation — the canonical denial's offending key ("policy_number",
// or anything else the model invented) is exactly the unvalidated string a
// (kind, key) retention would have captured. So the record keeps the key
// only when it is one of four compile-time literals, and says so with an
// explicit flag instead of silently dropping it.

import (
	"fmt"
	"strings"

	"claimops-api/internal/invest"
)

// RejectionState is the closed discriminator that separates "no record at
// all" from "the model submitted nothing" from "the model submitted
// something that was rejected, and this is the bounded trace of it".
//
// The discriminator is an explicit enum precisely so absence is never
// inferred from a nil-vs-empty-slice ambiguity: the zero value of the
// record slot is nil, and nil is classified as RejectionStateAbsent by
// RejectionStateOf.
type RejectionState string

const (
	// RejectionStateAbsent marks a run result carrying no record. It is
	// never stored inside a record (that would be contradictory); it is
	// the classification of a nil record slot.
	RejectionStateAbsent RejectionState = "ABSENT"
	// RejectionStateEmpty marks a decoded submission that was genuinely
	// empty: no hypotheses, no findings, no additive items. Nothing was
	// lost, so nothing is retained, but the record SAYS that so it cannot
	// be confused with a rejected report whose evidence went missing.
	RejectionStateEmpty RejectionState = "EMPTY_SUBMISSION"
	// RejectionStateRejected marks a decoded submission that carried
	// content and was denied; the counts and additive refs below are that
	// content, reduced to closed vocabulary.
	RejectionStateRejected RejectionState = "REJECTED_WITH_EVIDENCE"
)

// closedExternalKeys is the external-source vocabulary this package
// defines. A member may be retained verbatim in a RejectionRecord because
// it is a compile-time literal of this codebase, not model text.
//
// This set is intentionally narrower than, not shared with, the literal
// list inside validateMissingAdditive. That validator must stay
// byte-identical, so the record keeps its own copy and is validated
// against it. Divergence fails in the safe direction: a key added to the
// validator but not here would be omitted from records, never admitted
// into one.
var closedExternalKeys = map[string]struct{}{
	"policy":   {},
	"tpa":      {},
	"provider": {},
	"risk":     {},
}

// AdditiveRef is one rejected missing_additive item reduced to its closed
// kind plus, only when that key is a compile-time literal, the key itself.
// Detail is never present: it is free text and would carry the model output
// verbatim.
type AdditiveRef struct {
	// Kind is the closed additive kind vocabulary.
	Kind invest.MissingKind `json:"kind"`
	// Key is retained only when KeyInVocabulary is true; otherwise empty.
	Key string `json:"key,omitempty"`
	// KeyInVocabulary states whether Key is a closed-vocabulary literal.
	// A false value with a populated Key is rejected by
	// ValidateRejectionRecord, so no arbitrary key can ride along.
	KeyInVocabulary bool `json:"key_in_vocabulary"`
}

// RejectionRecord is the in-band trace of a denied submission: the action,
// the invalid-output class, and the structural shape of what was submitted.
// Counts are lengths; keys are compile-time literals. No text, no values,
// no raw bytes.
//
// Plain struct with no methods: wire types in this package carry zero
// methods, and the only method in non-test code is (*Loop).Run.
type RejectionRecord struct {
	State RejectionState `json:"state"`
	// Action is the validated act name from the closed 2-act vocabulary.
	Action string `json:"action"`
	// InvalidKind is the I-class code from the closed invalidKindString
	// set (e.g. "I5-report").
	InvalidKind string `json:"invalid_kind"`
	// HypothesisCount and FindingCount are collection lengths only.
	HypothesisCount int `json:"hypothesis_count"`
	FindingCount    int `json:"finding_count"`
	// Additive is the submitted missing_additive list, one bounded ref
	// per item, in submitted order.
	Additive []AdditiveRef `json:"additive"`
}

// RejectionStateOf classifies one run result. A nil record slot is the
// explicit RejectionStateAbsent state; the answer never depends on a slice
// length, so absent, empty, and rejected-with-evidence are always
// distinguishable.
func RejectionStateOf(o InvestigationOutput) RejectionState {
	if o.RejectionRecord == nil {
		return RejectionStateAbsent
	}
	return o.RejectionRecord.State
}

// buildRejectionRecord reduces one rejected act to its bounded record. It
// returns nil for any act that carries no report: a call_tool denial has
// nothing to trace, and the grounding path keeps the whole report in
// Partial, so neither needs a record.
func buildRejectionRecord(a ModelAction, kind invalidKind) *RejectionRecord {
	if a.Action != ActionSubmitReport || a.Report == nil {
		return nil
	}
	r := a.Report
	rec := &RejectionRecord{
		State:           RejectionStateRejected,
		Action:          ActionSubmitReport,
		InvalidKind:     invalidKindString(kind),
		HypothesisCount: len(r.Hypotheses),
		FindingCount:    len(r.Findings),
		Additive:        make([]AdditiveRef, 0, len(r.MissingAdditive)),
	}
	for _, m := range r.MissingAdditive {
		ref := AdditiveRef{Kind: m.Kind}
		if m.Kind == invest.MissingExternal {
			if _, ok := closedExternalKeys[m.Key]; ok {
				ref.Key = m.Key
				ref.KeyInVocabulary = true
			}
		}
		rec.Additive = append(rec.Additive, ref)
	}
	if rec.HypothesisCount == 0 && rec.FindingCount == 0 && len(rec.Additive) == 0 {
		rec.State = RejectionStateEmpty
	}
	return rec
}

// ValidateRejectionRecord checks one retained record standalone. A nil
// record is valid (absence is a legal state). Everything present must be
// closed vocabulary, and the discriminator must agree with the content, so
// a record can never claim to be empty while carrying counts.
func ValidateRejectionRecord(r *RejectionRecord) error {
	if r == nil {
		return nil
	}
	if r.State == RejectionStateAbsent {
		return fmt.Errorf("orchestrate: rejection record is present but claims ABSENT: %w", ErrModelContract)
	}
	switch r.State {
	case RejectionStateEmpty, RejectionStateRejected:
	default:
		return fmt.Errorf("orchestrate: rejection record has unknown state %q: %w", string(r.State), ErrModelContract)
	}
	switch r.Action {
	case ActionCallTool, ActionSubmitReport:
	default:
		return fmt.Errorf("orchestrate: rejection record has unknown action %q: %w", r.Action, ErrModelContract)
	}
	if !validInvalidKindString(r.InvalidKind) {
		return fmt.Errorf("orchestrate: rejection record has unknown invalid_kind %q: %w", r.InvalidKind, ErrModelContract)
	}
	if r.HypothesisCount < 0 || r.FindingCount < 0 {
		return fmt.Errorf("orchestrate: rejection record has a negative count: %w", ErrModelContract)
	}
	for i := range r.Additive {
		a := &r.Additive[i]
		switch a.Kind {
		case invest.MissingRequiredDocument, invest.MissingField, invest.MissingExternal:
		default:
			return fmt.Errorf("orchestrate: rejection record additive[%d] has unknown kind %q: %w",
				i, string(a.Kind), ErrModelContract)
		}
		// The content-free guarantee, enforced: a retained key must be one
		// of this package's literals, and an omitted key must really be
		// omitted rather than smuggled past the flag.
		if a.KeyInVocabulary {
			if _, ok := closedExternalKeys[a.Key]; !ok {
				return fmt.Errorf("orchestrate: rejection record additive[%d] claims a closed-vocabulary key %q: %w",
					i, a.Key, ErrModelContract)
			}
			if a.Kind != invest.MissingExternal {
				return fmt.Errorf("orchestrate: rejection record additive[%d] is kind %q with a closed-vocabulary key: %w",
					i, string(a.Kind), ErrModelContract)
			}
		} else if a.Key != "" {
			return fmt.Errorf("orchestrate: rejection record additive[%d] carries key %q without the closed-vocabulary flag: %w",
				i, a.Key, ErrModelContract)
		}
	}
	empty := r.HypothesisCount == 0 && r.FindingCount == 0 && len(r.Additive) == 0
	switch r.State {
	case RejectionStateEmpty:
		if !empty {
			return fmt.Errorf("orchestrate: rejection record claims EMPTY_SUBMISSION but carries content: %w", ErrModelContract)
		}
	case RejectionStateRejected:
		if empty {
			return fmt.Errorf("orchestrate: rejection record claims REJECTED_WITH_EVIDENCE but carries nothing: %w", ErrModelContract)
		}
	}
	return nil
}

// validInvalidKindString reports whether s names one of the closed I-class
// codes. Derived from invalidKindString rather than re-spelled, so the two
// cannot drift.
func validInvalidKindString(s string) bool {
	for k := invalidMalformed; k <= invalidOversize; k++ {
		if invalidKindString(k) == s {
			return true
		}
	}
	return false
}

// normalizeRejectionRecordCopy returns a canonical copy with the additive
// list nil-normalized, so marshaling is a pure function of content.
func normalizeRejectionRecordCopy(r *RejectionRecord) *RejectionRecord {
	if r == nil {
		return nil
	}
	out := *r
	if out.Additive == nil {
		out.Additive = []AdditiveRef{}
	} else {
		out.Additive = append([]AdditiveRef(nil), r.Additive...)
	}
	return &out
}

// rejectionRecordIsContentFree is a development-time assertion helper used by
// the package tests: it re-checks that no retained string is outside the
// closed vocabularies this file defines. Production code relies on
// buildRejectionRecord plus ValidateRejectionRecord instead; this exists so
// the invariant is pinned from both sides rather than trusted once.
func rejectionRecordIsContentFree(r *RejectionRecord) bool {
	if r == nil {
		return true
	}
	if strings.TrimSpace(r.Action) == "" || !validInvalidKindString(r.InvalidKind) {
		return false
	}
	for i := range r.Additive {
		a := &r.Additive[i]
		if !a.KeyInVocabulary {
			if a.Key != "" {
				return false
			}
			continue
		}
		if _, ok := closedExternalKeys[a.Key]; !ok {
			return false
		}
	}
	return true
}
