package worker

// D1 intake progression (APA-91): the advancer runs at the top of the
// new-pipeline path, before any pipeline work, and its failure must block
// the pipeline (fail-closed). These tests drive the full Handle path with a
// Parser set (the production shape) and prove:
//   - the advancer is consulted exactly once per delivery, under the
//     owning tenant, with the event's claim + document identity;
//   - a progression failure blocks the pipeline (no producer call);
//   - STALE_VERSION classifies TRANSIENT (redelivery heals, locked
//     Phase-1 decision) and is not remembered as a duplicate;
//   - deterministic TransitionError codes classify TERMINAL.

import (
	"context"
	"errors"
	"testing"

	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
	"claimops-api/internal/extract"
	"claimops-api/internal/ports"
)

func intakeProcessor(loader *fakeClaimLoader) *Processor {
	fetch := &fakeFetcher{fileName: "claim_form.pdf", mime: "application/pdf", content: "dummy content"}
	p := NewProcessor(fetch, &fakeStore{}, loader, &fakePolicyChecker{
		data: PolicyData{Number: "POL-1", Patient: "Alice", Active: true},
	})
	p.Parser = suffFakeParser{conf: 0.95, text: "dummy"}
	p.Extractors = map[string]extract.Extractor{
		"CLAIM_FORM": suffFakeExtractor{facts: extract.DocumentFacts{
			DocumentID: "doc-intake", DocType: "CLAIM_FORM",
			Fields: map[string]extract.ExtractedField{},
		}},
	}
	return p
}

func TestProcessor_IntakeAdvancer_ConsultedBeforePipeline(t *testing.T) {
	const tenant, claim, docID = "t-apa91", "c-1", "doc-intake-adv"
	loader := &fakeClaimLoader{view: ClaimView{PolicyNumber: "POL-1", ClaimVersion: 3}}
	p := intakeProcessor(loader)

	out := p.Handle(context.Background(), eventBytes(t, ports.DocumentIngestedSchemaVersion, tenant, claim, docID))

	if len(loader.advTenants) != 1 {
		t.Fatalf("advancer calls = %d, want exactly 1", len(loader.advTenants))
	}
	if loader.advTenants[0] != tenant || loader.advClaims[0] != claim || loader.advDocs[0] != docID {
		t.Fatalf("advancer scope = (%q,%q,%q), want (%q,%q,%q)",
			loader.advTenants[0], loader.advClaims[0], loader.advDocs[0], tenant, claim, docID)
	}
	// The pipeline ran past the advancer (insufficient CLAIM_FORM reaches
	// the producer boundary and lands PROCESSED/HITL, not FAILED).
	if out.Status != documents.StProcessed {
		t.Fatalf("status = %q, want PROCESSED (advancer nil must not block the pipeline)", out.Status)
	}
}

func TestProcessor_IntakeAdvancer_FailureBlocksPipeline(t *testing.T) {
	const tenant, claim, docID = "t-apa91", "c-1", "doc-intake-block"
	loader := &fakeClaimLoader{
		view:   ClaimView{PolicyNumber: "POL-1", ClaimVersion: 3},
		advErr: errors.New("intake store down"),
	}
	p := intakeProcessor(loader)

	out := p.Handle(context.Background(), eventBytes(t, ports.DocumentIngestedSchemaVersion, tenant, claim, docID))

	// Fail-closed: the pipeline must not run — no fetch, no parse, no
	// producer call, no envelope. An ordinary store fault is TRANSIENT.
	if out.Kind != OutcomeTransient {
		t.Fatalf("kind = %q, want TRANSIENT (ordinary store fault)", out.Kind)
	}
	if out.Status != documents.StFailed {
		t.Fatalf("status = %q, want FAILED", out.Status)
	}
	if out.Err == nil {
		t.Fatal("transient outcome must carry the advancer error")
	}
	if len(loader.recTenants) != 0 {
		t.Fatalf("producer consulted = %v, want none (pipeline blocked)", loader.recTenants)
	}
}

func TestProcessor_IntakeAdvancer_StaleVersionTransientNotRemembered(t *testing.T) {
	const tenant, claim, docID = "t-apa91", "c-1", "doc-intake-stale"
	loader := &fakeClaimLoader{
		view:   ClaimView{PolicyNumber: "POL-1", ClaimVersion: 3},
		advErr: &claims.TransitionError{Code: claims.CodeStaleVersion},
	}
	p := intakeProcessor(loader)

	first := p.Handle(context.Background(), eventBytes(t, ports.DocumentIngestedSchemaVersion, tenant, claim, docID))
	if first.Kind != OutcomeTransient {
		t.Fatalf("first kind = %q, want TRANSIENT (locked Phase-1 decision)", first.Kind)
	}
	// A transient outcome must not be remembered as a duplicate: the
	// redelivery reprocesses (and heals via fresh state).
	second := p.Handle(context.Background(), eventBytes(t, ports.DocumentIngestedSchemaVersion, tenant, claim, docID))
	if second.Kind != OutcomeTransient {
		t.Fatalf("redelivery kind = %q, want TRANSIENT (not remembered)", second.Kind)
	}
	if len(loader.advTenants) != 2 {
		t.Fatalf("advancer calls = %d, want 2 (redelivery reprocesses)", len(loader.advTenants))
	}
}

func TestProcessor_IntakeAdvancer_DeterministicErrorsTerminal(t *testing.T) {
	codes := []string{
		claims.CodeTenantMismatch,
		claims.CodeIllegalTransition,
		claims.CodeInvalidEventID,
		claims.CodeUnknownStatus,
		claims.CodeMalformedClaim,
	}
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			const tenant, claim, docID = "t-apa91", "c-1", "doc-intake-term"
			loader := &fakeClaimLoader{
				view:   ClaimView{PolicyNumber: "POL-1", ClaimVersion: 3},
				advErr: &claims.TransitionError{Code: code},
			}
			p := intakeProcessor(loader)

			out := p.Handle(context.Background(), eventBytes(t, ports.DocumentIngestedSchemaVersion, tenant, claim, docID))

			if out.Kind != OutcomeTerminal {
				t.Fatalf("kind = %q, want TERMINAL (deterministic state, retry cannot heal)", out.Kind)
			}
			if len(loader.recTenants) != 0 {
				t.Fatalf("producer consulted = %v, want none (pipeline blocked)", loader.recTenants)
			}
		})
	}
}
