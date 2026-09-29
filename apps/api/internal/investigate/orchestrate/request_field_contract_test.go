// APA-49, one layer down: the model-facing wire contract for the NESTED
// tool request inside a call_tool act.
//
// The first APA-49 fix named the act discriminator. That worked: the real
// ADR-002 model emits {"action":"call_tool",...} 3/3. The run still fails,
// one layer down, because investigate.Request declares no JSON tags:
//
//	type Request struct {
//	    Tool            invest.ToolName
//	    TenantID        string
//	    ...
//	}
//
// encoding/json v1 matches a JSON key to a field by exact tag, then by
// case-insensitive field name. "tenant_id" matches neither "TenantID" nor
// any tag, so DisallowUnknownFields rejects it. The model-facing contract
// therefore silently demanded PascalCase Go identifiers — a Go naming
// convention, not a wire convention, and not something a prompt can make
// an LLM produce reliably. The real model produced snake_case, matching
// every other name in the system, and was refused for it.
//
// This file pins the nested contract: the wire names the model uses are
// snake_case, the Go-native construction paths are untouched, and the one
// field the model got wrong for a *contract* reason (naming) is fixed
// while the one it got wrong for a *semantic* reason (inventing a field
// that does not exist) keeps failing closed.
//
// The strict boundary is unchanged. DisallowUnknownFields stays. No
// alias, no dual-key acceptance, no forgiving parser, no model-specific
// leniency.
package orchestrate

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// apa49RequestJSONKeys returns the wire property names investigate.Request
// decodes, read off the struct tags rather than restated here, so this
// file follows the schema instead of drifting from it.
func apa49RequestJSONKeys() []string {
	typ := reflect.TypeOf(investigate.Request{})
	out := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			out = append(out, typ.Field(i).Name)
			continue
		}
		out = append(out, name)
	}
	return out
}

// apa49NestedKeysAfter returns the depth-1 property names of the object
// literal that follows `"key":` in s, so a nested schema shown in the
// prompt can be read rather than guessed at.
func apa49NestedKeysAfter(s, key string) []string {
	needle := `"` + key + `":`
	idx := strings.Index(s, needle)
	if idx < 0 {
		return nil
	}
	brace := strings.Index(s[idx+len(needle):], "{")
	if brace < 0 {
		return nil
	}
	keys, _, ok := apa49ScanObject(s, idx+len(needle)+brace)
	if !ok {
		return nil
	}
	return keys
}

// apa49IsSnakeCase reports whether name is lower snake_case: only
// lower-case letters, digits, and single interior underscores, never
// leading, trailing, or doubled.
func apa49IsSnakeCase(name string) bool {
	if name == "" || name[0] == '_' || strings.HasSuffix(name, "_") {
		return false
	}
	prevUnderscore := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			prevUnderscore = false
		case r == '_':
			if prevUnderscore {
				return false
			}
			prevUnderscore = true
		default:
			return false
		}
	}
	return true
}

// apa49NestedAct renders the model-facing call_tool act with the nested
// request spelled in the given key style, so one test can drive the
// PascalCase contract the bug produced and the snake_case contract that
// replaces it.
func apa49NestedAct(t *testing.T, env invest.UnresolvedException, scope investigate.Scope, keys map[string]string, extra string) []byte {
	t.Helper()
	get := func(k string) string { return keys[k] }
	body := `{` +
		`"` + get("tenant_id") + `":"` + scope.TenantID + `",` +
		`"` + get("claim_id") + `":"` + scope.ClaimID + `",` +
		`"` + get("investigation_id") + `":"` + env.InvestigationID + `",` +
		`"` + get("request_id") + `":"` + scope.RequestID + `",` +
		`"` + get("tool") + `":"get_documents",` +
		`"` + get("limit") + `":10` +
		extra + `}`
	raw := []byte(`{"action":"call_tool","tool":"get_documents","request":` + body + `}`)
	if !json.Valid(raw) {
		t.Fatalf("test fixture is not valid JSON: %s", raw)
	}
	return raw
}

// ---------------------------------------------------------------------------
// The nested request schema the model is actually told to emit
// ---------------------------------------------------------------------------

