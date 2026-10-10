package orchestrate

// Offline integrity proofs for the two harness defects the APA-64 audit
// found. Everything here runs with no provider, no database, and no quota:
// the model seam is a scripted FakeModelClient wrapped in a qualModel, and
// the tool seam is the same in-memory stub executor the shared qualification
// path already uses.
//
// DEFECT 1 — the merged APA-64 file stated that the observed pacing was
// "~60.7s per-call". It is not. qualModel.pacedMS is a SUM over every
// Complete on a repetition's own fork (addPaced, called once per paced
// Complete at groq_qualification_live_test.go:531/577), and the evidence
// field reads that sum (ev.PacedMS = r.Model.PacedMS(), same file:1343).
// The three measured repetitions reported pacedMs = 60770 / 60794 / 60778
// with modelCalls = 3 each, so per-call pacing is ~20.26s and 60.7s is the
// three-turn TOTAL.
//
// DEFECT 2 — qualRun.ToolExecutions was populated ONLY by the S1-specific
// paths (apa52_s1_pg_live_test.go:689 and :1120, via s1JoinExecutions).
// runLiveSeeded never populated it, so for every non-S1 scenario
// ev.ToolExecutions was structurally always empty while ev.RecorderObserved
// reported the executions that really happened. The anti-vacuity predicate
// that read len(e.ToolExecutions) > 0 could therefore never observe an
// execution: it was not a gate, it was a constant false.
//
// The proofs below pin both corrections so they cannot silently rot back.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
)

// ---------------------------------------------------------------------------
// Offline seams
// ---------------------------------------------------------------------------

