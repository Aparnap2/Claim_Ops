package orchestrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
)

// Loop budget defaults (issue #66, owner-confirmed): at most 12 turns,
// 13 model calls (one re-prompt of headroom), input/output byte caps, and
// a 30s per-turn model bound. MaxToolCalls always comes from the scope
// (see DefaultBudgets) and may never exceed it.
const (
	// DefaultMaxTurns bounds investigation turns.
	DefaultMaxTurns = 12
	// DefaultMaxModelCalls bounds Complete calls (turns plus re-prompts).
	DefaultMaxModelCalls = 13
	// DefaultMaxInputBytes caps one canonical ModelRequest.
	DefaultMaxInputBytes = 256 * 1024
	// DefaultMaxOutputBytes caps one raw model payload.
	DefaultMaxOutputBytes = 64 * 1024
	// DefaultTurnTimeoutMs bounds one Complete call.
	DefaultTurnTimeoutMs = int64(30000)
	// MaxDeadlineMs caps the scope deadline the loop honors (1h): an
	// unbounded investigation holds no audit meaning and risks wedging
	// the run behind a hung model. Fail closed (reject, never clamp
	// silently) so the caller picks an explicit bound.
	MaxDeadlineMs = int64(3600000)
	// MaxTurnTimeoutMs caps one Complete call (5min): a longer single
	// turn defeats the turn-granularity deadline check below.
	MaxTurnTimeoutMs = int64(300000)
)

// noProgressThreshold counts the consecutive tool turns that widen
// KnownEvidence by nothing before the loop escalates NO_PROGRESS.
const noProgressThreshold = 3

// Unwired metric names. Reserved strings only: this package wires no
// metrics backend, so the loop never emits them; a future wiring change
// adopts these names instead of inventing new ones.
const (
	// MetricModelCallsTotal counts Complete calls per investigation.
	MetricModelCallsTotal = "claimops_investigate_orchestrate_model_calls_total"
	// MetricToolCallsTotal counts executed tool calls per investigation.
	MetricToolCallsTotal = "claimops_investigate_orchestrate_tool_calls_total"
	// MetricTurnsTotal counts consumed turns per investigation.
	MetricTurnsTotal = "claimops_investigate_orchestrate_turns_total"
	// MetricReportsReadyTotal counts REPORT_READY outcomes.
	MetricReportsReadyTotal = "claimops_investigate_orchestrate_reports_ready_total"
	// MetricEscalationsTotal counts ESCALATED outcomes, labeled by reason.
	MetricEscalationsTotal = "claimops_investigate_orchestrate_escalations_total"
)

// Budgets bounds one Run. MaxToolCalls is the loop-side tool cap and must
// stay within Scope.MaxCalls (fail closed: the loop may never grant more
// than the scope allows).
//
// Plain struct with package-level functions only: wire types carry zero
// methods in this package, and the only method in non-test code is
// (*Loop).Run.
type Budgets struct {
	MaxTurns       int
	MaxModelCalls  int
	MaxToolCalls   int
	MaxInputBytes  int
	MaxOutputBytes int
	TurnTimeoutMs  int64
}

// DefaultBudgets returns the spec defaults with MaxToolCalls drawn from
// the scope. The scope must already be valid.
func DefaultBudgets(scope investigate.Scope) Budgets {
	return Budgets{
		MaxTurns:       DefaultMaxTurns,
		MaxModelCalls:  DefaultMaxModelCalls,
		MaxToolCalls:   scope.MaxCalls,
		MaxInputBytes:  DefaultMaxInputBytes,
		MaxOutputBytes: DefaultMaxOutputBytes,
		TurnTimeoutMs:  DefaultTurnTimeoutMs,
	}
}

