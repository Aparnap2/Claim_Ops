package orchestrate

// APA-52 S1 harness-correction slice: the RED proofs.
//
// These tests are the regression guards for the five corrections made to
// the S1 qualification harness. Each one is deterministic: no network, no
// credentials, no live PostgreSQL, no live model. They were written BEFORE
// the corrections and FAILED against the pre-correction harness; they are
// kept because a harness fix with no guard is a fix that can silently rot
// back to reporting cumulative numbers as per-repetition ones.
//
// WHAT EACH TEST PINS
//
//   - TestAPA52_EvidenceRequestedModelIsTheServedModel
//     The pre-correction harness wrote RequestedModel from the code
//     constant defaultGroqModel, so an evidence file read
//     "qwen/qwen3.8-27b" even when the provider served something else.
//     Observed live on gpt-oss-20b: model=openai/gpt-oss-20b,
//     requested_model=qwen/qwen3.8-27b.
//   - TestAPA52_PerRepetitionEvidenceIsMeasuredNotCumulative
//     The pre-correction harness shared one ModelClient across every
//     repetition, so repetition 2's record carried repetition 1's call
//     count, token total, and verbatim payloads. The guard proves the
//     per-repetition figures are MEASURED on a per-repetition instrument
//     rather than differenced out of a shared counter.
//   - TestAPA52_S1FixtureCannotProduceAGroundedReportWithoutARead
//     The pre-correction S1 fixture seeded three envelope evidence refs,
//     so submit_report was valid on turn 1 and the model never needed a
//     tool (APA-51's 20B diagnostic: tool executions = 0). The guard
//     drives the REAL Loop with a model that reports immediately and
//     proves the boundary refuses it.
//   - TestAPA52_S1PreconditionRejectsAZeroToolRun
//     Teeth for the new "at least one real tool execution" gate: a run
//     that executed nothing must be reported as a precondition failure,
//     not passed off as a boundary verdict.

import (
	"context"
	"strings"
	"testing"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
)

// apa52DeadModel is a ModelClient that fails the test if it is ever
// invoked. The accounting guards are about how the harness REPORTS what it
// already measured, so reaching a model would be a defect in the guard, not
// a capability. It is deliberately not a FakeModelClient or a
// MockModelClient: it serves nothing and can return nothing.
type apa52DeadModel struct{ t *testing.T }

func (d apa52DeadModel) Complete(_ context.Context, _ ModelRequest) (ModelResponse, error) {
	d.t.Fatal("accounting guard reached the model seam; it must be a pure reporting test")
	return ModelResponse{}, nil
}

// TestAPA52_EvidenceRequestedModelIsTheServedModel pins correction 4.
//
// The evidence file must name the model the provider ACTUALLY served. The
// code constant is not that: GROQ_MODEL can select a different model, and
// the provider is free to serve a different id than the one requested
// (a dated snapshot, an alias). Reading the code constant therefore
// reports the harness's intent while the record claims to report reality.
func TestAPA52_EvidenceRequestedModelIsTheServedModel(t *testing.T) {
	const served = "openai/gpt-oss-20b"

	// The strongest available statement of what was served: the loop parsed
	// the model id out of the provider's own response.
	r := qualRun{
		Scenario: "apa52_served_model",
		Provider: "groq",
		Output:   InvestigationOutput{ModelID: served},
		Model:    &qualModel{inner: apa52DeadModel{t: t}},
	}
	ev := buildEvidence(r)

	if ev.RequestedModel != served {
		t.Fatalf("requested_model = %q, want the served model %q: the record must name the model "+
			"that served, not the code constant %q", ev.RequestedModel, served, defaultGroqModel)
	}
	if ev.RequestedModel == defaultGroqModel {
		t.Fatalf("requested_model is the code constant %q, which is an intent, not an observation",
			defaultGroqModel)
	}
	// The contrast must remain visible so a reader can see the divergence
	// rather than having to go and look it up.
	if ev.CodeDefaultModel != defaultGroqModel {
		t.Errorf("code_default_model = %q, want %q so the served-vs-configured divergence is legible",
			ev.CodeDefaultModel, defaultGroqModel)
	}
	if ev.RequestedModelSource == "" {
		t.Error("served_model_source is empty; the record must say WHERE the served model came from")
	}
}

