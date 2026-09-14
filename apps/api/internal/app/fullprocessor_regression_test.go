package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	workeradapter "claimops-api/internal/adapters/worker"
	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
	"claimops-api/internal/evidence"
	"claimops-api/internal/extract/xdoc"
	"claimops-api/internal/parser"
	"claimops-api/internal/ports"
)

// stubParser records Parse invocation and returns a canned error (fail
// closed). It implements parser.Parser without model deps.
type stubParser struct {
	calls int
	err   error
}

func (s *stubParser) Parse(_ context.Context, _ parser.TrustedDocument) (parser.ParsedDocument, error) {
	s.calls++
	return parser.ParsedDocument{}, s.err
}

func (s *stubParser) Name() string    { return "stub" }
func (s *stubParser) Version() string { return "0.0.1" }

// fpFetcher returns a canned file triple (no failures).
type fpFetcher struct {
	calls                int
	fileName, mime, body string
}

func (f *fpFetcher) Fetch(_ context.Context, _, _, _ string) (string, string, string, error) {
	f.calls++
	return f.fileName, f.mime, f.body, nil
}

// fpStore records document inserts for the best-effort FAILED row.
type fpStore struct {
	docCalls int
	docs     []documents.Document
}

func (s *fpStore) InsertDocument(_ context.Context, d documents.Document) (bool, error) {
	s.docCalls++
	s.docs = append(s.docs, d)
	return true, nil
}

func (s *fpStore) ListDocuments(_ context.Context, _ claims.ClaimID) ([]documents.Document, error) {
	return append([]documents.Document(nil), s.docs...), nil
}

func (s *fpStore) InsertEvidence(_ context.Context, _ evidence.FieldEvidence) (bool, error) {
	return true, nil
}

func (s *fpStore) ListEvidence(_ context.Context, _ claims.ClaimID) ([]evidence.FieldEvidence, error) {
	return nil, nil
}

func fullProcessorEvent(t *testing.T, docID string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"schema_version": ports.DocumentIngestedSchemaVersion,
		"tenant":         "t1",
		"claim":          "c1",
		"document_id":    docID,
		"sha256":         "abc123",
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestBuildFullProcessorInjectsSeams covers the #78 reconciliation seam:
// nil Parser/Extractors/Scope select documented defaults, the resolved
// values ride on both FullProcessor and the embedded worker.Processor,
// the six-entry xdoc registry validates, and Handle dispatches through
// the injected stub parser (fail-closed terminal, no envelope).
func TestBuildFullProcessorInjectsSeams(t *testing.T) {
	fp, err := BuildFullProcessor(ProcessorDeps{})
	if err != nil {
		t.Fatalf("BuildFullProcessor(defaults) err = %v", err)
	}
	if fp.Processor == nil {
		t.Fatal("FullProcessor.Processor is nil")
	}
	// Default parser is the LiteParse-only adapter (#30).
	if fp.Parser == nil || fp.Parser.Name() != "liteparse" {
		t.Fatalf("FullProcessor.Parser = %v, want liteparse default", fp.Parser)
	}
	// Seam exists on the embedded processor (BLOCKER resolved).
	if fp.Processor.Parser == nil {
		t.Fatal("Processor.Parser is nil, want injected default (seam exists)")
	}
	if fp.Processor.Parser.Name() != fp.Parser.Name() {
		t.Fatalf("Processor.Parser = %q, want %q (injected seam)", fp.Processor.Parser.Name(), fp.Parser.Name())
	}

	// Scope defaults: 5 calls / 60000ms / 5-tool read-only subset.
	if fp.ScopeMaxCalls != 5 {
		t.Fatalf("ScopeMaxCalls = %d, want 5", fp.ScopeMaxCalls)
	}
	if fp.ScopeDeadlineMs != 60000 {
		t.Fatalf("ScopeDeadlineMs = %d, want 60000", fp.ScopeDeadlineMs)
	}
	if len(fp.ScopeAllowTools) != 5 {
		t.Fatalf("len(ScopeAllowTools) = %d, want 5-tool read-only default", len(fp.ScopeAllowTools))
	}
	if fp.Processor.ScopeMaxCalls != fp.ScopeMaxCalls {
		t.Fatalf("Processor.ScopeMaxCalls = %d, want %d", fp.Processor.ScopeMaxCalls, fp.ScopeMaxCalls)
	}
	if fp.Processor.ScopeDeadlineMs != fp.ScopeDeadlineMs {
		t.Fatalf("Processor.ScopeDeadlineMs = %d, want %d", fp.Processor.ScopeDeadlineMs, fp.ScopeDeadlineMs)
	}
	if len(fp.Processor.ScopeAllowTools) != len(fp.ScopeAllowTools) {
		t.Fatalf("Processor.ScopeAllowTools len = %d, want %d", len(fp.Processor.ScopeAllowTools), len(fp.ScopeAllowTools))
	}

	// Registry dispatch key space: six xdoc entries, validated fail-closed.
	if len(fp.Extractors) != 6 {
		t.Fatalf("len(Extractors) = %d, want 6-entry xdoc registry", len(fp.Extractors))
	}
	for _, key := range []string{
		xdoc.DocClaimForm, xdoc.DocDischargeSummary, xdoc.DocHospitalBill,
		xdoc.DocPolicySchedule, xdoc.DocPreauthForm, xdoc.DocLabReport,
	} {
		if _, ok := fp.Extractors[key]; !ok {
			t.Fatalf("Extractors missing registry key %q", key)
		}
	}
	if err := workeradapter.ValidateExtractorRegistry(fp.Extractors); err != nil {
		t.Fatalf("ValidateExtractorRegistry(default) err = %v", err)
	}
	if len(fp.Processor.Extractors) != 6 {
		t.Fatalf("Processor.Extractors len = %d, want injected 6-entry registry", len(fp.Processor.Extractors))
	}

	// Swap Fetcher/Store with fakes and route Handle through the stub
	// parser: proves registry/scope seam wiring executes (stub invoked).
	stub := &stubParser{err: errors.New("stub parse boom")}
	fetch := &fpFetcher{fileName: "claim_form.pdf", mime: "application/pdf", body: "hello world content"}
	store := &fpStore{}
	fp.Processor.Parser = stub
	fp.Processor.Fetcher = fetch
	fp.Processor.Store = store

	out := fp.Processor.Handle(context.Background(), fullProcessorEvent(t, "doc-fp-1"))

	if stub.calls != 1 {
		t.Fatalf("stub Parse calls = %d, want 1 (registry dispatch reached parser)", stub.calls)
	}
	if fetch.calls != 1 {
		t.Fatalf("fetch calls = %d, want 1", fetch.calls)
	}
	if out.Kind != "SUCCESS" && out.Err == nil {
		t.Fatalf("stub-error outcome = %+v, want terminal error (fail-closed)", out)
	}
	if out.Status != documents.StFailed {
		t.Fatalf("stub-error status = %q, want %q", out.Status, documents.StFailed)
	}
	// Envelope fail-closed: deterministic failure carries no envelope.
	if len(out.ExceptionEnvelope) != 0 {
		t.Fatalf("stub-error ExceptionEnvelope len = %d, want 0 (fail-closed)", len(out.ExceptionEnvelope))
	}
	// Terminal FAILED row best-effort persisted for audit parity.
	if store.docCalls == 0 {
		t.Fatal("persistFailedDoc did not run (InsertDocument calls = 0)")
	}
}
