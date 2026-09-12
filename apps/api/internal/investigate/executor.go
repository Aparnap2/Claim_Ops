// Executor dispatches allowlisted tool calls under scope budgets. It is
// deliberately model-blind: no prompt, no model client, no planning, no
// confidence — it gates (allowlist, scope subset, tenant echo, budgets),
// bounds (ToolTimeout per attempt, Limit clamped to MaxRows), retries
// Upstream failures only, counts logical calls, and emits an audit hook.
//
// Imports are stdlib + invest only. Port sentinels are observed through
// the local Err* aliases in tool.go (errors.Is), never imported here, so
// this file compiles and tests with a fake registry and no adapters.
//
// Gate order per Execute: scope valid → request valid → allowlisted →
// scope-declared → registry has an implementation → tool echo → tenant
// echo → claim echo → request-ID binding → deadline → budget. The first
// failure wins and no backend is touched after a gate rejects.
package investigate

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"claimops-api/internal/invest"
)

// Executor gate sentinels (terminal: never retried).
var (
	// ErrToolNotAllowed rejects undeclared tools and tools with no
	// registry implementation.
	ErrToolNotAllowed = errors.New("investigate: tool not allowed for this scope")
	// ErrBudgetExceeded rejects calls past Scope.MaxCalls.
	ErrBudgetExceeded = errors.New("investigate: tool call budget exceeded")
	// ErrDeadlineExceeded rejects calls past the executor deadline.
	ErrDeadlineExceeded = errors.New("investigate: investigation deadline exceeded")
)

// AuditHook observes every executed call (success AND failure) for the
// append-only audit writer (audit.go provides AuditHookFor). It is
// best-effort: hook errors are swallowed so audit can never fail a tool
// result, and the hook MUST NOT call back into the Executor.
type AuditHook func(ctx context.Context, params AuditParams, callErr error)

// Executor dispatches ToolFunc implementations under per-scope budgets.
// One Executor serves one investigation: calls counts logical Execute
// calls (retries do NOT consume budget), deadline is the absolute
// investigation deadline (zero = none; Scope.DeadlineMs is enforced by
// the caller when constructing it, typically now+DeadlineMs).
type Executor struct {
	mu       sync.Mutex
	tools    map[invest.ToolName]ToolFunc
	calls    int
	deadline time.Time
	hook     AuditHook
}

// NewExecutor builds an Executor over a copy of registry (nil entries
// dropped; later caller mutations do not affect it). deadline is the
// absolute investigation deadline; pass time.Time{} for none.
func NewExecutor(registry map[invest.ToolName]ToolFunc, deadline time.Time) *Executor {
	cp := make(map[invest.ToolName]ToolFunc, len(registry))
	for name, fn := range registry {
		if fn != nil {
			cp[name] = fn
		}
	}
	return &Executor{tools: cp, deadline: deadline}
}

// SetAuditHook installs the post-call audit observer (nil clears it).
// Safe for concurrent use with Execute.
func (e *Executor) SetAuditHook(h AuditHook) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hook = h
}

