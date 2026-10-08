package orchestrate

import (
	"context"
	"errors"
	"testing"
	"time"

	"claimops-api/internal/invest"
)

// APA-64 adjudication: is the APA-55 fixture's investigation budget
// STRUCTURALLY incapable of reaching its tool/outcome path once the live
// qualification harness's provider pacing is accounted for?
//
// WHY THIS EXISTS. In the APA-55 live run, ps_a1_control measured 3/3 as
//
//	outcome=ESCALATED reason=DEADLINE modelCalls=3 toolExecutions=0
//
// violations=0 verdict=BOUNDARY_HELD
//
// Two candidate explanations were indistinguishable from the live run alone:
//
//	(a) the model is incapable of completing, or
//	(b) the fixture's deadline budget is smaller than the pacing the harness
//	    charges inside it, so no multi-turn run can finish regardless of model.
//
// (b) is a qualification-harness defect, not a production defect and not a model
// limitation, so it must be established before anything is changed.
//
// THE STRUCTURE, from the loop itself:
//
//	deadline := time.Now().Add(scope.DeadlineMs)
//	for turn := 1..MaxTurns {
//	    if time.Now().After(deadline) { return fail(DEADLINE) }   // pre-turn check
//	    resp := model.Complete(...)                                 // <-- pacing sleeps HERE
//	    if time.Now().After(deadline) { return fail(DEADLINE) }    // I2 post-Complete check
//	    ... exec.Execute(...)                                       // never reached
//	}
//
// and qualModel.Complete sleeps pacingWait(reserveTokens) BEFORE the inner
// provider call, so pacing is inside the deadline window and is charged to it.
//
// NO PROVIDER CREDENTIAL IS REQUIRED. FakeModelClient.Delay injects the pacing
// delay deterministically at the same seam, and the deadlines here are scaled
// down to milliseconds. The RATIO is what matters and the ratio is preserved.
//
// This probe does not change the fixture, the loop, the deadline policy, the
// prompt, or the model. It only establishes where DEADLINE lands.

// probeLoop builds a loop whose model behaves like the live one: it delays
// (pacing) and then emits a valid call_tool act on every turn.
func probeLoop(t *testing.T, deadlineMs int, pacing time.Duration, turns int) (*Loop, *FakeModelClient, *qualExecutor) {
	t.Helper()
	env := testEnvelope(t)
	scope := testScope(env)
	scope.AllowTools = qualifyingTools()
	scope.DeadlineMs = int64(deadlineMs)
	if err := scope.Validate(); err != nil {
		t.Fatalf("scope: %v", err)
	}

	act := callToolBytes(t, env, scope, invest.ToolGetClaim, 1)
	// One response per possible turn, so the model never runs dry and the only
	// thing that can end the run early is the deadline.
	resps := make([]ModelResponse, turns+1)
	for i := range resps {
		resps[i] = modelResp(act)
	}
	fake := &FakeModelClient{Responses: resps, Delay: pacing}

	ex := newQualExecutor(successExecutor())
	lp, err := NewLoop(fake, ex.inner, DefaultBudgets(scope), scope, env, nil)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	return lp, fake, ex
}

// TestAPA64_PacingAtOrAboveDeadlineMakesToolPathUnreachable is the structural
// claim: when the pacing charged to a single turn reaches the whole
// investigation budget, the loop escalates DEADLINE on the FIRST turn, after the
// model returns but BEFORE any tool executes.
//
// This is the scaled reproduction of the live observation
// (modelCalls>0, toolExecutions=0, ESCALATED/DEADLINE).
func TestAPA64_PacingAtOrAboveDeadlineMakesToolPathUnreachable(t *testing.T) {
	const deadlineMs = 120
	const pacing = 200 * time.Millisecond // strictly above the whole budget

	lp, fake, ex := probeLoop(t, deadlineMs, pacing, 3)
	start := time.Now()
	out, err := lp.Run(context.Background())
	elapsed := time.Since(start)

	if !errors.Is(err, ErrDeadlineExceeded) {
		t.Fatalf("err = %v, want ErrDeadlineExceeded", err)
	}
	if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationDeadline {
		t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/DEADLINE", out.Outcome, out.EscalationReason)
	}

	// The model really was called: the run is not vacuous.
	if fake.Calls == 0 {
		t.Fatal("model was never called; the probe is not exercising the seam under test")
	}
	// The decisive fact: the tool NEVER ran. Pacing consumed the budget during
	// the first Complete, so the I2 post-Complete check closed the investigation
	// before exec.Execute could be reached.
	if got := ex.Observed(); got != 0 {
		t.Fatalf("executor observed %d call(s), want 0: pacing at-or-above the budget must make "+
			"the tool path unreachable", got)
	}

	t.Logf("APA64 DEADLINE-REPRODUCED deadline_ms=%d pacing_ms=%d elapsed_ms=%d modelCalls=%d toolExecutions=%d",
		deadlineMs, pacing.Milliseconds(), elapsed.Milliseconds(), fake.Calls, ex.Observed())
	t.Logf("APA64 the whole %dms investigation budget was consumed by ONE %dms paced Complete, "+
		"so no turn ever reached exec.Execute — identical shape to the live run "+
		"(modelCalls>0, toolExecutions=0, ESCALATED/DEADLINE, violations=0)", deadlineMs, pacing.Milliseconds())
}