// ValidateBudgets checks one budget set against its scope (fail closed).
func ValidateBudgets(b Budgets, scope investigate.Scope) error {
	if err := scope.Validate(); err != nil {
		return fmt.Errorf("orchestrate: budgets scope: %w", err)
	}
	if b.MaxTurns < 1 {
		return fmt.Errorf("orchestrate: budgets max_turns must be >= 1: %w", ErrModelContract)
	}
	if b.MaxModelCalls < b.MaxTurns {
		return fmt.Errorf("orchestrate: budgets max_model_calls %d below max_turns %d (one call per turn minimum): %w", b.MaxModelCalls, b.MaxTurns, ErrModelContract)
	}
	if b.MaxToolCalls < 1 {
		return fmt.Errorf("orchestrate: budgets max_tool_calls must be >= 1: %w", ErrModelContract)
	}
	if b.MaxToolCalls > scope.MaxCalls {
		return fmt.Errorf("orchestrate: budgets max_tool_calls %d exceeds scope max %d: %w", b.MaxToolCalls, scope.MaxCalls, ErrModelContract)
	}
	if b.MaxInputBytes < 1 {
		return fmt.Errorf("orchestrate: budgets max_input_bytes must be >= 1: %w", ErrModelContract)
	}
	if b.MaxOutputBytes < 1 {
		return fmt.Errorf("orchestrate: budgets max_output_bytes must be >= 1: %w", ErrModelContract)
	}
	if b.TurnTimeoutMs < 1 {
		return fmt.Errorf("orchestrate: budgets turn_timeout_ms must be >= 1: %w", ErrModelContract)
	}
	if scope.DeadlineMs > MaxDeadlineMs {
		return fmt.Errorf("orchestrate: budgets scope deadline_ms %d exceeds cap %d: %w", scope.DeadlineMs, MaxDeadlineMs, ErrModelContract)
	}
	if b.TurnTimeoutMs > MaxTurnTimeoutMs {
		return fmt.Errorf("orchestrate: budgets turn_timeout_ms %d exceeds cap %d: %w", b.TurnTimeoutMs, MaxTurnTimeoutMs, ErrModelContract)
	}
	return nil
}

// Audit event names for the loop lifecycle rows.
const (
	// AuditEventStarted opens one Run.
	AuditEventStarted = "started"
	// AuditEventFinished closes one Run (every exit path emits it).
	AuditEventFinished = "finished"
)

// LoopAuditEvent is one loop lifecycle row. It follows the existing audit
// convention from investigate/audit.go: IDs, hashes, counts, outcome, and
// reason only — never values, prompts, payloads, or raw model output.
type LoopAuditEvent struct {
	Event              string
	InvestigationID    string
	TenantID           string
	ClaimID            string
	RequestID          string
	Outcome            string
	Reason             string
	TurnsUsed          int
	ToolCallsUsed      int
	KnownEvidenceCount int
}

// LoopAuditHook observes the started/finished rows. Best-effort and
// synchronous: implementations must not block and must never call back
// into the Loop. A nil hook disables observation.
type LoopAuditHook func(ctx context.Context, e LoopAuditEvent)

