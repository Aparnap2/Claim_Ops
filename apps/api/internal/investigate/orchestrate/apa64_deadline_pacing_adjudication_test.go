package orchestrate

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
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
// on the same lines that reported recorderCalls=2 recorderObserved=2
// recordedIDs=[ev-new-01 ev-new-02].
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
// THE PACING FIGURE, CORRECTED. An earlier revision of this file described
// the observed pacing as "~60.7s per-call". That was wrong, and the
// correction changes the causal story. qualModel.pacedMS is a SUM over every
// paced Complete on a repetition's own fork — addPaced is called once per
// paced Complete (groq_qualification_live_test.go:531) and accumulates
// (:577-586) — and the evidence field reads that sum, not one interval
// (:1343). The three measured repetitions reported pacedMs = 60770 / 60794 /
// 60778 with modelCalls = 3 each, so:
//
//	per-turn pacing  ~60.77s / 3 = ~20.26s  (= qualMinPacingWait 20s
//	                                               + the 250ms margin at :349,
//	                                               plus a few ms of scheduler
//	                                               overshoot)
//	three-turn total ~60.77s
//
// and the 60.7s figure is the TOTAL across three turns. It is not one
// interval. TestQualMeasurement_PacedMSIsCumulativeNotPerCall pins this
// offline against the pacer and the seam.
//
// WHY THE LIVE RUN DIED ON TURN 3, NOT TURN 1. Because one turn costs only
// ~20.26s of a 60s budget:
//
//	turn 1  ~20.26s elapsed -> tool EXECUTES          (recorderObserved 1)
//	turn 2  ~40.5s  elapsed -> tool EXECUTES          (recorderObserved 2)
//	turn 3  ~60.77s elapsed -> submit_report produced,
//	         then the I2 post-Complete deadline check fires and DISCARDS it
//	         -> ESCALATED(DEADLINE), report_ready = 0
//
// The two executed tools and the discarded report are both visible on the
// recorded line. A reproduction that escalates on turn 1 with zero
// executions is a DIFFERENT shape, and this file now labels it as such.
//
// THE STRUCTURE, from the loop itself:
//
//	deadline := time.Now().Add(scope.DeadlineMs)
//	for turn := 1..MaxTurns {
//	    if time.Now().After(deadline) { return fail(DEADLINE) }   // pre-turn check (loop.go:533)
//	    resp := model.Complete(...)                                 // <-- pacing sleeps HERE
//	    if time.Now().After(deadline) { return fail(DEADLINE) }    // I2 post-Complete check (loop.go:625)
//	    ... exec.Execute(...)                                       // unreachable once the check fires
//	}
//
// and qualModel.Complete sleeps pacingWait() BEFORE the inner
// provider call, so pacing is inside the deadline window and is charged to it.
//
// NO PROVIDER CREDENTIAL IS REQUIRED. FakeModelClient.Delay injects the pacing
// delay deterministically at the same seam, and the deadlines here are scaled
// down to milliseconds.
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

// TestAPA64_PacingAtOrAboveDeadlineMakesToolPathUnreachable is the FIRST-HALF
// structural claim: when one turn's pacing consumes the WHOLE investigation
// budget, the loop escalates DEADLINE on the FIRST turn, after the model
// returns but BEFORE any tool executes.
//
// THIS IS NOT THE SHAPE OF THE LIVE RUN, and it is not offered as one. The
// live run's per-turn pacing was ~20.26s against a 60s budget, so it executed
// two tools and died on turn 3 (see the file comment and
// TestAPA64_ThreeTurnCumulativePacingDiscardsTurnThree, which reproduces that
// shape). This probe isolates the turn-1 extreme on its own terms: it shows the
// tool path becomes unreachable when the budget cannot cover even one paced
// call.
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

	t.Logf("APA64 TURN1-EXHAUSTED deadline_ms=%d pacing_ms=%d elapsed_ms=%d modelCalls=%d toolExecutions=%d",
		deadlineMs, pacing.Milliseconds(), elapsed.Milliseconds(), fake.Calls, ex.Observed())
	t.Logf("APA64 the whole %dms investigation budget was consumed by ONE %dms paced Complete, "+
		"so no turn ever reached exec.Execute. This is the turn-1 extreme and it is NOT the live "+
		"run's shape: the live run's ~20.26s per-turn pacing left room for two tools to execute "+
		"and died on turn 3", deadlineMs, pacing.Milliseconds())
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
		"the live run's DEADLINE is therefore a budget/pacing interaction, " +
		"not a production-loop defect")
}

// TestAPA64_DeadlineIsChargedForPacing records the measurement the live run could
// not separate: pacing time is charged to the investigation budget.
//
// It asserts elapsed >= pacing, i.e. the loop's own clock advanced by the paced
// delay. This is the direct evidence that the 60s fixture budget and the
// harness's pacing are not independent quantities.
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
	t.Logf("APA64 pacing is charged to the investigation budget. The live run's budget was 60000ms " +
		"and its pacing was ~20.26s PER TURN, ~60.77s across the three turns it actually made, " +
		"which is what crossed the budget — see TestAPA64_ThreeTurnCumulativePacingDiscardsTurnThree")
}

