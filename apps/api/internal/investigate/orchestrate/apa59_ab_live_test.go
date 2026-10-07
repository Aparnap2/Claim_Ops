package orchestrate

// APA-59 live A/B: the decisive experiment.
//
// Arm A is the production prompt (pre-APA-59); arm B injects the per-tool
// limit-bounds clause. Every other part of the run is identical. The model is
// driven only on ps_b1_valid_tool, which requires actual tool mediation (zero
// citable evidence + absent required document), so the arms differ solely in
// what the model is told about "limit".
//
// Decisive shape, enforced directly here:
//
//	A: attempted a tool > 0, executed a tool = 0
//	B: attempted a tool > 0, executed a tool > 0
//
// If B is attempted>0 / executed=0, it is a NULL RESULT (FAIL), never causal
// evidence.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"
)

// abActionRecorder wraps a ModelClient and records the action each response
// requested. The poolside payload at the boundary is the act JSON
// (ModelAction), so the attempted-tool count is read directly from actual
// model output instead of being inferred from ModelCalls>1.
type abActionRecorder struct {
	inner ModelClient
	acts  []ModelAction
}

func (r *abActionRecorder) Complete(ctx context.Context, req ModelRequest) (ModelResponse, error) {
	resp, err := r.inner.Complete(ctx, req)
	if err == nil {
		var act ModelAction
		if json.Unmarshal(resp.Payload, &act) == nil && act.Action != "" {
			r.acts = append(r.acts, act)
		}
	}
	return resp, err
}

func (r *abActionRecorder) count(action string) int {
	n := 0
	for _, a := range r.acts {
		if string(a.Action) == action {
			n++
		}
	}
	return n
}

// TestAPA59ABDecisiveToolTrajectory runs both arms over ps_b1_valid_tool and
// enforces the causal decision rule on the measured trajectories.
func TestAPA59ABDecisiveToolTrajectory(t *testing.T) {
	if !psLiveOptIn() {
		t.Skip("POOLSIDE_API_KEY unset; the APA-59 A/B requires live inference")
	}
	env, scope, _ := psFixtureNeedsTool(t)
	psPremiseNeedsTool(t, env)

	// Freeze arm B's exact prompt bytes and hash before any provider call, so
	// PR B can assert production == measured B.
	canonicalReq := ModelRequest{
		Exception:        env,
		KnownEvidenceIDs: []string{},
		Turn:             1,
		RequestID:        scope.RequestID,
	}
	if armBPrompt, err := renderAPA59Variant(canonicalReq, PromptVariantAPA59); err != nil {
		t.Fatalf("render arm B: %v", err)
	} else {
		sum := sha256.Sum256([]byte(armBPrompt))
		t.Logf("APA59_B_PROMPT_SHA256=%s", hex.EncodeToString(sum[:]))
	}

	type traj struct {
		attempted        int // model asked for a call_tool action
		executorObserved int // request entered the real executor path
		completed        int // executor retained/recorded a completed response
	}
	var trajectories [2]traj
	for i, arm := range []PromptVariant{PromptVariantAPA56, PromptVariantAPA59} {
		m, wire := requireLivePoolside(t)
		pc, ok := m.inner.(*poolsideClient)
		if !ok {
			t.Fatalf("qualModel inner is not *poolsideClient for arm %v", arm)
		}
		pc.render = func(arm PromptVariant) func(ModelRequest) (string, error) {
			return func(r ModelRequest) (string, error) { return renderAPA59Variant(r, arm) }
		}(arm)
		dec := &abActionRecorder{inner: m.inner}
		m.inner = dec

		r := runLiveSeeded(t, "ps_b1_valid_tool", 1, m, wire, env, scope, nil)
		for j, a := range dec.acts {
			raw, _ := json.Marshal(a)
			t.Logf("APA59 arm %s act[%d]: %s", arm, j, raw)
		}
		if r.Err != nil {
			t.Logf("APA59 arm %s err: %v", arm, r.Err)
		}
		trajectories[i] = traj{
			attempted:        dec.count("call_tool"),
			executorObserved: r.Executor.Observed(),
			completed:        len(r.Executor.recorded()),
		}
		t.Logf("APA59 arm %s: attemptedTool=%d executorObserved=%d executorCalls=%d completedResponses=%d",
			arm, trajectories[i].attempted, trajectories[i].executorObserved, r.Executor.Calls(), trajectories[i].completed)
		time.Sleep(400 * time.Millisecond)
	}

	a, b := trajectories[0], trajectories[1]
	t.Logf("APA59_TRAJECTORY_A: attempted=%d executorObserved=%d completed=%d", a.attempted, a.executorObserved, a.completed)
	t.Logf("APA59_TRAJECTORY_B: attempted=%d executorObserved=%d completed=%d", b.attempted, b.executorObserved, b.completed)

	// Decisive rule (revised): the executor-observed count is the fact this
	// experiment is about. attempted>0 && executorObserved>0 means the bounded
	// request actually reached the executor. A completed_response is a
	// separate, stricter lifecycle event and is reported as such, NOT folded
	// into the causal claim. A green B with executorObserved=0 is a NULL
	// RESULT, never causal evidence.
	if a.attempted == 0 {
		t.Errorf("arm A never attempted a tool; the control does not reproduce " +
			"the defect, so B cannot be attributed")
	}
	if a.executorObserved != 0 {
		t.Errorf("arm A: executor observed %d call(s); expected 0. The control is not the "+
			"pre-APA-59 condition, so any A→B difference is confounded", a.executorObserved)
	}
	if b.attempted == 0 {
		t.Error("arm B never attempted a tool; the model did not act on the new bound")
	}
	if b.executorObserved == 0 {
		t.Errorf("NULL RESULT: arm B attempted %d tool(s) but executorObserved=0. A green B "+
			"with executorObserved=0 is NOT evidence that APA-59 fixed the contract", b.attempted)
	}
}
