package tools

import (
	"context"
	"reflect"
	"testing"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/ports"
)

func claimHeaderFixture() investigate.ClaimHeader {
	return investigate.ClaimHeader{
		ClaimID:     toolsClaim,
		Status:      "OPEN",
		Reference:   "ref-54",
		AmountPaise: 1000,
		Version:     1,
	}
}

func claimRequest(t *testing.T) investigate.Request {
	t.Helper()
	req, err := investigate.NewGetClaimRequest(toolsTenant, toolsClaim, toolsInv, toolsReq)
	if err != nil {
		t.Fatalf("build claim request: %v", err)
	}
	return req.ToRequest()
}

func TestClaimValid(t *testing.T) {
	reader := &fakeClaimReader{header: claimHeaderFixture()}
	fn := NewClaimTool(reader)
	got, err := fn(context.Background(), claimRequest(t))
	if err != nil {
		t.Fatalf("claim tool: %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("response Validate: %v", err)
	}
	if got.Tool != invest.ToolGetClaim {
		t.Fatalf("tool echo = %q, want get_claim", string(got.Tool))
	}
	if got.RowCount != 1 || !reflect.DeepEqual(got.IDs, []string{toolsClaim}) {
		t.Fatalf("response = %+v, want RowCount 1 IDs [%s]", got, toolsClaim)
	}
	if reader.gotTenant != toolsTenant || reader.gotClaim != toolsClaim {
		t.Fatalf("reader echo = %q/%q, want %q/%q", reader.gotTenant, reader.gotClaim, toolsTenant, toolsClaim)
	}
}

func TestClaimInvalidIDs(t *testing.T) {
	reader := &fakeClaimReader{header: claimHeaderFixture()}
	fn := NewClaimTool(reader)
	base := claimRequest(t)

	cases := map[string]investigate.Request{
		"wrong tool": func() investigate.Request {
			out := base
			out.Tool = invest.ToolGetDocuments
			return out
		}(),
		"blank tenant": func() investigate.Request {
			out := base
			out.TenantID = ""
			return out
		}(),
		"blank claim": func() investigate.Request {
			out := base
			out.ClaimID = ""
			return out
		}(),
		"bad investigation": func() investigate.Request {
			out := base
			out.InvestigationID = "bad-id"
			return out
		}(),
		"blank request": func() investigate.Request {
			out := base
			out.RequestID = ""
			return out
		}(),
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			before := reader.calls
			_, err := fn(context.Background(), req)
			if err == nil {
				t.Fatalf("want contract error, got nil")
			}
			requireToolsSentinel(t, err, ports.ErrContract)
			if reader.calls != before {
				t.Fatalf("reader called on invalid request (calls %d -> %d)", before, reader.calls)
			}
		})
	}
}

func TestClaimTenantMismatch(t *testing.T) {
	reader := &fakeClaimReader{err: toolsErr(ports.ErrTenantMismatch, "claim")}
	fn := NewClaimTool(reader)
	_, err := fn(context.Background(), claimRequest(t))
	if err == nil {
		t.Fatalf("want tenant mismatch, got nil")
	}
	requireToolsSentinel(t, err, ports.ErrTenantMismatch)
}

func TestClaimAbsentNotFound(t *testing.T) {
	reader := &fakeClaimReader{err: toolsErr(ports.ErrNotFound, "claim")}
	fn := NewClaimTool(reader)
	_, err := fn(context.Background(), claimRequest(t))
	if err == nil {
		t.Fatalf("want not found, got nil")
	}
	requireToolsSentinel(t, err, ports.ErrNotFound)
}

func TestClaimDeterministic(t *testing.T) {
	reader := &fakeClaimReader{header: claimHeaderFixture()}
	fn := NewClaimTool(reader)
	req := claimRequest(t)
	first, err := fn(context.Background(), req)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := fn(context.Background(), req)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("double-call mismatch:\nfirst %+v\nsecond %+v", first, second)
	}
	if reader.calls != 2 {
		t.Fatalf("reader calls = %d, want 2", reader.calls)
	}
}
