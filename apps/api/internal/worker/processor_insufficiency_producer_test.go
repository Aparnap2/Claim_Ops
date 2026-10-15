package worker

// ADR 010 producer (APA-91 slice): the durable VALIDATING -> EXCEPTION
// transition is reachable ONLY through the Parser-gated new pipeline
// (runNewPipeline). This is the positive path the app/retrybudget Tier-1
// tests can never cover — they construct via NewProcessor with a nil Parser,
// so they only prove the producer stays untouched on the legacy path.
//
// These tests drive runNewPipeline directly with a genuinely insufficient
// CLAIM_FORM (every required key MISSING), which fires the sufficiency gate
// and must consult the ClaimLoader's durable producer exactly once, under the
// owning tenant, with the loaded claim version. The second case injects a
// non-retryable store fault (pgconn class 23) and proves fail-closed: no
// envelope, no launch, no PROCESSED.

import (
	"context"
	"strings"
	"testing"

	"claimops-api/internal/documents"
	"claimops-api/internal/extract"

	"github.com/jackc/pgx/v5/pgconn"
)

func insuffProducerProcessor(facts extract.DocumentFacts, effective string, loader *fakeClaimLoader) *Processor {
	fetch := &fakeFetcher{fileName: "claim_form.pdf", mime: "application/pdf", content: "dummy content"}
	p := NewProcessor(fetch, &fakeStore{}, loader, &fakePolicyChecker{
		data: PolicyData{Number: "POL-1", Patient: "Alice", Active: true},
	})
	p.Parser = suffFakeParser{conf: 0.95, text: "dummy"}
	p.Extractors = map[string]extract.Extractor{effective: suffFakeExtractor{facts: facts}}
	return p
}

func TestProcessor_InsufficientEvidence_RecordsDurableTransition(t *testing.T) {
	const tenant, claim, docID, version = "t-apa91", "c-1", "doc-apa91-pos", 7
	facts := extract.DocumentFacts{DocumentID: docID, DocType: "CLAIM_FORM", Fields: map[string]extract.ExtractedField{}}
	loader := &fakeClaimLoader{view: ClaimView{PolicyNumber: "POL-1", ClaimVersion: version}}
	p := insuffProducerProcessor(facts, "CLAIM_FORM", loader)

	out, _ := p.runNewPipeline(context.Background(), tenant, claim, "abc123", docID, "CLAIM_FORM")

	if out.Status != documents.StProcessed || out.Kind != OutcomeSuccess {
		t.Fatalf("status=%q kind=%q, want PROCESSED/SUCCESS (insufficiency is HITL, not FAILED)", out.Status, out.Kind)
	}
	if len(loader.recTenants) != 1 {
		t.Fatalf("durable producer calls = %d, want exactly 1", len(loader.recTenants))
	}
	if loader.recTenants[0] != tenant || loader.recClaimIDs[0] != claim {
		t.Fatalf("producer scope = (%q,%q), want (%q,%q)", loader.recTenants[0], loader.recClaimIDs[0], tenant, claim)
	}
	if loader.recVersions[0] != version {
		t.Fatalf("producer expectedVersion = %d, want %d (the loaded claim version)", loader.recVersions[0], version)
	}
	ev := loader.recEventIDs[0]
	if ev == "" || !strings.Contains(ev, tenant) || !strings.Contains(ev, claim) || !strings.Contains(ev, docID) {
		t.Fatalf("producer eventID = %q, want the stable tenant/claim/doc identity", ev)
	}
	if out.ExceptionEnvelope == nil {
		t.Fatal("insufficient evidence must still yield the HITL exception envelope after the durable transition")
	}
}

func TestProcessor_InsufficientEvidence_ProducerFailureFailsClosed(t *testing.T) {
	const tenant, claim, docID = "t-apa91", "c-1", "doc-apa91-neg"
	facts := extract.DocumentFacts{DocumentID: docID, DocType: "CLAIM_FORM", Fields: map[string]extract.ExtractedField{}}
	// Class-23 integrity fault is not retryable: the pipeline must fail
	// closed rather than proceed with unrecorded durable claim state.
	loader := &fakeClaimLoader{
		view:   ClaimView{PolicyNumber: "POL-1", ClaimVersion: 3},
		recErr: &pgconn.PgError{Code: "23505"},
	}
	p := insuffProducerProcessor(facts, "CLAIM_FORM", loader)

	out, _ := p.runNewPipeline(context.Background(), tenant, claim, "abc123", docID, "CLAIM_FORM")

	if len(loader.recTenants) != 1 {
		t.Fatalf("durable producer calls = %d, want exactly 1 (consulted before failing closed)", len(loader.recTenants))
	}
	if out.Kind != OutcomeTerminal {
		t.Fatalf("kind=%q, want TERMINAL (non-retryable store fault fails closed)", out.Kind)
	}
	if out.Status != documents.StFailed {
		t.Fatalf("status=%q, want FAILED (never PROCESSED on an unrecorded durable transition)", out.Status)
	}
	if out.ExceptionEnvelope != nil {
		t.Fatal("must not build an envelope when the durable claim state could not be recorded")
	}
}

func TestProcessor_InsufficientEvidence_SupersededAckedNoLaunch(t *testing.T) {
	const tenant, claim, docID = "t-apa91", "c-1", "doc-apa91-sup"
	facts := extract.DocumentFacts{DocumentID: docID, DocType: "CLAIM_FORM", Fields: map[string]extract.ExtractedField{}}
	// The claim already carries this event AND has advanced past EXCEPTION
	// (a human/lifecycle decision). The producer refuses to re-authorize;
	// the redelivery must be acked with no envelope and no launch.
	loader := &fakeClaimLoader{
		view:   ClaimView{PolicyNumber: "POL-1", ClaimVersion: 9},
		recErr: ErrTransitionSuperseded,
	}
	p := insuffProducerProcessor(facts, "CLAIM_FORM", loader)

	out, _ := p.runNewPipeline(context.Background(), tenant, claim, "abc123", docID, "CLAIM_FORM")

	if out.Kind != OutcomeSuccess || out.Status != documents.StProcessed {
		t.Fatalf("status=%q kind=%q, want PROCESSED/SUCCESS (superseded duplicate is acked, not retried)", out.Status, out.Kind)
	}
	if out.ExceptionEnvelope != nil {
		t.Fatal("a superseded replay must not re-open an exception envelope")
	}
}
