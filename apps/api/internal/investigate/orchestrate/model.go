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
const PromptTemplate = `You are a bounded claim-investigation planner operating under a closed tool allowlist.

Rules:
- Respond with exactly one JSON object: either a call_tool act or a submit_report act.
- call_tool names one allowlisted tool and its bounded request. The writer tool is never callable.
- submit_report carries hypotheses, findings, one recommendation, and additive-only missing items.
- Every cited evidence ID must already be known: IDs from the exception envelope or from prior tool results.
- Every finding must name a hypothesis from the same report. The recommendation must cite findings from the same report.
- Hypothesis fact references must echo agreed snapshot entries exactly.
- Never emit unknown fields. Never emit free-form text outside the JSON object.

DATA:
%s`

// RenderPrompt renders the template over the canonical request bytes.
// It reads only validated ModelRequest fields, so no secret can enter.
func RenderPrompt(r ModelRequest) (string, error) {
	raw, err := CanonicalModelRequest(r)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(PromptTemplate, string(raw)), nil
}
