// Package orchestrate implements the bounded investigation orchestrator
// (issue #66): the deterministic loop around a scripted model client that
// turns one invest.UnresolvedException into an InvestigationOutput.
//
// Placement: this package is a subpackage of investigate. It imports invest
// and investigate; neither imports it. The invest package stays
// contracts-only (zero-methods rule untouched); the investigate executor,
// scope, audit, and tool envelope are consumed read-only and never
// modified here.
//
// Loop contract (reviewed design, implemented exactly):
//
//   - 2-act vocabulary: CALL_TOOL (tool + validated Request) or
//     SUBMIT_REPORT (hypotheses/findings/recommendation/missing-additive).
//   - Strict decoding everywhere (DisallowUnknownFields, trailing-data
//     reject). Invalid classes I1-I8 map onto ErrModelContract,
//     ErrToolDenied, ErrGrounding, and ErrModelEmpty. One re-prompt on
//     I1/I2/I7 only, same budget; a second consecutive invalid escalates
//     with INVALID_OUTPUT. Malformed acts never consume tool budget and
//     never grow KnownEvidence.
//   - Per turn: build the canonical ModelRequest (size-capped) -> Complete
//     under a turn timeout -> decode + validate -> CALL_TOOL runs Gate A,
//     the repetition check, and exec.Execute, then records a TurnRecord
//     and grows KnownEvidence from validated Response.IDs; SUBMIT_REPORT
//     runs grounding Gate B and finishes REPORT_READY. The loop NEVER
//     calls T11 itself (report persistence is a later stage).
//   - Budgets bound turns, model calls, tool calls (from Scope, fail
//     closed), input/output bytes, and the turn timeout. Exhaustion,
//     repetition, no-progress, second-invalid, and upstream failures
//     escalate with an EscalationReason plus Partial progress and the
//     AttemptLog. ModelUpstream is retried once; ctx cancellation is
//     never retried.
//   - Grounding: KnownEvidence is seeded from the envelope EvidenceRefs
//     and grown only from validated Response.IDs. The tenant comes from
//     Scope only (asserted equal before every Complete; split aborts F6).
//     ModelRequest carries exception + history + KnownIDs + turn +
//     requestID only: no secrets, no raw values, no AgreedRaw reads, no
//     FactRef creation from model text.
//   - Wire types carry zero methods (package-level Validate* functions
//     only, mirroring invest); the only methods in non-test code are
//     (*Loop).Run. No float64 anywhere on the wire. Canonical JSON v1
//     only (json/v2 stays rejected per decision log).
//   - Model access is the ModelClient seam. Tests use the scripted
//     FakeModelClient (see orchestrate_test.go); the Vertex provider is
//     deferred and there is no provider file here.
package orchestrate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/verify"
)

// Act discriminators for the 2-act vocabulary.
const (
	// ActionCallTool dispatches one bounded tool call.
	ActionCallTool = "call_tool"
	// ActionSubmitReport finishes the investigation with a report.
	ActionSubmitReport = "submit_report"
)

// ModelClient is the model seam: one bounded turn of cognition. It takes
// the canonical ModelRequest and returns raw output bytes plus the model
// identifier. Implementations perform no validation, no tool access, and
// no state mutation; the loop owns every boundary. Transport failures
// must wrap ErrModelUpstream so the loop retries once, then escalates.
type ModelClient interface {
	Complete(ctx context.Context, req ModelRequest) (ModelResponse, error)
}

// ModelRequest is the sole model input: the exception envelope, the
// IDs/hashes/counts-only turn history, the sorted KnownEvidence IDs, the
// 1-based turn number, and the propagated request ID. No secrets, no
// tokens, no raw values.
type ModelRequest struct {
	Exception        invest.UnresolvedException `json:"exception"`
	History          []TurnRecord               `json:"history"`
	KnownEvidenceIDs []string                   `json:"known_evidence_ids"`
	Turn             int                        `json:"turn"`
	RequestID        string                     `json:"request_id"`
}

