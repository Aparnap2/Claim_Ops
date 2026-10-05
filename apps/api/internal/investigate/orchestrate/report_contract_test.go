// APA-49, layer 3: the model-facing contract for the submit_report act.
//
// Layers 1 and 2 of this defect are fixed and verified against the real
// model: the act discriminator is `"action"` (3/3) and the nested request
// schema is documented snake_case, so both call_tool acts decode exactly
// 12/12. The report object was left as `[...]` at every nested level, and
// the model guessed it 12/12 byte-equivalent, wrongly in five ways:
//
//	fact_ref            "value"        where the contract says "agreed"
//	finding             "description"  where the contract says "summary"
//	recommendation      "reason"       where the contract says "rationale"
//	recommendation.action "manual_review" outside the closed 4-action enum
//	hypothesis          no falsifier, no status, no evidence_ids
//
// "value" was only the first unknown field in document order, so accepting
// it would merely move the failure. The fix is to state the report
// contract, not to widen it.
//
// Nothing here relaxes anything. DisallowUnknownFields stays. The closed
// recommendation enum stays. The grounding and re-judge gates stay. No Go
// struct changes: the authoritative types already carry correct snake_case
// tags (epistemic.go:43-47, 81-88, 125-130, 175-179;
// exception.go:211-215), and `agreed` is checked byte-for-byte against the
// agreed snapshot at grounding.go:168-171, so a second name for the agreed
// value would defeat the re-judge gate it exists to support.
//
// Two test kinds, both deterministic:
//
//	(a) agreement: the prompt states the report contract, and its canonical
//	    example is accepted by the real authoritative decoder.
//	(b) pins: the decoder behaviour that must NOT change.
package orchestrate

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"claimops-api/internal/invest"
)

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// apa49WireKeys returns the JSON property names a struct decodes, read off
// its tags so the tests follow production rather than restate it.
func apa49WireKeys(v any) []string {
	typ := reflect.TypeOf(v)
	out := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	return out
}

// apa49CanonExample returns the canonical valid example act the prompt must
// carry for the given action: the first prompt line that is itself a
// complete, parseable JSON object whose action is the one asked for. A
// shape with `[...]` placeholders is not parseable, so it cannot be
// returned, which is the point. Selecting by action matters because the
// prompt carries one example per act and each must be checked.
func apa49CanonExample(t *testing.T, prose, action string) string {
	t.Helper()
	for _, line := range strings.Split(prose, "\n") {
		candidate := strings.TrimSpace(line)
		if !strings.HasPrefix(candidate, "{") || !json.Valid([]byte(candidate)) {
			continue
		}
		var probe struct {
			Action string `json:"action"`
		}
		if err := json.Unmarshal([]byte(candidate), &probe); err != nil {
			continue
		}
		if probe.Action == action {
			return candidate
		}
	}
	return ""
}

// apa49RenderPrompt renders the production prompt for the standard fixture.
func apa49RenderPrompt(t *testing.T) string {
	t.Helper()
	env := testEnvelope(t)
	scope := testScope(env)
	prompt, err := RenderPrompt(ModelRequest{
		Exception:        env,
		History:          []TurnRecord{},
		KnownEvidenceIDs: KnownIDs(mustSeed(t, env)),
		Turn:             1,
		RequestID:        scope.RequestID,
	})
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	return prompt
}

// apa49ValidReportBytes builds a fully valid submit_report act used to prove
// the decoder still accepts good input after the prompt change.
func apa49ValidReportBytes(t *testing.T) []byte {
	t.Helper()
	return submitBytes(t, testReport(testEnvelope(t)))
}

// ---------------------------------------------------------------------------
// (a) Prompt/decoder agreement on the report contract
// ---------------------------------------------------------------------------

