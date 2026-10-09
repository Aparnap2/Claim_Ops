package orchestrate

// APA-67 hardening — close two fail-closed gaps in the record contract.
//
// Follow-up to 1ad1de4 (owner review). Neither gap is a runtime failure: the
// loop supplies the record at exactly one call site (failWithRecord at
// loop.go, reached only with EscalationInvalidOutput), so no production run
// produces a mis-placed record today. What is missing is the CONTRACT check
// that would make such a run impossible rather than merely unobserved.
//
// Gap 1: ValidateRejectionRecord accepted ActionCallTool, even though
// buildRejectionRecord only ever builds records for submit_report (it returns
// nil for a call_tool denial -- a tool denial has no report to trace). The
// validator was therefore strictly more permissive than the builder, which is
// the fail-open direction.
//
// Gap 2: ValidateInvestigationOutput validated a supplied record but never
// required it to belong to an INVALID_OUTPUT escalation. A record attached to
// a DEADLINE (or any other) escalation passed validation, even though no such
// record can ever be built.
//
// Both fixes are additive-restrictive: they can only narrow what validates.
// Nothing is relaxed. The 11 pre-existing TestAPA67_* tests are untouched and
// must stay green -- this file proves the narrowing, it does not re-prove the
// original gap.
//
// Deterministic, no provider: pure validator calls, no Run, no network.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"claimops-api/internal/invest"
)

// apa67ValidRecord is the well-formed submit_report record every case here is
// derived from: REJECTED_WITH_EVIDENCE, one hypothesis, closed vocabulary.
// Mutating this one literal keeps the cases honest -- a case fails for the
// reason under test and not because its fixture drifted.
func apa67ValidRecord() *RejectionRecord {
	return &RejectionRecord{
		State:           RejectionStateRejected,
		Action:          ActionSubmitReport,
		InvalidKind:     "I5-report",
		HypothesisCount: 1,
		Additive:        []AdditiveRef{},
	}
}

// ---------------------------------------------------------------------------
// Gap 1 — the record describes a denied SUBMIT_REPORT, never a tool call
// ---------------------------------------------------------------------------