// TestNestedRequestContract_SnakeCaseBinds is the RED proof for the naming
// half of the defect.
//
// The payload below is what the real model returned, with the identity it
// got from the DATA block. Every key is the name the system uses
// everywhere else. Today the strict decoder refuses it, because
// investigate.Request has no tags and "tenant_id" binds to nothing.
func TestNestedRequestContract_SnakeCaseBinds(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)

	t.Run("snake_case_nested_request_is_accepted_and_binds", func(t *testing.T) {
		payload := apa49NestedAct(t, env, scope, map[string]string{
			"tool": "tool", "tenant_id": "tenant_id", "claim_id": "claim_id",
			"investigation_id": "investigation_id", "request_id": "request_id", "limit": "limit",
		}, "")
		act, err := DecodeModelAction(payload, DefaultMaxOutputBytes)
		if err != nil {
			t.Fatalf("the model-facing nested request schema is refused: %v\n\n"+
				"investigate.Request declares no JSON tags, so encoding/json v1 can only "+
				"bind the Go field name. The wire contract therefore demanded "+
				"\"TenantID\"/\"ClaimID\"/\"InvestigationID\"/\"RequestID\", which is a Go "+
				"naming convention no prompt can make an LLM produce reliably.\n"+
				"payload: %s", err, payload)
		}
		if act.Request == nil {
			t.Fatal("accepted act carries no request")
		}
		// Every field the act needs downstream must have actually bound, not
		// merely survived decoding. A decode that silently produced an empty
		// identity would fail later and read as a model problem.
		for _, f := range []struct {
			name string
			got  string
			want string
		}{
			{"Tool", string(act.Request.Tool), "get_documents"},
			{"TenantID", act.Request.TenantID, scope.TenantID},
			{"ClaimID", act.Request.ClaimID, scope.ClaimID},
			{"InvestigationID", act.Request.InvestigationID, env.InvestigationID},
			{"RequestID", act.Request.RequestID, scope.RequestID},
		} {
			if f.got != f.want {
				t.Errorf("request.%s = %q, want %q: the wire key did not bind", f.name, f.got, f.want)
			}
		}
		if act.Request.Limit != 10 {
			t.Errorf("request.Limit = %d, want 10: the wire key did not bind", act.Request.Limit)
		}
		// And the decoded act must survive the production gate that the loop
		// runs next, otherwise a bound request is still a refused one.
		if err := ValidateModelAction(act, scope, env.InvestigationID); err != nil {
			t.Fatalf("snake_case nested request decoded but failed the production gate: %v", err)
		}
	})

	t.Run("every_request_wire_name_is_snake_case", func(t *testing.T) {
		// The rule, stated over the schema rather than over one payload: the
		// model-facing contract is snake_case. A future field added without
		// a tag would re-expose Go's PascalCase and re-break every model.
		for _, name := range apa49RequestJSONKeys() {
			if !apa49IsSnakeCase(name) {
				t.Errorf("investigate.Request exposes wire name %q, which is not snake_case.\n"+
					"Go naming conventions must never reach the model: the decoder can only "+
					"bind what a tag declares, so an untagged field silently demands the "+
					"exact Go field name", name)
			}
		}
	})

	t.Run("pascal_case_nested_request_is_rejected", func(t *testing.T) {
		// The bug is fixed, not accommodated: the Go-shaped spelling the
		// contract used to demand must now be an unknown field, exactly as
		// "act" is. Otherwise the two spellings coexist and the strict
		// boundary has quietly become a forgiving parser.
		payload := apa49NestedAct(t, env, scope, map[string]string{
			"tool": "Tool", "tenant_id": "TenantID", "claim_id": "ClaimID",
			"investigation_id": "InvestigationID", "request_id": "RequestID", "limit": "Limit",
		}, "")
		act, err := DecodeModelAction(payload, DefaultMaxOutputBytes)
		if err == nil {
			t.Fatalf("PascalCase nested request ACCEPTED (tool=%q). The contract is snake_case "+
				"only; accepting both spellings would make the boundary a forgiving parser.\n%s",
				act.Request.Tool, payload)
		}
		if !errors.Is(err, ErrModelContract) {
			t.Errorf("err = %v, want ErrModelContract", err)
		}
	})
}

// ---------------------------------------------------------------------------
// evidence_ids: the model's other failure, and why it stays a failure
// ---------------------------------------------------------------------------