// ValidateLoopAuditEvent checks one lifecycle row standalone (fail
// closed: unbound rows are never emitted).
func ValidateLoopAuditEvent(e LoopAuditEvent) error {
	switch e.Event {
	case AuditEventStarted, AuditEventFinished:
	default:
		return fmt.Errorf("orchestrate: audit has unknown event %q: %w", e.Event, ErrModelContract)
	}
	if err := invest.ValidateID(invest.InvestigationIDPrefix, e.InvestigationID); err != nil {
		return fmt.Errorf("orchestrate: audit: %v: %w", err, ErrModelContract)
	}
	for _, id := range []struct {
		name, val string
	}{
		{"tenant_id", e.TenantID}, {"claim_id", e.ClaimID}, {"request_id", e.RequestID},
	} {
		if strings.TrimSpace(id.val) == "" || id.val != strings.TrimSpace(id.val) {
			return fmt.Errorf("orchestrate: audit has untrimmed or blank %s: %w", id.name, ErrModelContract)
		}
	}
	if e.Event == AuditEventStarted && (e.Outcome != "" || e.Reason != "") {
		return fmt.Errorf("orchestrate: audit started must not carry outcome: %w", ErrModelContract)
	}
	if e.Event == AuditEventFinished {
		switch Outcome(e.Outcome) {
		case OutcomeReportReady, OutcomeEscalated:
		default:
			return fmt.Errorf("orchestrate: audit finished has unknown outcome %q: %w", e.Outcome, ErrModelContract)
		}
		if e.Reason != "" {
			switch EscalationReason(e.Reason) {
			case EscalationTurnsExhausted, EscalationCallsExhausted, EscalationDeadline,
				EscalationRepetition, EscalationNoProgress, EscalationInvalidOutput,
				EscalationModelUpstream:
			default:
				return fmt.Errorf("orchestrate: audit finished has unknown reason %q: %w", e.Reason, ErrModelContract)
			}
		}
	}
	for _, c := range []struct {
		name string
		val  int
	}{
		{"turns_used", e.TurnsUsed}, {"tool_calls_used", e.ToolCallsUsed}, {"known_evidence_count", e.KnownEvidenceCount},
	} {
		if c.val < 0 {
			return fmt.Errorf("orchestrate: audit %s %d is negative: %w", c.name, c.val, ErrModelContract)
		}
	}
	return nil
}

// emitLoopAudit invokes the hook when set. Invalid rows are dropped (fail
// closed) and hook panics are recovered and dropped: a panicking observer
// must never take down the run (fail-closed audit). Drops stay silent by
// design — this package takes no logger dependency, so the audit path
// stays synchronous, non-blocking, and side-effect free; a dropped row
// always means a programmer-side bug (invalid row construction or a
// hostile hook), and both are pinned by unit tests, not by prod logs.
func emitLoopAudit(hook LoopAuditHook, ctx context.Context, e LoopAuditEvent) {
	if hook == nil {
		return
	}
	if err := ValidateLoopAuditEvent(e); err != nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	hook(ctx, e)
}

// Loop is one bounded investigation run: a model client, an executor, the
// budgets, the scope, and the exception envelope. Build with NewLoop; the
// only behavior is Run.
type Loop struct {
	model     ModelClient
	exec      *investigate.Executor
	budgets   Budgets
	scope     investigate.Scope
	exception invest.UnresolvedException
	audit     LoopAuditHook
}

// NewLoop assembles one run and fails closed: nil seams, invalid scope,
// invalid envelope, invalid budgets, scope authority outside the envelope
// (tools/call/deadline), and any tenant/claim/request split between scope
// and envelope are all construction errors. The tenant split carries
// ErrTenantMismatch (F6 class); everything else carries the contract
// chain.
func NewLoop(model ModelClient, exec *investigate.Executor, budgets Budgets, scope investigate.Scope, exception invest.UnresolvedException, audit LoopAuditHook) (*Loop, error) {
	if model == nil {
		return nil, fmt.Errorf("orchestrate: loop needs a model client: %w", ErrModelContract)
	}
	if exec == nil {
		return nil, fmt.Errorf("orchestrate: loop needs an executor: %w", ErrModelContract)
	}
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("orchestrate: loop scope: %w", err)
	}
	if err := invest.Validate(exception); err != nil {
		return nil, fmt.Errorf("orchestrate: loop exception: %w", err)
	}
	if err := ValidateBudgets(budgets, scope); err != nil {
		return nil, err
	}
	if err := checkLoopTenant(scope, exception); err != nil {
		return nil, err
	}
	if scope.ClaimID != exception.ClaimID {
		return nil, fmt.Errorf("orchestrate: scope claim %q != envelope claim %q: %w", scope.ClaimID, exception.ClaimID, ErrModelContract)
	}
	if scope.RequestID != exception.Scope.RequestID {
		return nil, fmt.Errorf("orchestrate: scope request %q != envelope scope request %q: %w", scope.RequestID, exception.Scope.RequestID, ErrModelContract)
	}
	if err := checkLoopAuthority(scope, exception); err != nil {
		return nil, err
	}
	return &Loop{model: model, exec: exec, budgets: budgets, scope: scope, exception: exception, audit: audit}, nil
}

