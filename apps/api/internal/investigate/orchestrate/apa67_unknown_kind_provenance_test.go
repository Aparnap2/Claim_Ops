package orchestrate

// APA-67 provenance — the rejection record must be valid BY CONSTRUCTION for
// any input the model can produce.
//
// Finding under test (owner-reported, reproduced below): buildRejectionRecord
// copied MissingItem.Kind straight into AdditiveRef.Kind, but
// ValidateRejectionRecord admits only the three closed kinds. A report
// carrying an unknown kind ("guess") was therefore rejected by the report
// validator, retained verbatim by the builder, and then rejected AGAIN by
// ValidateRejectionRecord. failWithRecord's ValidateInvestigationOutput
// consequently failed and the loop aborted raw with an empty
// InvestigationOutput{} plus the record error -- discarding a real denial and
// turning ordinary model input into the "loop programmer error" path that
// loop.go documents as unreachable from outside the loop.
//
// That is a behaviour regression, not a hardening nicety: before APA-67 the same
// input produced a correct ESCALATED / INVALID_OUTPUT carrying the cause.
//
// GOVERNING RULE (stated here because it is what stops the next instance of
// this bug class, not this one field): every value in a RejectionRecord must be
// a compile-time literal of this package or a length of a model-controlled
// collection. A field whose value is model-authored must never be RETAINED; it
// must be reduced to a closed vocabulary plus an explicit flag saying whether
// the submitted value was in that vocabulary. The record may therefore be built
// from ANY decoded act the loop can hand it, and can never invalidate the
// escalation that carries it.
//
// Unknown additive Kind is now handled exactly parallel to the established
// key_in_vocabulary pattern: kind_in_vocabulary=true means Kind is one of the
// three closed literals; false means the submitted kind was out of vocabulary
// and the string was dropped, with the ENTRY KEPT so the item's existence and
// position in the submitted list still survive.
//
// Every assertion in this file runs through the WIRE (MarshalOutput /
// DecodeOutput / json.Marshal of the record's own additive slice) and through
// the existing public validators, never through a new Go field. That keeps the
// file compilable against the pre-fix tree, so the RED proof is a behavioural
// failure rather than a compile error -- the convention the sibling APA-67 file
// established. It also tests the contract at the boundary where the record
// actually travels, which is where an untrusted record arrives from another
// stage.
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

// unknownKindMarker is the synthetic offending kind the model would author.
// It is deliberately not a plausible vocabulary member, so its absence from a
// record cannot be confused with normalisation.
const unknownKindMarker = "guess"

// unknownKindMarkerKind is unknownKindMarker as the model would submit it.
var unknownKindMarkerKind = invest.MissingKind(unknownKindMarker)

// apa67KindWireAdditive is the wire view of one retained additive entry.
// `kind` is populated only when the submitted kind was a closed-vocabulary
// member; `kind_in_vocabulary` says which case holds.
type apa67KindWireAdditive struct {
	Kind             string `json:"kind"`
	KindInVocabulary bool   `json:"kind_in_vocabulary"`
	Key              string `json:"key"`
	KeyInVocabulary  bool   `json:"key_in_vocabulary"`
}

// apa67WireAdditiveOf returns the retained additive list as the wire sees it,
// which is the only place a flag or an omitted field can be observed without
// depending on a Go field name.
func apa67WireAdditiveOf(t *testing.T, refs []AdditiveRef) []apa67KindWireAdditive {
	t.Helper()
	raw, err := json.Marshal(refs)
	if err != nil {
		t.Fatalf("marshal additive: %v", err)
	}
	var wire []apa67KindWireAdditive
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal additive: %v", err)
	}
	if len(wire) != len(refs) {
		t.Fatalf("wire additive = %d entries, want %d", len(wire), len(refs))
	}
	return wire
}

