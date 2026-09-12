// Scope is the executor-side capability contract for one investigation:
// the tenant/claim echo, the per-case least-privilege tool subset, the
// call/time budgets the executor enforces (never requested by the model),
// and the propagated request ID used as the audit trace_id.
//
// It mirrors invest.ScopeConstraints without importing envelope semantics:
// callers building from an UnresolvedException copy ScopeConstraints field
// by field (same names, same order) so the two can never silently fork.
package investigate

import (
	"fmt"
	"strings"

	"claimops-api/internal/invest"
)

// Scope bounds one investigation's tool use.
type Scope struct {
	TenantID   string
	ClaimID    string
	AllowTools []invest.ToolName
	MaxCalls   int
	DeadlineMs int64
	RequestID  string
}

// Validate checks the scope standalone: trimmed non-blank tenant/claim,
// a non-empty duplicate-free allowlist drawn from invest.Allowlist(),
// MaxCalls >= 1, DeadlineMs >= 100ms, and a non-blank request ID (the
// audit trace binding — without it tool calls are unobservable).
func (s Scope) Validate() error {
	if strings.TrimSpace(s.TenantID) == "" {
		return fmt.Errorf("investigate: scope has blank tenant id: %w", ErrContract)
	}
	if s.TenantID != strings.TrimSpace(s.TenantID) {
		return fmt.Errorf("investigate: scope tenant id must be trimmed: %w", ErrContract)
	}
	if strings.TrimSpace(s.ClaimID) == "" {
		return fmt.Errorf("investigate: scope has blank claim id: %w", ErrContract)
	}
	if s.ClaimID != strings.TrimSpace(s.ClaimID) {
		return fmt.Errorf("investigate: scope claim id must be trimmed: %w", ErrContract)
	}
	if len(s.AllowTools) == 0 {
		return fmt.Errorf("investigate: scope allow_tools is empty (least privilege needs an explicit subset): %w", ErrContract)
	}
	seen := make(map[invest.ToolName]struct{}, len(s.AllowTools))
	for _, t := range s.AllowTools {
		if !invest.IsAllowlisted(t) {
			return fmt.Errorf("investigate: scope tool %q not in allowlist: %w", string(t), ErrContract)
		}
		if _, dup := seen[t]; dup {
			return fmt.Errorf("investigate: scope duplicates tool %q: %w", string(t), ErrContract)
		}
		seen[t] = struct{}{}
	}
	if s.MaxCalls < 1 {
		return fmt.Errorf("investigate: scope max_calls must be >= 1: %w", ErrContract)
	}
	if s.DeadlineMs < 100 {
		return fmt.Errorf("investigate: scope deadline_ms must be >= 100: %w", ErrContract)
	}
	if strings.TrimSpace(s.RequestID) == "" {
		return fmt.Errorf("investigate: scope needs a request id (audit trace binding): %w", ErrContract)
	}
	return nil
}

// IsAllowed is the subset check: it reports whether tool is both
// allowlisted globally and declared in this scope. Undeclared names
// return false — the executor rejects them before any backend is touched.
func (s Scope) IsAllowed(tool invest.ToolName) bool {
	if !invest.IsAllowlisted(tool) {
		return false
	}
	for _, t := range s.AllowTools {
		if t == tool {
			return true
		}
	}
	return false
}
