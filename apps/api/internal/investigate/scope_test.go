package investigate

import (
	"errors"
	"testing"

	"claimops-api/internal/invest"
)

func validScope() Scope {
	return Scope{
		TenantID:   "tnt-54-scope",
		ClaimID:    "clm-54-scope",
		AllowTools: []invest.ToolName{invest.ToolGetClaim, invest.ToolGetEvidence},
		MaxCalls:   3,
		DeadlineMs: 5000,
		RequestID:  "req-54-scope",
	}
}

func TestScopeValidationTable(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Scope)
		wantErr bool
	}{
		{"valid", func(*Scope) {}, false},
		{"blank tenant", func(s *Scope) { s.TenantID = "  " }, true},
		{"untrimmed tenant", func(s *Scope) { s.TenantID = " tnt-54-scope" }, true},
		{"blank claim", func(s *Scope) { s.ClaimID = "" }, true},
		{"empty allowlist", func(s *Scope) { s.AllowTools = nil }, true},
		{"unknown tool", func(s *Scope) { s.AllowTools = []invest.ToolName{"exec_sql"} }, true},
		{"duplicate tool", func(s *Scope) {
			s.AllowTools = []invest.ToolName{invest.ToolGetClaim, invest.ToolGetClaim}
		}, true},
		{"zero maxcalls", func(s *Scope) { s.MaxCalls = 0 }, true},
		{"deadline 99", func(s *Scope) { s.DeadlineMs = 99 }, true},
		{"deadline 100 ok", func(s *Scope) { s.DeadlineMs = 100 }, false},
		{"blank request id", func(s *Scope) { s.RequestID = "" }, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := validScope()
			c.mutate(&s)
			err := s.Validate()
			if c.wantErr && err == nil {
				t.Error("Validate() = nil, want error")
			}
			if !c.wantErr && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
			if err != nil && !errors.Is(err, ErrContract) {
				t.Errorf("Validate() = %v, want ErrContract in chain", err)
			}
		})
	}
}

func TestScopeSubsetHelper(t *testing.T) {
	s := validScope()
	if !s.IsAllowed(invest.ToolGetClaim) {
		t.Error("IsAllowed(get_claim) = false, want true (declared)")
	}
	if s.IsAllowed(invest.ToolGetDocuments) {
		t.Error("IsAllowed(get_documents) = true, want false (allowlisted but undeclared)")
	}
	if s.IsAllowed("exec_sql") {
		t.Error("IsAllowed(exec_sql) = true, want false (not allowlisted)")
	}
}
