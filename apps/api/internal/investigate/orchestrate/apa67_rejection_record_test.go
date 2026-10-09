package orchestrate

// APA-67 — retain an in-band, content-free record of a REJECTED report.
//
// Finding under test: on the validator rejection path the loop calls
// submitPartial, which returns nil whenever ValidateReport fails. A denial
// therefore reaches the escalation with Partial == nil and no in-band trace
// of what the model emitted. The grounding path does not lose the report.
//
// Contract proved here:
//   1. a rejected well-formed report is reconstructible from the escalation
//      record alone (no wire capture, no model-output logging);
//   2. the rejection stays terminal INVALID_OUTPUT and no validator or
//      grounding rule is weakened (the historical
//      `missing_additive[0] unknown external source "policy_number"` message
//      is still produced);
//   3. the record is diagnostic WITHOUT being a payload: synthetic PII/PHI
//      markers seeded anywhere in the model's report text appear nowhere in
//      it;
//   4. genuinely-empty is distinguishable from lost-evidence via an explicit
//      three-state discriminator, never by nil-vs-empty-slice ambiguity;
//   5. grounding-path behaviour is unchanged.
//
// The retained-record assertions run against the serialized escalation
// (MarshalOutput), which is what "the escalation record" means at the run
// boundary and what actually travels to the next stage. That keeps this file
// compilable against the pre-fix tree, so the RED proof is a behavioural
// failure rather than a compile error.
//
// Deterministic, no Groq: FakeModelClient, testEnvelope, testScope.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"claimops-api/internal/invest"
)

// ---------------------------------------------------------------------------
// Wire view of the retained record
//
// Decoded structurally so the assertions describe the contract, not a Go
// type. `key` is present only when the key is a member of the closed
// external-source vocabulary; `key_in_vocabulary` says which case holds.
// ---------------------------------------------------------------------------

type apa67WireAdditive struct {
	Kind            string `json:"kind"`
	Key             string `json:"key"`
	KeyInVocabulary bool   `json:"key_in_vocabulary"`
}

type apa67WireRecord struct {
	State           string              `json:"state"`
	Action          string              `json:"action"`
	InvalidKind     string              `json:"invalid_kind"`
	HypothesisCount int                 `json:"hypothesis_count"`
	FindingCount    int                 `json:"finding_count"`
	Additive        []apa67WireAdditive `json:"additive"`
}