// TestNestedRequestContract_EvidenceIDsStayUnsupported is the RED proof for
// the second observed decode error, and it pins the decision.
//
// The real model emitted "evidence_ids" inside the tool request in r2. That
// is a DIFFERENT kind of error from "tenant_id", and it must not be fixed
// the same way:
//
//   - "tenant_id" is a REAL field under the wrong name. The fix is to name
//     it correctly.
//   - "evidence_ids" is not a request field at all. investigate.Request is
//     an input envelope: it selects and bounds a read. Evidence IDs are
//     the OUTPUT of a read (Response.IDs) and the thing citations are
//     checked against at grounding time (KnownEvidence / TurnRecord.
//     ResponseIDs / report EvidenceIDs). No allowlisted tool takes evidence
//     IDs as a selector: get_evidence is bounded by cursor/limit/
//     source_type, search_evidence by query.
//
// So the model put an output name into an input envelope. Adding the field
// because a model produced it would be textbook hallucination-chasing, and
// worse, it would open a second, unvalidated path for naming evidence on
// the request side, bypassing the grounding gate that exists precisely to
// own that decision. The correct action is to keep rejecting it.
func TestNestedRequestContract_EvidenceIDsStayUnsupported(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	snake := map[string]string{
		"tool": "tool", "tenant_id": "tenant_id", "claim_id": "claim_id",
		"investigation_id": "investigation_id", "request_id": "request_id", "limit": "limit",
	}

	t.Run("evidence_ids_is_not_a_request_field", func(t *testing.T) {
		if _, ok := reflect.TypeOf(investigate.Request{}).FieldByName("EvidenceIDs"); ok {
			t.Fatal("investigate.Request has an EvidenceIDs field: a tool request may not " +
				"select evidence. Evidence IDs are read output, and naming them on the " +
				"request side would bypass the grounding gate")
		}
		for _, name := range apa49RequestJSONKeys() {
			if name == "evidence_ids" {
				t.Fatal(`investigate.Request declares an "evidence_ids" wire field`)
			}
		}
	})

	t.Run("evidence_ids_in_a_request_is_rejected", func(t *testing.T) {
		payload := apa49NestedAct(t, env, scope, snake, `,"evidence_ids":["ev-doc-01"]`)
		act, err := DecodeModelAction(payload, DefaultMaxOutputBytes)
		if err == nil {
			t.Fatalf("a request naming evidence_ids was ACCEPTED (tool=%q).\n"+
				"The model invented an output-side name for an input envelope; the boundary "+
				"must keep refusing it rather than growing the schema to fit.\npayload: %s",
				act.Request.Tool, payload)
		}
		if !errors.Is(err, ErrModelContract) {
			t.Errorf("err = %v, want ErrModelContract", err)
		}
		if k := invalidKindOf(err); k != invalidMalformed {
			t.Errorf("kind = %s, want %s", invalidKindString(k), invalidKindString(invalidMalformed))
		}
	})

	t.Run("evidence_ids_is_not_advertised_by_the_prompt", func(t *testing.T) {
		// The prompt must not teach the hallucination. If it ever names
		// evidence_ids inside a request, the decoder would have to accept
		// it to stay consistent, which is the thing being refused.
		prompt, err := RenderPrompt(apa49ContractRequest(t))
		if err != nil {
			t.Fatalf("RenderPrompt: %v", err)
		}
		nested := strings.Join(apa49NestedKeysAfter(apa49Prose(t, prompt), "request"), " ")
		for _, k := range apa49NestedKeysAfter(apa49Prose(t, prompt), "request") {
			if k == "evidence_ids" {
				t.Fatal(`the prompt advertises "evidence_ids" inside a tool request, which ` +
					"the decoder refuses; the two must never disagree")
			}
		}
		t.Logf("prompt advertises nested request keys: %s", nested)
	})
}

// ---------------------------------------------------------------------------
// Go-native construction is untouched
// ---------------------------------------------------------------------------