// TestReportContract_PromptSpecifiesTheReportSchema is the RED proof for
// layer 3.
//
// The prompt is the only place the model learns the report contract, and
// before this change it rendered the report object as `[...]` at every
// nested level. These subtests assert the contract is stated, and that the
// canonical example it carries survives the real authoritative decoder: a
// prompt whose own example is rejected by the contract is worse than no
// example at all.
func TestReportContract_PromptSpecifiesTheReportSchema(t *testing.T) {
	prose := apa49Prose(t, apa49RenderPrompt(t))

	t.Run("names_every_report_property", func(t *testing.T) {
		// Every property of every report container, named literally. Missing
		// one is what produced the observed guesses.
		for _, name := range []string{
			// Report container
			`"hypotheses"`, `"findings"`, `"recommendation"`, `"missing_additive"`,
			// Hypothesis
			`"statement"`, `"falsifier"`, `"status"`, `"evidence_ids"`, `"fact_refs"`,
			// Fact reference
			`"key"`, `"agreed"`, `"evidence_id"`,
			// Finding
			`"hypothesis_id"`, `"summary"`,
			// Recommendation
			`"rationale"`, `"finding_ids"`,
			// Missing item
			`"kind"`, `"detail"`,
		} {
			if !strings.Contains(prose, name) {
				t.Errorf("prompt does not name %s literally.\nThe model cannot guess a field "+
					"name reliably; the request schema had to be spelled out for exactly this "+
					"reason.\n--- prose ---\n%s", name, prose)
			}
		}
	})

	t.Run("names_the_closed_vocabularies", func(t *testing.T) {
		// A closed set the prompt does not list is a set the model will
		// invent. `manual_review` is precisely such an invention.
		for _, name := range []string{
			`"REQUEST_EVIDENCE"`, `"CONFIRM_EXCEPTION"`, `"REFER_HUMAN"`, `"REVERIFY"`,
			`"OPEN"`, `"SUPPORTED"`, `"REFUTED"`,
		} {
			if !strings.Contains(prose, name) {
				t.Errorf("prompt does not name the closed value %s.\nA closed vocabulary that is "+
					"not listed is a vocabulary the model will invent.\n--- prose ---\n%s", name, prose)
			}
		}
	})

	t.Run("rules_out_the_observed_wrong_names", func(t *testing.T) {
		// The exact fields the real model produced. Naming what is wrong is
		// what stops the model reaching for the obvious synonym, and it is
		// the difference between `evidence_ids`-in-a-request (invented) and
		// `value`-in-a-fact_ref (borrowed from the DATA block).
		for _, bad := range []string{
			`"value"`, `"description"`, `"reason"`, `"manual_review"`,
		} {
			if !strings.Contains(prose, bad) {
				t.Errorf("prompt does not rule out %s.\nThe model emitted this exact property "+
					"12/12; stating that it is not part of the contract is what closes the door "+
					"on the synonym.\n--- prose ---\n%s", bad, prose)
			}
		}
	})

	t.Run("states_mandatory_versus_optional", func(t *testing.T) {
		// The model omitted falsifier, status, and evidence_ids 12/12. A
		// shape that lists every property reads as "all of these are
		// required", which is only partly true, and a field wrongly believed
		// required produces an invented value instead of an omission.
		lowered := strings.ToLower(prose)
		need := []string{"required", "optional"}
		for _, word := range need {
			if !strings.Contains(lowered, word) {
				t.Errorf("prompt never states which fields are %s.\nThe model needs to know "+
					"what it may omit; a shape alone cannot say that.\n--- prose ---\n%s", word, prose)
			}
		}
	})

	t.Run("shows_a_canonical_valid_report_example", func(t *testing.T) {
		example := apa49CanonExample(t, prose, ActionSubmitReport)
		if example == "" {
			t.Fatalf("the prompt carries no parseable canonical act example.\nA shape built from "+
				"[...] placeholders cannot be decoded and therefore cannot be checked against "+
				"the contract it is meant to teach.\n--- prose ---\n%s", prose)
		}
		act, err := DecodeModelAction([]byte(example), DefaultMaxOutputBytes)
		if err != nil {
			t.Fatalf("the canonical example the prompt carries is REJECTED by the authoritative "+
				"decoder: %v\nA prompt whose own example fails the contract teaches the model to "+
				"fail.\nexample: %s", err, example)
		}
		if act.Action != ActionSubmitReport {
			t.Fatalf("canonical example action = %q, want %q: the report example is what is "+
				"missing", act.Action, ActionSubmitReport)
		}
		if act.Report == nil {
			t.Fatal("canonical example carries no report")
		}
		// Shape validity is the assertion. Grounding is deliberately not
		// asserted: the example cites example IDs, and whether they are
		// KNOWN is decided per run against real evidence, which the live
		// qualification re-checks.
		if err := ValidateReport(*act.Report); err != nil {
			t.Fatalf("the canonical example does not satisfy ValidateReport: %v\nexample: %s",
				err, example)
		}
		// And it must exercise the containers the model got wrong, or it
		// teaches nothing about them.
		if len(act.Report.Hypotheses) == 0 || len(act.Report.Findings) == 0 {
			t.Fatal("canonical example has an empty hypothesis or finding list, so it does not " +
				"demonstrate the containers the model was guessing at")
		}
		if len(act.Report.Hypotheses[0].FactRefs) == 0 {
			t.Error("canonical example has no fact reference, so it does not demonstrate the " +
				"`agreed` spelling the model was getting wrong")
		}
	})

	t.Run("example_properties_are_exactly_the_decodable_ones", func(t *testing.T) {
		example := apa49CanonExample(t, prose, ActionSubmitReport)
		if example == "" {
			t.Skip("no parseable example yet; covered by the canonical-example subtest")
		}
		allowed := map[string][]string{
			"act":            apa49WireKeys(ModelAction{}),
			"report":         apa49WireKeys(Report{}),
			"hypothesis":     apa49WireKeys(invest.Hypothesis{}),
			"fact_ref":       apa49WireKeys(invest.FactRef{}),
			"finding":        apa49WireKeys(invest.Finding{}),
			"recommendation": apa49WireKeys(invest.Recommendation{}),
			"missing_item":   apa49WireKeys(invest.MissingItem{}),
		}
		var check func(node any, path, kind string)
		check = func(node any, path, kind string) {
			switch v := node.(type) {
			case map[string]any:
				for k, sub := range v {
					if !apa49HasKey(allowed[kind], k) {
						t.Errorf("%s advertises %q, which %s does not decode (decodable: %v)",
							path, k, kind, allowed[kind])
						continue
					}
					switch k {
					case "hypotheses":
						check(sub, path+".hypotheses[]", "hypothesis")
					case "fact_refs":
						check(sub, path+".hypotheses[].fact_refs[]", "fact_ref")
					case "findings":
						check(sub, path+".findings[]", "finding")
					case "recommendation":
						check(sub, path+".recommendation", "recommendation")
					case "missing_additive":
						check(sub, path+".missing_additive[]", "missing_item")
					}
				}
			case []any:
				for i, sub := range v {
					check(sub, path, kind)
					_ = i
				}
			}
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(example), &decoded); err != nil {
			t.Fatalf("unmarshal example: %v", err)
		}
		check(decoded, "example", "act")
	})
}

