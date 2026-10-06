package orchestrate

// APA-56: the model-facing "missing_additive" contract correction.
//
// The defect this pins is a documentation gap in the prompt, not a validator
// defect and not a model defect. Before the change the prompt closed the
// "kind" set ("required_document", "field", "external") but never stated
// which vocabulary each kind's "key" is drawn from, and its only worked
// example carried an empty array. Qwen therefore paired the real claim field
// "policy_number" with kind "external", and validateMissingAdditive
// rejected the report:
//
//	orchestrate: missing_additive[0] unknown external source "policy_number"
//
// That happened on 17 of 17 measured live runs, including the
// sufficient-evidence control, so the control was INCORRECTLY-DENIED and the
// grounding/evidence/tenant boundaries under test were never reached.
//
// These tests hold three things fixed:
//   - the prompt must state the kind -> key mapping, and must name
//     "policy_number" as a field, because that is the exact fact the model
//     lacked;
//   - the prompt's external vocabulary must equal invest's authority, so the
//     two cannot drift apart silently;
//   - the validator must keep rejecting the observed bad pair and keep
//     accepting the corrected one. The validator is NOT to be weakened.
//
// The prompt's own non-empty examples are executed against the real decoder
// and validator: an example the contract rejects is worse than no example.

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	"claimops-api/internal/invest"
)

var apa56ExternalClause = regexp.MustCompile(
	`"external": the "key" is exactly one of ([^.]+)\.`)

// TestAPA56_PromptStatesTheKindKeyContract asserts the correction is present
// in the rendered prompt. It fails if the mapping is removed, or reduced back
// to a bare mention of the kind set without the per-kind key vocabulary.
func TestAPA56_PromptStatesTheKindKeyContract(t *testing.T) {
	prose := apa49Prose(t, apa49RenderPrompt(t))

	t.Run("states_each_kinds_key_vocabulary", func(t *testing.T) {
		// Each kind must be tied to the vocabulary its key comes from, not
		// merely listed as a legal kind value.
		for _, want := range []string{
			`"required_document": the "key" is one of the required document types`,
			`"field": the "key" is a claim field key that appears in the DATA block`,
			`"external": the "key" is exactly one of`,
		} {
			if !strings.Contains(prose, want) {
				t.Errorf("prompt never states %q, so the model cannot tell which\n"+
					"vocabulary a %q key is drawn from.\nprose:\n%s", want, "missing_additive", prose)
			}
		}
	})

	t.Run("names_policy_number_as_a_field_not_an_external", func(t *testing.T) {
		// The single most important sentence: the observed failure was
		// policy_number paired with kind "external". Naming the case closes
		// it; enumerating the four external keys alone would not.
		for _, want := range []string{
			`"policy_number" is a claim field`,
			`It is never "kind" "external"`,
		} {
			if !strings.Contains(prose, want) {
				t.Errorf("prompt does not state %q; this is the exact fact\n"+
					"that produced the observed incorrect denial.\nprose:\n%s", want, prose)
			}
		}
	})

	t.Run("states_the_carry_forward_rule", func(t *testing.T) {
		// Grounding requires the submitted list to be a superset of the
		// envelope's missing_evidence, so the model must be told to carry
		// those entries across rather than re-derive them.
		for _, want := range []string{
			`Carry every entry of the DATA block's "missing_evidence"`,
			`never drop one`,
		} {
			if !strings.Contains(prose, want) {
				t.Errorf("prompt does not state %q; the carry-forward rule is\n"+
					"what keeps grounding's additive-superset check satisfiable.\nprose:\n%s", want, prose)
			}
		}
	})

	t.Run("states_the_actual_sort_order", func(t *testing.T) {
		// validateMissingAdditive compares (kind, key, detail) with a NUL
		// separator. "keep the array sorted" alone did not tell the model
		// what the tuple was.
		if !strings.Contains(prose, `Sort the array by "kind", then "key", then "detail"`) {
			t.Errorf("prompt does not state the additive sort tuple; the\n" +
				"validator's own order is (kind, key, detail).")
		}
	})
}

// TestAPA56_ExternalVocabularyMatchesAuthority is the divergence test. The
// prompt's external key list must equal invest's closed vocabulary exactly:
// no missing key (the model would guess it) and no extra key (the model
// would be taught a key the validator rejects). Adding a fifth external
// source without updating the prompt fails here.
func TestAPA56_ExternalVocabularyMatchesAuthority(t *testing.T) {
	prose := apa49Prose(t, apa49RenderPrompt(t))

	m := apa56ExternalClause.FindStringSubmatch(prose)
	if m == nil {
		t.Fatal("prompt does not state an external key vocabulary; the\n" +
			"divergence check needs the clause to be present and unambiguous")
	}
	var got []string
	for _, part := range strings.Split(m[1], ",") {
		key := strings.Trim(strings.TrimSpace(part), `"`)
		if key == "" {
			t.Fatalf("unparseable external key in prompt clause %q", m[1])
		}
		got = append(got, key)
	}

	want := invest.ExternalSourceKeys()
	if slices.Equal(got, want) {
		return
	}
	if slices.Contains(got, "policy_number") {
		t.Fatalf("prompt teaches %q as an external key, which is a claim "+
			"field: that is the defect this issue fixed", "policy_number")
	}
	t.Errorf("prompt external vocabulary drifted from the authority\n"+
		"prompt:    %v\nauthority: %v\n"+
		"Both must change together: the prompt states what the model may\n"+
		"cite, invest decides what is accepted.", got, want)
}

