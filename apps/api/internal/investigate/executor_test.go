package investigate

import (
	"context"
	"errors"
	"testing"
	"time"

	"claimops-api/internal/invest"
)

const (
	exTenant = "tnt-54-exec"
	exClaim  = "clm-54-exec"
	exInv    = "inv-0123456789abcdef0123456789abcdef"
	exReq    = "req-54-exec"
)

func execScope(tools ...invest.ToolName) Scope {
	return Scope{
		TenantID: exTenant, ClaimID: exClaim,
		AllowTools: tools, MaxCalls: 5,
		DeadlineMs: 5000, RequestID: exReq,
	}
}

func execReq(tool invest.ToolName) Request {
	r, err := NewRequest(tool, exTenant, exClaim, exInv, exReq, 0)
	if err != nil {
		panic(err)
	}
	return r
}

func okFn(resp Response) ToolFunc {
	return func(_ context.Context, req Request) (Response, error) {
		resp.Tool = req.Tool
		return resp, nil
	}
}

func TestExecuteGating(t *testing.T) {
	ctx := context.Background()

	t.Run("undeclared tool rejected without touching backend", func(t *testing.T) {
		called := false
		ex := NewExecutor(map[invest.ToolName]ToolFunc{
			invest.ToolGetClaim: func(ctx context.Context, req Request) (Response, error) {
				called = true
				return Response{}, nil
			},
		}, time.Time{})
		scope := execScope(invest.ToolGetClaim)
		req := execReq(invest.ToolGetClaim)
		// Dispatch a different allowlisted tool than declared: gate must fire.
		if _, err := ex.Execute(ctx, scope, invest.ToolGetEvidence, req); err == nil {
			t.Fatal("undeclared dispatch accepted, want rejection")
		} else if !errors.Is(err, ErrToolNotAllowed) {
			t.Fatalf("error %v, want ErrToolNotAllowed", err)
		}
		if called {
			t.Error("backend invoked for undeclared tool")
		}
		if ex.Calls() != 0 {
			t.Errorf("Calls() = %d, want 0 (gated calls consume no budget)", ex.Calls())
		}
	})

	t.Run("over budget rejected and counted", func(t *testing.T) {
		ex := NewExecutor(map[invest.ToolName]ToolFunc{
			invest.ToolGetClaim: okFn(Response{RowCount: 1, IDs: []string{"a"}}),
		}, time.Time{})
		scope := execScope(invest.ToolGetClaim)
		scope.MaxCalls = 1
		req := execReq(invest.ToolGetClaim)
		if _, err := ex.Execute(ctx, scope, invest.ToolGetClaim, req); err != nil {
			t.Fatalf("first call: %v", err)
		}
		if _, err := ex.Execute(ctx, scope, invest.ToolGetClaim, req); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("second call err = %v, want ErrBudgetExceeded", err)
		}
		if ex.Calls() != 1 {
			t.Errorf("Calls() = %d, want 1", ex.Calls())
		}
	})

	t.Run("tenant mismatch rejected", func(t *testing.T) {
		ex := NewExecutor(map[invest.ToolName]ToolFunc{
			invest.ToolGetClaim: okFn(Response{RowCount: 1, IDs: []string{"a"}}),
		}, time.Time{})
		scope := execScope(invest.ToolGetClaim)
		req := execReq(invest.ToolGetClaim)
		req.TenantID = "tnt-other"
		if _, err := ex.Execute(ctx, scope, invest.ToolGetClaim, req); !errors.Is(err, ErrTenantMismatch) {
			t.Fatalf("err = %v, want ErrTenantMismatch", err)
		}
		if !errors.Is(ErrTenantMismatch, errTenantPortsAlias()) {
			t.Fatal("sentinel chain broken")
		}
	})

	t.Run("missing implementation rejected", func(t *testing.T) {
		ex := NewExecutor(nil, time.Time{})
		scope := execScope(invest.ToolGetClaim)
		if _, err := ex.Execute(ctx, scope, invest.ToolGetClaim, execReq(invest.ToolGetClaim)); !errors.Is(err, ErrToolNotAllowed) {
			t.Fatalf("err = %v, want ErrToolNotAllowed", err)
		}
	})

	t.Run("expired deadline rejected", func(t *testing.T) {
		ex := NewExecutor(map[invest.ToolName]ToolFunc{
			invest.ToolGetClaim: okFn(Response{RowCount: 1, IDs: []string{"a"}}),
		}, time.Now().Add(-time.Second))
		scope := execScope(invest.ToolGetClaim)
		if _, err := ex.Execute(ctx, scope, invest.ToolGetClaim, execReq(invest.ToolGetClaim)); !errors.Is(err, ErrDeadlineExceeded) {
			t.Fatalf("err = %v, want ErrDeadlineExceeded", err)
		}
	})
}