// ---------------------------------------------------------------------------
// (b) Decoder pins that must not change
// ---------------------------------------------------------------------------

// TestReportContract_DecoderPinsUnchanged records the decoder behaviour
// this layer must not disturb. Layer 3 is a prompt change; if any of these
// flip, the change has silently become a schema change.
func TestReportContract_DecoderPinsUnchanged(t *testing.T) {
	valid := apa49ValidReportBytes(t)

	t.Run("value_stays_rejected_in_a_fact_ref", func(t *testing.T) {
		// The exact live payload shape: `value` where the contract says
		// `agreed`. Adjudicated as a sibling DATA-block name
		// (FieldSourceView.Value, exception.go:127) borrowed into the wrong
		// container, NOT a legitimate field. A second name for the agreed
		// value would defeat the byte-for-byte re-judge check at
		// grounding.go:168-171.
		payload := apa49WithFactRefKey(t, valid, "value", "City Hospital")
		if act := apa49AssertRefused(t, payload); act.Report != nil {
			t.Error("a refused act still carried a report")
		}
		if _, err := DecodeModelAction(payload, DefaultMaxOutputBytes); err == nil ||
			!strings.Contains(err.Error(), `unknown field "value"`) {
			t.Errorf("err = %v, want it to name the offending property \"value\"", err)
		}
	})

	t.Run("description_stays_rejected_in_a_finding", func(t *testing.T) {
		payload := apa49WithFindingKey(t, valid, "description", "shows a conflict")
		apa49AssertRefused(t, payload)
	})

	t.Run("reason_stays_rejected_in_a_recommendation", func(t *testing.T) {
		payload := apa49WithRecommendationKey(t, valid, "reason", "needs review")
		apa49AssertRefused(t, payload)
	})

	t.Run("manual_review_stays_outside_the_recommendation_enum", func(t *testing.T) {
		// The model chose a plausible-sounding action from an open set. It
		// must keep failing, and it must fail as a CONTRACT error at the
		// vocabulary, not as an unknown-field error.
		payload := apa49WithRecommendationAction(t, valid, "manual_review")
		act, err := DecodeModelAction(payload, DefaultMaxOutputBytes)
		if err != nil {
			t.Fatalf("DecodeModelAction refused the whole act before the enum could be checked: "+
				"%v", err)
		}
		if err := ValidateModelAction(act, testScope(testEnvelope(t)), testEnvelope(t).InvestigationID); err == nil {
			t.Fatal("ValidateModelAction accepted recommendation action \"manual_review\": the " +
				"closed 4-action vocabulary must be enforced")
		} else if !errors.Is(err, ErrModelContract) {
			t.Errorf("err = %v, want ErrModelContract", err)
		}
	})

	t.Run("every_closed_action_is_accepted", func(t *testing.T) {
		for _, action := range []invest.RecommendationAction{
			invest.RecommendRequestEvidence,
			invest.RecommendConfirmException,
			invest.RecommendReferHuman,
			invest.RecommendReverify,
		} {
			act, err := DecodeModelAction(apa49WithRecommendationAction(t, valid, string(action)), DefaultMaxOutputBytes)
			if err != nil {
				t.Fatalf("%s refused at decode: %v", action, err)
			}
			if act.Report == nil {
				t.Fatalf("%s produced no report", action)
			}
			if err := ValidateReport(*act.Report); err != nil {
				t.Errorf("%s is a closed action but failed ValidateReport: %v", action, err)
			}
		}
	})

	t.Run("unknown_report_fields_stay_rejected", func(t *testing.T) {
		apa49AssertRefused(t, withUnknownField(t, valid, `"confidence":0.9`))
		apa49AssertRefused(t, withUnknownField(t, valid, `"outcome":"report_ready"`))
	})

	t.Run("missing_required_hypothesis_fields_stay_rejected", func(t *testing.T) {
		// Dropping falsifier must fail on the shape, at validation, not at
		// decode. This is the other half of the observed omission.
		payload := apa49WithoutHypothesisField(t, valid, "falsifier")
		act, err := DecodeModelAction(payload, DefaultMaxOutputBytes)
		if err != nil {
			t.Fatalf("omitting a required field failed at decode rather than validation: %v", err)
		}
		if err := ValidateModelAction(act, testScope(testEnvelope(t)), testEnvelope(t).InvestigationID); err == nil {
			t.Fatal("a hypothesis with no falsifier was accepted: structural uncertainty is " +
				"required and confidence floats are not a substitute")
		} else if !errors.Is(err, ErrModelContract) {
			t.Errorf("err = %v, want ErrModelContract", err)
		}
	})

	t.Run("valid_report_is_still_accepted", func(t *testing.T) {
		act, err := DecodeModelAction(valid, DefaultMaxOutputBytes)
		if err != nil {
			t.Fatalf("a valid report was refused: %v", err)
		}
		if err := ValidateReport(*act.Report); err != nil {
			t.Fatalf("ValidateReport: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Payload rewriters: swap one JSON property name inside a valid act, so
// each pin exercises the real decoder on real surrounding structure.
// ---------------------------------------------------------------------------

// apa49FirstElemPath walks report.<array>[0] returning the raw container.
func apa49FirstElemPath(t *testing.T, raw []byte, array string) map[string]json.RawMessage {
	t.Helper()
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("unmarshal act: %v", err)
	}
	var report map[string]json.RawMessage
	if err := json.Unmarshal(root["report"], &report); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(report[array], &list); err != nil {
		t.Fatalf("unmarshal %s: %v", array, err)
	}
	if len(list) == 0 {
		t.Fatalf("%s is empty in the fixture", array)
	}
	return list[0]
}

func apa49WithFactRefKey(t *testing.T, raw []byte, key string, _ ...string) []byte {
	t.Helper()
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("unmarshal act: %v", err)
	}
	var report map[string]json.RawMessage
	if err := json.Unmarshal(root["report"], &report); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	var hyps []map[string]json.RawMessage
	if err := json.Unmarshal(report["hypotheses"], &hyps); err != nil {
		t.Fatalf("unmarshal hypotheses: %v", err)
	}
	var refs []map[string]json.RawMessage
	if err := json.Unmarshal(hyps[0]["fact_refs"], &refs); err != nil {
		t.Fatalf("unmarshal fact_refs: %v", err)
	}
	// Replace the agreed value with the wrong property name, exactly as the
	// live model did.
	refs[0][key] = refs[0]["agreed"]
	delete(refs[0], "agreed")
	enc, err := json.Marshal(refs)
	if err != nil {
		t.Fatalf("marshal fact_refs: %v", err)
	}
	hyps[0]["fact_refs"] = enc
	encHyps, err := json.Marshal(hyps)
	if err != nil {
		t.Fatalf("marshal hypotheses: %v", err)
	}
	report["hypotheses"] = encHyps
	encReport, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	root["report"] = encReport
	out, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("marshal act: %v", err)
	}
	return out
}

func apa49WithFindingKey(t *testing.T, raw []byte, key, value string) []byte {
	t.Helper()
	findings := apa49FirstElemPath(t, raw, "findings")
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal value: %v", err)
	}
	findings[key] = encoded
	delete(findings, "summary")
	enc, err := json.Marshal(findings)
	if err != nil {
		t.Fatalf("marshal finding: %v", err)
	}
	return apa49ReplaceArrayElem(t, raw, "findings", enc)
}

func apa49WithRecommendationKey(t *testing.T, raw []byte, key, value string) []byte {
	t.Helper()
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("unmarshal act: %v", err)
	}
	var report map[string]json.RawMessage
	if err := json.Unmarshal(root["report"], &report); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(report["recommendation"], &rec); err != nil {
		t.Fatalf("unmarshal recommendation: %v", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal value: %v", err)
	}
	rec[key] = encoded
	delete(rec, "rationale")
	enc, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal recommendation: %v", err)
	}
	report["recommendation"] = enc
	encReport, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	root["report"] = encReport
	out, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("marshal act: %v", err)
	}
	return out
}

