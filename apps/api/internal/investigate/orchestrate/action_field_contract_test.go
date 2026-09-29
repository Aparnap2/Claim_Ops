// APA-49: the production prompt/decoder contract for the act discriminator.
//
// The defect this pins: PromptTemplate told the model it must answer with
// "either a call_tool act or a submit_report act". The ADR-002 canonical
// model (qwen/qwen3.8-27b) complied with that prose literally and emitted
// {"act":"call_tool",...}. ModelAction tags the discriminator `json:"action"`
// and DecodeModelAction runs with DisallowUnknownFields, so that payload
// classifies I1-malformed, the loop escalates INVALID_OUTPUT, and the run
// ends with zero valid actions. Reproduced against the live provider with
// the production prompt, 3/3 repeats.
//
// The architecture is unchanged and is what this file defends: LLM output
// enters a strict contract, and the contract is authoritative. The decoder
// is NOT widened. There is no "act" alias, no dual-key acceptance, no
// forgiving parser, and no provider-specific leniency. A model that emits
// the wrong property name is a model-quality failure that the boundary
// contains, exactly as it contained it in the live run.
//
// Two test kinds, both deterministic, neither touching the network:
//
//	(a) the prompt CONTRACT: the rendered prompt must name the JSON property
//	    literally and show the literal shape of each act, so it cannot
//	    describe a field the decoder refuses.
//	(b) the decoder CONTRACT: "action" accepts, "act" is refused, unknown
//	    fields are refused, and the refusal is a clean escalation that
//	    executes nothing.
package orchestrate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// apa49Prose returns the prompt's instruction block, i.e. everything before
// the DATA delimiter. The canonical request JSON that follows is data, not
// instruction, and must not be scanned as if the model had been told to
// emit it.
func apa49Prose(t *testing.T, prompt string) string {
	t.Helper()
	idx := strings.Index(prompt, "DATA:")
	if idx < 0 {
		t.Fatalf("prompt has no DATA: block; the template contract is broken")
	}
	return prompt[:idx]
}

// apa49ModelActionJSONKeys returns the wire property names ModelAction
// actually decodes, read off the struct tags rather than restated here, so
// a schema change is visible to this file rather than silently diverging
// from it.
func apa49ModelActionJSONKeys() []string {
	typ := reflect.TypeOf(ModelAction{})
	out := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		out = append(out, name)
	}
	return out
}

// apa49HasKey reports whether keys contains want.
func apa49HasKey(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}

// apa49TopLevelKeys returns the depth-1 property names of every
// brace-balanced object literal in s, in source order.
//
// It is a small scanner rather than encoding/json because the shapes a
// prompt shows are templates: their nested values may be placeholders, so
// they are not necessarily parseable JSON. The property NAMES, which are
// what the contract is about, always are. Nested objects and arrays are
// tracked for balance and skipped, so a nested key is never mistaken for a
// top-level one.
func apa49TopLevelKeys(s string) [][]string {
	var out [][]string
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		keys, end, ok := apa49ScanObject(s, i)
		if !ok {
			continue
		}
		out = append(out, keys)
		i = end - 1
	}
	return out
}

// apa49ScanObject scans the object literal opening at s[start] and returns
// its depth-1 property names plus the index just past its closing brace.
func apa49ScanObject(s string, start int) (keys []string, end int, ok bool) {
	depth := 0
	inStr, esc := false, false
	expectKey := false
	pending := ""
	hasKey := false

	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
				if depth == 1 && hasKey {
					keys = append(keys, pending)
					hasKey = false
					expectKey = false
				}
			default:
				if depth == 1 && expectKey {
					pending += string(c)
				}
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
			if depth == 1 && expectKey {
				pending = ""
				hasKey = true
			}
		case '{', '[':
			depth++
			if depth == 1 {
				expectKey = true
			}
		case '}', ']':
			depth--
			if depth == 0 {
				return keys, i + 1, true
			}
			if depth == 1 {
				expectKey = false
			}
		case ':':
			if depth == 1 {
				expectKey = false
			}
		case ',':
			if depth == 1 {
				expectKey = true
			}
		}
	}
	return nil, 0, false
}

// apa49AsActField re-keys a valid act payload from "action" to the given
// discriminator name, leaving every other byte untouched. This is exactly
// the transformation the real model applied: the act was otherwise
// well-formed and otherwise plausible.
func apa49AsActField(t *testing.T, valid []byte, field string) []byte {
	t.Helper()
	if !bytes.Contains(valid, []byte(`"action"`)) {
		t.Fatalf("base act does not carry the action property: %s", valid)
	}
	out := bytes.Replace(valid, []byte(`"action"`), []byte(`"`+field+`"`), 1)
	if !json.Valid(out) {
		t.Fatalf("re-keyed act is not valid JSON: %s", out)
	}
	return out
}