// ValidateModelRequest checks the request standalone.
func ValidateModelRequest(r ModelRequest) error {
	if err := invest.Validate(r.Exception); err != nil {
		return fmt.Errorf("orchestrate: model request: %v: %w", err, ErrModelContract)
	}
	last := 0
	for i := range r.History {
		if err := ValidateTurnRecord(r.History[i]); err != nil {
			return fmt.Errorf("orchestrate: model request history[%d]: %v: %w", i, err, ErrModelContract)
		}
		if r.History[i].Turn <= last {
			return fmt.Errorf("orchestrate: model request history turns not strictly increasing: %w", ErrModelContract)
		}
		last = r.History[i].Turn
	}
	if err := checkSortedUniqueStrings(r.KnownEvidenceIDs, "known evidence ids"); err != nil {
		return fmt.Errorf("orchestrate: model request: %v: %w", err, ErrModelContract)
	}
	if r.Turn < 1 {
		return fmt.Errorf("orchestrate: model request turn must be >= 1: %w", ErrModelContract)
	}
	if strings.TrimSpace(r.RequestID) == "" || r.RequestID != strings.TrimSpace(r.RequestID) {
		return fmt.Errorf("orchestrate: model request needs a trimmed request id: %w", ErrModelContract)
	}
	return nil
}

