// APA-50: the behavioural/state-transition policy the production prompt
// must state.
//
// WHAT THE LAST REAL-MODEL RUN PROVED. The schema contract is QUALIFIED:
// every act decoded cleanly, 0 invalid-output classes, so layers 1/2/3 of
// APA-49 are done and this file must not reopen them. What failed was a
// PROMPT-POLICY failure. PromptTemplate read, at model.go:265:
//
//	"Canonical act examples. Copy their structure; substitute only the
//	 text and the IDs you were given"
//
// with no statement of WHEN to transition between the two acts. The first
// canonical example is a call_tool, so the nearest thing to a template was
// a tool call, and the model did precisely what the prompt taught: it
// emitted a byte-identical call_tool act on all 6 loop turns across 3
// repetitions. The deterministic repetition guard
// (loop.go:667-671) then escalated REPETITION, which is the boundary
// working, not the model being contained by luck. S1 completion was 0/3
// REPORT_READY.
//
// WHY A PROMPT TEST AND NOT A LOOP TEST. The loop is correct and must stay
// untouched: an exact action/request repeat cannot observe anything new, so
// refusing it is the right decision. The defect is that the prompt never
// told the model the decision it was being asked to make. That is a
// property of PromptTemplate alone, and it is therefore testable
// deterministically, with no network, no credentials, and no model.
//
// WHY ORDERING IS PART OF THE CONTRACT. The prompt's own structure is the
// proximate cause: the examples were the only act-shaped content, and they
// sat where the decision policy should have been. A termination condition
// stated AFTER the examples is read as a footnote to them rather than as
// the rule that selects between them. So this file asserts placement, not
// just presence: every clause must first appear BEFORE the first canonical
// act example.
//
// WHAT IS NOT ASSERTED HERE, on purpose.
//   - No assertion that the model behaves. Prompt text is not behaviour and
//     a text contract must not be sold as one; the live run measures that.
//   - No per-model sequencing advice. "After get_evidence, always
//     submit_report" would overfit one scenario, would be false for every
//     other envelope, and would convert a general policy into a lookup
//     table. Every clause below is scenario-independent.
//   - No schema, decoder, grounding, budget, or repetition-guard
//     assertion. Those are other files' contracts and they must keep
//     passing unchanged.
package orchestrate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Normalisation: match a clause that spans lines, while still being able to
// say WHERE in the prompt it was found
// ---------------------------------------------------------------------------

// apa50Normalize lowercases s and collapses every whitespace run to a single
// space, so a rule split across bullet lines is matchable as one phrase. It
// returns the normalized text alongside, for each normalized byte, the index
// of the raw byte it came from, which is what lets an ordering assertion be
// made about a phrase that was located through normalization.
func apa50Normalize(s string) (string, []int) {
	var b strings.Builder
	b.Grow(len(s))
	off := make([]int, 0, len(s))
	inSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case ' ', '\t', '\n', '\r':
			if !inSpace {
				b.WriteByte(' ')
				off = append(off, i)
				inSpace = true
			}
			continue
		}
		inSpace = false
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
		off = append(off, i)
	}
	return b.String(), off
}

// apa50First returns the raw index of the first occurrence of needle in the
// normalized text, or -1 when absent.
func apa50First(norm string, offsets []int, needle string) int {
	i := strings.Index(norm, needle)
	if i < 0 {
		return -1
	}
	return offsets[i]
}

// apa50AnyFirst returns the raw index of the earliest occurrence among the
// needles, or -1 when none is present.
func apa50AnyFirst(norm string, offsets []int, needles []string) int {
	best := -1
	for _, n := range needles {
		if at := apa50First(norm, offsets, n); at >= 0 && (best < 0 || at < best) {
			best = at
		}
	}
	return best
}

