// Executor cross-cutting rejection tests (issue #54): capability,
// tenancy, and envelope-shape gates that sit ABOVE any single tool.
//
// Every case proves the first failure wins and no backend is touched after
// a gate rejects: the stub records invocation, Calls() stays 0, and the
// sentinel class is exact (ErrToolNotAllowed for capability, ErrContract
// for malformed envelopes, ErrTenantMismatch for cross-tenant echo).
//
// Helper names are x*-prefixed: execScope/execReq/okFn (executor_test.go),
// validScope (scope_test.go), and toolTenant (tool_test.go) are taken.
package investigate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"claimops-api/internal/invest"
)

const (
	xTenant = "tnt-54-cross"
	xClaim  = "clm-54-cross"
	xInv    = "inv-0123456789abcdef0123456789abcdef"
	xReqID  = "req-54-cross"
)

// xScope bounds one investigation to exactly the listed tools.
func xScope(tools ...invest.ToolName) Scope {
	return Scope{
		TenantID: xTenant, ClaimID: xClaim,
		AllowTools: tools, MaxCalls: 5,
		DeadlineMs: 5000, RequestID: xReqID,
	}
}

// xReq builds a valid generic envelope for tool (panics on construction
// failure: test setup, never the assertion).
func xReq(tool invest.ToolName) Request {
	r, err := NewRequest(tool, xTenant, xClaim, xInv, xReqID, 0)
	if err != nil {
		panic(err)
	}
	return r
}

// xStub returns a ToolFunc that records invocation and echoes the request
// tool on the response.
func xStub(called *bool, resp Response) ToolFunc {
	return func(_ context.Context, req Request) (Response, error) {
		*called = true
		resp.Tool = req.Tool
		return resp, nil
	}
}

func TestCrossUndeclaredCapabilityRejected(t *testing.T) {
	ctx := context.Background()
	var called bool
	ex := NewExecutor(map[invest.ToolName]ToolFunc{
		invest.ToolGetEvidence: xStub(&called, Response{RowCount: 1, IDs: []string{"ev-01"}}),
	}, time.Time{})
	// get_evidence is allowlisted globally but NOT declared in this scope:
	// the gate must fire before the backend is touched.
	scope := xScope(invest.ToolGetClaim)
	if _, err := ex.Execute(ctx, scope, invest.ToolGetEvidence, xReq(invest.ToolGetEvidence)); err == nil {
		t.Fatal("undeclared capability accepted, want rejection")
	} else if !errors.Is(err, ErrToolNotAllowed) {
		t.Fatalf("error %v, want ErrToolNotAllowed", err)
	}
	if called {
		t.Error("backend invoked for undeclared capability")
	}
	if ex.Calls() != 0 {
		t.Errorf("Calls() = %d, want 0 (gated calls consume no budget)", ex.Calls())
	}
}

func TestCrossUnallowlistedNameRejected(t *testing.T) {
	ctx := context.Background()
	var called bool
	ex := NewExecutor(map[invest.ToolName]ToolFunc{
		invest.ToolGetClaim: xStub(&called, Response{RowCount: 1, IDs: []string{"a"}}),
	}, time.Time{})
	// "exec_sql" is not allowlisted at all: the allowlist gate fires even
	// though the envelope itself is valid for get_claim.
	req := xReq(invest.ToolGetClaim)
	if _, err := ex.Execute(ctx, xScope(invest.ToolGetClaim), invest.ToolName("exec_sql"), req); err == nil {
		t.Fatal("unallowlisted tool accepted, want rejection")
	} else if !errors.Is(err, ErrToolNotAllowed) {
		t.Fatalf("error %v, want ErrToolNotAllowed", err)
	}
	if called {
		t.Error("backend invoked for unallowlisted tool")
	}
}

func TestCrossScopeViolationRejected(t *testing.T) {
	ctx := context.Background()
	var called bool
	ex := NewExecutor(map[invest.ToolName]ToolFunc{
		invest.ToolGetClaim: xStub(&called, Response{RowCount: 1, IDs: []string{"a"}}),
	}, time.Time{})
	// An empty allowlist violates least privilege: scope validation fails
	// before any backend, budget, or audit work.
	bad := xScope()
	bad.AllowTools = nil
	if _, err := ex.Execute(ctx, bad, invest.ToolGetClaim, xReq(invest.ToolGetClaim)); err == nil {
		t.Fatal("empty-scope call accepted, want rejection")
	} else if !errors.Is(err, ErrContract) {
		t.Fatalf("error %v, want ErrContract", err)
	}
	if called {
		t.Error("backend invoked for invalid scope")
	}
	if ex.Calls() != 0 {
		t.Errorf("Calls() = %d, want 0", ex.Calls())
	}
}