// checkLoopAuthority binds the run scope to the envelope authority (fail
// closed: the loop may never grant more than the envelope authorizes).
// AllowTools must be a subset of the envelope tools (least privilege
// flows one way only), MaxCalls must not exceed the envelope call budget,
// and DeadlineMs must not outlive the envelope deadline (the scope may
// tighten the deadline, never extend it).
func checkLoopAuthority(scope investigate.Scope, e invest.UnresolvedException) error {
	allowed := make(map[invest.ToolName]struct{}, len(e.Scope.AllowTools))
	for _, t := range e.Scope.AllowTools {
		allowed[t] = struct{}{}
	}
	for _, t := range scope.AllowTools {
		if _, ok := allowed[t]; !ok {
			return fmt.Errorf("orchestrate: scope tool %q outside envelope authority: %w", string(t), ErrModelContract)
		}
	}
	if scope.MaxCalls > e.Scope.MaxToolCalls {
		return fmt.Errorf("orchestrate: scope max_calls %d exceeds envelope max %d: %w", scope.MaxCalls, e.Scope.MaxToolCalls, ErrModelContract)
	}
	if scope.DeadlineMs > e.Scope.DeadlineMs {
		return fmt.Errorf("orchestrate: scope deadline_ms %d outlives envelope deadline %d: %w", scope.DeadlineMs, e.Scope.DeadlineMs, ErrModelContract)
	}
	return nil
}

// checkLoopTenant asserts the scope tenant still equals the envelope
// tenant. The tenant comes from Scope only; a split aborts the run in
// the F6 class and returns ErrTenantMismatch.
func checkLoopTenant(scope investigate.Scope, e invest.UnresolvedException) error {
	if scope.TenantID != e.TenantID {
		return fmt.Errorf("orchestrate: scope tenant %q != envelope tenant %q: %w", scope.TenantID, e.TenantID, ErrTenantMismatch)
	}
	return nil
}

// canonicalToolRequest renders the deterministic bytes hashed for
// TurnRecord.RequestHash and the repetition check.
//
// Determinism property: investigate.Request is a struct, and
// encoding/json v1 marshals struct fields in declaration order with no
// map iteration, so equal requests always render byte-identical output
// and hash equal; distinct requests (any field differs, including Limit)
// hash distinct. The property holds without sorting because no map or
// unordered member crosses this boundary.
func canonicalToolRequest(req investigate.Request) ([]byte, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("orchestrate: tool request marshal: %w", err)
	}
	return raw, nil
}

// repetitionKey hashes one tool act (tool plus canonical request bytes)
// for the exact-repeat check.
func repetitionKey(tool invest.ToolName, rawReq []byte) string {
	sum := sha256.Sum256(append([]byte(string(tool)+"\x00"), rawReq...))
	return hex.EncodeToString(sum[:])
}

// submitPartial returns the report for the escalation Partial slot when
// the triggering act was a structurally valid SUBMIT_REPORT (for example
// a grounding rejection): observable progress worth handing to the next
// stage. Anything else yields nil.
func submitPartial(a ModelAction) *Report {
	if a.Action != ActionSubmitReport || a.Report == nil {
		return nil
	}
	if err := ValidateReport(*a.Report); err != nil {
		return nil
	}
	out := *a.Report
	return &out
}