// Calls reports the logical call count (budget consumption).
func (e *Executor) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// Execute runs one bounded tool call: gate → budget → up to MaxRetries
// attempts with a fresh ToolTimeout context each → audit hook. On success
// the response is validated and its Tool echo checked before return.
func (e *Executor) Execute(ctx context.Context, scope Scope, tool invest.ToolName, req Request) (Response, error) {
	if err := scope.Validate(); err != nil {
		return Response{}, err
	}
	if err := req.Validate(); err != nil {
		return Response{}, err
	}
	if !invest.IsAllowlisted(tool) {
		return Response{}, fmt.Errorf("investigate: tool %q not allowlisted: %w", string(tool), ErrToolNotAllowed)
	}
	if !scope.IsAllowed(tool) {
		return Response{}, fmt.Errorf("investigate: tool %q not declared in scope: %w", string(tool), ErrToolNotAllowed)
	}
	e.mu.Lock()
	fn, ok := e.tools[tool]
	e.mu.Unlock()
	if !ok {
		return Response{}, fmt.Errorf("investigate: no implementation for tool %q: %w", string(tool), ErrToolNotAllowed)
	}
	if req.Tool != tool {
		return Response{}, fmt.Errorf("investigate: request tool %q != dispatched %q: %w", string(req.Tool), string(tool), ErrContract)
	}
	if req.TenantID != scope.TenantID {
		return Response{}, fmt.Errorf("investigate: request tenant %q != scope tenant %q: %w", req.TenantID, scope.TenantID, ErrTenantMismatch)
	}
	if req.ClaimID != scope.ClaimID {
		return Response{}, fmt.Errorf("investigate: request claim %q != scope claim %q: %w", req.ClaimID, scope.ClaimID, ErrContract)
	}
	if req.RequestID != scope.RequestID {
		return Response{}, fmt.Errorf("investigate: request id %q != scope request id %q: %w", req.RequestID, scope.RequestID, ErrContract)
	}
	if !e.deadline.IsZero() && time.Now().After(e.deadline) {
		return Response{}, fmt.Errorf("investigate: deadline passed: %w", ErrDeadlineExceeded)
	}
	e.mu.Lock()
	if e.calls >= scope.MaxCalls {
		e.mu.Unlock()
		return Response{}, fmt.Errorf("investigate: call %d exceeds max %d: %w", e.calls+1, scope.MaxCalls, ErrBudgetExceeded)
	}
	e.calls++
	e.mu.Unlock()

	// Defense in depth: constructors already bound Limit, but a
	// hand-built Request at exactly MaxRows is the most it may ask.
	if max, err := MaxRows(tool); err == nil && req.Limit > max {
		req.Limit = max
	}

	var out Response
	var err error
	for attempt := 1; attempt <= MaxRetries; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, ToolTimeout)
		out, err = fn(callCtx, req)
		cancel()
		if err == nil {
			break
		}
		// Cancelled/timed-out parent: never retry a dead context.
		if ctx.Err() != nil {
			err = fmt.Errorf("investigate: %s attempt %d: context done: %w", string(tool), attempt, ctx.Err())
			break
		}
		// Upstream-only retry: every other class returns immediately.
		if !errors.Is(err, ErrUpstream) {
			break
		}
	}
	if err != nil {
		e.audit(ctx, scope, req, Response{}, err)
		return Response{}, err
	}
	if out.Tool != tool {
		bad := fmt.Errorf("investigate: response tool %q != dispatched %q: %w", string(out.Tool), string(tool), ErrContract)
		e.audit(ctx, scope, req, Response{}, bad)
		return Response{}, bad
	}
	if verr := out.Validate(); verr != nil {
		e.audit(ctx, scope, req, Response{}, verr)
		return Response{}, verr
	}
	e.audit(ctx, scope, req, out, nil)
	return out, nil
}

// audit builds the IDs/hashes/counts-only params and invokes the hook.
// Hook errors are swallowed by contract (AuditHookFor is best-effort).
func (e *Executor) audit(ctx context.Context, scope Scope, req Request, out Response, callErr error) {
	e.mu.Lock()
	h := e.hook
	e.mu.Unlock()
	if h == nil {
		return
	}
	h(ctx, AuditParams{
		TenantID:        scope.TenantID,
		ClaimID:         scope.ClaimID,
		InvestigationID: req.InvestigationID,
		Tool:            req.Tool,
		Entity:          auditEntity(req.Tool),
		EntityID:        auditEntityID(req),
		RequestID:       scope.RequestID,
		RowCount:        out.RowCount,
		ContentHash:     out.Hash,
		EvidenceIDs:     append([]string(nil), out.IDs...),
	}, callErr)
}

// auditEntityID prefers the tool subject (policy/case/encounter/exception
// row) and falls back to the claim for claim-wide reads.
func auditEntityID(req Request) string {
	if req.SubjectID != "" {
		return req.SubjectID
	}
	return req.ClaimID
}