func TestCrossTenantAndClaimEcho(t *testing.T) {
	ctx := context.Background()
	newEx := func() (*Executor, *bool) {
		var called bool
		return NewExecutor(map[invest.ToolName]ToolFunc{
			invest.ToolGetClaim: xStub(&called, Response{RowCount: 1, IDs: []string{"a"}}),
		}, time.Time{}), &called
	}
	t.Run("cross-tenant request rejected", func(t *testing.T) {
		ex, called := newEx()
		req := xReq(invest.ToolGetClaim)
		req.TenantID = "tnt-other"
		if _, err := ex.Execute(ctx, xScope(invest.ToolGetClaim), invest.ToolGetClaim, req); !errors.Is(err, ErrTenantMismatch) {
			t.Fatalf("err = %v, want ErrTenantMismatch", err)
		}
		if *called {
			t.Error("backend invoked for cross-tenant request")
		}
	})
	t.Run("cross-claim request rejected", func(t *testing.T) {
		ex, called := newEx()
		req := xReq(invest.ToolGetClaim)
		req.ClaimID = "clm-other"
		if _, err := ex.Execute(ctx, xScope(invest.ToolGetClaim), invest.ToolGetClaim, req); !errors.Is(err, ErrContract) {
			t.Fatalf("err = %v, want ErrContract", err)
		}
		if *called {
			t.Error("backend invoked for cross-claim request")
		}
	})
	t.Run("request-id binding rejected", func(t *testing.T) {
		ex, called := newEx()
		req := xReq(invest.ToolGetClaim)
		req.RequestID = "req-other"
		if _, err := ex.Execute(ctx, xScope(invest.ToolGetClaim), invest.ToolGetClaim, req); !errors.Is(err, ErrContract) {
			t.Fatalf("err = %v, want ErrContract", err)
		}
		if *called {
			t.Error("backend invoked for unbound request id")
		}
	})
}

func TestCrossOversizedT11PayloadRejected(t *testing.T) {
	ctx := context.Background()
	var called bool
	ex := NewExecutor(map[invest.ToolName]ToolFunc{
		invest.ToolCreateInvestigationReport: xStub(&called, Response{RowCount: 1, IDs: []string{xInv}}),
	}, time.Time{})
	scope := xScope(invest.ToolCreateInvestigationReport)
	// 1 MiB + 1 byte: envelope validation fails before dispatch.
	big := Request{
		Tool:     invest.ToolCreateInvestigationReport,
		TenantID: xTenant, ClaimID: xClaim,
		InvestigationID: xInv, RequestID: xReqID, Limit: 1,
		SubjectID: "ex-0123456789abcdef0123456789abcdef",
		Hash:      strings.Repeat("a", 64),
		Payload:   make([]byte, MaxReportBytes+1),
	}
	if err := big.Validate(); !errors.Is(err, ErrContract) {
		t.Fatalf("Validate() = %v, want ErrContract for oversized payload", err)
	}
	if _, err := ex.Execute(ctx, scope, invest.ToolCreateInvestigationReport, big); !errors.Is(err, ErrContract) {
		t.Fatalf("Execute() = %v, want ErrContract for oversized payload", err)
	}
	if called {
		t.Error("backend invoked for oversized T11 payload")
	}
	if ex.Calls() != 0 {
		t.Errorf("Calls() = %d, want 0", ex.Calls())
	}
}

func TestCrossMalformedArgsRejected(t *testing.T) {
	ctx := context.Background()
	newEx := func() (*Executor, *bool) {
		var called bool
		return NewExecutor(map[invest.ToolName]ToolFunc{
			invest.ToolGetClaim: xStub(&called, Response{RowCount: 1, IDs: []string{"a"}}),
		}, time.Time{}), &called
	}
	t.Run("cursor on T1 rejected", func(t *testing.T) {
		ex, called := newEx()
		req := xReq(invest.ToolGetClaim)
		req.Cursor = "doc-01" // T1 owns no cursor knob
		if _, err := ex.Execute(ctx, xScope(invest.ToolGetClaim), invest.ToolGetClaim, req); !errors.Is(err, ErrContract) {
			t.Fatalf("err = %v, want ErrContract", err)
		}
		if *called {
			t.Error("backend invoked for knob-violating request")
		}
	})
	t.Run("payload on non-T11 rejected", func(t *testing.T) {
		ex, called := newEx()
		req := xReq(invest.ToolGetClaim)
		req.Payload = []byte(`{"a":1}`) // payload is T11-only
		if _, err := ex.Execute(ctx, xScope(invest.ToolGetClaim), invest.ToolGetClaim, req); !errors.Is(err, ErrContract) {
			t.Fatalf("err = %v, want ErrContract", err)
		}
		if *called {
			t.Error("backend invoked for payload-carrying T1 request")
		}
	})
	t.Run("missing registry implementation rejected", func(t *testing.T) {
		ex := NewExecutor(nil, time.Time{})
		scope := xScope(invest.ToolGetDocuments)
		req, err := NewRequest(invest.ToolGetDocuments, xTenant, xClaim, xInv, xReqID, 0)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		if _, err := ex.Execute(ctx, scope, invest.ToolGetDocuments, req); !errors.Is(err, ErrToolNotAllowed) {
			t.Fatalf("err = %v, want ErrToolNotAllowed", err)
		}
	})
}