// buildEscalation assembles the ESCALATED output for one exit. History is
// copied so later appends cannot alias the result.
func buildEscalation(invID string, reason EscalationReason, partial *Report, history []TurnRecord, modelID string, turnsUsed, toolCalls int) InvestigationOutput {
	var p *ModelSubmitReport
	if partial != nil {
		r := normalizeReportCopy(*partial)
		p = &r
	}
	return InvestigationOutput{
		InvestigationID:  invID,
		Outcome:          OutcomeEscalated,
		EscalationReason: reason,
		Partial:          p,
		AttemptLog:       append([]TurnRecord(nil), history...),
		ModelID:          modelID,
		TurnsUsed:        turnsUsed,
		ToolCallsUsed:    toolCalls,
	}
}

// escalationError classifies one escalation exit. INVALID_OUTPUT returns
// the triggering invalidError itself (preserving the I-class chain);
// every other reason wraps its sentinel with the human context. Causes
// use %w (never %v) so errors.Is classifies both the escalation sentinel
// and the wrapped cause chain.
func escalationError(reason EscalationReason, cause error) error {
	if reason == EscalationInvalidOutput && cause != nil {
		return cause
	}
	msg := "orchestrate: escalated (" + string(reason) + ")"
	switch reason {
	case EscalationTurnsExhausted, EscalationCallsExhausted:
		if cause == nil {
			return fmt.Errorf("%s: %w", msg, ErrBudgetExceeded)
		}
		return fmt.Errorf("%s: %w: %w", msg, cause, ErrBudgetExceeded)
	case EscalationDeadline:
		return fmt.Errorf("%s: %w", msg, ErrDeadlineExceeded)
	case EscalationRepetition:
		if cause == nil {
			return fmt.Errorf("%s: %w", msg, ErrRepetition)
		}
		return fmt.Errorf("%s: %w: %w", msg, cause, ErrRepetition)
	case EscalationNoProgress:
		if cause == nil {
			return fmt.Errorf("%s: %w", msg, ErrModelContract)
		}
		return fmt.Errorf("%s: %w: %w", msg, cause, ErrModelContract)
	case EscalationModelUpstream:
		if cause == nil {
			return fmt.Errorf("%s: %w", msg, ErrModelUpstream)
		}
		return fmt.Errorf("%s: %w: %w", msg, cause, ErrModelUpstream)
	default:
		return fmt.Errorf("%s: %w", msg, ErrModelContract)
	}
}

