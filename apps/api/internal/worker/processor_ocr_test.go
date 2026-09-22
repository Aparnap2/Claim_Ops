package worker

import (
	"context"
	"testing"

	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
	"claimops-api/internal/parser"
)

// APA-12 RED: HITL low-evidence gate integration.
// Observable contract: low-quality evidence (low OCR confidence, unavailable,
// or invalid) must reach HITL/exception handling, not silent SUCCESS without
// envelope. High confidence must not escalate.

func TestProcessor_OCR_LowConfidence_ReachesHITL(t *testing.T) {
	// Simulate full-chain with low aggregate OCR confidence (0.70)
	// Processor should produce an outcome that reaches HITL/exception path:
	// i.e., ExceptionCodes non-empty or ExceptionEnvelope present.
	conf, err := documents.NewOCRConfidence(0.70)
	if err != nil {
		t.Fatalf("NewOCRConfidence: %v", err)
	}
	if !conf.IsLow(0.85) {
		t.Fatalf("0.70 should be low")
	}
	// The processor gate is: if conf.IsLow(0.85) then should escalate.
	// This test will be wired through processor.runNewPipeline's aggregate check.
	// For now, RED: no such gate exists, so we assert the gate function exists.
	if !shouldEscalateForOCR(conf) {
		t.Fatalf("shouldEscalateForOCR(0.70)=false, want true")
	}
}

func TestProcessor_OCR_AtThreshold_ReachesHITL(t *testing.T) {
	conf, _ := documents.NewOCRConfidence(0.85)
	if !shouldEscalateForOCR(conf) {
		t.Fatalf("0.85 should escalate (boundary)")
	}
}

func TestProcessor_OCR_HighConfidence_NoHITL(t *testing.T) {
	conf, _ := documents.NewOCRConfidence(0.90)
	if shouldEscalateForOCR(conf) {
		t.Fatalf("0.90 should not escalate")
	}
}

func TestProcessor_OCR_Unavailable_ReachesHITL(t *testing.T) {
	conf := documents.UnavailableOCRConfidence()
	if !shouldEscalateForOCR(conf) {
		t.Fatalf("Unavailable should escalate")
	}
}

// mockOCRParser returns one page with one block at the configured confidence.
type mockOCRParser struct {
	conf    float64
	name    string
	version string
}

func (m mockOCRParser) Name() string    { return m.name }
func (m mockOCRParser) Version() string { return m.version }
func (m mockOCRParser) Parse(_ context.Context, in parser.TrustedDocument) (parser.ParsedDocument, error) {
	doc := parser.ParsedDocument{
		DocumentID: in.DocumentID,
		Metadata: parser.DocumentMetadata{
			ParserName: m.name, ParserVersion: m.version,
			SourceSHA256: in.SHA256, SourceMedia: in.MediaType, PageCount: 1,
		},
		Pages: []parser.ParsedPage{{
			Number: 1,
			Blocks: []parser.ContentBlock{{
				ID: "b1", Type: parser.BlockText, Text: "patient_name: Alice",
				Evidence:   parser.EvidenceLocation{DocumentID: in.DocumentID, Page: 1, BlockID: "b1"},
				Confidence: m.conf,
			}},
		}},
	}
	return doc, nil
}

func TestProcessor_FullChain_LowOCRHitsHITL(t *testing.T) {
	fetch, store, loader, checker := happyFixture()
	p := NewProcessor(fetch, store, loader, checker)
	p.Parser = mockOCRParser{conf: 0.70, name: "mock-ocr", version: "test"}
	// Need claim/policy that otherwise passes (no verify exception) — happyFixture already does.
	// Override claim to make verify pass: keep loader/checker as happy.
	// Run via runNewPipeline directly to exercise OCR gate.
	out, _ := p.runNewPipeline(context.Background(), "t1", "c1", "abc123", "doc-low-ocr", "")
	if out.Status != documents.StProcessed {
		t.Fatalf("low OCR status=%q, want PROCESSED (HITL is exception, not FAILED)", out.Status)
	}
	found := false
	for _, c := range out.ExceptionCodes {
		if c == "LOW_OCR_CONFIDENCE" {
			found = true
		}
	}
	if !found {
		t.Fatalf("low OCR (0.70) should reach HITL/exception handling, got codes %v", out.ExceptionCodes)
	}
	if out.ExceptionEnvelope == nil {
		t.Fatalf("low OCR should produce an exception envelope (HITL input)")
	}
}

func TestProcessor_FullChain_AtThresholdHitsHITL(t *testing.T) {
	fetch, store, loader, checker := happyFixture()
	p := NewProcessor(fetch, store, loader, checker)
	p.Parser = mockOCRParser{conf: 0.85, name: "mock-ocr", version: "test"}
	out, _ := p.runNewPipeline(context.Background(), "t1", "c1", "abc123", "doc-at-thr", "")
	found := false
	for _, c := range out.ExceptionCodes {
		if c == "LOW_OCR_CONFIDENCE" {
			found = true
		}
	}
	if !found {
		t.Fatalf("0.85 at threshold should HITL, got codes %v", out.ExceptionCodes)
	}
}

func TestProcessor_FullChain_HighOCRDoesNotHitHITL(t *testing.T) {
	fetch, store, loader, checker := happyFixture()
	p := NewProcessor(fetch, store, loader, checker)
	p.Parser = mockOCRParser{conf: 0.90, name: "mock-ocr", version: "test"}
	out, _ := p.runNewPipeline(context.Background(), "t1", "c1", "abc123", "doc-high-ocr", "")
	for _, c := range out.ExceptionCodes {
		if c == "LOW_OCR_CONFIDENCE" {
			t.Fatalf("0.90 should not HITL, but got LOW_OCR_CONFIDENCE in %v", out.ExceptionCodes)
		}
	}
}

func TestProcessor_FullChain_UnavailableOCRHitsHITL(t *testing.T) {
	// Simulate parser that returns no blocks -> aggregate is unavailable -> HITL
	p := NewProcessor(&fakeFetcher{fileName: "claim_form.pdf", mime: "application/pdf", content: "patient_name: Alice\n"}, &fakeStore{}, &fakeClaimLoader{view: ClaimView{PolicyNumber: "POL-1", PatientName: "Alice"}}, &fakePolicyChecker{data: PolicyData{Number: "POL-1", Active: true}})
	p.Parser = mockOCRParser{conf: 0.90, name: "liteparse", version: "test"}
	// Override to return empty pages (unavailable)
	emptyParser := mockEmptyParser{}
	p.Parser = emptyParser
	out, _ := p.runNewPipeline(context.Background(), "t1", "c1", "abc123", "doc-unavail", "")
	found := false
	for _, c := range out.ExceptionCodes {
		if c == "LOW_OCR_CONFIDENCE" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unavailable OCR (empty doc) should HITL, got codes %v", out.ExceptionCodes)
	}
	_ = claims.TenantID("t1") // keep import used
}

type mockEmptyParser struct{}

func (m mockEmptyParser) Name() string    { return "mock-empty" }
func (m mockEmptyParser) Version() string { return "test" }
func (m mockEmptyParser) Parse(_ context.Context, in parser.TrustedDocument) (parser.ParsedDocument, error) {
	return parser.ParsedDocument{
		DocumentID: in.DocumentID,
		Metadata: parser.DocumentMetadata{
			ParserName: m.Name(), ParserVersion: m.Version(),
			SourceSHA256: in.SHA256, SourceMedia: in.MediaType, PageCount: 0,
		},
		Pages: nil,
	}, nil
}