// apa50FirstExampleLine returns the line index, within prose, of the first
// canonical act example: the first line that is itself a complete, parseable
// JSON object carrying the action discriminator. A shape built from
// placeholders is not parseable and therefore is not the attractor, so it is
// not what ordering must be measured against.
func apa50FirstExampleLine(t *testing.T, prose string) int {
	t.Helper()
	for i, line := range strings.Split(prose, "\n") {
		candidate := strings.TrimSpace(line)
		if !strings.HasPrefix(candidate, "{") || !apa50ValidJSON(candidate) {
			continue
		}
		if apa50ActionOf(candidate) != "" {
			return i
		}
	}
	t.Fatalf("the prompt carries no parseable canonical act example, so the ordering "+
		"assertion has nothing to order against.\n--- prose ---\n%s", prose)
	return -1
}

// ---------------------------------------------------------------------------
// The clause table
// ---------------------------------------------------------------------------

// apa50Clause is one required statement of the behavioural policy.
//
// anyOf is satisfied by any one phrasing: the test requires the PROMPT to
// commit to the idea, not to one exact sentence, so the wording can stay
// compact and readable without becoming a contract in itself.
//
// alsoAnyOf is a conjunction of disjunctions: every inner group must be
// satisfied at least once by the same clause. This is what stops a clause
// from being satisfied by an unrelated sentence that happens to contain one
// keyword, and it is where the substance of the policy is pinned.
type apa50Clause struct {
	name      string
	anyOf     []string
	alsoAnyOf [][]string
	why       string
}

// apa50Clauses is the state-transition policy the prompt must state, in the
// order the policy itself reads.
func apa50Clauses() []apa50Clause {
	return []apa50Clause{
		{
			name: "no_identical_repeat_after_a_tool_result",
			anyOf: []string{
				"do not repeat the same tool call with the same arguments",
				"never repeat a tool call with the same arguments",
				"never repeat the same tool call",
				"never repeat an identical tool call",
				"do not repeat an identical tool call",
			},
			alsoAnyOf: [][]string{
				{"repeat", "repeating", "re-issuing", "reissue", "duplicate"},
				{"same arguments", "identical arguments", "same tool call",
					"identical tool call", "same request", "identical request"},
				{"after a successful tool result", "after a tool result",
					"after a successful tool call", "after an earlier tool result",
					"after a successful tool", "after a tool call"},
			},
			why: "The observed real-model failure was a byte-identical call_tool act " +
				"on all 6 loop turns. Nothing in the prompt forbade it; the loop had to.",
		},
		{
			name: "tool_call_only_when_evidence_is_insufficient",
			anyOf: []string{
				"only call another tool when",
				"call another tool only when",
				"only call a tool when",
				"call a tool only when",
				"call one more tool only when",
				"only call one more tool when",
			},
			alsoAnyOf: [][]string{
				{"insufficient", "not enough", "lacking", "incomplete", "still missing", "missing"},
				{"report field", "required report field", "required field", "report"},
			},
			why: "Tool-calling needs a stated justification or it defaults to always, which " +
				"is the other half of the same defect: an unconditional tool call.",
		},
		{
			name: "or_an_allowed_next_step_tool_is_genuinely_needed",
			anyOf: []string{
				"or an allowed", "or an allowlisted", "or a genuinely",
			},
			alsoAnyOf: [][]string{
				{"next-step tool", "next step tool", "next-step"},
				{"genuinely needed", "genuinely required", "truly needed", "genuinely useful", "needed"},
			},
			why: "The second, legitimate reason to keep calling tools. Stating only the " +
				"first reason would push the model to submit on the first turn instead.",
		},
		{
			name: "terminate_with_report_when_evidence_is_sufficient",
			anyOf: []string{
				"sufficient grounded evidence", "sufficient evidence",
				"enough grounded evidence", "grounded evidence is sufficient",
			},
			alsoAnyOf: [][]string{{ActionSubmitReport}},
			why: "Termination branch one. Without it the model has no stated reason to stop " +
				"calling tools.",
		},
		{
			name: "call_the_resolving_tool_when_a_gap_is_resolvable",
			anyOf: []string{
				"an allowed tool can resolve", "an allowlisted tool can resolve",
				"a tool can resolve", "can resolve the gap", "can close the gap",
				"can supply", "can fetch",
			},
			alsoAnyOf: [][]string{
				{"call that tool", "call it", "call that one", "call the tool",
					"call a tool", "call the next"},
			},
			why: "Termination branch two: the gap is real and reachable, so the correct act " +
				"is a tool call, not a report.",
		},
		{
			name: "report_the_gap_when_no_allowed_tool_can_resolve_it",
			anyOf: []string{
				"no allowed tool can resolve", "no allowlisted tool can resolve",
				"no tool can resolve", "no remaining tool can resolve",
			},
			alsoAnyOf: [][]string{
				{`"missing_additive"`, "missing_additive"},
				{"missing-information", "missing information", "missing item",
					"missing items", "what is missing"},
			},
			why: "Termination branch three, and the one that turns a stuck loop into a " +
				"correct answer: report the missing information instead of retrying.",
		},
		{
			name: "never_repeat_an_identical_action_request_pair",
			anyOf: []string{
				"never repeat an identical action/request pair",
				"never repeat an identical action and request pair",
				"never repeat an identical action or request pair",
				"never repeat an identical request pair",
				"never repeat an identical action",
			},
			alsoAnyOf: [][]string{
				{"action/request", "action and request", "action or request",
					"request pair", "action pair"},
			},
			why: "The loop keys the repetition guard on the tool plus the canonical request " +
				"bytes (loop.go:667, repetitionKey at loop.go:330). The prompt has to " +
				"describe the same thing in model-facing terms.",
		},
		{
			name: "examples_illustrate_structure_and_the_policy_chooses",
			anyOf: []string{
				"illustrate the structure", "illustrate structure",
				"show the structure", "show structure",
				"illustrate its structure", "illustrate only the structure",
			},
			alsoAnyOf: [][]string{
				{"policy above", "the policy", "policy decides", "policy governs",
					"policy that governs", "governed by the policy", "policy rules",
					"state policy", "policy selects", "policy decides between"},
			},
			why: "The attractor itself. The examples demonstrate shape; the choice between " +
				"them belongs to the policy. Left unqualified, the nearest template wins.",
		},
	}
}

