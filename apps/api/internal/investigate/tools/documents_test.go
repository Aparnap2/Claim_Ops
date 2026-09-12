package tools

import (
	"context"
	"reflect"
	"testing"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/ports"
)

func documentsFixture() []investigate.DocumentMeta {
	return []investigate.DocumentMeta{
		{DocumentID: "doc-02", DocType: "bill", SHA256: "bb", Status: "READY"},
		{DocumentID: "doc-01", DocType: "discharge", SHA256: "aa", Status: "READY"},
	}
}

func documentsRequest(t *testing.T, limit int, cursor string) investigate.Request {
	t.Helper()
	built, err := investigate.NewGetDocumentsRequest(toolsTenant, toolsClaim, toolsInv, toolsReq, limit, cursor)
	if err != nil {
		t.Fatalf("build documents request (limit=%d cursor=%q): %v", limit, cursor, err)
	}
	return built.ToRequest()
}

func TestDocumentsValid(t *testing.T) {
	reader := &fakeDocReader{page: investigate.DocumentPage{
		Documents:  documentsFixture(),
		Truncated:  false,
		NextCursor: "",
	}}
	fn := NewDocumentsTool(reader)
	got, err := fn(context.Background(), documentsRequest(t, 10, ""))
	if err != nil {
		t.Fatalf("documents tool: %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("response Validate: %v", err)
	}
	if got.Tool != invest.ToolGetDocuments {
		t.Fatalf("tool echo = %q, want get_documents", string(got.Tool))
	}
	if got.RowCount != 2 {
		t.Fatalf("RowCount = %d, want 2", got.RowCount)
	}
	// ToResponse sorts IDs for determinism even when the page is unordered.
	if !reflect.DeepEqual(got.IDs, []string{"doc-01", "doc-02"}) {
		t.Fatalf("IDs = %v, want sorted [doc-01 doc-02]", got.IDs)
	}
	if reader.got.tenant != toolsTenant || reader.got.claim != toolsClaim {
		t.Fatalf("reader echo = %q/%q, want %q/%q", reader.got.tenant, reader.got.claim, toolsTenant, toolsClaim)
	}
	if reader.got.limit != 10 || reader.got.cursor != "" {
		t.Fatalf("reader paging = limit %d cursor %q, want 10/\"\"", reader.got.limit, reader.got.cursor)
	}
}

func TestDocumentsLimitClamp(t *testing.T) {
	zero := DocumentsRequest{
		TenantID: toolsTenant, ClaimID: toolsClaim,
		InvestigationID: toolsInv, RequestID: toolsReq, Limit: 0,
	}
	if err := zero.validate(); err != nil {
		t.Fatalf("validate limit 0: %v", err)
	}
	if zero.Limit != investigate.MaxRowsGetDocuments {
		t.Fatalf("limit 0 clamped to %d, want %d", zero.Limit, investigate.MaxRowsGetDocuments)
	}

	huge := DocumentsRequest{
		TenantID: toolsTenant, ClaimID: toolsClaim,
		InvestigationID: toolsInv, RequestID: toolsReq, Limit: 999,
	}
	if err := huge.validate(); err != nil {
		t.Fatalf("validate limit 999: %v", err)
	}
	if huge.Limit != investigate.MaxRowsGetDocuments {
		t.Fatalf("limit 999 clamped to %d, want %d", huge.Limit, investigate.MaxRowsGetDocuments)
	}
	if investigate.MaxRowsGetDocuments != 50 {
		t.Fatalf("MaxRowsGetDocuments = %d, want 50", investigate.MaxRowsGetDocuments)
	}
}

func TestDocumentsCursorPaging(t *testing.T) {
	reader := &fakeDocReader{page: investigate.DocumentPage{
		Documents:  documentsFixture()[:1],
		Truncated:  true,
		NextCursor: "doc-02",
	}}
	fn := NewDocumentsTool(reader)

	first, err := fn(context.Background(), documentsRequest(t, 1, ""))
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if !first.Truncated {
		t.Fatalf("first page Truncated = false, want true")
	}

	second, err := fn(context.Background(), documentsRequest(t, 1, "doc-02"))
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if reader.got.cursor != "doc-02" {
		t.Fatalf("reader cursor = %q, want %q", reader.got.cursor, "doc-02")
	}
	if reader.got.limit != 1 {
		t.Fatalf("reader limit = %d, want 1", reader.got.limit)
	}
	_ = second
}

func TestDocumentsEmptyClaim(t *testing.T) {
	reader := &fakeDocReader{page: investigate.DocumentPage{}}
	fn := NewDocumentsTool(reader)
	got, err := fn(context.Background(), documentsRequest(t, 10, ""))
	if err != nil {
		t.Fatalf("empty page: %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("response Validate: %v", err)
	}
	if got.RowCount != 0 {
		t.Fatalf("RowCount = %d, want 0", got.RowCount)
	}
	if len(got.IDs) != 0 {
		t.Fatalf("IDs = %v, want empty", got.IDs)
	}
}

func TestDocumentsDeterministic(t *testing.T) {
	reader := &fakeDocReader{page: investigate.DocumentPage{
		Documents: documentsFixture(),
	}}
	fn := NewDocumentsTool(reader)
	req := documentsRequest(t, 10, "")
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
}

func TestDocumentsReaderErrorPropagates(t *testing.T) {
	reader := &fakeDocReader{err: toolsErr(ports.ErrUpstream, "documents")}
	fn := NewDocumentsTool(reader)
	_, err := fn(context.Background(), documentsRequest(t, 10, ""))
	if err == nil {
		t.Fatalf("want upstream error, got nil")
	}
	requireToolsSentinel(t, err, ports.ErrUpstream)
}