// TestAPA67_RecordRequiresSubmitReportAction pins the record's action to the
// single act buildRejectionRecord can produce.
//
// A call_tool denial loses no report, so there is nothing to trace and the
// builder emits no record at all. Accepting ActionCallTool in the validator
// admitted a state the builder cannot reach -- the diagnostic could claim a
// tool call was rejected "with evidence" that never existed. Fail closed on
// it.
func TestAPA67_RecordRequiresSubmitReportAction(t *testing.T) {
	cases := []struct {
		name    string
		action  string
		wantErr string
	}{
		{
			// The only action a real record can carry.
			name:   "submit_report is accepted",
			action: ActionSubmitReport,
		},
		{
			// The gap: a tool denial is never recorded, so a record
			// claiming one is fabricated.
			name:    "call_tool is rejected",
			action:  ActionCallTool,
			wantErr: `must be submit_report`,
		},
		{
			// Still unknown vocabulary, still closed.
			name:    "unknown action is rejected",
			action:  "write_claim",
			wantErr: `must be submit_report`,
		},
		{
			name:    "empty action is rejected",
			action:  "",
			wantErr: `must be submit_report`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := apa67ValidRecord()
			rec.Action = tc.action
			err := ValidateRejectionRecord(rec)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("action %q rejected: %v", tc.action, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("action %q accepted; only submit_report is recordable", tc.action)
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

// TestAPA67_CallToolRecordRejectedThroughTheOutputBoundary proves gap 1 is
// closed at the run boundary too, not only when the record validator is
// called directly: a mis-labelled record can never reach the wire.
func TestAPA67_CallToolRecordRejectedThroughTheOutputBoundary(t *testing.T) {
	rec := apa67ValidRecord()
	rec.Action = ActionCallTool
	out := InvestigationOutput{
		InvestigationID:  tInvID,
		Outcome:          OutcomeEscalated,
		EscalationReason: EscalationInvalidOutput,
		RejectionRecord:  rec,
		AttemptLog:       []TurnRecord{},
	}
	if err := ValidateInvestigationOutput(out); err == nil {
		t.Fatal("ESCALATED carrying a call_tool record was accepted")
	} else if !errors.Is(err, ErrModelContract) {
		t.Fatalf("err = %v, want ErrModelContract", err)
	}
	if _, err := MarshalOutput(out); err == nil {
		t.Fatal("MarshalOutput emitted a call_tool record; the wire must fail closed")
	}
}

// ---------------------------------------------------------------------------
// Gap 2 — a record belongs exclusively to an INVALID_OUTPUT escalation
// ---------------------------------------------------------------------------

// TestAPA67_RecordOnlyOnInvalidOutputEscalation pins where a record may live.
//
// buildRejectionRecord is reached from exactly one call site, which passes
// EscalationInvalidOutput. Every other exit class (budget, deadline,
// repetition, no-progress, upstream) is emitted with a nil record. So a
// non-nil record on any of those reasons is unreachable by construction, and
// validating it as legal only widens the fail-open surface: a future wiring
// bug could staple a denial trace onto a DEADLINE escalation and nothing
// downstream would object.
//
// The REPORT_READY row is the pre-existing rule, re-asserted here so this
// table covers the whole outcome space and the older check cannot regress
// unnoticed.
func TestAPA67_RecordOnlyOnInvalidOutputEscalation(t *testing.T) {
	cases := []struct {
		name    string
		out     InvestigationOutput
		wantErr string
	}{
		{
			// The positive case, retained: this is the one shape the
			// loop actually builds and it must keep passing.
			name: "invalid_output escalation accepts a record",
			out: InvestigationOutput{
				Outcome:          OutcomeEscalated,
				EscalationReason: EscalationInvalidOutput,
				RejectionRecord:  apa67ValidRecord(),
			},
		},
		{
			name: "deadline escalation rejects a record",
			out: InvestigationOutput{
				Outcome:          OutcomeEscalated,
				EscalationReason: EscalationDeadline,
				RejectionRecord:  apa67ValidRecord(),
			},
			wantErr: `rejection_record requires INVALID_OUTPUT`,
		},
		{
			name: "turns exhausted escalation rejects a record",
			out: InvestigationOutput{
				Outcome:          OutcomeEscalated,
				EscalationReason: EscalationTurnsExhausted,
				RejectionRecord:  apa67ValidRecord(),
			},
			wantErr: `rejection_record requires INVALID_OUTPUT`,
		},
		{
			name: "calls exhausted escalation rejects a record",
			out: InvestigationOutput{
				Outcome:          OutcomeEscalated,
				EscalationReason: EscalationCallsExhausted,
				RejectionRecord:  apa67ValidRecord(),
			},
			wantErr: `rejection_record requires INVALID_OUTPUT`,
		},
		{
			name: "repetition escalation rejects a record",
			out: InvestigationOutput{
				Outcome:          OutcomeEscalated,
				EscalationReason: EscalationRepetition,
				RejectionRecord:  apa67ValidRecord(),
			},
			wantErr: `rejection_record requires INVALID_OUTPUT`,
		},
		{
			name: "no_progress escalation rejects a record",
			out: InvestigationOutput{
				Outcome:          OutcomeEscalated,
				EscalationReason: EscalationNoProgress,
				RejectionRecord:  apa67ValidRecord(),
			},
			wantErr: `rejection_record requires INVALID_OUTPUT`,
		},
		{
			name: "model_upstream escalation rejects a record",
			out: InvestigationOutput{
				Outcome:          OutcomeEscalated,
				EscalationReason: EscalationModelUpstream,
				RejectionRecord:  apa67ValidRecord(),
			},
			wantErr: `rejection_record requires INVALID_OUTPUT`,
		},
		{
			name: "report_ready rejects a record (pre-existing rule)",
			out: func() InvestigationOutput {
				env := testEnvelope(t)
				rep := testReport(env)
				return InvestigationOutput{
					Outcome:         OutcomeReportReady,
					Report:          &rep,
					RejectionRecord: apa67ValidRecord(),
				}
			}(),
			wantErr: `REPORT_READY must not carry rejection_record`,
		},
		{
			// Absence stays legal everywhere: this narrowing must not
			// have made a record mandatory.
			name: "invalid_output escalation without a record stays valid",
			out: InvestigationOutput{
				Outcome:          OutcomeEscalated,
				EscalationReason: EscalationInvalidOutput,
			},
		},
		{
			name: "deadline escalation without a record stays valid",
			out: InvestigationOutput{
				Outcome:          OutcomeEscalated,
				EscalationReason: EscalationDeadline,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := tc.out
			out.InvestigationID = tInvID
			out.AttemptLog = []TurnRecord{}

			err := ValidateInvestigationOutput(out)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("rejected a legal output: %v", err)
				}
				// The positive cases must also survive the wire.
				if _, merr := MarshalOutput(out); merr != nil {
					t.Fatalf("MarshalOutput rejected a legal output: %v", merr)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted: %v", out)
			}
			if !errors.Is(err, ErrModelContract) {
				t.Fatalf("err = %v, want ErrModelContract", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("rejected for the wrong reason: %v", err)
			}
			// The run boundary fails closed too: nothing is emitted.
			if _, merr := MarshalOutput(out); merr == nil {
				t.Fatal("MarshalOutput emitted an output that validation rejected")
			}
		})
	}
}

// TestAPA67_MisplacedRecordRejectedThroughTheWireBoundary proves gap 2 is
// closed on the decode path as well. DecodeOutput validates, so a record
// carried across a stage boundary on a non-INVALID_OUTPUT escalation is
// refused on arrival rather than silently trusted.
func TestAPA67_MisplacedRecordRejectedThroughTheWireBoundary(t *testing.T) {
	// Hand-build the bytes rather than MarshalOutput, which already refuses
	// to produce them -- this is exactly the untrusted-input case: a record
	// riding in on a DEADLINE escalation from another stage.
	raw := []byte(`{"investigation_id":"` + tInvID +
		`","outcome":"ESCALATED","escalation_reason":"DEADLINE",` +
		`"rejection_record":{"state":"REJECTED_WITH_EVIDENCE","action":"submit_report",` +
		`"invalid_kind":"I5-report","hypothesis_count":1,"finding_count":0,"additive":[]},` +
		`"attempt_log":[],"turns_used":0,"tool_calls_used":0}`)

	if _, err := DecodeOutput(raw); err == nil {
		t.Fatal("DecodeOutput accepted a record on a DEADLINE escalation")
	} else if !errors.Is(err, ErrModelContract) {
		t.Fatalf("err = %v, want ErrModelContract", err)
	}
}

// TestAPA67_RealEscalationsStillCarryRecordsOrNothing guards the two gates
// against over-reach. Both are reachable shapes of a real run: the validator
// rejection keeps the record and drops Partial, while the grounding rejection
// keeps Partial and has no record to keep. Neither may be broken by this
// hardening, and neither may acquire a record it never had.
func TestAPA67_RealEscalationsStillCarryRecordsOrNothing(t *testing.T) {
	t.Run("validator rejection keeps the record", func(t *testing.T) {
		env := testEnvelope(t)
		scope := testScope(env)
		fake := &FakeModelClient{Responses: []ModelResponse{
			modelResp(rejectedExternalBytes(t, env)),
		}}
		out, err := newTestLoop(t, fake, successExecutor(), scope, env).Run(context.Background())
		if !errors.Is(err, ErrModelContract) {
			t.Fatalf("err = %v, want ErrModelContract", err)
		}
		if out.EscalationReason != EscalationInvalidOutput {
			t.Fatalf("Reason = %q, want %q", out.EscalationReason, EscalationInvalidOutput)
		}
		if out.RejectionRecord == nil {
			t.Fatal("validator rejection lost its record; the narrowing changed the real path")
		}
		if err := ValidateRejectionRecord(out.RejectionRecord); err != nil {
			t.Fatalf("the loop's own record no longer validates: %v", err)
		}
		if err := ValidateInvestigationOutput(out); err != nil {
			t.Fatalf("ValidateInvestigationOutput: %v", err)
		}
		if _, err := MarshalOutput(out); err != nil {
			t.Fatalf("MarshalOutput: %v", err)
		}
	})

	t.Run("grounding rejection keeps partial and no record", func(t *testing.T) {
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
		if out.RejectionRecord != nil {
			t.Fatalf("grounding rejection gained a record: %+v", out.RejectionRecord)
		}
		if err := ValidateInvestigationOutput(out); err != nil {
			t.Fatalf("ValidateInvestigationOutput: %v", err)
		}
	})
}