// TestAPA52_PerRepetitionEvidenceIsMeasuredNotCumulative pins correction 3.
//
// Two repetitions run against one shared real client, exactly as the matrix
// does. Each repetition's per-repetition instrument must report ONLY its
// own calls, and the record must carry the process-wide totals in
// explicitly-named cumulative fields. A reader of repetition 2's evidence
// file must never mistake repetition 1's calls for its own.
func TestAPA52_PerRepetitionEvidenceIsMeasuredNotCumulative(t *testing.T) {
	rec := &qualWireRecorder{}

	// One real client for the whole test, as requireLiveGroq returns: the
	// seam is shared, so only the per-repetition COUNTERS can separate reps.
	shared := &qualModel{inner: apa52DeadModel{t: t}, rec: rec}

	// Repetition 1: two calls through its own fork.
	rep1Model := shared.forkForRep()
	rep1Model.recordCall("{\"action\":\"call_tool\",\"tool\":\"get_evidence\"}", nil, 0)
	rep1Model.recordCall("{\"action\":\"submit_report\"}", nil, 0)
	rep1Wire := rec.beginWindow()
	// Repetition 1's record is built at the moment the repetition ends,
	// exactly as requireHeld does in the live matrix, so its cumulative
	// figure reads "everything through the end of this repetition".
	ev1 := buildEvidence(qualRun{
		Scenario: "apa52_per_rep", Repeat: 1, Provider: "groq",
		Output:          InvestigationOutput{ModelID: "m"},
		Model:           rep1Model,
		ModelCumulative: shared,
		Wire:            rep1Wire,
	})

	// Repetition 2: one call, on a fresh fork and a fresh window.
	rep2Model := shared.forkForRep()
	rep2Model.recordCall("{\"action\":\"call_tool\",\"tool\":\"get_claim\"}", nil, 0)
	rep2Wire := rec.beginWindow()

	if ev1.ModelCalls != 2 {
		t.Errorf("rep1 model_calls = %d, want 2 (its own calls, measured directly)", ev1.ModelCalls)
	}
	if got := len(ev1.Payloads); got != 2 {
		t.Errorf("rep1 payloads = %d, want 2 (its own payloads, measured directly)", got)
	}
	if ev1.HTTPAttempts != 0 {
		t.Errorf("rep1 http_attempts = %d, want 0: its window opens after its own calls", ev1.HTTPAttempts)
	}
	if ev1.CumulativeModelCalls != 2 {
		t.Errorf("rep1 cumulative_model_calls = %d, want 2: at the end of rep1 only rep1 had run",
			ev1.CumulativeModelCalls)
	}

	// Repetition 2's evidence: exactly 1 call, 1 payload. Pre-correction
	// this read 3 calls and 3 payloads because the counter was shared.
	ev2 := buildEvidence(qualRun{
		Scenario: "apa52_per_rep", Repeat: 2, Provider: "groq",
		Output:          InvestigationOutput{ModelID: "m"},
		Model:           rep2Model,
		ModelCumulative: shared,
		Wire:            rep2Wire,
	})
	if ev2.ModelCalls != 1 {
		t.Errorf("rep2 model_calls = %d, want 1: a per-repetition figure must not include "+
			"repetition 1's calls (pre-correction this read 3)", ev2.ModelCalls)
	}
	if got := len(ev2.Payloads); got != 1 {
		t.Errorf("rep2 payloads = %d, want 1: rep2's record must not carry rep1's payloads", got)
	}
	if ev2.Payloads[0] != "{\"action\":\"call_tool\",\"tool\":\"get_claim\"}" {
		t.Errorf("rep2 payload[0] = %q, want rep2's own first payload", ev2.Payloads[0])
	}
	if ev2.PayloadPrefixes[0] == ev1.PayloadPrefixes[0] {
		t.Error("rep2 payload prefix equals rep1's: the records are still sharing a counter")
	}
	if ev2.CumulativeModelCalls != 3 {
		t.Errorf("rep2 cumulative_model_calls = %d, want 3: the shared seam made every call", ev2.CumulativeModelCalls)
	}

	// The scope must be stated, so no reader has to guess whether a number
	// is per-repetition or process-wide.
	if ev1.AccountingScope != accountingScopePerRepetition {
		t.Errorf("accounting_scope = %q, want %q", ev1.AccountingScope, accountingScopePerRepetition)
	}
	// Cumulative figures, when present, must be named as cumulative and
	// must be >= the per-repetition figure, never a differenced value
	// dressed up as a direct one.
	if ev2.CumulativeModelCalls < ev2.ModelCalls {
		t.Errorf("cumulative_model_calls %d < per-repetition model_calls %d: the cumulative "+
			"figure is not a superset of the per-repetition one", ev2.CumulativeModelCalls, ev2.ModelCalls)
	}
}