// ---------------------------------------------------------------------------
// (a) The prompt contract
// ---------------------------------------------------------------------------

// TestPromptContract_ActionIsTheNamedJSONField is the RED proof for the
// prompt half of APA-49.
//
// It asserts two things about the rendered production prompt: it names the
// JSON property "action" literally rather than describing it in prose, and
// it shows the literal shape of each act with only properties the decoder
// accepts. Then it drives a real-model-shaped response, the one the real
// ADR-002 model actually produced, through the production decoder and
// requires it to be refused.
//
// Deterministic: no network, no credentials, no model.
func TestPromptContract_ActionIsTheNamedJSONField(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	req := ModelRequest{
		Exception:        env,
		History:          []TurnRecord{},
		KnownEvidenceIDs: KnownIDs(mustSeed(t, env)),
		Turn:             1,
		RequestID:        scope.RequestID,
	}
	prompt, err := RenderPrompt(req)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	prose := apa49Prose(t, prompt)

	t.Run("names_the_action_json_property", func(t *testing.T) {
		// The defect was prose that never said which JSON property carried
		// the discriminator. The model filled the gap from the English verb
		// and chose "act". The property name must now appear literally,
		// quoted, alongside both of its closed values.
		for _, want := range []string{`"action"`, `"call_tool"`, `"submit_report"`} {
			if !strings.Contains(prose, want) {
				t.Errorf("prompt does not name %s literally.\nThe discriminator must be stated as a "+
					"JSON property, not described in prose: a model that reads \"act\" as the field "+
					"name emits {\"act\":...}, which the strict decoder refuses.\n--- prose ---\n%s",
					want, prose)
			}
		}
	})

	t.Run("forbids_the_act_property_by_name", func(t *testing.T) {
		// Naming the property is necessary but not sufficient. The prompt
		// must also close the door on the reading that caused the failure,
		// so a model that still reaches for "act" has been told no.
		if !strings.Contains(prose, `never "act"`) {
			t.Errorf("prompt does not rule out the \"act\" property name.\n--- prose ---\n%s", prose)
		}
	})

	t.Run("shows_literal_shapes_for_both_acts", func(t *testing.T) {
		shapes := apa49TopLevelKeys(prose)
		if len(shapes) < 2 {
			t.Fatalf("prompt shows %d act object shape(s), want at least 2 (one per act).\n"+
				"A shape is the only unambiguous way to tell a model which properties exist.\n"+
				"--- prose ---\n%s", len(shapes), prose)
		}
		allowed := apa49ModelActionJSONKeys()
		for n, keys := range shapes {
			if !apa49HasKey(keys, "action") {
				t.Errorf("shape %d (%v) does not carry the \"action\" discriminator", n, keys)
			}
			for _, k := range keys {
				if k == "act" {
					t.Errorf("shape %d advertises the \"act\" property, which the strict decoder "+
						"refuses as an unknown field: %v", n, keys)
					continue
				}
				if !apa49HasKey(allowed, k) {
					t.Errorf("shape %d advertises property %q, which ModelAction does not decode "+
						"(decodable: %v): the prompt would be describing a field the contract refuses",
						n, k, allowed)
				}
			}
		}
	})

	t.Run("real_model_shaped_act_property_is_refused", func(t *testing.T) {
		// The payload the real model actually returned against the broken
		// prompt, re-keyed to "act" and otherwise untouched.
		shape := apa49AsActField(t, callToolBytes(t, env, scope, "get_evidence", 1), "act")
		act, err := DecodeModelAction(shape, DefaultMaxOutputBytes)
		if err == nil {
			t.Fatalf("strict decoder ACCEPTED a payload keyed %q and produced action %q.\n"+
				"The decoder contract is authoritative and must not be widened: %s",
				"act", act.Action, shape)
		}
		if !errors.Is(err, ErrModelContract) {
			t.Errorf("err = %v, want ErrModelContract", err)
		}
		if k := invalidKindOf(err); k != invalidMalformed {
			t.Errorf("kind = %s, want %s", invalidKindString(k), invalidKindString(invalidMalformed))
		}
		if !strings.Contains(err.Error(), `unknown field "act"`) {
			t.Errorf("error %q does not name the offending property, so the refusal is not diagnosable", err)
		}
	})
}