// ---------------------------------------------------------------------------
// (a) The policy must be stated at all
// ---------------------------------------------------------------------------

// TestPromptStatePolicy_TerminationPolicyIsStated is the RED proof for
// APA-50.
//
// Against the pre-change prompt every clause below fails: the prompt
// described the two acts and their shapes exhaustively, and said nothing at
// all about when to leave one and enter the other.
func TestPromptStatePolicy_TerminationPolicyIsStated(t *testing.T) {
	prose := apa49Prose(t, apa49RenderPrompt(t))
	norm, offsets := apa50Normalize(prose)

	for _, c := range apa50Clauses() {
		t.Run(c.name, func(t *testing.T) {
			if apa50AnyFirst(norm, offsets, c.anyOf) < 0 {
				t.Errorf("the prompt never states this part of the transition policy.\n"+
					"Why it matters: %s\nThe prompt states what each act looks like and never "+
					"states when to choose one over the other, so the model has no basis for "+
					"transitioning and copies the nearest example.\n--- prose ---\n%s",
					c.why, prose)
				return
			}
			for i, group := range c.alsoAnyOf {
				if apa50AnyFirst(norm, offsets, group) < 0 {
					t.Errorf("the policy statement for %q is present but does not also carry "+
						"one of %v (group %d).\nA keyword-level match is not a policy: the "+
						"clause has to name the whole rule or the model still has to guess.\n"+
						"Why it matters: %s\n--- prose ---\n%s", c.name, group, i, c.why, prose)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// (b) The policy must precede the canonical examples
// ---------------------------------------------------------------------------

// apa50OrderingProblems returns, as data, every way prose fails the ordering
// contract: a policy clause that is stated only after the first canonical act
// example, or a clause that is not stated at all and therefore cannot be
// placed. It is pure so that a synthetic prompt can be used to prove the
// check has teeth, and so that this file never has to express the contract
// twice.
func apa50OrderingProblems(t *testing.T, prose string, clauses []apa50Clause) []string {
	t.Helper()
	norm, offsets := apa50Normalize(prose)
	exampleAt := apa50RawOffsetOfLine(prose, apa50FirstExampleLine(t, prose))

	var problems []string
	for _, c := range clauses {
		at := apa50AnyFirst(norm, offsets, c.anyOf)
		switch {
		case at < 0:
			// Absence is reported in detail by the presence test. Here it is
			// still a failure, because a clause that is not stated cannot be
			// positioned: leaving it out would make this whole file pass
			// vacuously against a prompt that teaches nothing.
			problems = append(problems, fmt.Sprintf(
				"policy clause %q is not stated at all, so it cannot be placed before the "+
					"examples; a missing policy is also an absent ordering", c.name))
		case at > exampleAt:
			problems = append(problems, fmt.Sprintf(
				"policy clause %q first appears at offset %d, AFTER the first canonical act "+
					"example at offset %d (prose line %d); the examples are the nearest template "+
					"in the prompt, so a rule stated after them is read as a footnote to them",
				c.name, at, exampleAt, apa50LineOfOffset(prose, at)))
		}
	}
	return problems
}

// TestPromptStatePolicy_PolicyPrecedesTheCanonicalExamples pins the
// ordering, which is part of the defect rather than a stylistic preference.
//
// The pre-change prompt put the examples immediately after the shape rules,
// so the only act-shaped content in the prompt was a tool call followed by a
// report, and the model anchored on the first. A termination condition
// printed after the examples reads as commentary on them rather than as the
// rule that selects between them, which is why presence alone would not have
// been enough to fix this.
func TestPromptStatePolicy_PolicyPrecedesTheCanonicalExamples(t *testing.T) {
	prose := apa49Prose(t, apa49RenderPrompt(t))
	for _, p := range apa50OrderingProblems(t, prose, apa50Clauses()) {
		t.Errorf("ordering: %s\n--- prose ---\n%s", p, prose)
	}
}

// TestPromptStatePolicy_OrderingCheckHasTeeth is the anti-vacuity proof for
// the ordering check above: it must reject a prompt that carries a complete
// policy AFTER the examples.
//
// The policy block is generated from the clause table's own canonical
// phrasings, so the locator is guaranteed to find every clause and the ONLY
// variable under test is placement. The example block is the production one,
// so the "first canonical act example" anchor is exercised against the real
// shape. Without this, an ordering check that returned nothing for any input
// would look exactly like a satisfied one.
func TestPromptStatePolicy_OrderingCheckHasTeeth(t *testing.T) {
	rendered := apa49Prose(t, apa49RenderPrompt(t))
	exampleBlock := strings.TrimSpace(
		rendered[apa50RawOffsetOfLine(rendered, apa50FirstExampleLine(t, rendered)):])
	if !strings.Contains(exampleBlock, ActionCallTool) ||
		!strings.Contains(exampleBlock, ActionSubmitReport) {
		t.Fatalf("the extracted example block does not carry both acts, so the teeth proof "+
			"would be measuring the wrong thing:\n%s", exampleBlock)
	}

	const rules = `You are a bounded claim-investigation planner.

Rules:
- Respond with exactly one JSON object.
`
	// Correct order: policy, then examples. This is the shape the fix adopts.
	ordered := rules + "\n" + apa50PolicySkeleton() + "\n\nCanonical act examples:\n" + exampleBlock
	if problems := apa50OrderingProblems(t, ordered, apa50Clauses()); len(problems) != 0 {
		t.Fatalf("the ordering check rejected a correctly ordered synthetic prompt: %v\n%s",
			problems, ordered)
	}

	// Inverted order: examples, then policy. Every clause is still stated, so
	// the only reason to reject this is placement.
	inverted := rules + "\nCanonical act examples:\n" + exampleBlock + "\n\n" + apa50PolicySkeleton()
	problems := apa50OrderingProblems(t, inverted, apa50Clauses())
	moved := 0
	for _, p := range problems {
		if strings.Contains(p, "AFTER the first canonical act example") {
			moved++
		}
	}
	if moved == 0 {
		t.Fatalf("the ordering check did not report a placement violation for a prompt whose "+
			"policy sits entirely AFTER the examples, so it cannot see placement.\nproblems=%v\n"+
			"--- inverted prompt ---\n%s", problems, inverted)
	}
	t.Logf("APA50-ORDERING-TEETH ordered prompt accepted; inverted prompt rejected with %d "+
		"placement violation(s): %s", moved, problems[0])
}

// apa50PolicySkeleton renders one bullet per clause using that clause's own
// canonical phrasing, so a synthetic prompt is guaranteed to state every
// clause the checker looks for. It deliberately says nothing about
// production: it exists only so placement can be varied while the clause set
// is held fixed.
func apa50PolicySkeleton() string {
	var b strings.Builder
	b.WriteString("State policy:\n")
	for _, c := range apa50Clauses() {
		b.WriteString("- " + c.anyOf[0] + "\n")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// (c) The superseded imperative must be gone
// ---------------------------------------------------------------------------

// TestPromptStatePolicy_TheCopyExamplesImperativeIsGone pins the removal of
// the instruction that caused the failure.
//
// "Copy their structure; substitute only the text and the IDs you were
// given" is not wrong as far as it goes, but it was the ONLY instruction
// governing the examples, and "copy" is the verb that invites the model to
// reproduce the first example verbatim. The replacement must still teach the
// examples' structure, so this test does not forbid the examples; it forbids
// the unqualified copy imperative, and the presence test requires the
// qualified statement in its place.
func TestPromptStatePolicy_TheCopyExamplesImperativeIsGone(t *testing.T) {
	prose := apa49Prose(t, apa49RenderPrompt(t))
	norm, _ := apa50Normalize(prose)

	for _, banned := range []string{
		"copy their structure",
		"copy its structure",
		"copy the structure",
		"copy them",
	} {
		if at := strings.Index(norm, banned); at >= 0 {
			t.Errorf("the prompt still tells the model to %q (offset %d).\n"+
				"That imperative is what produced a byte-identical call_tool act on every "+
				"turn of the last live run. The examples may still show structure; the "+
				"instruction to copy them verbatim must be replaced by the statement that "+
				"they illustrate structure and the policy chooses between them.\n"+
				"--- prose ---\n%s", banned, at, prose)
		}
	}
}

// ---------------------------------------------------------------------------
// (d) The policy must not smuggle in a schema change
// ---------------------------------------------------------------------------

// TestPromptStatePolicy_PolicyAdvertisesNoUndecodableShape keeps this change
// strictly a behavioural one.
//
// The policy section is prose about decisions. If it ever grows a brace-
// balanced object literal, that literal becomes a shape the prompt teaches,
// and action_field_contract_test.go requires every such literal to carry only
// properties ModelAction decodes. Asserting it here keeps the new section
// honest on its own terms instead of relying on another file to notice.
func TestPromptStatePolicy_PolicyAdvertisesNoUndecodableShape(t *testing.T) {
	prose := apa49Prose(t, apa49RenderPrompt(t))
	allowed := apa49ModelActionJSONKeys()

	for n, keys := range apa49TopLevelKeys(prose) {
		for _, k := range keys {
			if !apa49HasKey(allowed, k) {
				t.Errorf("prompt shape %d advertises property %q, which the strict decoder "+
					"refuses (decodable: %v).\nA behaviour-only change must not describe a "+
					"field the contract does not accept.", n, k, allowed)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// apa50ValidJSON reports whether s is a single complete JSON document.
func apa50ValidJSON(s string) bool { return json.Valid([]byte(s)) }

// apa50ActionOf returns the action discriminator of a JSON object, or "".
func apa50ActionOf(s string) string {
	var probe struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal([]byte(s), &probe); err != nil {
		return ""
	}
	return probe.Action
}

// apa50LineOfOffset returns the 0-based line index containing raw offset at.
func apa50LineOfOffset(s string, at int) int {
	if at <= 0 {
		return 0
	}
	line := 0
	for i := 0; i < at && i < len(s); i++ {
		if s[i] == '\n' {
			line++
		}
	}
	return line
}

// apa50RawOffsetOfLine returns the byte offset at which 0-based line begins.
func apa50RawOffsetOfLine(s string, line int) int {
	if line <= 0 {
		return 0
	}
	seen := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '\n' {
			continue
		}
		seen++
		if seen == line {
			return i + 1
		}
	}
	return len(s)
}