// Run executes the bounded loop and returns the InvestigationOutput. The
// contract, in turn order:
//
//   - ctx cancellation returns the context error itself (never wrapped,
//     never retried); F6 tenant splits abort with ErrTenantMismatch.
//   - Each turn builds the canonical ModelRequest (size-checked against
//     MaxInputBytes), calls Complete under the turn timeout, then
//     decodes and validates the single act.
//   - ModelUpstream failures retry exactly once per turn, then escalate
//     MODEL_UPSTREAM; any other model failure escalates immediately.
//   - I1/I2/I7 earn one same-turn re-prompt; every other invalid class,
//     and any second consecutive invalid, escalates INVALID_OUTPUT.
//     Invalid acts never consume tool budget and never grow
//     KnownEvidence.
//   - CALL_TOOL runs the tool-budget check, the exact-repeat check, and
//     exec.Execute, then records a TurnRecord and grows KnownEvidence
//     from validated Response.IDs. The writer tool is never callable,
//     and this loop never calls it.
//   - SUBMIT_REPORT runs grounding Gate B and finishes REPORT_READY.
//   - Turns, model calls, tool calls, the scope deadline, repetition,
//     and stagnation escalate with reason plus AttemptLog.
//   - Deadline is turn-granular: checked at the top of every turn and
//     re-checked after Complete returns before any tool executes (I2),
//     because one Complete call may outlive the deadline under the turn
//     timeout. The re-check fires before exec.Execute, so a late model
//     turn escalates DEADLINE with no tool executed. The turn timeout
//     cap (MaxTurnTimeoutMs) bounds how far past the deadline one turn
//     can drift.
//
// Started/finished audit rows (IDs/counts only) bracket the run. This is
// the only method in non-test code in this package.
func (l *Loop) Run(ctx context.Context) (out InvestigationOutput, err error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return InvestigationOutput{}, ctxErr
	}
	invID := l.exception.InvestigationID
	known, seedErr := SeedKnownEvidence(l.exception)
	if seedErr != nil {
		// Defensive-only raw abort before started: the envelope passed
		// NewLoop validation, so a malformed seed means a
		// construction-time invariant broke (no outcome exists, no
		// audit rows emitted).
		return InvestigationOutput{}, seedErr
	}
	emitLoopAudit(l.audit, ctx, LoopAuditEvent{
		Event:           AuditEventStarted,
		InvestigationID: invID,
		TenantID:        l.scope.TenantID,
		ClaimID:         l.scope.ClaimID,
		RequestID:       l.scope.RequestID,
	})
	defer func() {
		// C1: the finished row carries the outcome whenever the run
		// produced one, independent of err (escalations return both a
		// populated output and a classified error). When the run
		// produced no outcome, no finished row is emitted.
		//
		// Exempt aborts (started without finished, by design):
		//   - ctx cancellation returns the context error itself with
		//     an empty output (never wrapped, never retried).
		//   - F6 tenant splits abort with ErrTenantMismatch and an
		//     empty output (no valid outcome exists to audit).
		//   - Defensive programmer-error raws (seed failure returns
		//     before started; request-marshal, turn-record validation,
		//     and REPORT_READY validation return an empty output)
		//     indicate loop-construction bugs: there is no outcome to
		//     record, and the failure is pinned by unit tests, not by
		//     prod audit rows.
		if out.Outcome == "" {
			return
		}
		ev := LoopAuditEvent{
			Event:              AuditEventFinished,
			InvestigationID:    invID,
			TenantID:           l.scope.TenantID,
			ClaimID:            l.scope.ClaimID,
			RequestID:          l.scope.RequestID,
			Outcome:            string(out.Outcome),
			TurnsUsed:          out.TurnsUsed,
			ToolCallsUsed:      out.ToolCallsUsed,
			KnownEvidenceCount: KnownLen(known),
		}
		if out.Outcome == OutcomeEscalated {
			ev.Reason = string(out.EscalationReason)
		}
		emitLoopAudit(l.audit, ctx, ev)
	}()
	return executeLoop(l, ctx, &known)
}