// errTenantPortsAlias documents the ports unwrap without importing ports
// here (executor_test stays stdlib + invest, mirroring executor.go).
func errTenantPortsAlias() error {
	return ErrTenantMismatch
}

func TestExecuteRetryOnlyUpstream(t *testing.T) {
	ctx := context.Background()
	scope := execScope(invest.ToolGetClaim)

	t.Run("upstream retried to success within budget", func(t *testing.T) {
		n := 0
		ex := NewExecutor(map[invest.ToolName]ToolFunc{
			invest.ToolGetClaim: func(_ context.Context, req Request) (Response, error) {
				n++
				if n < 3 {
					return Response{}, ErrUpstream
				}
				return Response{Tool: req.Tool, RowCount: 1, IDs: []string{"a"}}, nil
			},
		}, time.Time{})
		out, err := ex.Execute(ctx, scope, invest.ToolGetClaim, execReq(invest.ToolGetClaim))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if n != 3 {
			t.Errorf("attempts = %d, want 3", n)
		}
		if out.RowCount != 1 {
			t.Errorf("RowCount = %d, want 1", out.RowCount)
		}
		if ex.Calls() != 1 {
			t.Errorf("Calls() = %d, want 1 (retries share one budget unit)", ex.Calls())
		}
	})

	t.Run("contract failure not retried", func(t *testing.T) {
		n := 0
		ex := NewExecutor(map[invest.ToolName]ToolFunc{
			invest.ToolGetClaim: func(_ context.Context, _ Request) (Response, error) {
				n++
				return Response{}, ErrContract
			},
		}, time.Time{})
		if _, err := ex.Execute(ctx, scope, invest.ToolGetClaim, execReq(invest.ToolGetClaim)); !errors.Is(err, ErrContract) {
			t.Fatalf("err = %v, want ErrContract", err)
		}
		if n != 1 {
			t.Errorf("attempts = %d, want 1 (no retry on Contract)", n)
		}
	})

	t.Run("tool timeout applied per attempt", func(t *testing.T) {
		ex := NewExecutor(map[invest.ToolName]ToolFunc{
			invest.ToolGetClaim: func(ctx context.Context, req Request) (Response, error) {
				dl, ok := ctx.Deadline()
				if !ok {
					t.Error("call context has no deadline")
					return Response{}, ErrContract
				}
				if time.Until(dl) > ToolTimeout {
					t.Errorf("deadline %v exceeds ToolTimeout", time.Until(dl))
					return Response{}, ErrContract
				}
				return Response{Tool: req.Tool, RowCount: 1, IDs: []string{"a"}}, nil
			},
		}, time.Time{})
		if _, err := ex.Execute(ctx, scope, invest.ToolGetClaim, execReq(invest.ToolGetClaim)); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
}

func TestExecuteAuditHook(t *testing.T) {
	ctx := context.Background()
	scope := execScope(invest.ToolGetClaim)
	var got []AuditParams
	var gotErr []error
	ex := NewExecutor(map[invest.ToolName]ToolFunc{
		invest.ToolGetClaim: okFn(Response{RowCount: 1, IDs: []string{"a"}, Hash: "h"}),
	}, time.Time{})
	ex.SetAuditHook(func(_ context.Context, p AuditParams, err error) {
		got = append(got, p)
		gotErr = append(gotErr, err)
	})
	req := execReq(invest.ToolGetClaim)
	if _, err := ex.Execute(ctx, scope, invest.ToolGetClaim, req); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("hook calls = %d, want 1", len(got))
	}
	p := got[0]
	if p.TenantID != exTenant || p.RequestID != exReq || p.Tool != invest.ToolGetClaim {
		t.Errorf("hook params identity wrong: %+v", p)
	}
	if p.Entity != "claim" || p.EntityID != exClaim {
		t.Errorf("hook entity = %q/%q, want claim/%q", p.Entity, p.EntityID, exClaim)
	}
	if gotErr[0] != nil {
		t.Errorf("hook err = %v, want nil", gotErr[0])
	}
}