func apa49WithRecommendationAction(t *testing.T, raw []byte, action string) []byte {
	t.Helper()
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("unmarshal act: %v", err)
	}
	var report map[string]json.RawMessage
	if err := json.Unmarshal(root["report"], &report); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(report["recommendation"], &rec); err != nil {
		t.Fatalf("unmarshal recommendation: %v", err)
	}
	encoded, err := json.Marshal(action)
	if err != nil {
		t.Fatalf("marshal action: %v", err)
	}
	rec["action"] = encoded
	enc, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal recommendation: %v", err)
	}
	report["recommendation"] = enc
	encReport, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	root["report"] = encReport
	out, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("marshal act: %v", err)
	}
	return out
}

func apa49WithoutHypothesisField(t *testing.T, raw []byte, field string) []byte {
	t.Helper()
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("unmarshal act: %v", err)
	}
	var report map[string]json.RawMessage
	if err := json.Unmarshal(root["report"], &report); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	var hyps []map[string]json.RawMessage
	if err := json.Unmarshal(report["hypotheses"], &hyps); err != nil {
		t.Fatalf("unmarshal hypotheses: %v", err)
	}
	delete(hyps[0], field)
	encHyps, err := json.Marshal(hyps)
	if err != nil {
		t.Fatalf("marshal hypotheses: %v", err)
	}
	report["hypotheses"] = encHyps
	encReport, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	root["report"] = encReport
	out, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("marshal act: %v", err)
	}
	return out
}

// apa49ReplaceArrayElem swaps report[array][0] with a pre-marshalled element.
func apa49ReplaceArrayElem(t *testing.T, raw []byte, array string, elem json.RawMessage) []byte {
	t.Helper()
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("unmarshal act: %v", err)
	}
	var report map[string]json.RawMessage
	if err := json.Unmarshal(root["report"], &report); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	var list []json.RawMessage
	if err := json.Unmarshal(report[array], &list); err != nil {
		t.Fatalf("unmarshal %s: %v", array, err)
	}
	list[0] = elem
	enc, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal %s: %v", array, err)
	}
	report[array] = enc
	encReport, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	root["report"] = encReport
	out, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("marshal act: %v", err)
	}
	return out
}