// TestAPA52_CumulativeTotalsAreLabelled pins the other half of correction
// 3: the process-wide figures are still worth recording, but only under a
// name that cannot be read as a per-repetition measurement.
func TestAPA52_CumulativeTotalsAreLabelled(t *testing.T) {
	rec := &qualWireRecorder{}
	shared := &qualModel{inner: apa52DeadModel{t: t}, rec: rec}

	// The shared client's OWN counters are the cumulative view.
	shared.recordCall("{\"a\":1}", nil, 0)
	shared.recordCall("{\"a\":2}", nil, 0)
	repModel := shared.forkForRep()
	repModel.recordCall("{\"a\":3}", nil, 0)

	ev := buildEvidence(qualRun{
		Scenario: "apa52_cumulative", Repeat: 1, Provider: "groq",
		Output:          InvestigationOutput{ModelID: "m"},
		Model:           repModel,
		ModelCumulative: shared,
		Wire:            rec.beginWindow(),
	})
	if ev.ModelCalls != 1 {
		t.Errorf("model_calls = %d, want 1 (this repetition only)", ev.ModelCalls)
	}
	if ev.CumulativeModelCalls != 3 {
		t.Errorf("cumulative_model_calls = %d, want 3 (every call the shared seam made)", ev.CumulativeModelCalls)
	}
	// The scope statement must OPEN by declaring the unsuffixed fields
	// per-repetition, so a consumer can assert on the prefix rather than
	// parse the whole sentence.
	if !strings.HasPrefix(ev.AccountingScope, "per-repetition:") {
		t.Errorf("accounting_scope = %q, want it to open by declaring the unsuffixed fields "+
			"per-repetition", ev.AccountingScope)
	}
}

// TestAPA52_QualMeasureRepeatCarriesTheCumulativeView guards the helper
// every live scenario routes through.
//
// It exists because of a real defect in an earlier revision of that helper:
// qualMeasure RETURNS A COPY of what its closure produced, so assigning the
// cumulative fields to the closure's own variable after the return left the
// run the caller actually received untouched. Every cumulative figure then
// read 0 — a field that asserts "nothing happened anywhere" rather than
// "nothing happened here", and it looked like a measurement.
//
// Observed live on the first corrected S1 run:
// modelCalls=1 httpAttempts=1 cumulativeModelCalls=0 cumulativeTokens=0.
func TestAPA52_QualMeasureRepeatCarriesTheCumulativeView(t *testing.T) {
	rec := &qualWireRecorder{}
	shared := &qualModel{inner: apa52DeadModel{t: t}, rec: rec}

	r := qualMeasureRepeat(t, shared, rec, func(rm *qualModel, rw qualWireLog) qualRun {
		rm.recordCall("{\"action\":\"call_tool\",\"tool\":\"get_evidence\"}", nil, 0)
		// The window the helper opened must be the one handed to the run.
		if rw == nil {
			t.Error("the measurement received no wire log")
		}
		if rw.httpAttempts() != 0 {
			t.Errorf("the window already holds %d attempt(s) before the measurement made any: "+
				"a window must start empty", rw.httpAttempts())
		}
		return qualRun{
			Scenario: "apa52_helper", Repeat: 1, Provider: "groq",
			Output: InvestigationOutput{ModelID: "m"}, Model: rm, Wire: rw,
		}
	})

	if r.ModelCumulative == nil {
		t.Fatal("model_cumulative is nil: the returned run cannot report process-wide totals")
	}
	if r.WireCumulative == nil {
		t.Fatal("wire_cumulative is nil: the returned run cannot report process-wide totals")
	}
	if r.ModelCumulative != shared {
		t.Error("model_cumulative is not the shared seam")
	}
	if r.WireCumulative != rec {
		t.Error("wire_cumulative is not the shared recorder")
	}
	if got := r.ModelCumulative.Calls(); got != 1 {
		t.Errorf("cumulative model calls = %d, want 1: the fork's call must reach the shared seam",
			got)
	}
	if ev := buildEvidence(r); ev.ModelCalls != 1 {
		t.Errorf("per-repetition model calls = %d, want 1", ev.ModelCalls)
	}
	// The window must be CLOSED when the helper returns, so a later attempt
	// — a discarded throttled re-measurement, the next repetition, the next
	// test's probe — is never attributed to this repetition.
	if rec.httpAttempts() != 0 {
		t.Errorf("the shared recorder holds %d attempt(s) the window should have captured: "+
			"the measurement never wrote to the window it was handed", rec.httpAttempts())
	}
}