// CanonicalModelRequest renders the canonical bytes: validate first (fail
// closed), then encode with encoding/json v1. Histories keep turn order
// (content, never re-sorted); KnownEvidenceIDs are sorted into a copy.
func CanonicalModelRequest(r ModelRequest) ([]byte, error) {
	if err := ValidateModelRequest(r); err != nil {
		return nil, err
	}
	c := r
	c.History = append([]TurnRecord(nil), r.History...)
	c.KnownEvidenceIDs = append([]string(nil), r.KnownEvidenceIDs...)
	slices.Sort(c.KnownEvidenceIDs)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("orchestrate: model request marshal: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// ModelResponse is one raw model turn: opaque output bytes plus the
// model identifier. The loop decodes and validates the payload.
type ModelResponse struct {
	Payload []byte
	ModelID string
}

// ValidateModelResponse checks the size cap. Empty payloads pass here
// and classify as I7 (ErrModelEmpty) at decode time.
func ValidateModelResponse(r ModelResponse, maxOutputBytes int) error {
	if maxOutputBytes < 1 {
		return fmt.Errorf("orchestrate: model response needs a positive byte cap: %w", ErrModelContract)
	}
	if len(r.Payload) > maxOutputBytes {
		return newInvalidError(invalidOversize, ErrModelContract, fmt.Errorf("payload %d bytes exceeds %d", len(r.Payload), maxOutputBytes))
	}
	return nil
}

// ModelAction is one decoded model act: exactly one of the two acts.
// CALL_TOOL carries the tool plus the full validated tool Request
// (identity echoing scope); SUBMIT_REPORT carries the terminal report.
type ModelAction struct {
	Action  string               `json:"action"`
	Tool    invest.ToolName      `json:"tool,omitempty"`
	Request *investigate.Request `json:"request,omitempty"`
	Report  *Report              `json:"report,omitempty"`
}

// Report is the terminal cognitive output: candidate hypotheses with
// required falsifiers, findings resolving hypotheses, one closed-action
// recommendation, and additive-only missing items. It carries no
// transition, no verdict, and no outcome vocabulary: T11 persistence and
// any human decision live outside this package.
type Report struct {
	Hypotheses      []invest.Hypothesis   `json:"hypotheses"`
	Findings        []invest.Finding      `json:"findings"`
	Recommendation  invest.Recommendation `json:"recommendation"`
	MissingAdditive []invest.MissingItem  `json:"missing_additive"`
}

// TurnRecord is one executed tool turn as the model sees it: turn number,
// tool, request hash, response IDs, row count, and error code. No values,
// no text, no raw output.
type TurnRecord struct {
	Turn        int             `json:"turn"`
	Tool        invest.ToolName `json:"tool"`
	RequestHash string          `json:"request_hash"`
	ResponseIDs []string        `json:"response_ids"`
	RowCount    int             `json:"row_count"`
	ErrorCode   string          `json:"error_code"`
}

// ValidateTurnRecord checks one history record standalone.
func ValidateTurnRecord(r TurnRecord) error {
	if r.Turn < 1 {
		return fmt.Errorf("orchestrate: turn record turn must be >= 1: %w", ErrModelContract)
	}
	if !invest.IsAllowlisted(r.Tool) {
		return fmt.Errorf("orchestrate: turn record tool %q not allowlisted: %w", string(r.Tool), ErrModelContract)
	}
	if !validHex64(r.RequestHash) {
		return fmt.Errorf("orchestrate: turn record needs a 64-hex request hash: %w", ErrModelContract)
	}
	if err := checkSortedUniqueStrings(r.ResponseIDs, "turn record response ids"); err != nil {
		return err
	}
	if len(r.ResponseIDs) > investigate.MaxResponseIDs {
		return fmt.Errorf("orchestrate: turn record carries %d ids, cap %d: %w", len(r.ResponseIDs), investigate.MaxResponseIDs, ErrModelContract)
	}
	if r.RowCount < 0 || r.RowCount > investigate.MaxResponseIDs {
		return fmt.Errorf("orchestrate: turn record row_count %d outside 0..%d: %w", r.RowCount, investigate.MaxResponseIDs, ErrModelContract)
	}
	if !validErrorCode(r.ErrorCode) {
		return fmt.Errorf("orchestrate: turn record has unknown error code %q: %w", r.ErrorCode, ErrModelContract)
	}
	return nil
}

// PromptTemplate is the code-constant prompt: fixed instructions plus one
// DATA block. Defense in depth only; the real boundary is validation, so
// injection inside envelope fields cannot widen what the loop accepts.
//
// The discriminator rule below is normative prompt text, not decoration.
// ModelAction tags the discriminator `json:"action"` and DecodeModelAction
// runs with DisallowUnknownFields, so an object keyed "act" is an unknown
// field, classifies I1-malformed, and escalates INVALID_OUTPUT. Naming the
// natural-language verb "act" instead of the JSON property is exactly what
// produced the APA-49 production failure: the ADR-002 canonical model
// complied with the prose and emitted {"act":"call_tool",...} three times
// out of three. The property is therefore stated literally, the "act"
// reading is ruled out by name, and the literal shape of each act is shown
// so the model never has to infer a field name.
//
// The nested request and report schemas are documented for the same reason
// and at the same time. Showing the act object while hiding what it contains
// leaves the model to invent the property names, and it does so reliably and
// wrongly: "tenant_id" (a real field under the wrong name), "evidence_ids"
// (not a request field at all), then, once the request bound, a report object
// invented twelve times out of twelve with "value" for "agreed", "description"
// for "summary", "reason" for "rationale", and "manual_review" for an action
// outside the closed enum. The names are therefore stated literally, the
// wrong readings are ruled out by name, and each act carries one complete
// parseable example, so the model copies structure instead of inventing it.
//
// The behavioural policy below is normative prompt text for the same reason
// the schema text is. Once layers 1/2/3 were fixed, every act the real model
// emitted decoded cleanly and 0 invalid-output classes fired, so the schema
// contract was QUALIFIED — and the run still ended 0/3 REPORT_READY: the
// model emitted a byte-identical call_tool act on all 6 loop turns across 3
// repetitions and the deterministic repetition guard escalated REPETITION.
// The guard was right; the prompt had never told the model that repeating an
// act observes nothing, nor when to leave the act it was shown first. A
// prompt that enumerates two acts and their shapes without ever stating the
// transition between them teaches a shape, not a decision.
//
// So the state policy is stated explicitly, and it is stated BEFORE the
// canonical examples. That placement is load-bearing rather than stylistic:
// the examples were the only act-shaped content in the prompt, so a rule
// printed after them reads as commentary on them, and the model anchors on
// the nearest template. The examples are still here and still teach
// structure — they are demoted from decision to illustration, which is what
// the replacement lead-in says in as many words.
//
// The policy is deliberately general. It is conditioned on whether the
// evidence is sufficient and whether an allowed tool can close the gap, never
// on which tool was called or on which turn it is, so it states a decision
// procedure instead of a script for one scenario. Naming a tool-to-act
// sequence would have been overfitting: it would be false for every other
// envelope and would turn a policy into a lookup table.
//
// What is deliberately NOT here: Go struct definitions, and any second name
// for the agreed value. The authoritative types already carry correct
// snake_case tags, and grounding compares a fact reference's "agreed" byte for
// byte against the agreed snapshot, so "value" must stay a wrong answer rather
// than become an accepted synonym.
const PromptTemplate = `You are a bounded claim-investigation planner operating under a closed tool allowlist.

Rules:
- Respond with exactly one JSON object, and with nothing outside it.
- That object must carry the property "action". Its value is exactly "call_tool" or "submit_report". The property name is "action", never "act".
- For the value "call_tool", also carry "tool" (one allowlisted tool name) and "request" (that tool's bounded request object). The writer tool is never callable.
- The "request" object carries exactly these properties, all lower snake_case: "tool", "tenant_id", "claim_id", "investigation_id", "request_id", "limit", and only for tools that own them, "cursor", "query", "subject_id", "source_type", "hash", "payload". Its "tool" repeats the act's "tool". Its four identity values repeat tenant_id, claim_id, investigation_id, and request_id from the DATA block verbatim.
- Knob ownership is fixed: a knob set on a tool that does not own it is rejected.
  - "cursor": optional; owned by get_documents, get_evidence. Opaque page token copied from the previous response's next_cursor.
  - "query": optional; owned by search_evidence. Lexical search text.
  - "subject_id": optional; owned by get_policy_context, get_external_policy_status, get_tpa_case, get_provider_encounter, create_investigation_report. Never a document id, evidence id, claim id, or investigation id: a policy identifier for get_policy_context and get_external_policy_status, a TPA case identifier for get_tpa_case, a provider encounter identifier for get_provider_encounter, the exception subject identifier for create_investigation_report. A DocumentID returned by get_documents is NOT a valid subject_id: get_documents does not produce a subject identifier, and get_evidence does not accept subject_id.
  - "source_type": optional; owned by get_evidence. Evidence source filter.
  - "hash": required; owned by create_investigation_report. The report's sha256 hex.
  - "payload": required; owned by create_investigation_report. The canonical report bytes.
- A "request" selects and bounds a read. It never carries evidence IDs: "evidence_ids" is not a request property. Cite evidence only inside a "submit_report" report, using IDs that are already in "known_evidence_ids" or were returned by an earlier turn.
- For the value "submit_report", carry "report" with "hypotheses" (at least one), "findings" (at least one), "recommendation", and "missing_additive" (optional: omit it or send []).
- A hypothesis carries "id", "statement", "falsifier", "status", "fact_refs", and "evidence_ids". "falsifier" is required and states what cited evidence would refute the hypothesis; never substitute a confidence. "status" is exactly one of "OPEN", "SUPPORTED", "REFUTED". Sort every "evidence_ids" list.
- A fact reference inside "fact_refs" carries exactly "key", "agreed", "evidence_id", all required. "agreed" copies the agreed snapshot value verbatim and "evidence_id" is one of that key's evidence rows. The property is "agreed": there is no "value" property in a fact reference.
- A finding carries "id", "hypothesis_id", "summary", "evidence_ids". The property is "summary", never "description".
- "recommendation" carries "action", "rationale", "finding_ids". "action" is exactly one of "REQUEST_EVIDENCE", "CONFIRM_EXCEPTION", "REFER_HUMAN", "REVERIFY"; there is no "manual_review". The property is "rationale", never "reason".
%s
- Cite only the agreed snapshot and evidence you were given. Never invent, rename, or reinterpret an evidence ID or an agreed value.
- Every cited evidence ID must already be known: IDs from the exception envelope or from prior tool results.
- Every finding must name a hypothesis from the same report. The recommendation must cite findings from the same report.
- Never emit a property outside the shapes below. Never emit free-form text outside the JSON object.

State policy. Observe, decide, then act; the decision comes before the act:
- Observe: the exception, the turn number, the history of executed calls, and the evidence IDs you already know.
- Decide, then act on the decision. Every turn is exactly one of these three:
  - The grounded evidence is sufficient for every required report field: act submit_report.
  - The evidence is insufficient and an allowed tool can resolve the gap: call that tool, once.
  - The evidence is insufficient and no allowed tool can resolve the gap: act submit_report and name what is missing in "missing_additive". That is the answer, not a failure, and it ends the investigation.
- Call another tool only when the evidence you already have is insufficient for a required report field, or an allowed next-step tool is genuinely needed to close the gap. A read you did not need is not progress.
- After a tool result, never repeat an identical tool call: its "tool" name and "request_hash" fingerprint already appear in your history, so a second identical read observes nothing.
- Never repeat an identical action/request pair, matching on that same "tool" and "request_hash". If you are about to reissue an act already in your history, you are about to loop: decide again under this policy instead.

Canonical act examples. They illustrate the structure only; which of the two you emit is decided by the state policy above, never by the example that looks closest. Substitute the text and the IDs you were given:
{"action":"call_tool","tool":"get_documents","request":{"tool":"get_documents","tenant_id":"tnt-...","claim_id":"clm-...","investigation_id":"inv-...","request_id":"req-...","limit":10}}
{"action":"submit_report","report":{"hypotheses":[{"id":"h-01","statement":"The policy number conflict stems from transcription variance.","falsifier":"A pinned policy record showing the claimed number as active.","status":"OPEN","fact_refs":[{"key":"hospital_name","agreed":"City Hospital","evidence_id":"ev-doc-02"}],"evidence_ids":["ev-doc-01","ev-doc-02"]}],"findings":[{"id":"f-01","hypothesis_id":"h-01","summary":"The claim form and the policy schedule state different policy numbers.","evidence_ids":["ev-doc-01","ev-doc-02"]}],"recommendation":{"action":"REFER_HUMAN","rationale":"A human must determine which policy number is authoritative.","finding_ids":["f-01"]},"missing_additive":[]}}
A non-empty "missing_additive" is shaped the same way, one entry per gap, sorted by "kind" then "key" then "detail". Here a claim field, an upstream source, and an absent required document are three different kinds:
{"action":"submit_report","report":{"hypotheses":[{"id":"h-01","statement":"The billed total exceeds the policy-schedule allowance.","falsifier":"A pinned policy schedule showing the allowed amount for this admission.","status":"OPEN","fact_refs":[{"key":"hospital_name","agreed":"City Hospital","evidence_id":"ev-doc-02"}],"evidence_ids":["ev-doc-01","ev-doc-02"]}],"findings":[{"id":"f-01","hypothesis_id":"h-01","summary":"The hospital bill total and the policy schedule allowance disagree.","evidence_ids":["ev-doc-01","ev-doc-02"]}],"recommendation":{"action":"REQUEST_EVIDENCE","rationale":"The allowance cannot be confirmed without the pinned policy schedule.","finding_ids":["f-01"]},"missing_additive":[{"kind":"external","key":"policy","detail":"R1: no pinned policy evidence for the claimed policy_number"},{"kind":"field","key":"policy_number","detail":"R1: claimed value conflicts with the policy schedule value"},{"kind":"required_document","key":"HOSPITAL_BILL","detail":"R8: required document absent"}]}}

DATA:
%s`

// additiveKindKeyClause is the model-facing statement of which vocabulary a
// "missing_additive" key must be drawn from, given the entry's "kind".
//
// APA-56. The prompt previously closed only the "kind" set and left every
// "key" vocabulary unstated, so Qwen paired the real claim field
// "policy_number" with kind "external" and the additive validator rejected
// the report on 17 of 17 live runs, including the sufficient-evidence
// control. The rule that decides the answer is a real claim field, so its
// kind is "field"; saying so is what the model was missing.
//
// The external vocabulary is read from invest rather than spelled here, so
// the prompt cannot drift from the authority that rejects unknown keys.
// TestAPA56_ExternalVocabularyMatchesAuthority pins the two together.
func additiveKindKeyClause() string {
	ext := invest.ExternalSourceKeys()
	quoted := make([]string, len(ext))
	for i, k := range ext {
		quoted[i] = `"` + k + `"`
	}
	docs := []string{verify.DocClaimForm, verify.DocDischargeSummary, verify.DocHospitalBill}
	return fmt.Sprintf(`- A "missing_additive" entry carries "kind", "key", "detail". Its "kind" decides which vocabulary the "key" is drawn from, and a key outside the vocabulary for the kind you chose is rejected:
  - "required_document": the "key" is one of the required document types %s.
  - "field": the "key" is a claim field key that appears in the DATA block, such as "policy_number".
  - "external": the "key" is exactly one of %s. These name upstream sources and nothing else.
- "policy_number" is a claim field, so it is "kind" "field". It is never "kind" "external", which is reserved for the upstream source names above.
- Carry every entry of the DATA block's "missing_evidence" across into "missing_additive" with its "kind", "key", and "detail" unchanged. The list is additive: you may add an entry, never drop one.
- Sort the array by "kind", then "key", then "detail".`,
		strings.Join(docs, ", "), strings.Join(quoted, ", "))
}

// RenderPrompt renders the template over the canonical request bytes.
// It reads only validated ModelRequest fields, so no secret can enter.
func RenderPrompt(r ModelRequest) (string, error) {
	raw, err := CanonicalModelRequest(r)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(PromptTemplate, additiveKindKeyClause(), string(raw)), nil
}