// apa67RecordOf returns the retained record's wire object and the raw
// sub-document bytes, failing closed when the key is absent.
func apa67RecordOf(t *testing.T, out InvestigationOutput) (apa67WireRecord, []byte) {
	t.Helper()
	raw, err := MarshalOutput(out)
	if err != nil {
		t.Fatalf("MarshalOutput: %v", err)
	}
	var envelope struct {
		RejectionRecord json.RawMessage `json:"rejection_record"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal escalation: %v", err)
	}
	if len(envelope.RejectionRecord) == 0 {
		t.Fatalf("escalation carries no rejection_record (partial=%v): "+
			"the validator rejection discarded the submitted report with no in-band trace",
			out.Partial == nil)
	}
	var rec apa67WireRecord
	if err := json.Unmarshal(envelope.RejectionRecord, &rec); err != nil {
		t.Fatalf("unmarshal rejection_record: %v", err)
	}
	return rec, envelope.RejectionRecord
}

// apa67RawRecordKey is the serialized key of the retained record on the
// escalation wire contract.
const apa67RawRecordKey = "rejection_record"

// rejectedExternalBytes renders the canonical APA-56 denial: a structurally
// fine report whose single missing_additive item is classified external but
// keyed "policy_number", which is not in the closed external vocabulary.
// ValidateReport (via validateMissingAdditive) rejects it, so submitPartial
// returns nil and the report is otherwise lost.
func rejectedExternalBytes(t *testing.T, env invest.UnresolvedException) []byte {
	t.Helper()
	rep := testReport(env)
	rep.MissingAdditive = []invest.MissingItem{{
		Kind:   invest.MissingExternal,
		Key:    "policy_number",
		Detail: "R1: no pinned policy evidence",
	}}
	return submitBytes(t, rep)
}

// ---------------------------------------------------------------------------
// AC1 — a rejected, well-formed report is reconstructible from the record
// ---------------------------------------------------------------------------

// TestAPA67_RejectedReportReconstructibleFromEscalationAlone is the primary
// gap proof: before APA-67 the escalation carries nothing about the rejected
// submission, so a wrong-source denial cannot be diagnosed from the run.
func TestAPA67_RejectedReportReconstructibleFromEscalationAlone(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(rejectedExternalBytes(t, env)),
	}}
	out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
	if !errors.Is(err, ErrModelContract) {
		t.Fatalf("err = %v, want an ErrModelContract classification", err)
	}
	// The report really was discarded: the record is the ONLY in-band
	// evidence, which is the whole point of the issue.
	if out.Partial != nil {
		t.Fatalf("Partial = %+v, want nil (rejected report is not a valid Partial)", out.Partial)
	}
	if out.Report != nil {
		t.Fatalf("ESCALATED must not carry Report, got %+v", out.Report)
	}

	rec, _ := apa67RecordOf(t, out)

	// Every fact below is reconstructible from the escalation alone.
	if rec.State != "REJECTED_WITH_EVIDENCE" {
		t.Fatalf("state = %q, want REJECTED_WITH_EVIDENCE", rec.State)
	}
	if rec.Action != "submit_report" {
		t.Fatalf("action = %q, want submit_report", rec.Action)
	}
	if rec.InvalidKind != "I5-report" {
		t.Fatalf("invalid_kind = %q, want I5-report", rec.InvalidKind)
	}
	if rec.HypothesisCount != 1 {
		t.Fatalf("hypothesis_count = %d, want 1", rec.HypothesisCount)
	}
	if rec.FindingCount != 1 {
		t.Fatalf("finding_count = %d, want 1", rec.FindingCount)
	}
	if len(rec.Additive) != 1 {
		t.Fatalf("additive = %+v, want exactly one entry", rec.Additive)
	}
	if rec.Additive[0].Kind != "external" {
		t.Fatalf("additive[0].kind = %q, want external", rec.Additive[0].Kind)
	}
	// The offending key was OUTSIDE the closed vocabulary, so it is not
	// retained as text -- retaining it would have stored the exact
	// model-authored string that caused the denial. The record still says
	// the item was an external source, which is precisely what makes the
	// wrong-source denial diagnosable without holding the payload.
	if rec.Additive[0].KeyInVocabulary {
		t.Fatal(`additive[0].key_in_vocabulary = true for key "policy_number", ` +
			"which is not a closed-vocabulary literal")
	}
	if rec.Additive[0].Key != "" {
		t.Fatalf("additive[0].key = %q, want omitted (an out-of-vocabulary key is model text)", rec.Additive[0].Key)
	}
}

// TestAPA67_ClosedVocabularyKeysRetainedAndModelKeysDropped exercises the
// builder directly on the load-bearing rule: a missing_additive key is
// retained ONLY when it is one of this package's compile-time external
// literals. required_document and field keys are model-authored text, and
// an out-of-vocabulary external key is the offending string itself, so all
// three are dropped while the kind survives.
func TestAPA67_ClosedVocabularyKeysRetainedAndModelKeysDropped(t *testing.T) {
	rep := testReport(invest.UnresolvedException{})
	rep.MissingAdditive = []invest.MissingItem{
		{Kind: invest.MissingExternal, Key: "policy", Detail: "d"},
		{Kind: invest.MissingExternal, Key: "tpa", Detail: "d"},
		{Kind: invest.MissingExternal, Key: "provider", Detail: "d"},
		{Kind: invest.MissingExternal, Key: "risk", Detail: "d"},
		{Kind: invest.MissingExternal, Key: "policy_number", Detail: "d"},
		{Kind: invest.MissingField, Key: "admission_date", Detail: "d"},
		{Kind: invest.MissingRequiredDocument, Key: "CLAIM_FORM", Detail: "d"},
	}
	a := ModelAction{Action: ActionSubmitReport, Report: &rep}
	rec := buildRejectionRecord(a, invalidReport)
	if rec == nil {
		t.Fatal("buildRejectionRecord returned nil for a rejected submit_report")
	}
	if err := ValidateRejectionRecord(rec); err != nil {
		t.Fatalf("ValidateRejectionRecord rejected a well-built record: %v", err)
	}
	if !rejectionRecordIsContentFree(rec) {
		t.Fatalf("record is not content-free: %+v", rec)
	}
	wantKey := []string{"policy", "tpa", "provider", "risk", "", "", ""}
	if len(rec.Additive) != len(wantKey) {
		t.Fatalf("additive = %d entries, want %d", len(rec.Additive), len(wantKey))
	}
	for i, want := range wantKey {
		got := rec.Additive[i]
		if got.Key != want {
			t.Fatalf("additive[%d].key = %q, want %q (kind %q)",
				i, got.Key, want, string(got.Kind))
		}
		if want == "" && got.KeyInVocabulary {
			t.Fatalf("additive[%d] claims closed-vocabulary for dropped key %q", i, got.Key)
		}
		if want != "" && !got.KeyInVocabulary {
			t.Fatalf("additive[%d] key %q retained without the vocabulary flag", i, got.Key)
		}
	}
	// Detail is never carried, in any case.
	raw, err := MarshalOutput(InvestigationOutput{
		InvestigationID:  tInvID,
		Outcome:          OutcomeEscalated,
		EscalationReason: EscalationInvalidOutput,
		RejectionRecord:  rec,
		AttemptLog:       []TurnRecord{},
	})
	if err != nil {
		t.Fatalf("MarshalOutput: %v", err)
	}
	if strings.Contains(string(raw), `"d"`) {
		t.Fatalf("additive Detail leaked into the escalation: %s", raw)
	}
}

// TestAPA67_BuilderIgnoresActsWithoutAReport keeps the record scoped to the
// gap it fills: a call_tool denial has no report to trace, so it produces
// no record at all.
func TestAPA67_BuilderIgnoresActsWithoutAReport(t *testing.T) {
	if rec := buildRejectionRecord(ModelAction{Action: ActionCallTool}, invalidDenied); rec != nil {
		t.Fatalf("call_tool denial produced a record: %+v", rec)
	}
	if rec := buildRejectionRecord(ModelAction{Action: ActionSubmitReport}, invalidReport); rec != nil {
		t.Fatalf("submit_report without a report produced a record: %+v", rec)
	}
}

// ---------------------------------------------------------------------------
// AC2 — terminal INVALID_OUTPUT, no rule weakened
// ---------------------------------------------------------------------------

// TestAPA67_RejectionStaysTerminalWithHistoricalMessage pins the exact
// denial APA-55/APA-56 recorded as evidence. This test PASSES before and
// after APA-67 by construction: it is the do-not-weaken guard, not the gap.
func TestAPA67_RejectionStaysTerminalWithHistoricalMessage(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(rejectedExternalBytes(t, env)),
	}}
	out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
	if err == nil {
		t.Fatal("wrong-kind external source was accepted; the validator is the containment boundary")
	}
	if out.EscalationReason != EscalationInvalidOutput {
		t.Fatalf("Reason = %q, want terminal %q", out.EscalationReason, EscalationInvalidOutput)
	}
	if out.Outcome != OutcomeEscalated {
		t.Fatalf("Outcome = %q, want ESCALATED", out.Outcome)
	}
	if !errors.Is(err, ErrModelContract) {
		t.Fatalf("err = %v, want ErrModelContract", err)
	}
	if !strings.Contains(err.Error(), `missing_additive[0] unknown external source "policy_number"`) {
		t.Fatalf("historical message lost, rejected for the wrong reason: %v", err)
	}
	// No accept-and-warn: the report is not delivered.
	if out.Report != nil {
		t.Fatal("ESCALATED delivered a Report; the rejection must stay terminal")
	}
	if err := ValidateInvestigationOutput(out); err != nil {
		t.Fatalf("escalation is not a valid output: %v", err)
	}
}

// TestAPA67_ValidatorRulesUnchangedByRecord proves retaining a record does
// not re-open any rule: the same report is still rejected, and every other
// additive defect still fails closed.
func TestAPA67_ValidatorRulesUnchangedByRecord(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	known, err := SeedKnownEvidence(env)
	if err != nil {
		t.Fatalf("SeedKnownEvidence: %v", err)
	}
	cases := []struct {
		name    string
		mutate  func(*Report)
		wantMsg string
	}{
		{
			name: "unknown external source",
			mutate: func(r *Report) {
				r.MissingAdditive = []invest.MissingItem{{
					Kind: invest.MissingExternal, Key: "policy_number", Detail: "d",
				}}
			},
			wantMsg: `unknown external source "policy_number"`,
		},
		{
			name: "blank key",
			mutate: func(r *Report) {
				r.MissingAdditive = []invest.MissingItem{{
					Kind: invest.MissingField, Key: "   ", Detail: "d",
				}}
			},
			wantMsg: "has blank key",
		},
		{
			name: "blank detail",
			mutate: func(r *Report) {
				r.MissingAdditive = []invest.MissingItem{{
					Kind: invest.MissingField, Key: "admission_date", Detail: " ",
				}}
			},
			wantMsg: "has blank detail",
		},
		{
			name: "unknown kind",
			mutate: func(r *Report) {
				r.MissingAdditive = []invest.MissingItem{{
					Kind: invest.MissingKind("guess"), Key: "admission_date", Detail: "d",
				}}
			},
			wantMsg: "has unknown kind",
		},
		{
			name: "no hypotheses",
			mutate: func(r *Report) {
				r.Hypotheses = []invest.Hypothesis{}
			},
			wantMsg: "at least one hypothesis",
		},
		{
			name: "no findings",
			mutate: func(r *Report) {
				r.Findings = []invest.Finding{}
			},
			wantMsg: "at least one finding",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := testReport(env)
			tc.mutate(&rep)
			raw := submitBytes(t, rep)
			a, derr := DecodeModelAction(raw, 1<<20)
			if derr != nil {
				return // rejected at the decoder, which is also containment
			}
			verr := ValidateModelAction(a, scope, env.InvestigationID)
			if verr == nil {
				t.Fatalf("%s was accepted; validation must not be relaxed", tc.name)
			}
			if !strings.Contains(verr.Error(), tc.wantMsg) {
				t.Fatalf("rejected for the wrong reason: %v", verr)
			}
			if !errors.Is(verr, ErrModelContract) {
				t.Fatalf("err = %v, want ErrModelContract", verr)
			}
			// Grounding rules still stand on their own.
			if gerr := CheckReportGrounding(rep, known, env); tc.name == "no hypotheses" && gerr == nil {
				t.Fatal("grounding accepted a report with no hypotheses")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// AC3 — sensitive content is not retained
// ---------------------------------------------------------------------------

// TestAPA67_RetainedRecordCarriesNoSensitiveMarkers seeds synthetic
// markers into every free-text position of the model's report — including
// the offending missing_additive key, the one field a naive (kind, key)
// retention would leak — and asserts they appear nowhere in the record.
func TestAPA67_RetainedRecordCarriesNoSensitiveMarkers(t *testing.T) {
	const (
		personMarker = "TEST_PERSON_001"
		secretMarker = "TEST_SECRET_CLAIMOPS_123456"
	)
	markers := []string{personMarker, secretMarker}

	placements := []struct {
		name string
		// rejectsAlready reports whether mutate alone produces a report the
		// validator denies; otherwise the canonical wrong-source item is
		// appended so a record must exist. Free-text edits alone leave a
		// valid, grounded report that never escalates.
		rejectsAlready bool
		mutate         func(*Report)
	}{
		{name: "hypothesis statement", mutate: func(r *Report) {
			r.Hypotheses[0].Statement = "Subject " + personMarker + " filed a claim."
		}},
		{name: "hypothesis falsifier", mutate: func(r *Report) {
			r.Hypotheses[0].Falsifier = "Refuted if " + secretMarker + " appears."
		}},
		{name: "finding summary", mutate: func(r *Report) {
			r.Findings[0].Summary = "Reviewed for " + personMarker + "."
		}},
		{name: "recommendation rationale", mutate: func(r *Report) {
			r.Recommendation.Rationale = "Escalate, token " + secretMarker + "."
		}},
		{name: "missing_additive detail", rejectsAlready: true, mutate: func(r *Report) {
			r.MissingAdditive = []invest.MissingItem{{
				Kind:   invest.MissingExternal,
				Key:    "policy_number",
				Detail: "Missing record for " + personMarker + " / " + secretMarker,
			}}
		}},
		{name: "missing_additive key", rejectsAlready: true, mutate: func(r *Report) {
			// Worst case for retention: the offending key IS the secret.
			r.MissingAdditive = []invest.MissingItem{{
				Kind:   invest.MissingExternal,
				Key:    personMarker,
				Detail: "keyed by " + secretMarker,
			}}
		}},
		{name: "field-keyed additive", mutate: func(r *Report) {
			// A perfectly VALID field item whose key is the secret: retained
			// verbatim by a naive (kind, key) design, which must not happen.
			r.MissingAdditive = []invest.MissingItem{{
				Kind:   invest.MissingField,
				Key:    secretMarker,
				Detail: "detail for " + personMarker,
			}}
		}},
	}

	for _, p := range placements {
		t.Run(p.name, func(t *testing.T) {
			env := testEnvelope(t)
			scope := testScope(env)
			rep := testReport(env)
			p.mutate(&rep)
			if !p.rejectsAlready {
				// Force the canonical I5 denial so a record must exist.
				rep.MissingAdditive = append(rep.MissingAdditive, invest.MissingItem{
					Kind:   invest.MissingExternal,
					Key:    "policy_number",
					Detail: "R1: no pinned policy evidence",
				})
			}
			if err := ValidateReport(rep); err == nil {
				t.Fatalf("%s: fixture is not rejected, so no record could exist", p.name)
			}
			fake := &FakeModelClient{Responses: []ModelResponse{
				modelResp(submitBytes(t, rep)),
			}}
			out, runErr := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
			if runErr == nil {
				t.Fatalf("%s: run accepted a report carrying a marker", p.name)
			}
			if out.EscalationReason != EscalationInvalidOutput {
				t.Fatalf("%s: Reason = %q, want INVALID_OUTPUT", p.name, out.EscalationReason)
			}
			_, recordBytes := apa67RecordOf(t, out)
			for _, m := range markers {
				if strings.Contains(string(recordBytes), m) {
					t.Fatalf("%s: retained record leaks marker %q: %s", p.name, m, recordBytes)
				}
			}
			// On this path nothing else in the escalation may carry them
			// either (Partial is nil by construction here).
			raw, err := MarshalOutput(out)
			if err != nil {
				t.Fatalf("MarshalOutput: %v", err)
			}
			for _, m := range markers {
				if strings.Contains(string(raw), m) {
					t.Fatalf("%s: escalation leaks marker %q", p.name, m)
				}
			}
		})
	}
}

// TestAPA67_ValidateRejectionRecordFailsClosed proves the retained record
// is validated at the run boundary like every other field, and that the
// content-free guarantee is enforced rather than assumed. Nothing here is
// accepted that is not closed vocabulary.
func TestAPA67_ValidateRejectionRecordFailsClosed(t *testing.T) {
	if err := ValidateRejectionRecord(nil); err != nil {
		t.Fatalf("nil record must be valid (absence is legal): %v", err)
	}
	valid := func() *RejectionRecord {
		return &RejectionRecord{
			State:           RejectionStateRejected,
			Action:          ActionSubmitReport,
			InvalidKind:     "I5-report",
			HypothesisCount: 1,
			Additive:        []AdditiveRef{},
		}
	}
	if err := ValidateRejectionRecord(valid()); err != nil {
		t.Fatalf("well-formed record rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*RejectionRecord)
	}{
		{"absent state inside a record", func(r *RejectionRecord) { r.State = RejectionStateAbsent }},
		{"unknown state", func(r *RejectionRecord) { r.State = "SOMETHING" }},
		{"unknown action", func(r *RejectionRecord) { r.Action = "write_claim" }},
		{"unknown invalid kind", func(r *RejectionRecord) { r.InvalidKind = "I99" }},
		{"empty invalid kind", func(r *RejectionRecord) { r.InvalidKind = "" }},
		{"negative hypothesis count", func(r *RejectionRecord) { r.HypothesisCount = -1 }},
		{"unknown additive kind", func(r *RejectionRecord) {
			r.Additive = []AdditiveRef{{Kind: invest.MissingKind("guess")}}
		}},
		{"vocabulary flag with a model key", func(r *RejectionRecord) {
			r.Additive = []AdditiveRef{{
				Kind: invest.MissingExternal, Key: "policy_number", KeyInVocabulary: true,
			}}
		}},
		{"model key smuggled past the flag", func(r *RejectionRecord) {
			r.Additive = []AdditiveRef{{
				Kind: invest.MissingField, Key: "TEST_PERSON_001",
			}}
		}},
		{"closed key on a non-external kind", func(r *RejectionRecord) {
			r.Additive = []AdditiveRef{{
				Kind: invest.MissingField, Key: "policy", KeyInVocabulary: true,
			}}
		}},
		{"empty state with content", func(r *RejectionRecord) { r.State = RejectionStateEmpty }},
		{"rejected state with nothing", func(r *RejectionRecord) {
			*r = RejectionRecord{
				State: RejectionStateRejected, Action: ActionSubmitReport, InvalidKind: "I5-report",
				Additive: []AdditiveRef{},
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := valid()
			tc.mutate(rec)
			if err := ValidateRejectionRecord(rec); err == nil {
				t.Fatalf("%s: accepted; the record must fail closed", tc.name)
			} else if !errors.Is(err, ErrModelContract) {
				t.Fatalf("%s: err = %v, want ErrModelContract", tc.name, err)
			}
		})
	}
}

// TestAPA67_ReportReadyRejectsARejectionRecord keeps the success path and
// the denial path mutually exclusive.
func TestAPA67_ReportReadyRejectsARejectionRecord(t *testing.T) {
	env := testEnvelope(t)
	out := InvestigationOutput{
		InvestigationID: tInvID,
		Outcome:         OutcomeReportReady,
		Report:          &Report{}, // replaced below
		AttemptLog:      []TurnRecord{},
	}
	rep := testReport(env)
	out.Report = &rep
	if err := ValidateInvestigationOutput(out); err != nil {
		t.Fatalf("baseline REPORT_READY invalid: %v", err)
	}
	out.RejectionRecord = &RejectionRecord{
		State: RejectionStateRejected, Action: ActionSubmitReport,
		InvalidKind: "I5-report", HypothesisCount: 1, Additive: []AdditiveRef{},
	}
	if err := ValidateInvestigationOutput(out); err == nil {
		t.Fatal("REPORT_READY accepted a rejection record")
	}
}

// ---------------------------------------------------------------------------
// AC4 — empty submission is distinguishable from lost evidence
// ---------------------------------------------------------------------------

// TestAPA67_DiscriminatorDistinguishesAllThreeStates exercises ABSENT,
// EMPTY_SUBMISSION and REJECTED_WITH_EVIDENCE explicitly. No nil-vs-empty
// slice inference anywhere.
func TestAPA67_DiscriminatorDistinguishesAllThreeStates(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		// Grounding rejection: the report is fully retained in Partial, so
		// nothing is lost and no record is produced.
		env := testEnvelope(t)
		scope := testScope(env)
		rep := testReport(env, "ev-new-01")
		rep.Findings[0].EvidenceIDs = []string{"ev-invented-01"}
		fake := &FakeModelClient{Responses: []ModelResponse{
			modelResp(callToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
			modelResp(submitBytes(t, rep)),
		}}
		out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
		if !errors.Is(err, ErrGrounding) {
			t.Fatalf("err = %v, want ErrGrounding", err)
		}
		raw, merr := MarshalOutput(out)
		if merr != nil {
			t.Fatalf("MarshalOutput: %v", merr)
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if v, ok := envelope[apa67RawRecordKey]; ok {
			t.Fatalf("grounding rejection carries %s=%s; nothing is lost there", apa67RawRecordKey, v)
		}
	})

	t.Run("empty_submission", func(t *testing.T) {
		env := testEnvelope(t)
		scope := testScope(env)
		// A genuinely empty submission: a report object with no content at
		// all. There is nothing to retain, but the record must SAY that,
		// so it is distinguishable from a lost non-empty report.
		empty := Report{}
		empty.Hypotheses = []invest.Hypothesis{}
		empty.Findings = []invest.Finding{}
		empty.MissingAdditive = []invest.MissingItem{}
		fake := &FakeModelClient{Responses: []ModelResponse{
			modelResp(submitBytes(t, empty)),
		}}
		out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
		if !errors.Is(err, ErrModelContract) {
			t.Fatalf("err = %v, want ErrModelContract", err)
		}
		rec, _ := apa67RecordOf(t, out)
		if rec.State != "EMPTY_SUBMISSION" {
			t.Fatalf("state = %q, want EMPTY_SUBMISSION", rec.State)
		}
		if rec.HypothesisCount != 0 || rec.FindingCount != 0 || len(rec.Additive) != 0 {
			t.Fatalf("empty submission retained content: %+v", rec)
		}
	})

	t.Run("rejected_with_evidence", func(t *testing.T) {
		env := testEnvelope(t)
		scope := testScope(env)
		fake := &FakeModelClient{Responses: []ModelResponse{
			modelResp(rejectedExternalBytes(t, env)),
		}}
		out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
		if !errors.Is(err, ErrModelContract) {
			t.Fatalf("err = %v, want ErrModelContract", err)
		}
		rec, _ := apa67RecordOf(t, out)
		if rec.State != "REJECTED_WITH_EVIDENCE" {
			t.Fatalf("state = %q, want REJECTED_WITH_EVIDENCE", rec.State)
		}
		// An empty submission and a lost report are therefore never
		// confusable: the discriminator, not a slice length, separates them.
		if rec.HypothesisCount == 0 && rec.FindingCount == 0 && len(rec.Additive) == 0 {
			t.Fatal("rejected-with-evidence must not be indistinguishable from empty")
		}
	})
}

// ---------------------------------------------------------------------------
// AC5 — grounding path unchanged
// ---------------------------------------------------------------------------

// TestAPA67_GroundingRejectionStillRetainsPartialUnchanged proves APA-67
// did not move the grounding path: a grounding rejection still populates
// InvestigationOutput.Partial with the full report exactly as before, and
// adds no record.
func TestAPA67_GroundingRejectionStillRetainsPartialUnchanged(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	rep := testReport(env, "ev-new-01")
	rep.Findings[0].EvidenceIDs = []string{"ev-invented-01"}
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(callToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
		modelResp(submitBytes(t, rep)),
	}}
	out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
	if !errors.Is(err, ErrGrounding) {
		t.Fatalf("err = %v, want ErrGrounding", err)
	}
	if out.EscalationReason != EscalationInvalidOutput {
		t.Fatalf("Reason = %q, want INVALID_OUTPUT", out.EscalationReason)
	}
	// Unchanged behaviour: the whole report survives on this path.
	if out.Partial == nil {
		t.Fatal("grounding rejection lost Partial; APA-67 must not change the grounding path")
	}
	if len(out.Partial.Hypotheses) != 1 || len(out.Partial.Findings) != 1 {
		t.Fatalf("Partial was truncated: %+v", out.Partial)
	}
	if err := ValidateInvestigationOutput(out); err != nil {
		t.Fatalf("ValidateInvestigationOutput: %v", err)
	}
	// And no new record is layered on top of it.
	raw, merr := MarshalOutput(out)
	if merr != nil {
		t.Fatalf("MarshalOutput: %v", merr)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := envelope[apa67RawRecordKey]; ok {
		t.Fatalf("grounding path gained a %s; behaviour must be unchanged", apa67RawRecordKey)
	}
}

// TestAPA67_ReportReadyCarriesNoRecord keeps the success path clean: a
// delivered report needs no rejection record.
func TestAPA67_ReportReadyCarriesNoRecord(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(callToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
		modelResp(submitBytes(t, testReport(env, "ev-new-01"))),
	}}
	out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, merr := MarshalOutput(out)
	if merr != nil {
		t.Fatalf("MarshalOutput: %v", merr)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := envelope[apa67RawRecordKey]; ok {
		t.Fatalf("REPORT_READY carries %s; omitempty must keep the success wire byte-identical", apa67RawRecordKey)
	}
}
