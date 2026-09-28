package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"claimops-api/internal/documents"
	"claimops-api/internal/parser"
	"claimops-api/internal/parser/liteparse"
)

// APA-48: harness faults must not become failTerminal.
//
// Before this change a missing interpreter produced parser.ErrParseFailure,
// so processor.go:824 failTerminal()'d the document: it persisted a FAILED
// document row and acknowledged the message, permanently recording a good
// customer's file as unparseable because our own image was misbuilt. The
// correct routing reuses the EXISTING OutcomeTransient kind (the same
// non-2xx/Nack signal used for cancellation, store and load faults): the
// document is fine, the environment is not, and redelivery may heal it.
//
// fail closed, never open: still no extraction, no evidence, no SUCCESS.

// errParser is a parser stub that always fails with the given error.
type errParser struct{ err error }

func (p errParser) Name() string    { return liteparse.AdapterName }
func (p errParser) Version() string { return liteparse.AdapterVersion }
func (p errParser) Parse(context.Context, parser.TrustedDocument) (parser.ParsedDocument, error) {
	return parser.ParsedDocument{}, p.err
}

func TestParserRuntimeFaultIsTransientNotBadDocument(t *testing.T) {
	fetch, store, loader, checker := happyFixture()
	p := NewProcessor(fetch, store, loader, checker)
	// Exactly the production shape: the adapter wrapped a harness fault.
	p.Parser = errParser{err: fmt.Errorf("liteparse shim harness: %w", liteparse.ErrRuntimeUnavailable)}

	out := p.Handle(context.Background(), goodEvent(t, "doc-runtime"))

	if out.Kind != OutcomeTransient {
		t.Fatalf("runtime fault kind = %q, want %q (not failTerminal/bad-document)",
			out.Kind, OutcomeTransient)
	}
	if out.Status != documents.StFailed {
		t.Fatalf("runtime fault status = %q, want %q", out.Status, documents.StFailed)
	}
	if out.Extraction != documents.ExtractionNotAttempted {
		t.Fatalf("runtime fault extraction = %q, want %q (fail closed)",
			out.Extraction, documents.ExtractionNotAttempted)
	}
	if out.Err == nil {
		t.Fatal("runtime fault Err = nil, want the cause surfaced")
	}
	if !errors.Is(out.Err, liteparse.ErrRuntimeUnavailable) {
		t.Fatalf("runtime fault Err = %v, want it to wrap ErrRuntimeUnavailable", out.Err)
	}

	// The critical assertion: the environment fault must not be recorded as
	// a defective document. A FAILED row is a durable statement about the
	// customer's file; there is no such statement here.
	store.mu.Lock()
	failedRows := 0
	for _, d := range store.docs {
		if d.ID == "doc-runtime" {
			failedRows++
		}
	}
	evRows := len(store.ev)
	store.mu.Unlock()
	if failedRows != 0 {
		t.Fatalf("persisted %d document row(s) for doc-runtime; a runtime fault must not blame the document", failedRows)
	}
	if evRows != 0 {
		t.Fatalf("persisted %d evidence row(s); a failed parse must produce no evidence", evRows)
	}
}

// Guard against over-correction: a real document fault stays terminal and
// still records the FAILED row, so bad documents are not silently retried
// forever.
func TestDocumentParseFaultStaysTerminal(t *testing.T) {
	fetch, store, loader, checker := happyFixture()
	p := NewProcessor(fetch, store, loader, checker)
	p.Parser = errParser{err: fmt.Errorf("liteparse parse failed: %w", parser.ErrParseFailure)}

	out := p.Handle(context.Background(), goodEvent(t, "doc-badfile"))

	if out.Kind != OutcomeTerminal {
		t.Fatalf("document fault kind = %q, want %q", out.Kind, OutcomeTerminal)
	}
	if out.Extraction != documents.ExtractionNotAttempted {
		t.Fatalf("document fault extraction = %q, want %q", out.Extraction, documents.ExtractionNotAttempted)
	}
	if !errors.Is(out.Err, parser.ErrParseFailure) {
		t.Fatalf("document fault Err = %v, want it to wrap ErrParseFailure", out.Err)
	}
	store.mu.Lock()
	failedRows := 0
	for _, d := range store.docs {
		if d.ID == "doc-badfile" {
			failedRows++
		}
	}
	store.mu.Unlock()
	if failedRows != 1 {
		t.Fatalf("persisted %d FAILED row(s) for a bad document, want 1", failedRows)
	}
}