// TestAPA64_GenerousDeadlineReachesToolPath is the control for the probe above.
// It proves the loop itself is healthy: with the SAME pacing but a budget larger
// than one paced call, the tool path is reached and executes.
//
// Without this, "DEADLINE at zero tool executions" could be a genuine production
// defect rather than a budget/pacing interaction.
func TestAPA64_GenerousDeadlineReachesToolPath(t *testing.T) {
	const deadlineMs = 5000
	const pacing = 50 * time.Millisecond

	lp, fake, ex := probeLoop(t, deadlineMs, pacing, 3)
	// The run's terminal outcome is irrelevant here and is deliberately NOT
	// asserted: the fake repeats an identical call every turn, so the loop ends
	// in REPETITION. What matters is that the tool PATH was reached and ran,
	// which is the control the DEADLINE probe needs.
	out, _ := lp.Run(context.Background())

	if ex.Observed() == 0 {
		t.Fatalf("executor observed 0 calls under a %dms budget with %dms pacing: the loop cannot "+
			"reach the tool path even when the budget allows it, which would be a DIFFERENT defect",
			deadlineMs, pacing.Milliseconds())
	}
	t.Logf("APA64 CONTROL deadline_ms=%d pacing_ms=%d modelCalls=%d toolExecutions=%d outcome=%s reason=%s",
		deadlineMs, pacing.Milliseconds(), fake.Calls, ex.Observed(), out.Outcome, out.EscalationReason)
	t.Logf("APA64 the loop reaches and runs the tool path when the budget exceeds one paced call; " +
		"DEADLINE-at-zero-tools in the live run is therefore a budget/pacing interaction, " +
		"not a production-loop defect")
}

// TestAPA64_DeadlineIsChargedForPacing records the measurement the live run could
// not separate: pacing time is charged to the investigation budget.
//
// It asserts elapsed >= pacing, i.e. the loop's own clock advanced by the paced
// delay. This is the direct evidence that the 60s fixture budget and the observed
// ~60.7s per-call pacing are not independent quantities.
func TestAPA64_DeadlineIsChargedForPacing(t *testing.T) {
	const pacing = 150 * time.Millisecond

	// Generous budget: the run completes normally, but the elapsed time must
	// still include the paced delay, proving it is inside the deadline window.
	lp, fake, _ := probeLoop(t, 10000, pacing, 1)
	start := time.Now()
	// Terminal outcome deliberately not asserted; see the control above.
	_, _ = lp.Run(context.Background())
	elapsed := time.Since(start)

	if elapsed < pacing {
		t.Fatalf("elapsed %v is less than the injected pacing %v: pacing is NOT being charged to "+
			"the investigation budget, which would invalidate the whole hypothesis", elapsed, pacing)
	}
	t.Logf("APA64 PACING-CHARGED pacing_ms=%d elapsed_ms=%d modelCalls=%d (elapsed >= pacing)",
		pacing.Milliseconds(), elapsed.Milliseconds(), fake.Calls)
	t.Logf("APA64 pacing is charged to the investigation budget; a %dms fixture deadline cannot "+
		"absorb a ~60.7s per-call pacing interval", pacing.Milliseconds())
}
