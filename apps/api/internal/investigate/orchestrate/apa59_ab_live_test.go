package orchestrate

// APA-59 live A/B: the decisive experiment.
//
// Arm A is the production prompt (pre-APA-59); arm B injects the per-tool
// limit-bounds clause. Every other part of the run is identical. The model is
// driven only on ps_b1_valid_tool, which requires actual tool mediation (zero
// citable evidence + absent required document), so the arms differ solely in
// what the model is told about "limit".
//
// Three lifecycle metrics are reported separately, because they are distinct
// events rather than proxies for one another:
//
//	attemptedTool       the model emitted a call-tool action
//	executorObserved    the request crossed into the real executor
//	completedResponses  the executor produced/retained a completed response
//
// Decisive shape, enforced directly here:
//
//	A: attempted a tool > 0, executor observed = 0
//	B: attempted a tool > 0, executor observed > 0
//
// executorObserved is the decisive boundary metric for this experiment,
// because that is the seam the hypothesis is about: did a bounded,
// contract-legal request actually cross into the executor? completedResponses
// is supplementary evidence of a stricter lifecycle event and is deliberately
// NOT fused into the causal claim.
//
// If B is attempted>0 / executorObserved=0, it is a NULL RESULT (FAIL), never
// causal evidence. Equally, an A that reaches the executor means the control
// did not reproduce the pre-APA-59 condition, so any A-to-B difference is
// confounded and the run is rejected rather than reported as a pass.
//
// Reaching terminal completion is out of scope for APA-59. A run that escalates
// after the bounded request — for example on the repetition guard — is recorded
// as such and does not qualify end-to-end investigation.

import (
	"context"
	"encoding/json"
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
