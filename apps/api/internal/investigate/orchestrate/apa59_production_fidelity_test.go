package orchestrate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
)

// Production prompt fidelity for APA-59.
//
// APA-59 was measured with a treatment prompt built by injecting one clause
// into the production prompt (PR #127, arm B). That evidence is only a
// statement about production if the prompt that was measured is the prompt
// production renders. This file pins that, so the recorded causal evidence
// cannot silently decay into a claim about a prompt that no longer ships.

// frozenAPA59BPromptSHA256 is the sha256 of the arm-B prompt bytes that were
// actually sent to the provider during the APA-59 live A/B measurement. It was
// logged by TestAPA59ABDecisiveToolTrajectory before any provider call:
//
//	APA59_B_PROMPT_SHA256=f57cf7ad58d360331e2cae9a914da3dea7df557751a41e2946667a3a1a1cdd5b
//
// This is the load-bearing constant for the whole APA-59 conclusion. Changing
// it requires a fresh measurement, not a rerun.
const frozenAPA59BPromptSHA256 = "f57cf7ad58d360331e2cae9a914da3dea7df557751a41e2946667a3a1a1cdd5b"

// apa59MeasuredRequest is the exact request the A/B rendered and measured.
//
// It matches TestAPA59ABDecisiveToolTrajectory, which renders
// ModelRequest{Exception: env, KnownEvidenceIDs: []string{}, Turn: 1,
// RequestID: scope.RequestID} from psFixtureNeedsTool. KnownEvidenceIDs is
// empty on purpose: that is the zero-citable-evidence condition that forces
// real tool mediation, which is what the experiment needs.
func apa59MeasuredRequest(t *testing.T) ModelRequest {
	t.Helper()
	env, scope, _ := psFixtureNeedsTool(t)
	return ModelRequest{
		Exception:        env,
		KnownEvidenceIDs: []string{},
		Turn:             1,
		RequestID:        scope.RequestID,
	}
}

// TestAPA59_ProductionPromptIsTheFrozenMeasuredPrompt is the fidelity gate:
// production RenderPrompt must render byte-identically to the prompt whose
// behaviour was measured.
//
// This is a hash assertion, deliberately. A substring or "contains the clause"
// check would pass for any prompt that merely mentions a limit bound, including
// one that says the wrong numbers or places the rule where the model does not
// read it. Only whole-prompt byte equality proves the recorded evidence
// describes production.
func TestAPA59_ProductionPromptIsTheFrozenMeasuredPrompt(t *testing.T) {
	req := apa59MeasuredRequest(t)

	production, err := RenderPrompt(req)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}

	sum := sha256.Sum256([]byte(production))
	got := hex.EncodeToString(sum[:])
	if got != frozenAPA59BPromptSHA256 {
		t.Errorf("PROMPT DRIFT: production RenderPrompt no longer renders the prompt that "+
			"APA-59 measured.\n  frozen measured sha256: %s\n  production   sha256: %s\n"+
			"The recorded APA-59 causal evidence describes the frozen prompt above, not this one. "+
			"Either the production prompt changed, or the measurement must be repeated.",
			frozenAPA59BPromptSHA256, got)
	}
}

// TestAPA59_ProductionPromptStatesEveryAuthoritativeBound pins the substance
// independently of the hash, so that a failure here names what drifted rather
// than only reporting two digests.
//
// Bounds are read from investigate.MaxRows, never restated, so this cannot
// drift from the validator that rejects an over-bound request.
func TestAPA59_ProductionPromptStatesEveryAuthoritativeBound(t *testing.T) {
	production, err := RenderPrompt(apa59MeasuredRequest(t))
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}

	for _, tool := range limitBoundsTools() {
		max, err := investigate.MaxRows(tool)
		if err != nil {
			t.Fatalf("MaxRows(%s): %v", tool, err)
		}
		// Compare whole units, never substrings: "at most 1" is a substring of
		// "at most 10", and a substring comparison here would reproduce exactly
		// the defect this whole chain exists to catch.
		want := fmt.Sprintf("%s at most %d", string(tool), max)
		if !strings.Contains(production, want) {
			t.Errorf("production prompt does not state the authoritative bound %q for tool %q; "+
				"the model would have to guess a legal limit for this tool", want, string(tool))
		}
	}
}

// TestAPA59_ProductionPromptClosesTheSingleRowTrap guards the specific
// inference the example teaches by omission: get_documents shows limit 10,
// which is legal, while get_claim is capped at 1. Without an explicit rule the
// example value leaks across tools, which is the defect APA-59 measured.
func TestAPA59_ProductionPromptClosesTheSingleRowTrap(t *testing.T) {
	production, err := RenderPrompt(apa59MeasuredRequest(t))
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}

	claimMax, err := investigate.MaxRows(invest.ToolGetClaim)
	if err != nil {
		t.Fatalf("MaxRows(get_claim): %v", err)
	}
	if claimMax != 1 {
		t.Fatalf("precondition changed: get_claim MaxRows is %d, not 1; "+
			"this test encodes the single-row case and must be revisited, not silently weakened", claimMax)
	}

	for _, want := range []string{
		`a single-row tool must send "limit" 1`,
		`is rejected before the tool runs`,
	} {
		if !strings.Contains(production, want) {
			t.Errorf("production prompt is missing the guard %q; the model is left to infer the "+
				"per-tool bound from an example that only shows a multi-row tool", want)
		}
	}
}