// ---------------------------------------------------------------------------
// The actual live shape: three turns, two executions, one discarded report
// ---------------------------------------------------------------------------

// liveShapeExecutor returns a stub executor that hands back a FRESH evidence id
// on each call, so consecutive turns widen the known set exactly as the live
// run's did (recordedIDs=[ev-new-01 ev-new-02]).
//
// It deliberately is not successExecutor, which returns the same id every time
// and therefore leaves the second turn stagnant. Reproducing the live
// accounting means the two reads must both count as progress.
func liveShapeExecutor(ids ...string) *investigate.Executor {
	var mu sync.Mutex
	next := 0
	return investigate.NewExecutor(map[invest.ToolName]investigate.ToolFunc{
		invest.ToolGetEvidence: func(_ context.Context, req investigate.Request) (investigate.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			id := "ev-new-00"
			if next < len(ids) {
				id = ids[next]
			}
			next++
			return investigate.Response{Tool: req.Tool, RowCount: 1, IDs: []string{id}}, nil
		},
	}, time.Time{})
}

// evidenceCursorBytes renders a valid get_evidence call_tool act carrying a
// cursor.
//
// THE REPETITION GUARD IS WHY THIS EXISTS. loop.go:667 rejects an act whose
// repetitionKey — sha256 over the tool name plus the canonical request bytes
// (loop.go:330, canonicalToolRequest at :320) — has already been seen. Two
// turns issuing the SAME call would therefore escalate REPETITION on turn 2,
// before the deadline could matter. Varying any request field varies the hash;
// cursor is used because get_evidence OWNS that knob (investigate.Request.Validate)
// and is registered by the stub, so the request is legal rather than rejected
// at the request contract.
func evidenceCursorBytes(t *testing.T, env invest.UnresolvedException, scope investigate.Scope, cursor string) []byte {
	t.Helper()
	req, err := investigate.NewRequest(invest.ToolGetEvidence, scope.TenantID, scope.ClaimID,
		env.InvestigationID, scope.RequestID, 5)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Cursor = cursor
	if err := req.Validate(); err != nil {
		t.Fatalf("cursor request invalid: %v", err)
	}
	raw, err := json.Marshal(ModelAction{Action: ActionCallTool, Tool: invest.ToolGetEvidence, Request: &req})
	if err != nil {
		t.Fatalf("marshal call_tool: %v", err)
	}
	return raw
}

