package orchestrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"claimops-api/internal/invest"
)

// Outcome is the closed terminal set for one investigation run.
type Outcome string

const (
	// OutcomeReportReady marks a grounded SUBMIT_REPORT: Report carries
	// the terminal cognitive output and no escalation reason is set.
	OutcomeReportReady Outcome = "REPORT_READY"
	// OutcomeEscalated marks a loop exit without an accepted report:
	// EscalationReason names the exit class and AttemptLog plus Partial
	// carry the observable progress.
	OutcomeEscalated Outcome = "ESCALATED"
)

// EscalationReason is the closed exit-class set for OutcomeEscalated.
type EscalationReason string

const (
	// EscalationTurnsExhausted marks MaxTurns spent without an accepted
	// report (wraps ErrBudgetExceeded).
	EscalationTurnsExhausted EscalationReason = "TURNS_EXHAUSTED"
	// EscalationCallsExhausted marks the model-call or tool-call budget
	// spent (wraps ErrBudgetExceeded).
	EscalationCallsExhausted EscalationReason = "CALLS_EXHAUSTED"
	// EscalationDeadline marks the scope deadline passing mid-run
	// (wraps ErrDeadlineExceeded).
	EscalationDeadline EscalationReason = "DEADLINE"
	// EscalationRepetition marks an exact-repeat tool call
	// (wraps ErrRepetition).
	EscalationRepetition EscalationReason = "REPETITION"
	// EscalationNoProgress marks consecutive tool turns that widen
	// KnownEvidence by nothing (wraps ErrModelContract).
	EscalationNoProgress EscalationReason = "NO_PROGRESS"
	// EscalationInvalidOutput marks a second consecutive invalid act or
	// a non-repromptable invalid class (carries the triggering
	// invalidError, hence ErrModelContract/ErrToolDenied/ErrGrounding).
	EscalationInvalidOutput EscalationReason = "INVALID_OUTPUT"
	// EscalationModelUpstream marks model transport failing after the
	// single retry (wraps ErrModelUpstream).
	EscalationModelUpstream EscalationReason = "MODEL_UPSTREAM"
)

// ModelSubmitReport is the terminal cognitive output the loop accepts on
// SUBMIT_REPORT. It aliases Report (validated by ValidateReport, grounded
// by CheckReportGrounding); the distinct name keeps the output contract
// explicit at the run boundary.
type ModelSubmitReport = Report

// InvestigationOutput is the sole Run result: either a grounded report or
// an escalation with the observable progress. AttemptLog carries
// hashes/counts/IDs only (see TurnRecord); no values, no text, no raw
// output cross this boundary.
//
// Plain struct with package-level functions only: wire types carry zero
// methods in this package, and the only method in non-test code is
// (*Loop).Run.
type InvestigationOutput struct {
	InvestigationID  string             `json:"investigation_id"`
	Outcome          Outcome            `json:"outcome"`
	Report           *ModelSubmitReport `json:"report,omitempty"`
	EscalationReason EscalationReason   `json:"escalation_reason,omitempty"`
	Partial          *ModelSubmitReport `json:"partial,omitempty"`
	AttemptLog       []TurnRecord       `json:"attempt_log"`
	ModelID          string             `json:"model_id,omitempty"`
	TurnsUsed        int                `json:"turns_used"`
	ToolCallsUsed    int                `json:"tool_calls_used"`
}