// qual65CursorBytes renders a valid call_tool act for get_evidence carrying a
// DISTINCT cursor, so two consecutive turns are different requests.
//
// This is what the repetition guard keys on: loop.go:667 hashes the tool plus
// the canonical request bytes (repetitionKey at loop.go:330), and
// canonicalToolRequest marshals the whole investigate.Request struct, so any
// differing field — cursor included — produces a different hash. get_evidence
// is used because it is both registered by the shared stub executor and one of
// the tools that OWNS the cursor knob (investigate.Request.Validate), so the
// request is legal rather than rejected at the request contract.
func qual65CursorBytes(t *testing.T, env invest.UnresolvedException, scope investigate.Scope, cursor string) []byte {
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

// qual65OfflineSeeded drives runLiveSeeded — the shared non-S1 measurement
// path — with a scripted model and NO provider. It returns both the raw run
// and the evidence the harness actually renders from it, because the defect
// was in the evidence, not the loop.
func qual65OfflineSeeded(t *testing.T, scenario string, resps []ModelResponse) (qualRun, qualEvidence) {
	t.Helper()
	env := testEnvelope(t)
	scope := testScope(env)
	scope.AllowTools = qualifyingTools()
	scope.DeadlineMs = 5000
	if err := scope.Validate(); err != nil {
		t.Fatalf("scope: %v", err)
	}
	fake := &FakeModelClient{Responses: resps}
	m := &qualModel{inner: fake, provider: "offline-fake"}
	r := runLiveSeeded(t, scenario, 1, m, nil, env, scope, nil)
	return r, buildEvidence(r)
}

// ---------------------------------------------------------------------------
// RED PROOF 1 — ToolExecutions is populated on the non-S1 path
// ---------------------------------------------------------------------------

// TestQualMeasurement_NonS1PathRecordsToolExecutions is the RED proof for the
// measurement fix.
//
// BEFORE the fix this failed at the first assertion with
// len(ev.ToolExecutions) == 0 while ev.RecorderObserved == 2: the same run
// reported two executions on the recorder line and an empty tool-execution
// list, which is exactly the contradiction the live APA-55 log line showed
// (recorderCalls=2 recorderObserved=2 recordedIDs=[ev-new-01 ev-new-02] with
// toolExecutions=0 on the same line).
func TestQualMeasurement_NonS1PathRecordsToolExecutions(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	scope.AllowTools = qualifyingTools()

	// Two distinct tool calls, then a grounded submit_report.
	resps := []ModelResponse{
		modelResp(qual65CursorBytes(t, env, scope, "cur-01")),
		modelResp(qual65CursorBytes(t, env, scope, "cur-02")),
		modelResp(submitBytes(t, testReport(env, "ev-new-02"))),
	}
	r, ev := qual65OfflineSeeded(t, "qual65_two_tools", resps)

	if r.Executor == nil {
		t.Fatal("no recording executor on the run")
	}
	observed := r.Executor.Observed()
	if observed != 2 {
		t.Fatalf("recorder observed %d execution(s), want 2: the fixture must actually exercise "+
			"the tool path or this proof measures nothing", observed)
	}

	if got := len(ev.ToolExecutions); got != observed {
		t.Fatalf("len(ev.ToolExecutions) = %d, want %d (== recorderObserved). The non-S1 path "+
			"reports an empty tool-execution record for a run in which %d tool(s) demonstrably "+
			"executed, so every anti-vacuity predicate reading this field is vacuously false",
			got, observed, observed)
	}

	// The record must name the tool and the call, not merely count. A count a
	// no-op executor could satisfy is exactly the proxy the correction removes.
	for i, x := range ev.ToolExecutions {
		if x.Tool != string(invest.ToolGetEvidence) {
			t.Errorf("tool_executions[%d].Tool = %q, want %q", i, x.Tool, invest.ToolGetEvidence)
		}
		if x.EvidenceIDs == nil {
			t.Errorf("tool_executions[%d] carries no evidence ids for a call that returned one; "+
				"the record cannot be independent evidence of what ran", i)
		}
	}
	if len(ev.ToolExecutions) == 2 && ev.ToolExecutions[0].Tool == ev.ToolExecutions[1].Tool {
		t.Logf("both recorded executions name %q; they are distinguished by the cursor the loop "+
			"hashed, which the shared audit seam does not carry", ev.ToolExecutions[0].Tool)
	}

	t.Logf("QUAL-MEASURE two-execution run: recorderObserved=%d recorderCalls=%d len(ev.ToolExecutions)=%d "+
		"toolEvidenceIDs=%v", ev.RecorderObserved, ev.RecorderCalls, len(ev.ToolExecutions), ev.ToolEvidenceIDs)
}

// TestQualMeasurement_ZeroExecutionsReportsZero is the other half of the
// proof: the measurement must not simply always report non-zero. A run whose
// first act is a submit_report over seeded evidence executes nothing, and the
// record must say so.
func TestQualMeasurement_ZeroExecutionsReportsZero(t *testing.T) {
	env := testEnvelope(t)
	resps := []ModelResponse{modelResp(submitBytes(t, testReport(env)))}

	r, ev := qual65OfflineSeeded(t, "qual65_no_tools", resps)

	if got := r.Executor.Observed(); got != 0 {
		t.Fatalf("recorder observed %d execution(s) on a run that issues no tool call, want 0", got)
	}
	if got := len(ev.ToolExecutions); got != 0 {
		t.Fatalf("len(ev.ToolExecutions) = %d on a run with zero executions, want 0: the record "+
			"reports executions that never happened", got)
	}
	if ev.Outcome != string(OutcomeReportReady) {
		t.Fatalf("outcome = %q, want %q: this fixture grounds over seeded evidence alone", ev.Outcome, OutcomeReportReady)
	}
	t.Logf("QUAL-MEASURE zero-execution run: recorderObserved=%d len(ev.ToolExecutions)=%d outcome=%s",
		ev.RecorderObserved, len(ev.ToolExecutions), ev.Outcome)
}

// ---------------------------------------------------------------------------
// RED PROOF 2 — why the old anti-vacuity predicate was vacuous
// ---------------------------------------------------------------------------

// qual65OldOutcomeNeedsTool is the PRE-correction psOutcomeNeedsTool
// condition, transcribed verbatim so the history is executable rather than a
// claimed.
//
// THIS IS THE DOCUMENTATION OF WHY THE PREDICATE WAS WRONG. Run against the
// exact shape the live APA-55 line produced — executions observed by the
// recorder, tool_executions empty — it reports a tool-use scenario as never
// having executed a tool.
//
// It returns its problems instead of calling t.Errorf because a real
// t.Errorf cannot be captured: an assertion that the gate REJECTS has to be
// expressed against something observable. The corrected gate is still called
// directly below in the accepting direction, so the transcription is checked
// against the real function rather than trusted.
func qual65OldOutcomeNeedsTool(evs []qualEvidence) (executed, attempted int, problems []string) {
	for _, e := range evs {
		if len(e.ToolExecutions) > 0 { // <-- the vacuous precondition
			executed++
		}
		if e.ModelCalls > 1 {
			attempted++
		}
	}
	if attempted == 0 {
		problems = append(problems, "no tool call was even ATTEMPTED")
	}
	if executed == 0 {
		problems = append(problems, "a tool was attempted but EXECUTED in none")
	}
	return executed, attempted, problems
}

func TestQualMeasurement_OldAntiVacuityPredicateWasVacuous(t *testing.T) {
	// The live APA-55 evidence shape, verbatim from the recorded log line:
	// the recorder observed two executions and their evidence IDs, while the
	// same line reported toolExecutions=0 because the non-S1 path never
	// populated the field.
	live := []qualEvidence{{
		Scenario:         "ps_b1_valid_tool",
		Repeat:           1,
		ModelCalls:       3,
		RecorderCalls:    2,
		RecorderObserved: 2,
		RecordedIDs:      []string{"ev-new-01", "ev-new-02"},
		ToolExecutions:   nil, // what the pre-correction harness produced
		Outcome:          string(OutcomeEscalated),
		EscalationReason: string(EscalationDeadline),
	}}

	// The old predicate, evaluated on that literal.
	oldExecuted, oldAttempted, oldProblems := qual65OldOutcomeNeedsTool(live)
	if oldExecuted != 0 {
		t.Fatalf("the old precondition counted %d executed run(s) on evidence whose tool-execution "+
			"record is empty; it is len(e.ToolExecutions) > 0, so it can only count zero",
			oldExecuted)
	}
	if oldAttempted == 0 {
		t.Fatalf("the ATTEMPTED side of the old gate also reads zero on modelCalls=%d; the proof "+
			"needs evidence where the two DISAGREE, which is the entire point", live[0].ModelCalls)
	}
	if len(oldProblems) == 0 {
		t.Fatal("the pre-correction gate accepted evidence in which two tools demonstrably executed " +
			"and the tool-execution record was empty; if it now accepts, this proof has stopped " +
			"documenting the defect it exists to document")
	}
	t.Logf("QUAL-VACUITY pre-correction gate on live-shaped evidence: attempted=%d executed=%d "+
		"problems=%v — the gate fires even though recorderObserved=%d and recordedIDs=%v prove two "+
		"tools ran", oldAttempted, oldExecuted, oldProblems, live[0].RecorderObserved, live[0].RecordedIDs)

	// The corrected precondition reads the authoritative observation instead,
	// so it counts the execution the recorder saw.
	newExecuted := 0
	for _, e := range live {
		if e.RecorderObserved > 0 {
			newExecuted++
		}
	}
	if newExecuted != 1 {
		t.Fatalf("the corrected precondition counted %d executed run(s), want 1: recorderObserved=%d",
			newExecuted, live[0].RecorderObserved)
	}

	// The REAL corrected gate, called directly: it must accept exactly the
	// evidence the old gate rejected. This is the direction that can be
	// asserted against a live t.Errorf, and it is the one the repoint exists
	// for. Before the repoint it fails here with "EXECUTED in none".
	psOutcomeNeedsTool(t, live)

	// The repoint must not have turned the gate into a no-op. Both remaining
	// rejection directions are asserted on the transcription, checked against
	// the real gate's two conditions rather than against its error strings.
	blind := []qualEvidence{{
		Scenario: "ps_b1_valid_tool", Repeat: 1,
		ModelCalls: 1, RecorderObserved: 0, RecordedIDs: nil,
	}}
	if _, _, p := qual65OldOutcomeNeedsTool(blind); len(p) < 2 {
		t.Fatalf("a run that neither attempted nor executed a tool produced %d problem(s), want both "+
			"preconditions to fire", len(p))
	}

	// The ATTEMPTED-vs-EXECUTED gap the function documents is preserved: a run
	// that attempted but never executed still fails, and that gap is a real
	// finding rather than a pass.
	gapped := []qualEvidence{{
		Scenario: "ps_b1_valid_tool", Repeat: 1,
		ModelCalls: 3, RecorderObserved: 0,
	}}
	ge, ga, gp := qual65OldOutcomeNeedsTool(gapped)
	if ga == 0 || ge != 0 || len(gp) != 1 {
		t.Fatalf("attempted-but-never-executed evidence produced attempted=%d executed=%d "+
			"problems=%v, want attempted>0, executed==0 and exactly the EXECUTED problem",
			ga, ge, gp)
	}
	t.Logf("QUAL-VACUITY gap preserved: attempted=%d executed=%d problems=%v", ga, ge, gp)
}

// ---------------------------------------------------------------------------
// RED PROOF 3 — the pacing figure is a TOTAL, not a per-call interval
// ---------------------------------------------------------------------------

// TestQualMeasurement_PacedMSIsCumulativeNotPerCall pins the corrected
// factual claim about DEFECT 1 without contacting a provider and without
// sleeping for the real 20s pacing floor.
func TestQualMeasurement_PacedMSIsCumulativeNotPerCall(t *testing.T) {
	// The three measured repetitions from /tmp/apa55_final.log.
	measured := []struct {
		pacedMS    int64
		modelCalls int
	}{
		{60770, 3}, {60794, 3}, {60778, 3},
	}
	for _, m := range measured {
		perCall := float64(m.pacedMS) / float64(m.modelCalls)
		// The deterministic floor path: qualMinPacingWait plus the 250ms
		// margin groq_qualification_live_test.go:349 adds to every wait.
		floorMS := float64(qualMinPacingWait/time.Millisecond) + 250
		if perCall < floorMS || perCall > floorMS+50 {
			t.Fatalf("pacedMs=%d over modelCalls=%d implies %.0fms per call, want the %.0fms "+
				"pacing floor (%.0fms + 250ms) plus a few ms of scheduler overshoot",
				m.pacedMS, m.modelCalls, perCall, floorMS, floorMS-250)
		}
		if int64(perCall) == m.pacedMS {
			t.Fatalf("pacedMs=%d equals the per-call figure; the recorded field is the SUM over "+
				"the repetition's Complete calls, not one interval", m.pacedMS)
		}
		t.Logf("QUAL-PACING pacedMs=%d modelCalls=%d -> %.0fms per call (floor %.0fms); "+
			"%.0fms is the three-turn TOTAL, not a per-call interval",
			m.pacedMS, m.modelCalls, perCall, floorMS, perCall)
	}

	// The structural half, proved on the seam itself rather than by
	// arithmetic: pacedMS accumulates across a fork AND its parent, so the
	// per-repetition figure is a sum over that repetition's own calls and the
	// parent's figure is a superset.
	shared := &qualModel{provider: "offline-fake"}
	rep := shared.forkForRep()
	rep.addPaced(100)
	rep.addPaced(250)
	if got := rep.PacedMS(); got != 350 {
		t.Fatalf("fork pacedMS = %d after two paced Completions, want 350 (a sum, not a "+
			"last-value overwrite)", got)
	}
	if got := shared.PacedMS(); got != 350 {
		t.Fatalf("parent pacedMS = %d, want 350: addPaced must propagate to each ancestor or the "+
			"cumulative view silently reads zero", got)
	}
	t.Logf("QUAL-PACING accumulation proven on the seam: fork=%dms parent=%dms after two paced calls",
		rep.PacedMS(), shared.PacedMS())

	// The pacing floor itself, read straight off the pacer. The fixture is a
	// HEADER-LESS attempt — the shape the floor exists for, because "a
	// throttled reply often does [omit them]" — so LimitTokens and
	// RemainingTokens are 0, ResetTokensSecs is 0, the fast path is refused,
	// and the wait collapses to the floor plus the fixed 250ms margin. That
	// is the branch the live run took: the measured ~20.256s per call is
	// 20.000s + 0.250s plus ~6ms of scheduler overshoot, not the provider's
	// reported 38-40s window (which would have shown ~38.25s per call).
	rec := &qualWireRecorder{}
	rec.record(qualWireAttempt{
		Seq: 1, Status: 429, AuthHeaderPresent: false,
	})
	got := rec.pacingWait()
	want := qualMinPacingWait + 250*time.Millisecond
	if got != want {
		t.Fatalf("pacingWait = %v, want %v (the floor plus the 250ms margin); the live per-call "+
			"figure is this number, not three times it", got, want)
	}
	t.Logf("QUAL-PACING pacingWait floor = %v; three calls = %v, which exceeds the 60s fixture deadline",
		got, 3*got)
	if 3*got <= 60*time.Second {
		t.Fatalf("three paced Completions total %v, which does NOT exceed the 60s fixture deadline; "+
			"the causal claim under correction would be false", 3*got)
	}
}

// TestQualMeasurement_FailedExecutionIsStillRecordedAndJoined closes the last
// gap in the measurement fix.
//
// A tool call that fails terminally still fires the audit seam (the executor
// audits failures as well as successes), so it must appear in the execution
// record AND still be joinable to an attempt-log row. That row carries no
// response IDs on a failed call, so it can only be matched by tool plus the
// empty-ID/zero-row shape — which is exactly the branch the join adds for it.
func TestQualMeasurement_FailedExecutionIsStillRecordedAndJoined(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	scope.AllowTools = qualifyingTools()
	scope.DeadlineMs = 5000
	if err := scope.Validate(); err != nil {
		t.Fatalf("scope: %v", err)
	}

	// failingExecutor returns an Upstream transient for get_evidence. The
	// executor retries internally, then surfaces it; the loop records an error
	// turn with no response IDs. The distinct cursors keep the repetition
	// guard from firing first.
	resps := []ModelResponse{
		modelResp(qual65CursorBytes(t, env, scope, "cur-01")),
		modelResp(qual65CursorBytes(t, env, scope, "cur-02")),
		modelResp(submitBytes(t, testReport(env))),
	}
	fake := &FakeModelClient{Responses: resps}
	ex := newQualExecutor(failingExecutor())
	lp, err := NewLoop(fake, ex.inner, DefaultBudgets(scope), scope, env, nil)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	out, runErr := lp.Run(context.Background())
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}

	if got := ex.Observed(); got != 2 {
		t.Fatalf("recorder observed %d execution(s), want 2: a FAILED call still executes and "+
			"must still be recorded, or a broken tool is invisible in the record", got)
	}

	execs, problems := qualJoinExecutionTurns(ex.executions(), out.AttemptLog)
	if len(problems) != 0 {
		t.Fatalf("join reported %d problem(s) on a run whose failed calls DID reach the attempt "+
			"log: %v", len(problems), problems)
	}
	if len(execs) != 2 {
		t.Fatalf("len(execs) = %d, want 2", len(execs))
	}
	for i, x := range execs {
		if x.ErrorCode == errorCodeOK || x.Error == "" {
			t.Errorf("tool_executions[%d] records a failed call as ErrorCode=%q Error=%q; a failure "+
				"that reads as a success would let the gate pass on a broken tool", i, x.ErrorCode, x.Error)
		}
		if x.EvidenceIDs != nil {
			t.Errorf("tool_executions[%d] carries evidence ids %v for a call that returned none; "+
				"that would make a rebuilt known-set disagree with the loop's", i, x.EvidenceIDs)
		}
		// The join is what makes the record tie back to the loop's own turn.
		if x.Turn != i+1 {
			t.Errorf("tool_executions[%d].Turn = %d, want %d: a failed execution must still be "+
				"joined to its attempt-log row", i, x.Turn, i+1)
		}
		if x.RequestHash == "" {
			t.Errorf("tool_executions[%d].RequestHash is empty; the record cannot be tied to the "+
				"repetition guard's decision", i)
		}
	}

	// The counter and the record cannot disagree, by construction.
	if len(execs) != ex.Observed() {
		t.Fatalf("len(execs) = %d but Observed() = %d; the record and the authoritative counter "+
			"must never diverge", len(execs), ex.Observed())
	}

	t.Logf("QUAL-MEASURE failed-execution run: recorderObserved=%d len(execs)=%d "+
		"errorCodes=%v turns=%v requests=%v report_ready=%v",
		ex.Observed(), len(execs),
		[]string{execs[0].ErrorCode, execs[1].ErrorCode},
		[]int{execs[0].Turn, execs[1].Turn},
		[]bool{execs[0].RequestHash != "", execs[1].RequestHash != ""},
		out.Outcome == OutcomeReportReady)
}