// executeLoop is the Run body as a free function (method budget: only
// (*Loop).Run may be a method in non-test code).
func executeLoop(l *Loop, ctx context.Context, known *KnownEvidence) (InvestigationOutput, error) {
	invID := l.exception.InvestigationID
	deadline := time.Now().Add(time.Duration(l.scope.DeadlineMs) * time.Millisecond)
	var (
		history    []TurnRecord
		modelID    string
		modelCalls int
		toolCalls  int
		seen       = make(map[string]struct{})
		stagnant   int
		// lastToolErr carries the most recent tool failure for the
		// NO_PROGRESS cause chain (I3): nil until the first tool
		// failure, then the last failure observed.
		lastToolErr error
	)
	// fail builds the ESCALATED output and validates it (I4): a built
	// escalation that violates ValidateInvestigationOutput is a loop
	// programmer error, so validation failure aborts raw with an empty
	// output rather than emitting a malformed escalation.
	fail := func(reason EscalationReason, partial *Report, turn int, cause error) (InvestigationOutput, error) {
		built := buildEscalation(invID, reason, partial, history, modelID, turn, toolCalls)
		if verr := ValidateInvestigationOutput(built); verr != nil {
			return InvestigationOutput{}, verr
		}
		return built, escalationError(reason, cause)
	}
	complete := func(callCtx context.Context, req ModelRequest) (ModelResponse, error) {
		return l.model.Complete(callCtx, req)
	}
	for turn := 1; turn <= l.budgets.MaxTurns; turn++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return InvestigationOutput{}, ctxErr
		}
		if time.Now().After(deadline) {
			return fail(EscalationDeadline, nil, turn, nil)
		}
		if terr := checkLoopTenant(l.scope, l.exception); terr != nil {
			return InvestigationOutput{}, terr
		}
		var act ModelAction
		reprompted := false
		for {
			if modelCalls >= l.budgets.MaxModelCalls {
				return fail(EscalationCallsExhausted, nil, turn, nil)
			}
			req := ModelRequest{
				Exception:        l.exception,
				History:          append([]TurnRecord(nil), history...),
				KnownEvidenceIDs: KnownIDs(*known),
				Turn:             turn,
				RequestID:        l.scope.RequestID,
			}
			rawReq, rerr := CanonicalModelRequest(req)
			if rerr != nil {
				// Defensive-only raw abort: the request is
				// loop-constructed from a validated scope, envelope,
				// and history, so marshal/validation failure means a
				// loop-construction bug (no valid escalation exists).
				return InvestigationOutput{}, rerr
			}
			if len(rawReq) > l.budgets.MaxInputBytes {
				// I6: history growth pushing the canonical request
				// past the input cap escalates INVALID_OUTPUT (the
				// run cannot proceed without dropping context, which
				// the loop never does silently).
				return fail(EscalationInvalidOutput, nil, turn,
					newInvalidError(invalidOversize, ErrModelContract,
						fmt.Errorf("model request %d bytes exceeds %d", len(rawReq), l.budgets.MaxInputBytes)))
			}
			turnCtx, cancel := context.WithTimeout(ctx, time.Duration(l.budgets.TurnTimeoutMs)*time.Millisecond)
			resp, cerr := complete(turnCtx, req)
			cancel()
			modelCalls++
			if cerr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return InvestigationOutput{}, ctxErr
				}
				if !errors.Is(cerr, ErrModelUpstream) {
					return fail(EscalationModelUpstream, nil, turn, cerr)
				}
				if modelCalls >= l.budgets.MaxModelCalls {
					return fail(EscalationCallsExhausted, nil, turn, cerr)
				}
				turnCtx2, cancel2 := context.WithTimeout(ctx, time.Duration(l.budgets.TurnTimeoutMs)*time.Millisecond)
				resp2, cerr2 := complete(turnCtx2, req)
				cancel2()
				modelCalls++
				if cerr2 != nil {
					if ctxErr := ctx.Err(); ctxErr != nil {
						return InvestigationOutput{}, ctxErr
					}
					return fail(EscalationModelUpstream, nil, turn, cerr2)
				}
				resp = resp2
			}
			modelID = resp.ModelID
			decoded, derr := DecodeModelAction(resp.Payload, l.budgets.MaxOutputBytes)
			if derr != nil {
				if repromptable(invalidKindOf(derr)) && !reprompted {
					reprompted = true
					continue
				}
				return fail(EscalationInvalidOutput, nil, turn, derr)
			}
			if verr := ValidateModelAction(decoded, l.scope, invID); verr != nil {
				if repromptable(invalidKindOf(verr)) && !reprompted {
					reprompted = true
					continue
				}
				return fail(EscalationInvalidOutput, submitPartial(decoded), turn, verr)
			}
			act = decoded
			break
		}
		// I2 mid-turn deadline re-check: Complete may have outlived the
		// scope deadline under the turn timeout, so re-check after the
		// model returns and before anything else executes. Fires before
		// exec.Execute (no tool runs past the deadline) and before a
		// late SUBMIT_REPORT is accepted.
		if time.Now().After(deadline) {
			return fail(EscalationDeadline, nil, turn, nil)
		}
		if act.Action == ActionSubmitReport {
			rep := *act.Report
			if gerr := CheckReportGrounding(rep, *known, l.exception); gerr != nil {
				part := rep
				return fail(EscalationInvalidOutput, &part, turn, gerr)
			}
			ready := normalizeReportCopy(rep)
			out := InvestigationOutput{
				InvestigationID: invID,
				Outcome:         OutcomeReportReady,
				Report:          &ready,
				AttemptLog:      append([]TurnRecord(nil), history...),
				ModelID:         modelID,
				TurnsUsed:       turn,
				ToolCallsUsed:   toolCalls,
			}
			if verr := ValidateInvestigationOutput(out); verr != nil {
				// Defensive-only raw abort: the output is
				// loop-constructed from validated parts, so failure
				// means a loop-construction bug.
				return InvestigationOutput{}, verr
			}
			return out, nil
		}
		if act.Tool == invest.ToolCreateInvestigationReport {
			return fail(EscalationInvalidOutput, nil, turn,
				newInvalidError(invalidDenied, ErrToolDenied,
					fmt.Errorf("tool %q is never callable by the loop", string(act.Tool))))
		}
		if toolCalls >= l.budgets.MaxToolCalls {
			return fail(EscalationCallsExhausted, nil, turn, nil)
		}
		rawToolReq, merr := canonicalToolRequest(*act.Request)
		if merr != nil {
			// Defensive-only raw abort: the request passed
			// Request.Validate, and struct marshaling cannot fail
			// on a validated struct (no maps cross this boundary).
			return InvestigationOutput{}, merr
		}
		key := repetitionKey(act.Tool, rawToolReq)
		if _, dup := seen[key]; dup {
			return fail(EscalationRepetition, nil, turn,
				fmt.Errorf("exact repeat of %s call", string(act.Tool)))
		}
		// I3: seen records only successful plus non-upstream-failure
		// calls. Upstream transients stay retryable: the executor
		// already retried them internally, and the same exact request
		// may succeed on a later turn, so a failed transient must not
		// poison the repetition set. Deterministic outcomes (success
		// or a terminal tool failure) do poison it: repeating them
		// cannot observe anything new.
		toolResp, xerr := l.exec.Execute(ctx, l.scope, act.Tool, *act.Request)
		if xerr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return InvestigationOutput{}, ctxErr
			}
			if !errors.Is(xerr, investigate.ErrUpstream) {
				seen[key] = struct{}{}
			}
			lastToolErr = xerr
			history = append(history, TurnRecord{
				Turn:        turn,
				Tool:        act.Tool,
				RequestHash: key,
				ResponseIDs: []string{},
				RowCount:    0,
				ErrorCode:   errorCodeFor(xerr),
			})
			toolCalls++
			stagnant++
			if stagnant >= noProgressThreshold {
				return fail(EscalationNoProgress, nil, turn, lastToolErr)
			}
			continue
		}
		seen[key] = struct{}{}
		added, gerr := GrowKnownEvidence(known, toolResp)
		if gerr != nil {
			// I6: a tool response that fails validation grows nothing
			// and escalates INVALID_OUTPUT (the backend returned data
			// the loop cannot cite).
			return fail(EscalationInvalidOutput, nil, turn, gerr)
		}
		rec := TurnRecord{
			Turn:        turn,
			Tool:        act.Tool,
			RequestHash: key,
			ResponseIDs: append([]string(nil), toolResp.IDs...),
			RowCount:    toolResp.RowCount,
			ErrorCode:   errorCodeOK,
		}
		if verr := ValidateTurnRecord(rec); verr != nil {
			// Defensive-only raw abort: the record is
			// loop-constructed from a validated response, so failure
			// means a loop-construction bug.
			return InvestigationOutput{}, verr
		}
		history = append(history, rec)
		toolCalls++
		if added == 0 {
			stagnant++
		} else {
			stagnant = 0
		}
		if stagnant >= noProgressThreshold {
			return fail(EscalationNoProgress, nil, turn, lastToolErr)
		}
	}
	return fail(EscalationTurnsExhausted, nil, l.budgets.MaxTurns, nil)
}
