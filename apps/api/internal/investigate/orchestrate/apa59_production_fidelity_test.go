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

// apa59BoundsLinePrefix opens the production bounds clause line. Parsing starts
// from the rendered prompt rather than from the clause function, so the gate
// reads what the model is actually shown rather than what the renderer would
// return if asked.
const apa59BoundsLinePrefix = `- "limit" is bounded per tool`

// apa59StatedBoundsInPrompt parses the per-tool bounds out of a rendered prompt
// into whole tool-to-integer units.
func apa59StatedBoundsInPrompt(t *testing.T, prompt string) map[string]int {
	t.Helper()
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(line, apa59BoundsLinePrefix) {
			return apa59StatedBounds(line)
		}
	}
	t.Fatalf("rendered prompt has no line beginning %q; the production bounds clause "+
		"has drifted or been removed", apa59BoundsLinePrefix)
	return nil
}

// boundDefect is one tool whose prompt-stated bound disagrees with the
// authority that rejects an over-bound request.
type boundDefect struct {
	Tool      string
	Stated    int
	Authority int
}

func (d boundDefect) String() string {
	return fmt.Sprintf("%s: prompt states %d, authority allows %d", d.Tool, d.Stated, d.Authority)
}

// apa59BoundDefects compares parsed, whole-unit bounds against investigate.MaxRows
// as exact integers.
//
// This is deliberately integer equality and never a substring or prefix test.
// The prompt and the authority are rendered as text ("get_claim at most 1"),
// and "get_claim at most 10" CONTAINS "get_claim at most 1" as a substring, so
// a Contains-based gate passes exactly the 1 -> 10 defect it exists to reject.
// That collision is not hypothetical: it is the bug this whole chain was filed
// against. TestAPA59_BoundGateRejectsThePrefixCollision proves it.
func apa59BoundDefects(stated map[string]int) []boundDefect {
	var defects []boundDefect
	for _, tool := range limitBoundsTools() {
		name := string(tool)
		max, err := investigate.MaxRows(tool)
		if err != nil {
			defects = append(defects, boundDefect{Tool: name, Stated: -1, Authority: -1})
			continue
		}
		got, ok := stated[name]
		if !ok {
			defects = append(defects, boundDefect{Tool: name, Stated: -1, Authority: max})
			continue
		}
		if got != max {
			defects = append(defects, boundDefect{Tool: name, Stated: got, Authority: max})
		}
	}
	return defects
}

// TestAPA59_ProductionPromptStatesEveryAuthoritativeBound pins the substance
// independently of the prompt hash, so that a failure names what drifted rather
// than only reporting two digests.
func TestAPA59_ProductionPromptStatesEveryAuthoritativeBound(t *testing.T) {
	production, err := RenderPrompt(apa59MeasuredRequest(t))
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}

	stated := apa59StatedBoundsInPrompt(t, production)
	if n, want := len(stated), len(limitBoundsTools()); n != want {
		t.Errorf("parsed %d bound statements out of the production prompt, want %d; "+
			"every tool carrying a \"limit\" must be stated or the model guesses", n, want)
	}
	for _, d := range apa59BoundDefects(stated) {
		t.Errorf("BOUND MISMATCH %s; the model would emit a request the validator rejects", d)
	}
}

// TestAPA59_BoundGateRejectsThePrefixCollision is the adversarial RED proof for
// the gate above. It injects the exact historical defect -- get_claim widened
// from 1 to 10 -- into the rendered production prompt and requires the gate to
// catch it.
//
// Without this test the gate could silently regress to a Contains check and go
// green on the very defect it was written to reject.
func TestAPA59_BoundGateRejectsThePrefixCollision(t *testing.T) {
	production, err := RenderPrompt(apa59MeasuredRequest(t))
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}

	// Precondition: the real prompt states the authoritative single-row bound.
	if !strings.Contains(production, "get_claim at most 1") {
		t.Fatal("precondition changed: production does not state \"get_claim at most 1\"; " +
			"this proof encodes the 1 -> 10 collision and must be revisited, not weakened")
	}

	// The trap, stated explicitly: after widening 1 -> 10, the correct string is
	// still a substring of the prompt. A Contains-based gate cannot see it.
	faulty := strings.Replace(production, "get_claim at most 1", "get_claim at most 10", 1)
	if faulty == production {
		t.Fatal("injection failed: the authoritative bound string was not present to widen")
	}
	if !strings.Contains(faulty, "get_claim at most 1") {
		t.Fatal("precondition changed: expected the widened \"at most 10\" text to still " +
			"contain \"at most 1\" as a substring; that collision is what makes a " +
			"substring gate unsound and this proof is no longer testing it")
	}

	// Drive the real path the model reads: text -> parse -> compare.
	stated := apa59StatedBoundsInPrompt(t, faulty)
	if got := stated["get_claim"]; got != 10 {
		t.Fatalf("parser did not read the injected fault: get_claim parsed as %d, want 10", got)
	}

	var found bool
	for _, d := range apa59BoundDefects(stated) {
		if d.Tool == string(invest.ToolGetClaim) && d.Stated == 10 && d.Authority == 1 {
			found = true
		}
	}
	if !found {
		t.Error("GATE IS BROKEN: the prompt states get_claim at most 10 while the authority " +
			"allows 1, but the gate reported no defect. A substring comparison would have " +
			"passed this; the gate must compare parsed integers")
	}

	// The same gate must be clean on the real prompt, or "always reports a
	// defect" would pass the test above for the wrong reason.
	if defects := apa59BoundDefects(apa59StatedBoundsInPrompt(t, production)); len(defects) != 0 {
		t.Errorf("production prompt is not clean under the gate that just proved itself: %v", defects)
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
