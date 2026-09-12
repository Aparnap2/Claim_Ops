package investigate

import (
	"errors"
	"strings"
	"testing"

	"claimops-api/internal/invest"
	"claimops-api/internal/ports"
)

const (
	toolTenant = "tnt-54-tools"
	toolClaim  = "clm-54-tools"
	toolInv    = "inv-0123456789abcdef0123456789abcdef"
	toolReq    = "req-54-0001"
)

func TestSentinelsWrapPorts(t *testing.T) {
	cases := []struct {
		name  string
		local error
		ports error
	}{
		{"notfound", ErrNotFound, ports.ErrNotFound},
		{"contract", ErrContract, ports.ErrContract},
		{"tenant", ErrTenantMismatch, ports.ErrTenantMismatch},
		{"upstream", ErrUpstream, ports.ErrUpstream},
	}
	for _, c := range cases {
		if !errors.Is(c.local, c.ports) {
			t.Errorf("%s: local sentinel does not unwrap to ports sentinel", c.name)
		}
	}
}

func TestMaxRowsTable(t *testing.T) {
	want := map[invest.ToolName]int{
		invest.ToolGetClaim:                  1,
		invest.ToolGetPolicyContext:          1,
		invest.ToolGetDocuments:              50,
		invest.ToolGetEvidence:               50,
		invest.ToolSearchEvidence:            20,
		invest.ToolGetVerificationFindings:   1,
		invest.ToolGetExternalPolicyStatus:   1,
		invest.ToolGetTPACase:                20,
		invest.ToolGetProviderEncounter:      1,
		invest.ToolGetRiskSignals:            20,
		invest.ToolCreateInvestigationReport: 1,
	}
	if len(want) != len(invest.Allowlist()) {
		t.Fatalf("table covers %d tools, allowlist has %d", len(want), len(invest.Allowlist()))
	}
	for tool, max := range want {
		got, err := MaxRows(tool)
		if err != nil {
			t.Errorf("MaxRows(%s): %v", string(tool), err)
			continue
		}
		if got != max {
			t.Errorf("MaxRows(%s) = %d, want %d", string(tool), got, max)
		}
	}
	if _, err := MaxRows("drop_table"); err == nil {
		t.Error("MaxRows(unknown) = nil error, want fail closed")
	} else if !errors.Is(err, ErrContract) {
		t.Errorf("MaxRows(unknown) error %v, want ErrContract", err)
	}
}

func TestNewRequestValidation(t *testing.T) {
	if _, err := NewRequest("exec_sql", toolTenant, toolClaim, toolInv, toolReq, 1); err == nil {
		t.Error("NewRequest(unallowlisted) = nil, want error")
	}
	if _, err := NewRequest(invest.ToolGetClaim, "  ", toolClaim, toolInv, toolReq, 1); err == nil {
		t.Error("NewRequest(blank tenant) = nil, want error")
	}
	r, err := NewRequest(invest.ToolGetClaim, toolTenant, toolClaim, toolInv, toolReq, 0)
	if err != nil {
		t.Fatalf("NewRequest default limit: %v", err)
	}
	if r.Limit != MaxRowsGetClaim {
		t.Errorf("default limit = %d, want %d", r.Limit, MaxRowsGetClaim)
	}
	if _, err := NewRequest(invest.ToolGetClaim, toolTenant, toolClaim, toolInv, toolReq, 2); err == nil {
		t.Error("NewRequest(limit above MaxRows) = nil, want error")
	}
	// Knob ownership: query rides T5 only.
	bad := Request{Tool: invest.ToolGetClaim, TenantID: toolTenant, ClaimID: toolClaim,
		InvestigationID: toolInv, RequestID: toolReq, Limit: 1, Query: "x"}
	if err := bad.Validate(); err == nil {
		t.Error("query on get_claim validates, want contract error")
	}
	// Query cap: 201 runes rejected on T5.
	if _, err := NewSearchEvidenceRequest(toolTenant, toolClaim, toolInv, toolReq, strings.Repeat("a", 201), 0); err == nil {
		t.Error("201-rune query accepted, want rejection")
	}
	// Payload is T11-only.
	badP := Request{Tool: invest.ToolGetClaim, TenantID: toolTenant, ClaimID: toolClaim,
		InvestigationID: toolInv, RequestID: toolReq, Limit: 1, Payload: []byte("{}")}
	if err := badP.Validate(); err == nil {
		t.Error("payload on get_claim validates, want contract error")
	}
}

func TestPerToolRoundTrip(t *testing.T) {
	// T3 page → envelope → validate.
	d3, err := NewGetDocumentsRequest(toolTenant, toolClaim, toolInv, toolReq, 0, "")
	if err != nil {
		t.Fatalf("NewGetDocumentsRequest: %v", err)
	}
	if err := d3.ToRequest().Validate(); err != nil {
		t.Fatalf("documents ToRequest invalid: %v", err)
	}
	meta, err := NewGetDocumentsResponse([]DocumentMeta{
		{DocumentID: "doc-02", DocType: "bill", SHA256: "ab", Status: "ok"},
		{DocumentID: "doc-01", DocType: "form", SHA256: "cd", Status: "ok"},
	}, false, "")
	if err != nil {
		t.Fatalf("NewGetDocumentsResponse: %v", err)
	}
	resp := meta.ToResponse()
	if err := resp.Validate(); err != nil {
		t.Fatalf("documents ToResponse invalid: %v", err)
	}
	if len(resp.IDs) != 2 || resp.IDs[0] != "doc-01" {
		t.Errorf("documents IDs not sorted: %v", resp.IDs)
	}
	// T9 doc-ref cap: 51 rejected, 50 accepted.
	many := make([]string, 51)
	for i := range many {
		many[i] = "doc-ref"
	}
	if _, err := NewGetProviderEncounterResponse("enc-01", "ok", "h", many); err == nil {
		t.Error("51 doc refs accepted, want cap-50 rejection")
	}
	// T11: bad hash, bad bytes, good.
	if _, err := NewCreateReportRequest(toolTenant, toolClaim, toolInv, toolReq,
		"ex-0123456789abcdef0123456789abcdef", []byte(`{"a":1}`), "zzz"); err == nil {
		t.Error("bad report hash accepted, want rejection")
	}
	if _, err := NewCreateReportRequest(toolTenant, toolClaim, toolInv, toolReq,
		"ex-0123456789abcdef0123456789abcdef", []byte(`{oops`), strings.Repeat("a", 64)); err == nil {
		t.Error("non-JSON payload accepted, want rejection")
	}
	hash := strings.Repeat("b", 64)
	c11, err := NewCreateReportRequest(toolTenant, toolClaim, toolInv, toolReq,
		"ex-0123456789abcdef0123456789abcdef", []byte(`{"a":1}`), hash)
	if err != nil {
		t.Fatalf("NewCreateReportRequest: %v", err)
	}
	if err := c11.ToRequest().Validate(); err != nil {
		t.Fatalf("report ToRequest invalid: %v", err)
	}
}