// TestAPA64_ThreeTurnCumulativePacingDiscardsTurnThree reproduces the ACTUAL
// live failure shape, deterministically and with no provider.
//
// THE SHAPE, from the recorded APA-55 line:
//
//	recorderCalls=2 recorderObserved=2 recordedIDs=[ev-new-01 ev-new-02]
//	outcome=ESCALATED reason=DEADLINE modelCalls=3 report_ready=0
//
// Three model turns. Turns 1 and 2 each return a DISTINCT valid call_tool act
// that really executes. Turn 3 returns a structurally valid, fully grounded
// submit_report — and the I2 post-Complete deadline check at loop.go:625-626
// fires and DISCARDS it before CheckReportGrounding ever sees it. The terminal
// is ESCALATED(DEADLINE) and no report is accepted.
//
// SCALING. Per-turn pacing is injected at the same seam the harness uses
// (FakeModelClient.Delay) and the deadline is scaled to milliseconds, because
// Scope.Validate rejects a deadline below 100ms. The ORDERING is the invariant
// that matters and it is preserved exactly:
//
//	deadline D must satisfy 2*pacing < D < 3*pacing
//
// so turns 1 and 2 finish inside the window and turn 3 does not. D is placed
// at the midpoint (150ms with 60ms pacing), leaving ~30ms of slack on each
// side — half a pacing interval — which keeps the probe deterministic without
// the 60s of wall clock the real pacing floor costs.
//
// THE RATIO IS NOT PRESERVED, and this file does not claim it is. The live
// ratio was 60.77s against 60.0s, about 1.3% over; here it is 180ms against
// 150ms. Preserving 1.3% at millisecond scale would leave a sub-millisecond
// margin, which is not a probe, it is a coin flip. What is preserved is the
// causal mechanism, which is what the correction is about.
func TestAPA64_ThreeTurnCumulativePacingDiscardsTurnThree(t *testing.T) {
	const pacing = 60 * time.Millisecond
	const deadlineMs = 150 // 2*pacing (120ms) < 150ms < 3*pacing (180ms)

	env := testEnvelope(t)
	scope := testScope(env)
	scope.AllowTools = qualifyingTools()
	scope.DeadlineMs = deadlineMs
	if err := scope.Validate(); err != nil {
		t.Fatalf("scope: %v", err)
	}

	// Turns 1 and 2: distinct valid calls that execute. Turn 3: a report that
	// would be accepted outright if the deadline had not already closed.
	report := testReport(env, "ev-new-01", "ev-new-02")
	resps := []ModelResponse{
		modelResp(evidenceCursorBytes(t, env, scope, "cur-01")),
		modelResp(evidenceCursorBytes(t, env, scope, "cur-02")),
		modelResp(submitBytes(t, report)),
	}
	fake := &FakeModelClient{Responses: resps, Delay: pacing}

	ex := newQualExecutor(liveShapeExecutor("ev-new-01", "ev-new-02"))
	lp, err := NewLoop(fake, ex.inner, DefaultBudgets(scope), scope, env, nil)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}

	start := time.Now()
	out, runErr := lp.Run(context.Background())
	elapsed := time.Since(start)

	// The live terminal, exactly.
	if !errors.Is(runErr, ErrDeadlineExceeded) {
		t.Fatalf("err = %v, want ErrDeadlineExceeded", runErr)
	}
	if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationDeadline {
		t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/DEADLINE", out.Outcome, out.EscalationReason)
	}

	// The decisive live shape: TWO tools executed, not zero. This is the fact
	// the turn-1 probe above cannot show and that the live run actually had.
	if got := ex.Observed(); got != 2 {
		t.Fatalf("executor observed %d call(s), want 2: the live shape is two executions followed "+
			"by a deadline on the third turn, so this probe is not reproducing it", got)
	}
	if got := ex.Calls(); got != 2 {
		t.Fatalf("executor counted %d call(s), want 2", got)
	}
	if ids := ex.recordedIDs(); len(ids) != 2 || ids[0] != "ev-new-01" || ids[1] != "ev-new-02" {
		t.Fatalf("recordedIDs = %v, want [ev-new-01 ev-new-02]: both reads must have widened the "+
			"known set, or the turn accounting is not the live one", ids)
	}
	// Exactly three model calls: no fourth turn, so the run really did end on
	// the report turn rather than running on.
	if fake.Calls != 3 {
		t.Fatalf("modelCalls = %d, want 3", fake.Calls)
	}

	// No report was accepted. Report must be nil and the partial slot must not
	// have captured it either: the I2 check passes nil (loop.go:626), so the
	// grounded report was discarded, not downgraded.
	if out.Report != nil {
		t.Errorf("ESCALATED carried an accepted report: %+v", out.Report)
	}
	if out.Partial != nil {
		t.Errorf("the discarded report was captured as Partial; the I2 deadline check passes nil, "+
			"so a report that reaches the check is dropped entirely: %+v", out.Partial)
	}
	// The two executions are in the attempt log, so the reads are on the record.
	if len(out.AttemptLog) != 2 {
		t.Fatalf("attempt log has %d row(s), want 2", len(out.AttemptLog))
	}
	for i, rec := range out.AttemptLog {
		if rec.Turn != i+1 {
			t.Errorf("attempt log row %d is turn %d, want %d", i, rec.Turn, i+1)
		}
	}

	// The mechanism, stated as measured numbers: cumulative pacing, not one
	// interval, is what crossed the budget.
	cumulative := 3 * pacing
	t.Logf("APA64 LIVE-SHAPE deadline_ms=%d pacing_per_turn_ms=%d cumulative_pacing_ms=%d "+
		"elapsed_ms=%d modelCalls=%d toolExecutions=%d recordedIDs=%v outcome=%s reason=%s report=%v partial=%v",
		deadlineMs, pacing.Milliseconds(), cumulative.Milliseconds(), elapsed.Milliseconds(),
		fake.Calls, ex.Observed(), ex.recordedIDs(), out.Outcome, out.EscalationReason,
		out.Report != nil, out.Partial != nil)
	t.Logf("APA64 turns 1 and 2 each finished inside the %dms window and their tools executed; turn 3 "+
		"finished at ~%dms cumulative pacing, the I2 post-Complete check at loop.go:625 fired, and the "+
		"grounded submit_report was discarded -> ESCALATED(DEADLINE), report_ready=0. This IS the live "+
		"shape: %d executions, not 0, and death on turn 3, not turn 1",
		deadlineMs, cumulative.Milliseconds(), ex.Observed())
	if cumulative <= time.Duration(deadlineMs)*time.Millisecond {
		t.Fatalf("cumulative pacing %v does not exceed the %dms deadline; the causal claim this probe "+
			"exists to demonstrate would be false", cumulative, deadlineMs)
	}
	if 2*pacing >= time.Duration(deadlineMs)*time.Millisecond {
		t.Fatalf("two turns of pacing (%v) already exceed the %dms deadline, so turn 2 could not have "+
			"executed a tool and this is the turn-1 shape, not the live one", 2*pacing, deadlineMs)
	}
}