// ValidateInvestigationOutput checks one run result standalone.
func ValidateInvestigationOutput(o InvestigationOutput) error {
	if err := invest.ValidateID(invest.InvestigationIDPrefix, o.InvestigationID); err != nil {
		return fmt.Errorf("orchestrate: output: %v: %w", err, ErrModelContract)
	}
	switch o.Outcome {
	case OutcomeReportReady:
		if o.Report == nil {
			return fmt.Errorf("orchestrate: output REPORT_READY without a report: %w", ErrModelContract)
		}
		if err := ValidateReport(*o.Report); err != nil {
			return fmt.Errorf("orchestrate: output report: %v: %w", err, ErrModelContract)
		}
		if o.EscalationReason != "" {
			return fmt.Errorf("orchestrate: output REPORT_READY must not carry a reason: %w", ErrModelContract)
		}
		if o.Partial != nil {
			return fmt.Errorf("orchestrate: output REPORT_READY must not carry partial: %w", ErrModelContract)
		}
	case OutcomeEscalated:
		switch o.EscalationReason {
		case EscalationTurnsExhausted, EscalationCallsExhausted, EscalationDeadline,
			EscalationRepetition, EscalationNoProgress, EscalationInvalidOutput,
			EscalationModelUpstream:
		default:
			return fmt.Errorf("orchestrate: output ESCALATED with unknown reason %q: %w", string(o.EscalationReason), ErrModelContract)
		}
		if o.Report != nil {
			return fmt.Errorf("orchestrate: output ESCALATED must not carry a report: %w", ErrModelContract)
		}
		if o.Partial != nil {
			if err := ValidateReport(*o.Partial); err != nil {
				return fmt.Errorf("orchestrate: output partial: %v: %w", err, ErrModelContract)
			}
		}
	default:
		return fmt.Errorf("orchestrate: output has unknown outcome %q: %w", string(o.Outcome), ErrModelContract)
	}
	last := 0
	for i := range o.AttemptLog {
		if err := ValidateTurnRecord(o.AttemptLog[i]); err != nil {
			return fmt.Errorf("orchestrate: output attempt_log[%d]: %v: %w", i, err, ErrModelContract)
		}
		if o.AttemptLog[i].Turn <= last {
			return fmt.Errorf("orchestrate: output attempt turns not strictly increasing: %w", ErrModelContract)
		}
		last = o.AttemptLog[i].Turn
	}
	if o.ModelID != "" && (strings.TrimSpace(o.ModelID) == "" || o.ModelID != strings.TrimSpace(o.ModelID)) {
		return fmt.Errorf("orchestrate: output model id must be trimmed non-blank: %w", ErrModelContract)
	}
	if o.TurnsUsed < len(o.AttemptLog) {
		return fmt.Errorf("orchestrate: output turns_used %d below attempt count %d: %w", o.TurnsUsed, len(o.AttemptLog), ErrModelContract)
	}
	if o.ToolCallsUsed < 0 || o.ToolCallsUsed > len(o.AttemptLog) {
		return fmt.Errorf("orchestrate: output tool_calls_used %d outside 0..%d: %w", o.ToolCallsUsed, len(o.AttemptLog), ErrModelContract)
	}
	if o.ToolCallsUsed > o.TurnsUsed {
		return fmt.Errorf("orchestrate: output tool_calls_used exceeds turns_used: %w", ErrModelContract)
	}
	return nil
}

// normalizeReportCopy returns a canonical copy: sorted additive items and
// nil-normalized slices, without reordering hypotheses, findings, or
// review-order content. Marshal path only.
func normalizeReportCopy(r Report) Report {
	out := r
	out.Hypotheses = append([]invest.Hypothesis(nil), r.Hypotheses...)
	out.Findings = append([]invest.Finding(nil), r.Findings...)
	if out.Hypotheses == nil {
		out.Hypotheses = []invest.Hypothesis{}
	}
	if out.Findings == nil {
		out.Findings = []invest.Finding{}
	}
	for i := range out.Hypotheses {
		h := &out.Hypotheses[i]
		if h.FactRefs == nil {
			h.FactRefs = []invest.FactRef{}
		}
		if h.EvidenceIDs == nil {
			h.EvidenceIDs = []string{}
		}
	}
	for i := range out.Findings {
		if out.Findings[i].EvidenceIDs == nil {
			out.Findings[i].EvidenceIDs = []string{}
		}
	}
	if out.Recommendation.FindingIDs == nil {
		out.Recommendation.FindingIDs = []string{}
	}
	out.MissingAdditive = sortMissingAdditive(r.MissingAdditive)
	return out
}

// normalizeOutputCopy returns a canonical copy with every nil slice
// normalized to empty, so marshaling is a pure function of content.
// Attempt order is preserved verbatim (turn order, never re-sorted).
func normalizeOutputCopy(o InvestigationOutput) InvestigationOutput {
	out := o
	if out.AttemptLog == nil {
		out.AttemptLog = []TurnRecord{}
	} else {
		out.AttemptLog = append([]TurnRecord(nil), o.AttemptLog...)
	}
	for i := range out.AttemptLog {
		if out.AttemptLog[i].ResponseIDs == nil {
			out.AttemptLog[i].ResponseIDs = []string{}
		}
	}
	if out.Report != nil {
		r := normalizeReportCopy(*out.Report)
		out.Report = &r
	}
	if out.Partial != nil {
		p := normalizeReportCopy(*out.Partial)
		out.Partial = &p
	}
	return out
}

// MarshalOutput renders the canonical bytes for one run result: validate
// first (fail closed), then encode a canonicalized copy with
// encoding/json v1. Equivalent outputs produce byte-identical results.
func MarshalOutput(o InvestigationOutput) ([]byte, error) {
	if err := ValidateInvestigationOutput(o); err != nil {
		return nil, err
	}
	c := normalizeOutputCopy(o)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("orchestrate: output marshal: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// DecodeOutput parses and validates one run result with strict field
// checking: unknown fields (including stage-foreign keys) are rejected,
// trailing data is rejected, and the result must pass
// ValidateInvestigationOutput.
func DecodeOutput(data []byte) (InvestigationOutput, error) {
	var o InvestigationOutput
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return InvestigationOutput{}, fmt.Errorf("orchestrate: decode output: %v: %w", err, ErrModelContract)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return InvestigationOutput{}, fmt.Errorf("orchestrate: decode output: trailing data: %w", ErrModelContract)
	}
	o = normalizeOutputCopy(o)
	if err := ValidateInvestigationOutput(o); err != nil {
		return InvestigationOutput{}, err
	}
	return o, nil
}