// ---------------------------------------------------------------------------
// (b) The decoder contract
// ---------------------------------------------------------------------------

// TestDecoderContract_ActionFieldIsAuthoritative is the regression pin for
// the exact defect, so it cannot be re-introduced by any later change.
//
// Four required properties, each pinned directly:
//   - {"action":"call_tool",...}     is accepted
//   - {"action":"submit_report",...} is accepted
//   - {"act":"call_tool",...}        is REJECTED
//   - unknown fields                 are REJECTED
//
// Plus the two structural properties that make those pins mean something:
// the discriminator tag is literally "action" (so the fix was not a rename),
// and the prompt advertises nothing the decoder refuses (so the prompt and
// the decoder cannot drift apart again, which is how this defect arose).
func TestDecoderContract_ActionFieldIsAuthoritative(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	callTool := callToolBytes(t, env, scope, "get_evidence", 1)
	submit := submitBytes(t, testReport(env))

	t.Run("discriminator_tag_is_action", func(t *testing.T) {
		field, ok := reflect.TypeOf(ModelAction{}).FieldByName("Action")
		if !ok {
			t.Fatal("ModelAction has no Action field")
		}
		if got := field.Tag.Get("json"); got != "action" {
			t.Fatalf("ModelAction.Action tag = %q, want %q: the decoder contract is authoritative "+
				"and its name is fixed, not a matter of preference", got, "action")
		}
	})

	t.Run("act_key_is_not_a_second_discriminator", func(t *testing.T) {
		// "Fixing" the production failure by accepting both "act" and
		// "action" would turn a strict contract into a forgiving parser and
		// is explicitly out of bounds. The schema must carry exactly one
		// discriminator and "act" must not be among the decodable names.
		keys := apa49ModelActionJSONKeys()
		if apa49HasKey(keys, "act") {
			t.Fatalf("ModelAction decodes an \"act\" property: %v. The contract is strict by "+
				"design; a second discriminator is a schema change, not a fix", keys)
		}
		if n := len(keys); n != 4 {
			t.Fatalf("ModelAction decodes %d properties (%v), want exactly 4 "+
				"(action, tool, request, report): the 2-act schema must not have grown", n, keys)
		}
	})

	t.Run("action_call_tool_accepted", func(t *testing.T) {
		act, err := DecodeModelAction(callTool, DefaultMaxOutputBytes)
		if err != nil {
			t.Fatalf("DecodeModelAction refused a well-formed call_tool act: %v\n%s", err, callTool)
		}
		if act.Action != ActionCallTool {
			t.Errorf("Action = %q, want %q", act.Action, ActionCallTool)
		}
		if err := ValidateModelAction(act, scope, env.InvestigationID); err != nil {
			t.Errorf("ValidateModelAction refused a well-formed call_tool act: %v", err)
		}
	})

	t.Run("action_submit_report_accepted", func(t *testing.T) {
		act, err := DecodeModelAction(submit, DefaultMaxOutputBytes)
		if err != nil {
			t.Fatalf("DecodeModelAction refused a well-formed submit_report act: %v\n%s", err, submit)
		}
		if act.Action != ActionSubmitReport {
			t.Errorf("Action = %q, want %q", act.Action, ActionSubmitReport)
		}
		if err := ValidateModelAction(act, scope, env.InvestigationID); err != nil {
			t.Errorf("ValidateModelAction refused a well-formed submit_report act: %v", err)
		}
	})

	t.Run("act_call_tool_rejected", func(t *testing.T) {
		act := apa49AssertRefused(t, apa49AsActField(t, callTool, "act"))
		if act.Action != "" {
			t.Errorf("refused act still carried Action = %q", act.Action)
		}
	})

	t.Run("act_submit_report_rejected", func(t *testing.T) {
		act := apa49AssertRefused(t, apa49AsActField(t, submit, "act"))
		if act.Action != "" {
			t.Errorf("refused act still carried Action = %q", act.Action)
		}
	})

	t.Run("unknown_field_rejected", func(t *testing.T) {
		apa49AssertRefused(t, withUnknownField(t, callTool, `"confidence":0.9`))
		apa49AssertRefused(t, withUnknownField(t, submit, `"outcome":"report_ready"`))
	})

	t.Run("every_closed_act_value_round_trips_under_action_only", func(t *testing.T) {
		// Driven off the production constants, so a future act value cannot
		// be added without this pin following it.
		for _, value := range []string{ActionCallTool, ActionSubmitReport} {
			t.Run(value, func(t *testing.T) {
				base := callTool
				if value == ActionSubmitReport {
					base = submit
				}
				if _, err := DecodeModelAction(base, DefaultMaxOutputBytes); err != nil {
					t.Fatalf("{\"action\":%q} refused: %v", value, err)
				}
				apa49AssertRefused(t, apa49AsActField(t, base, "act"))
			})
		}
	})

	t.Run("prompt_advertises_only_decodable_properties", func(t *testing.T) {
		// The prompt/decoder agreement, stated as one assertion. This is
		// the property whose absence produced APA-49: the decoder was
		// right and the prompt was silent, so the model chose a property
		// name out of prose and every run escalated.
		req := ModelRequest{
			Exception:        env,
			History:          []TurnRecord{},
			KnownEvidenceIDs: KnownIDs(mustSeed(t, env)),
			Turn:             1,
			RequestID:        scope.RequestID,
		}
		prompt, err := RenderPrompt(req)
		if err != nil {
			t.Fatalf("RenderPrompt: %v", err)
		}
		prose := apa49Prose(t, prompt)
		shapes := apa49TopLevelKeys(prose)
		if len(shapes) == 0 {
			t.Fatalf("the production prompt states no act object shape, so the model is left to "+
				"infer the property name from prose. That is the APA-49 defect.\n--- prose ---\n%s", prose)
		}
		allowed := apa49ModelActionJSONKeys()
		for n, keys := range shapes {
			if !apa49HasKey(keys, "action") {
				t.Errorf("shape %d (%v) does not name the \"action\" discriminator the decoder requires", n, keys)
			}
			for _, k := range keys {
				if !apa49HasKey(allowed, k) {
					t.Errorf("shape %d advertises %q, which the decoder refuses (decodable: %v)", n, k, allowed)
				}
			}
		}
	})

	t.Run("act_property_through_the_loop_escalates_without_executing", func(t *testing.T) {
		// The production consequence the live run observed, pinned
		// deterministically. A payload keyed "act" is contained, not
		// crashed, not executed, and not accepted: one re-prompt, then
		// INVALID_OUTPUT with no tool budget spent and no evidence grown.
		bad := apa49AsActField(t, callToolBytes(t, env, scope, "get_evidence", 1), "act")
		fake := &FakeModelClient{Responses: []ModelResponse{modelResp(bad), modelResp(bad)}}
		ex := successExecutor()
		out, err := newTestLoop(t, fake, ex, scope, env).Run(context.Background())
		if !errors.Is(err, ErrModelContract) {
			t.Fatalf("err = %v, want ErrModelContract", err)
		}
		if k := invalidKindOf(err); k != invalidMalformed {
			t.Errorf("kind = %s, want %s", invalidKindString(k), invalidKindString(invalidMalformed))
		}
		if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationInvalidOutput {
			t.Errorf("Outcome=%q Reason=%q, want ESCALATED/INVALID_OUTPUT", out.Outcome, out.EscalationReason)
		}
		if out.Report != nil {
			t.Error("a payload keyed \"act\" produced an accepted report")
		}
		if ex.Calls() != 0 {
			t.Errorf("executor Calls() = %d, want 0: a refused act must execute nothing", ex.Calls())
		}
		if len(out.AttemptLog) != 0 {
			t.Errorf("attempt log has %d rows, want 0: a refused act must not grow KnownEvidence", len(out.AttemptLog))
		}
		if out.ToolCallsUsed != 0 {
			t.Errorf("tool_calls_used = %d, want 0: a refused act must not consume tool budget", out.ToolCallsUsed)
		}
	})
}

// apa49AssertRefused requires the production decoder to refuse raw at
// I1-malformed / ErrModelContract, and returns the zero act so the caller
// can assert nothing leaked through.
func apa49AssertRefused(t *testing.T, raw []byte) ModelAction {
	t.Helper()
	act, err := DecodeModelAction(raw, DefaultMaxOutputBytes)
	if err == nil {
		t.Fatalf("strict decoder ACCEPTED a payload it must refuse.\nAccepting it would mean "+
			"the contract was loosened, which is out of bounds by design.\npayload: %s", raw)
	}
	if !errors.Is(err, ErrModelContract) {
		t.Errorf("err = %v, want ErrModelContract", err)
	}
	if k := invalidKindOf(err); k != invalidMalformed {
		t.Errorf("kind = %s, want %s", invalidKindString(k), invalidKindString(invalidMalformed))
	}
	return act
}