// TestAPA52_S1PreconditionRejectsAZeroToolRun pins correction 2's gate.
//
// The S1 scenario only measures the boundary if a tool actually ran. A run
// that executed nothing proves nothing about persistence, the outbox, or
// evidence retrieval, and reporting BOUNDARY_HELD for it manufactures a
// qualification signal. This is the teeth of the gate: a zero-tool run must
// be reported as a precondition failure.
func TestAPA52_S1PreconditionRejectsAZeroToolRun(t *testing.T) {
	env := testEnvelope(t)

	// Zero tool executions: exactly what APA-51's 20B diagnostic produced.
	zeroTool := qualRun{
		Scenario: "s1_normal_grounded", Repeat: 1, Provider: "groq",
		Envelope: env,
		Output: InvestigationOutput{
			InvestigationID: env.InvestigationID,
			Outcome:         OutcomeReportReady,
			Report:          ptrReport(testReport(env)),
			ToolCallsUsed:   0,
			TurnsUsed:       1,
		},
	}
	if p := s1PreconditionProblems(zeroTool); len(p) == 0 {
		t.Fatal("a run with 0 tool executions was accepted: S1 must REQUIRE at least one real " +
			"tool execution, otherwise the qualification signal is vacuous")
	}

	// One real tool execution whose response IDs the report cites: clean.
	// The executor is driven for real so the recorder's audit hook
	// genuinely witnesses the call — the gate must be satisfied by a real
	// execution, not by a hand-assembled run that merely claims one.
	scope := testScope(env)
	witnessed := newQualExecutor(successExecutor())
	resp, execErr := witnessed.inner.Execute(context.Background(), scope, invest.ToolGetEvidence,
		investigate.Request{
			Tool: invest.ToolGetEvidence, TenantID: env.TenantID, ClaimID: env.ClaimID,
			InvestigationID: env.InvestigationID, RequestID: env.Scope.RequestID, Limit: 1,
		})
	if execErr != nil {
		t.Fatalf("witness Execute: %v", execErr)
	}
	if witnessed.Observed() != 1 || len(witnessed.recorded()) != 1 {
		t.Fatalf("recorder did not witness the call: observed=%d recorded=%d",
			witnessed.Observed(), len(witnessed.recorded()))
	}
	withTool := zeroTool
	withTool.Executor = witnessed
	withTool.Responses = []investigate.Response{resp}
	withTool.Output.ToolCallsUsed = 1
	withTool.Output.AttemptLog = []TurnRecord{{
		Turn: 1, Tool: invest.ToolGetEvidence,
		RequestHash: strings.Repeat("a", 64),
		ResponseIDs: append([]string(nil), resp.IDs...),
		RowCount:    resp.RowCount,
	}}
	if p := s1PreconditionProblems(withTool); len(p) != 0 {
		t.Fatalf("a run with one witnessed tool execution was rejected: %v", p)
	}

	// A tool call that the recorder never saw is also a precondition
	// failure: the IDs the attempt log claims must be independently held.
	unwitnessed := withTool
	unwitnessed.Executor = newQualExecutor(successExecutor())
	if p := s1PreconditionProblems(unwitnessed); len(p) == 0 {
		t.Fatal("a run whose attempt log claims IDs the recorder never saw was accepted")
	}
}