// TestNestedRequestContract_GoNativePathsUnchanged is the compatibility
// proof. Adding wire tags renames the model-facing contract; it must not
// disturb any path that constructs a Request in Go, which is every
// production path that reaches a tool today.
func TestNestedRequestContract_GoNativePathsUnchanged(t *testing.T) {
	t.Run("native_literal_marshals_and_binds_back_identically", func(t *testing.T) {
		want := investigate.Request{Tool: invest.ToolGetDocuments, TenantID: "t", ClaimID: "c"}
		raw, err := canonicalToolRequest(want)
		if err != nil {
			t.Fatalf("canonicalToolRequest: %v", err)
		}
		var got investigate.Request
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("a Go-native Request no longer round-trips: %v\nbytes: %s", err, raw)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Go-native Request did not survive its own wire round trip:\nwant %+v\ngot  %+v\nbytes: %s",
				want, got, raw)
		}
	})

	t.Run("determinism_property_holds", func(t *testing.T) {
		// The property canonicalToolRequest documents: equal requests render
		// byte-identical and hash equal, distinct requests hash distinct.
		// Wire tags change the bytes, so the property has to be re-checked,
		// not assumed.
		a := investigate.Request{Tool: invest.ToolGetEvidence, TenantID: "t", ClaimID: "c", Limit: 1}
		same := a
		other := a
		other.Cursor = "cur-2"
		paged := a
		paged.Limit = 2

		ra, err := canonicalToolRequest(a)
		if err != nil {
			t.Fatalf("canonicalToolRequest: %v", err)
		}
		rs, err := canonicalToolRequest(same)
		if err != nil {
			t.Fatalf("canonicalToolRequest: %v", err)
		}
		ro, err := canonicalToolRequest(other)
		if err != nil {
			t.Fatalf("canonicalToolRequest: %v", err)
		}
		rp, err := canonicalToolRequest(paged)
		if err != nil {
			t.Fatalf("canonicalToolRequest: %v", err)
		}
		if repetitionKey(a.Tool, ra) != repetitionKey(same.Tool, rs) {
			t.Error("equal requests hashed differently: the determinism property is broken")
		}
		if repetitionKey(a.Tool, ra) == repetitionKey(paged.Tool, rp) {
			t.Error("requests differing only in Limit hashed the same: the repetition check " +
				"would let a paged re-read look like an exact repeat")
		}
		if repetitionKey(a.Tool, ra) == repetitionKey(other.Tool, ro) {
			t.Error("requests differing only in Cursor hashed the same: an exact-repeat " +
				"escalation would miss a paged re-read of the same tool")
		}
	})

	t.Run("per_tool_constructors_still_validate", func(t *testing.T) {
		// The per-tool typed constructors are the documented way to build a
		// Request. They must be untouched by a wire-schema change.
		if _, err := investigate.NewRequest(invest.ToolGetDocuments, "t", "c",
			"inv-abcdef0123456789abcdef0123456789", "r", 1); err != nil {
			t.Errorf("NewRequest: %v", err)
		}
		gd, err := investigate.NewGetDocumentsRequest("t", "c", "inv-abcdef0123456789abcdef0123456789", "r", 10, "")
		if err != nil {
			t.Fatalf("NewGetDocumentsRequest: %v", err)
		}
		if err := gd.ToRequest().Validate(); err != nil {
			t.Errorf("ToRequest().Validate(): %v", err)
		}
		ge, err := investigate.NewGetEvidenceRequest("t", "c", "inv-abcdef0123456789abcdef0123456789", "r", 10, "", "")
		if err != nil {
			t.Fatalf("NewGetEvidenceRequest: %v", err)
		}
		if err := ge.ToRequest().Validate(); err != nil {
			t.Errorf("ToRequest().Validate(): %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// The prompt must document the nested schema
// ---------------------------------------------------------------------------

// TestNestedRequestContract_PromptDocumentsTheNestedSchema is the RED proof
// that the prompt and the nested decoder agree.
//
// The act shapes alone stopped the "act" failure and exposed this one. A
// prompt that shows the act object but hides its request object leaves the
// model to invent the request field names, which is precisely how
// "evidence_ids" appeared.
func TestNestedRequestContract_PromptDocumentsTheNestedSchema(t *testing.T) {
	prose := apa49Prose(t, mustRenderContractPrompt(t))

	nested := apa49NestedKeysAfter(prose, "request")
	if len(nested) == 0 {
		t.Fatalf("the prompt shows no nested request object, so the model must invent the "+
			"request field names. That is how the real model produced \"tenant_id\" and "+
			"\"evidence_ids\".\n--- prose ---\n%s", prose)
	}

	allowed := apa49RequestJSONKeys()
	for _, k := range nested {
		if !apa49HasKey(allowed, k) {
			t.Errorf("prompt advertises request field %q, which investigate.Request does not "+
				"decode (decodable: %v): the prompt would be describing a field the boundary refuses", k, allowed)
			continue
		}
		if !apa49IsSnakeCase(k) {
			t.Errorf("prompt advertises request field %q, which is not snake_case: the model "+
				"would be taught the Go spelling, which the decoder will not bind", k)
		}
	}

	// The fields the production gate actually requires. validateCallTool
	// compares the request's own tool against the act's tool and echoes five
	// identity values, so a shape that omits any of them teaches the model
	// to fail one layer down from where it failed last time.
	required := []string{"tool", "tenant_id", "claim_id", "investigation_id", "request_id", "limit"}
	for _, want := range required {
		if !apa49HasKey(nested, want) {
			t.Errorf("prompt's nested request shape omits the required field %q.\n"+
				"Required by the production gate: validateCallTool compares request.tool "+
				"against the act tool, and Executor.Execute echoes tenant/claim/request id.\n"+
				"documented: %v", want, nested)
		}
	}
	t.Logf("prompt documents nested request fields: %v", nested)
}

// apa49ContractRequest is a valid ModelRequest for rendering.
func apa49ContractRequest(t *testing.T) ModelRequest {
	t.Helper()
	env := testEnvelope(t)
	scope := testScope(env)
	return ModelRequest{
		Exception:        env,
		History:          []TurnRecord{},
		KnownEvidenceIDs: KnownIDs(mustSeed(t, env)),
		Turn:             1,
		RequestID:        scope.RequestID,
	}
}

// mustRenderContractPrompt renders the production prompt or fails.
func mustRenderContractPrompt(t *testing.T) string {
	t.Helper()
	prompt, err := RenderPrompt(apa49ContractRequest(t))
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	return prompt
}