// TestAPA56_PolicyNumberClassifiedAsFieldIsAccepted is the corrected half of
// the causal experiment: the same key the model actually emitted is accepted
// once it carries the kind the contract states.
func TestAPA56_PolicyNumberClassifiedAsFieldIsAccepted(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	rep := testReport(env)
	rep.MissingAdditive = []invest.MissingItem{{
		Kind:   invest.MissingField,
		Key:    "policy_number",
		Detail: "R1: claimed value conflicts with the policy schedule value",
	}}
	raw, err := json.Marshal(ModelAction{Action: ActionSubmitReport, Report: &rep})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	a, err := DecodeModelAction(raw, 1<<20)
	if err != nil {
		t.Fatalf("decode rejected a corrected additive: %v", err)
	}
	if err := ValidateModelAction(a, scope, env.InvestigationID); err != nil {
		t.Fatalf("ValidateModelAction rejected kind \"field\" key \"policy_number\": %v", err)
	}
	known, err := SeedKnownEvidence(env)
	if err != nil {
		t.Fatalf("SeedKnownEvidence: %v", err)
	}
	if err := CheckReportGrounding(*a.Report, known, env); err != nil {
		t.Fatalf("grounding rejected kind \"field\" key \"policy_number\": %v", err)
	}
}

// TestAPA56_PolicyNumberClassifiedAsExternalIsRejected pins the observed
// failure so the validator can never be weakened into accepting it. The
// message is asserted because that exact string is the evidence in APA-55.
func TestAPA56_PolicyNumberClassifiedAsExternalIsRejected(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	rep := testReport(env)
	rep.MissingAdditive = []invest.MissingItem{{
		Kind:   invest.MissingExternal,
		Key:    "policy_number",
		Detail: "R1: no pinned policy evidence",
	}}
	raw, err := json.Marshal(ModelAction{Action: ActionSubmitReport, Report: &rep})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	a, err := DecodeModelAction(raw, 1<<20)
	if err != nil {
		return // rejected at the decoder, which is also containment
	}
	err = ValidateModelAction(a, scope, env.InvestigationID)
	if err == nil {
		t.Fatal(`kind "external" key "policy_number" was accepted; the ` +
			"validator is the containment boundary and must reject it")
	}
	if !strings.Contains(err.Error(), `unknown external source "policy_number"`) {
		t.Fatalf("rejected for the wrong reason: %v", err)
	}
	if !errors.Is(err, ErrModelContract) {
		t.Fatalf("err = %v, want an ErrModelContract classification", err)
	}
}

// TestAPA56_CanonicalNonEmptyExamplesSurviveTheContract executes every
// non-empty missing_additive example the prompt carries against the real
// decoder and validator. The prompt is the model's only spec, so an example
// the contract rejects teaches the model to fail.
func TestAPA56_CanonicalNonEmptyExamplesSurviveTheContract(t *testing.T) {
	prose := apa49Prose(t, apa49RenderPrompt(t))
	env := testEnvelope(t)
	scope := testScope(env)

	seen := 0
	for _, line := range strings.Split(prose, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `{"action":"submit_report"`) {
			continue
		}
		if !strings.Contains(line, `"missing_additive":[`) || strings.Contains(line, `"missing_additive":[]`) {
			continue
		}
		seen++

		a, err := DecodeModelAction([]byte(line), 1<<20)
		if err != nil {
			t.Errorf("prompt example %d is rejected by the decoder: %v\n%s", seen, err, line)
			continue
		}
		if err := ValidateModelAction(a, scope, env.InvestigationID); err != nil {
			t.Errorf("prompt example %d is rejected by the validator: %v\n%s", seen, err, line)
			continue
		}
		known, err := SeedKnownEvidence(env)
		if err != nil {
			t.Fatalf("SeedKnownEvidence: %v", err)
		}
		if err := CheckReportGrounding(*a.Report, known, env); err != nil {
			t.Errorf("prompt example %d is rejected by grounding: %v\n%s", seen, err, line)
		}
	}
	if seen == 0 {
		t.Fatal("prompt carries no non-empty missing_additive example; an empty\n" +
			"array teaches the model nothing about the kind -> key mapping")
	}
}