// apa67RecordWireOf returns the retained additive list as it appears inside a
// marshalled escalation, so the round trip is observed at the boundary rather
// than through the in-memory struct.
func apa67RecordWireOf(t *testing.T, raw []byte) ([]apa67KindWireAdditive, error) {
	t.Helper()
	var envelope struct {
		RejectionRecord struct {
			Additive []apa67KindWireAdditive `json:"additive"`
		} `json:"rejection_record"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	return envelope.RejectionRecord.Additive, nil
}

// apa67DecodeRecord builds a full INVALID_OUTPUT escalation carrying one
// rejection_record whose additive list is the supplied JSON, and decodes it
// through DecodeOutput. This is the untrusted-wire path: a record arriving from
// another stage. Returns the decoded record on success, or the validation
// error that refused it.
func apa67DecodeRecord(t *testing.T, additiveJSON string) (*RejectionRecord, error) {
	t.Helper()
	raw := []byte(`{"investigation_id":"` + tInvID +
		`","outcome":"ESCALATED","escalation_reason":"INVALID_OUTPUT",` +
		`"rejection_record":{"state":"REJECTED_WITH_EVIDENCE","action":"submit_report",` +
		`"invalid_kind":"I5-report","hypothesis_count":1,"finding_count":0,"additive":[` +
		additiveJSON + `]},` +
		`"attempt_log":[],"turns_used":0,"tool_calls_used":0}`)
	out, err := DecodeOutput(raw)
	if err != nil {
		return nil, err
	}
	if out.RejectionRecord == nil {
		t.Fatalf("decoded escalation carries no rejection_record: %s", raw)
	}
	return out.RejectionRecord, nil
}

// rejectedUnknownKindBytes renders a report whose single missing_additive item
// carries an out-of-vocabulary kind. validateMissingAdditive rejects it as
// I5-report ("has unknown kind"), so submitPartial returns nil and a record
// must exist for the denial to be diagnosable.
func rejectedUnknownKindBytes(t *testing.T, env invest.UnresolvedException) []byte {
	t.Helper()
	rep := testReport(env)
	rep.MissingAdditive = []invest.MissingItem{{
		Kind:   unknownKindMarkerKind,
		Key:    "admission_date",
		Detail: "model-authored detail",
	}}
	return submitBytes(t, rep)
}

// ---------------------------------------------------------------------------
// AC1 — Run level: the denial survives with its cause
// ---------------------------------------------------------------------------

// TestAPA67_UnknownAdditiveKindPreservesTheEscalation is the primary
// regression proof. Before the fix this run returned an EMPTY
// InvestigationOutput and a record-validation error, because the record built
// from model input could not validate the escalation carrying it. After the fix
// the run must escalate exactly as it did before APA-67: ESCALATED /
// INVALID_OUTPUT, original cause intact, offending string not retained.
func TestAPA67_UnknownAdditiveKindPreservesTheEscalation(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	fake := &FakeModelClient{Responses: []ModelResponse{
		modelResp(rejectedUnknownKindBytes(t, env)),
	}}
	out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())

	// The loop must NOT abort raw. executeLoop documents a record that
	// fails ValidateInvestigationOutput as a loop PROGRAMMER error and
	// deliberately returns InvestigationOutput{} for it; model input must
	// never be able to reach that path.
	if out.Outcome != OutcomeEscalated {
		t.Fatalf("Outcome = %q (report=%v partial=%v record=%v err=%v), want %q: "+
			"an unknown additive kind invalidated its own escalation, so the loop "+
			"aborted raw and discarded a real denial",
			string(out.Outcome), out.Report != nil, out.Partial != nil,
			out.RejectionRecord != nil, err, string(OutcomeEscalated))
	}
	if out.EscalationReason != EscalationInvalidOutput {
		t.Fatalf("EscalationReason = %q, want %q", string(out.EscalationReason), string(EscalationInvalidOutput))
	}
	if err == nil {
		t.Fatal("run returned no error; the rejection must stay terminal")
	}
	if !errors.Is(err, ErrModelContract) {
		t.Fatalf("err = %v, want ErrModelContract", err)
	}
	// The ORIGINAL validation cause must survive verbatim: the escalation
	// still has to say WHY the report was denied.
	if !strings.Contains(err.Error(), `missing_additive[0] has unknown kind "`+unknownKindMarker+`"`) {
		t.Fatalf("original cause lost, denied for the wrong reason: %v", err)
	}
	// The record must be valid on its own, not merely present.
	if out.RejectionRecord == nil {
		t.Fatal("escalation carries no rejection_record; the denial became undiagnosable in-band")
	}
	if verr := ValidateRejectionRecord(out.RejectionRecord); verr != nil {
		t.Fatalf("ValidateRejectionRecord rejected the loop's own record: %v", verr)
	}
	if verr := ValidateInvestigationOutput(out); verr != nil {
		t.Fatalf("ValidateInvestigationOutput: %v", verr)
	}

	// Wire level: the escalation is well formed and carries the record.
	raw, merr := MarshalOutput(out)
	if merr != nil {
		t.Fatalf("MarshalOutput: %v", merr)
	}
	var envelope struct {
		RejectionRecord json.RawMessage `json:"rejection_record"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal escalation: %v", err)
	}
	if len(envelope.RejectionRecord) == 0 {
		t.Fatalf("escalation carries no rejection_record: %s", raw)
	}
	var rec struct {
		State           string                  `json:"state"`
		Action          string                  `json:"action"`
		InvalidKind     string                  `json:"invalid_kind"`
		HypothesisCount int                     `json:"hypothesis_count"`
		FindingCount    int                     `json:"finding_count"`
		Additive        []apa67KindWireAdditive `json:"additive"`
	}
	if err := json.Unmarshal(envelope.RejectionRecord, &rec); err != nil {
		t.Fatalf("unmarshal rejection_record: %v", err)
	}
	if rec.State != string(RejectionStateRejected) {
		t.Fatalf("state = %q, want %q", rec.State, string(RejectionStateRejected))
	}
	if rec.Action != ActionSubmitReport {
		t.Fatalf("action = %q, want submit_report", rec.Action)
	}
	if rec.InvalidKind != "I5-report" {
		t.Fatalf("invalid_kind = %q, want I5-report", rec.InvalidKind)
	}
	if len(rec.Additive) != 1 {
		t.Fatalf("additive = %+v, want exactly one entry (the out-of-vocabulary item "+
			"must be REPRESENTED, not dropped)", rec.Additive)
	}
	// The item survives; its kind does not.
	if rec.Additive[0].KindInVocabulary {
		t.Fatalf(`additive[0].kind_in_vocabulary = true for kind %q, which is not a `+
			"closed-vocabulary member", unknownKindMarker)
	}
	if rec.Additive[0].Kind != "" {
		t.Fatalf("additive[0].kind = %q, want omitted: the submitted kind is model text "+
			"and must not be retained", rec.Additive[0].Kind)
	}
	// The load-bearing assertion: the offending string is nowhere in the
	// escalation at all, record or otherwise.
	if strings.Contains(string(raw), unknownKindMarker) {
		t.Fatalf("escalation retains the model-authored kind %q: %s", unknownKindMarker, raw)
	}
}

// TestAPA67_UnknownKindRunRejectionMatchesPreAPA67Behaviour pins the
// behaviour-preservation claim directly: an unknown additive kind is denied
// exactly like every other out-of-vocabulary additive defect, and produces the
// same shape of denial as the historical wrong-source case. This is the
// "before APA-67 it was correct" baseline, asserted as a contract.
func TestAPA67_UnknownKindRunRejectionMatchesPreAPA67Behaviour(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	run := func(raw []byte) (InvestigationOutput, error) {
		t.Helper()
		fake := &FakeModelClient{Responses: []ModelResponse{modelResp(raw)}}
		return newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
	}

	unknown, unknownErr := run(rejectedUnknownKindBytes(t, env))
	wrongSource, wrongSourceErr := run(rejectedExternalBytes(t, env))

	for _, tc := range []struct {
		name string
		out  InvestigationOutput
		err  error
	}{
		{"unknown kind", unknown, unknownErr},
		{"wrong external source", wrongSource, wrongSourceErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.out.Outcome != OutcomeEscalated {
				t.Fatalf("Outcome = %q (err=%v), want ESCALATED", string(tc.out.Outcome), tc.err)
			}
			if tc.out.EscalationReason != EscalationInvalidOutput {
				t.Fatalf("EscalationReason = %q, want INVALID_OUTPUT", string(tc.out.EscalationReason))
			}
			if !errors.Is(tc.err, ErrModelContract) {
				t.Fatalf("err = %v, want ErrModelContract", tc.err)
			}
			if tc.out.RejectionRecord == nil {
				t.Fatal("no rejection_record; the denial was discarded")
			}
			if verr := ValidateInvestigationOutput(tc.out); verr != nil {
				t.Fatalf("ValidateInvestigationOutput: %v", verr)
			}
			if tc.out.Report != nil {
				t.Fatal("ESCALATED delivered a Report; the rejection must stay terminal")
			}
		})
	}
	// The two denials differ only in which additive item is offending, so
	// the record shape must agree. Guarded: the subtests above already
	// report a lost record, and this must not mask them with a panic.
	if unknown.RejectionRecord != nil && wrongSource.RejectionRecord != nil {
		if unknown.RejectionRecord.State != wrongSource.RejectionRecord.State {
			t.Fatalf("record state differs between two same-class denials: %q vs %q",
				unknown.RejectionRecord.State, wrongSource.RejectionRecord.State)
		}
		if len(unknown.RejectionRecord.Additive) != len(wrongSource.RejectionRecord.Additive) {
			t.Fatalf("additive arity differs between two same-class denials: %d vs %d",
				len(unknown.RejectionRecord.Additive), len(wrongSource.RejectionRecord.Additive))
		}
	}
}

// ---------------------------------------------------------------------------
// AC2 — builder unit: unknown kind represented, string dropped
// ---------------------------------------------------------------------------

// TestAPA67_BuilderRecordsOutOfVocabularyKindWithoutRetainingIt exercises the
// builder directly across the shapes an out-of-vocabulary kind arrives in.
func TestAPA67_BuilderRecordsOutOfVocabularyKindWithoutRetainingIt(t *testing.T) {
	const (
		personMarker = "TEST_PERSON_001"
		secretMarker = "TEST_SECRET_CLAIMOPS_123456"
	)
	cases := []struct {
		name string
		// additive is the model's submitted list, verbatim.
		additive []invest.MissingItem
		// wantKinds is the retained kind per entry; "" means an
		// out-of-vocabulary kind was dropped.
		wantKinds []string
	}{
		{
			name: "single unknown kind",
			additive: []invest.MissingItem{
				{Kind: unknownKindMarkerKind, Key: "admission_date", Detail: "d"},
			},
			wantKinds: []string{""},
		},
		{
			// Worst case for a naive retention: the offending kind is
			// itself the secret.
			name: "unknown kind carrying a PII marker",
			additive: []invest.MissingItem{
				{Kind: invest.MissingKind(personMarker), Key: secretMarker, Detail: "d"},
			},
			wantKinds: []string{""},
		},
		{
			// An unknown kind must not inherit the external-key retention
			// rule, even when the key is one of the four compile-time
			// literals.
			name: "unknown kind wearing a closed-vocabulary key",
			additive: []invest.MissingItem{
				{Kind: unknownKindMarkerKind, Key: "policy", Detail: "d"},
			},
			wantKinds: []string{""},
		},
		{
			name: "unknown kind with blank key and detail",
			additive: []invest.MissingItem{
				{Kind: invest.MissingKind(""), Key: "  ", Detail: ""},
			},
			wantKinds: []string{""},
		},
		{
			// Positional information must survive: each entry is kept so
			// the arity and order of the submitted list stay diagnosable.
			name: "mixed known and unknown kinds keep arity and order",
			additive: []invest.MissingItem{
				{Kind: invest.MissingRequiredDocument, Key: "CLAIM_FORM", Detail: "d"},
				{Kind: unknownKindMarkerKind, Key: "k", Detail: "d"},
				{Kind: invest.MissingExternal, Key: "policy", Detail: "d"},
				{Kind: invest.MissingKind(personMarker), Key: "k2", Detail: "d"},
				{Kind: invest.MissingField, Key: "admission_date", Detail: "d"},
			},
			wantKinds: []string{"required_document", "", "external", "", "field"},
		},
		{
			name: "every kind unknown",
			additive: []invest.MissingItem{
				{Kind: invest.MissingKind("one"), Key: "a", Detail: "d"},
				{Kind: invest.MissingKind("two"), Key: "b", Detail: "d"},
				{Kind: invest.MissingKind("three"), Key: "c", Detail: "d"},
			},
			wantKinds: []string{"", "", ""},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := testReport(invest.UnresolvedException{})
			rep.MissingAdditive = tc.additive
			rec := buildRejectionRecord(ModelAction{Action: ActionSubmitReport, Report: &rep}, invalidReport)
			if rec == nil {
				t.Fatal("buildRejectionRecord returned nil for a rejected submit_report")
			}
			// The core invariant: valid by construction.
			if err := ValidateRejectionRecord(rec); err != nil {
				t.Fatalf("record built from model input does not validate (%v): "+
					"it invalidated the escalation that carried it", err)
			}
			if !rejectionRecordIsContentFree(rec) {
				t.Fatalf("record is not content-free: %+v", rec)
			}
			if len(rec.Additive) != len(tc.wantKinds) {
				t.Fatalf("additive = %d entries, want %d (items must be represented, not dropped)",
					len(rec.Additive), len(tc.wantKinds))
			}
			wire := apa67WireAdditiveOf(t, rec.Additive)
			for i, want := range tc.wantKinds {
				if got := wire[i].Kind; got != want {
					t.Fatalf("additive[%d].kind = %q, want %q", i, got, want)
				}
				if want == "" && wire[i].KindInVocabulary {
					t.Fatalf("additive[%d] claims closed vocabulary for a dropped kind", i)
				}
				if want != "" && !wire[i].KindInVocabulary {
					t.Fatalf("additive[%d] retained kind %q without the vocabulary flag", i, want)
				}
			}
			// The offending strings must be absent from the MARSHALLED
			// record, which is what actually travels to the next stage.
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
			for _, m := range []string{unknownKindMarker, personMarker, secretMarker} {
				if strings.Contains(string(raw), m) {
					t.Fatalf("marshalled record retains model-authored text %q: %s", m, raw)
				}
			}
		})
	}
}

// TestAPA67_BuilderKeepsKnownKindsFlaggedAndBehaviourUnchanged guards against
// over-reach: a record built from a report whose kinds are all in vocabulary
// must be unchanged by this fix -- every kind retained and flagged true. This
// is the do-not-weaken direction.
func TestAPA67_BuilderKeepsKnownKindsFlaggedAndBehaviourUnchanged(t *testing.T) {
	rep := testReport(invest.UnresolvedException{})
	rep.MissingAdditive = []invest.MissingItem{
		{Kind: invest.MissingRequiredDocument, Key: "CLAIM_FORM", Detail: "d"},
		{Kind: invest.MissingField, Key: "admission_date", Detail: "d"},
		{Kind: invest.MissingExternal, Key: "policy", Detail: "d"},
	}
	rec := buildRejectionRecord(ModelAction{Action: ActionSubmitReport, Report: &rep}, invalidReport)
	if err := ValidateRejectionRecord(rec); err != nil {
		t.Fatalf("ValidateRejectionRecord: %v", err)
	}
	want := []string{"required_document", "field", "external"}
	if len(rec.Additive) != len(want) {
		t.Fatalf("additive = %d entries, want %d", len(rec.Additive), len(want))
	}
	wire := apa67WireAdditiveOf(t, rec.Additive)
	for i, w := range want {
		if wire[i].Kind != w {
			t.Fatalf("additive[%d].kind = %q, want %q", i, wire[i].Kind, w)
		}
		if !wire[i].KindInVocabulary {
			t.Fatalf("additive[%d] retained kind %q without the vocabulary flag", i, w)
		}
	}
	// The pre-existing key rule is untouched by the kind change.
	if wire[2].Key != "policy" || !wire[2].KeyInVocabulary {
		t.Fatalf("external closed-vocabulary key rule regressed: %+v", wire[2])
	}
	if wire[0].Key != "" || wire[1].Key != "" {
		t.Fatalf("model-authored key retention rule regressed: %+v", wire)
	}
}

// ---------------------------------------------------------------------------
// AC3 — the flag rule fails closed, and the key rule keeps direct coverage
// ---------------------------------------------------------------------------

// TestAPA67_KindVocabularyFlagFailsClosed proves the kind flag is a
// constraint, not a bypass. Each contradiction between the flag and the
// retained kind is refused, and the pre-existing KEY rules are re-proved HERE
// with a correctly flagged kind, so they keep direct coverage independent of
// the order in which the kind rule happens to trip.
//
// Driven through DecodeOutput so it is also the untrusted-wire case: a record
// arriving from another stage with a self-contradictory additive entry.
func TestAPA67_KindVocabularyFlagFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		// entry is the additive element as it appears on the wire.
		entry    string
		wantErr  string
		wantPass bool
	}{
		{
			// THE representation of an out-of-vocabulary kind: the entry
			// survives, the string does not. This must validate.
			name:     "dropped kind with the flag false is valid",
			entry:    `{"kind_in_vocabulary":false,"key_in_vocabulary":false}`,
			wantPass: true,
		},
		{
			name:     "closed kind with the flag true is valid",
			entry:    `{"kind":"external","kind_in_vocabulary":true,"key":"policy","key_in_vocabulary":true}`,
			wantPass: true,
		},
		{
			// The flag cannot launder an unknown kind.
			name:    `unknown kind claiming the vocabulary flag`,
			entry:   `{"kind":"guess","kind_in_vocabulary":true,"key_in_vocabulary":false}`,
			wantErr: "unknown kind",
		},
		{
			// Model text cannot ride along behind a false flag.
			name:    "unknown kind smuggled past the flag",
			entry:   `{"kind":"guess","kind_in_vocabulary":false,"key_in_vocabulary":false}`,
			wantErr: "closed-vocabulary flag",
		},
		{
			name:    "PII marker kind smuggled past the flag",
			entry:   `{"kind":"TEST_PERSON_001","kind_in_vocabulary":false,"key_in_vocabulary":false}`,
			wantErr: "closed-vocabulary flag",
		},
		{
			// The key rules, now reached directly because the kind is
			// correctly flagged. Coverage the sibling table loses to check
			// ordering.
			name:    "vocabulary key flag with a model key",
			entry:   `{"kind":"external","kind_in_vocabulary":true,"key":"policy_number","key_in_vocabulary":true}`,
			wantErr: "closed-vocabulary key",
		},
		{
			name:    "model key smuggled past the key flag",
			entry:   `{"kind":"field","kind_in_vocabulary":true,"key":"TEST_PERSON_001","key_in_vocabulary":false}`,
			wantErr: "without the closed-vocabulary flag",
		},
		{
			name:    "closed key on a non-external kind",
			entry:   `{"kind":"field","kind_in_vocabulary":true,"key":"policy","key_in_vocabulary":true}`,
			wantErr: "closed-vocabulary key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, err := apa67DecodeRecord(t, tc.entry)
			if tc.wantPass {
				if err != nil {
					t.Fatalf("legal record refused at the wire boundary: %v", err)
				}
				if verr := ValidateRejectionRecord(rec); verr != nil {
					t.Fatalf("legal record rejected standalone: %v", verr)
				}
				// And it must re-encode without smuggling any string the
				// decoded form did not legitimately carry: a dropped kind
				// stays dropped across the round trip.
				raw, merr := MarshalOutput(InvestigationOutput{
					InvestigationID:  tInvID,
					Outcome:          OutcomeEscalated,
					EscalationReason: EscalationInvalidOutput,
					RejectionRecord:  rec,
					AttemptLog:       []TurnRecord{},
				})
				if merr != nil {
					t.Fatalf("MarshalOutput rejected a legal record: %v", merr)
				}
				back, berr := apa67RecordWireOf(t, raw)
				if berr != nil {
					t.Fatalf("re-decode marshalled escalation: %v", berr)
				}
				if len(back) != 1 {
					t.Fatalf("re-encoded additive = %d entries, want 1", len(back))
				}
				if (back[0].Kind == "") != !back[0].KindInVocabulary {
					t.Fatalf("re-encoded additive[0] = %+v: kind %q disagrees with flag %v",
						back[0], back[0].Kind, back[0].KindInVocabulary)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted: the record must fail closed on %q", tc.name)
			}
			if !errors.Is(err, ErrModelContract) {
				t.Fatalf("err = %v, want ErrModelContract", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("rejected for the wrong reason: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// AC4 — the general rule: valid by construction for arbitrary model input
// ---------------------------------------------------------------------------

// TestAPA67_RecordIsValidByConstructionForArbitraryModelInput is the
// whole-file statement of the governing rule, expressed as a property over the
// one entry point a rejected act can take. Every report shape a decoded act can
// carry must yield a record that validates and is content-free, so the record
// can never invalidate its own escalation.
//
// Deliberately a property over many shapes rather than one fixture: the bug
// class is "a model-authored string reached a closed-vocabulary field", and it
// recurs one field at a time unless the invariant itself is tested.
func TestAPA67_RecordIsValidByConstructionForArbitraryModelInput(t *testing.T) {
	// Nothing here is a closed-vocabulary member; each shape attacks a
	// different field of AdditiveRef at once.
	const (
		markerA = "TEST_PERSON_001"
		markerB = "TEST_SECRET_CLAIMOPS_123456"
	)
	shapes := []struct {
		name     string
		additive []invest.MissingItem
	}{
		{"all fields model-authored at once", []invest.MissingItem{
			{Kind: invest.MissingKind(markerA), Key: markerB, Detail: markerA},
		}},
		{"unknown kind, known kinds interleaved", []invest.MissingItem{
			{Kind: invest.MissingKind(markerA), Key: "a", Detail: "d"},
			{Kind: invest.MissingField, Key: "b", Detail: "d"},
			{Kind: invest.MissingKind(markerB), Key: "c", Detail: "d"},
		}},
		{"external kind with a model key and unknown neighbours", []invest.MissingItem{
			{Kind: invest.MissingExternal, Key: markerB, Detail: "d"},
			{Kind: invest.MissingKind(markerA), Key: "policy", Detail: "d"},
			{Kind: invest.MissingExternal, Key: "risk", Detail: "d"},
		}},
		{"kind case-mangled out of vocabulary", []invest.MissingItem{
			{Kind: invest.MissingKind("External"), Key: "policy", Detail: "d"},
			{Kind: invest.MissingKind("FIELD"), Key: "x", Detail: "d"},
		}},
		{"every additive item out of vocabulary", []invest.MissingItem{
			{Kind: invest.MissingKind(markerA), Key: "a", Detail: "d"},
			{Kind: invest.MissingKind(markerB), Key: "b", Detail: "d"},
			{Kind: invest.MissingKind("   "), Key: "", Detail: ""},
		}},
	}
	for _, s := range shapes {
		t.Run(s.name, func(t *testing.T) {
			rep := testReport(invest.UnresolvedException{})
			rep.MissingAdditive = s.additive
			rec := buildRejectionRecord(ModelAction{Action: ActionSubmitReport, Report: &rep}, invalidReport)
			if rec == nil {
				t.Fatal("buildRejectionRecord returned nil for a rejected submit_report")
			}
			if err := ValidateRejectionRecord(rec); err != nil {
				t.Fatalf("record from arbitrary model input does not validate: %v", err)
			}
			if !rejectionRecordIsContentFree(rec) {
				t.Fatalf("record from arbitrary model input is not content-free: %+v", rec)
			}
			// Counts are the only model-shaped numbers retained, and they
			// are lengths: never negative, never text.
			if rec.HypothesisCount != len(rep.Hypotheses) || rec.FindingCount != len(rep.Findings) {
				t.Fatalf("counts = %d/%d, want %d/%d",
					rec.HypothesisCount, rec.FindingCount, len(rep.Hypotheses), len(rep.Findings))
			}
			if rec.HypothesisCount < 0 || rec.FindingCount < 0 {
				t.Fatalf("record carries a negative count: %+v", rec)
			}
			// Action and InvalidKind are this package's literals.
			if rec.Action != ActionSubmitReport {
				t.Fatalf("action = %q, want submit_report", rec.Action)
			}
			if !validInvalidKindString(rec.InvalidKind) {
				t.Fatalf("invalid_kind %q is not a closed I-class code", rec.InvalidKind)
			}
			// Nothing model-authored survives the wire.
			out := InvestigationOutput{
				InvestigationID:  tInvID,
				Outcome:          OutcomeEscalated,
				EscalationReason: EscalationInvalidOutput,
				RejectionRecord:  rec,
				AttemptLog:       []TurnRecord{},
			}
			if verr := ValidateInvestigationOutput(out); verr != nil {
				t.Fatalf("ValidateInvestigationOutput: %v", verr)
			}
			raw, merr := MarshalOutput(out)
			if merr != nil {
				t.Fatalf("MarshalOutput: %v", merr)
			}
			for _, m := range []string{markerA, markerB} {
				if strings.Contains(string(raw), m) {
					t.Fatalf("escalation retains model-authored text %q: %s", m, raw)
				}
			}
		})
	}
}

// TestAPA67_NoModelAuthoredStringReachesAnyRecordField enumerates every string
// field of the record and asserts that a record built from a maximally
// adversarial submission holds no submitted string in ANY of them. Field-wise
// rather than fixture-wise, so adding a new model-derived string field without
// auditing it fails here rather than in production.
func TestAPA67_NoModelAuthoredStringReachesAnyRecordField(t *testing.T) {
	const marker = "TEST_SECRET_CLAIMOPS_123456"
	rep := testReport(invest.UnresolvedException{})
	rep.Hypotheses[0].Statement = "statement for " + marker
	rep.Hypotheses[0].Falsifier = "falsifier for " + marker
	rep.Findings[0].Summary = "summary for " + marker
	rep.Recommendation.Rationale = "rationale for " + marker
	rep.MissingAdditive = []invest.MissingItem{
		{Kind: invest.MissingKind(marker), Key: marker, Detail: marker},
	}
	rec := buildRejectionRecord(ModelAction{Action: ActionSubmitReport, Report: &rep}, invalidReport)
	if err := ValidateRejectionRecord(rec); err != nil {
		t.Fatalf("ValidateRejectionRecord: %v", err)
	}
	wire := apa67WireAdditiveOf(t, rec.Additive)
	if len(wire) != 1 {
		t.Fatalf("additive = %d entries, want 1", len(wire))
	}
	// Every string-valued field, named. A new model-derived string field
	// must be added here, which is the point of writing it out.
	fields := map[string]string{
		"State":            string(rec.State),
		"Action":           rec.Action,
		"InvalidKind":      rec.InvalidKind,
		"Additive[0].Kind": wire[0].Kind,
		"Additive[0].Key":  wire[0].Key,
	}
	for name, got := range fields {
		if got == marker {
			t.Fatalf("model-authored text reached record field %s", name)
		}
	}
	// And the whole marshalled escalation, as the boundary check.
	raw, merr := MarshalOutput(InvestigationOutput{
		InvestigationID:  tInvID,
		Outcome:          OutcomeEscalated,
		EscalationReason: EscalationInvalidOutput,
		RejectionRecord:  rec,
		AttemptLog:       []TurnRecord{},
	})
	if merr != nil {
		t.Fatalf("MarshalOutput: %v", merr)
	}
	if strings.Contains(string(raw), marker) {
		t.Fatalf("marshalled escalation retains model-authored text %q: %s", marker, raw)
	}
}
