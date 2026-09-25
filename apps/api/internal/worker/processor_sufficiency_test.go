package worker

// APA-31 wiring RED: sufficiency gate in runNewPipeline (fakes, no live
// infra, no real LLM).
//
// Pre-wire gap (honest): runNewPipeline with a single all-MISSING document
// already yields engine R8 exceptions (DocsPresent single, so the other two
// required docs are absent) — it never verifies Passed=true in the
// single-doc pipeline. The true Passed=true slip-through needs all three
// required docs present-but-empty (proven in sufficiency.TestCurrentPipelineSlipThrough).
// What the wiring adds is the sufficiency-specific signal: an R8 finding at
// MEDIUM with the document as evidence pointer (SynthesizeFinding) plus the
// existing envelope/HITL path, with boundary (minus-one) and unknown-class
// fail-closed routing. These tests fail pre-wire (no MEDIUM R8 with doc
// pointer; R8 counts 2 not 3 for CLAIM_FORM, 3 not 4 for unknown) and pass
// post-wire. The sufficient case passes pre- and post-wire (no over-blocking
// guard; if the gate over-blocked it would inject a third R8).
//
// Wiring-stage note (Evidence nil): envelope Build resolves AGREED provenance
// against nil evidence, so cases with AGREED fields (sufficient, boundary)
// build no envelope (best-effort, pre-existing). They are asserted via R8
// counts in ExceptionCodes only. Empty-facts cases (all-MISSING, unknown)
// carry no AGREED fields, so Build succeeds and the envelope assertion
// proves HITL routing.

import (
	"context"
	"testing"

	"claimops-api/internal/documents"
	"claimops-api/internal/extract"
	"claimops-api/internal/invest"
	"claimops-api/internal/parser"
	"claimops-api/internal/verify"
)

type suffFakeParser struct {
	conf float64
	text string
}

func (m suffFakeParser) Name() string    { return "suff-fake" }
func (m suffFakeParser) Version() string { return "test" }

func (m suffFakeParser) Parse(_ context.Context, in parser.TrustedDocument) (parser.ParsedDocument, error) {
	return parser.ParsedDocument{
		DocumentID: in.DocumentID,
		Metadata: parser.DocumentMetadata{
			ParserName: m.Name(), ParserVersion: m.Version(),
			SourceSHA256: in.SHA256, SourceMedia: in.MediaType, PageCount: 1,
		},
		Pages: []parser.ParsedPage{{
			Number: 1,
			Blocks: []parser.ContentBlock{{
				ID: "b1", Type: parser.BlockText, Text: m.text,
				Evidence:            parser.EvidenceLocation{DocumentID: in.DocumentID, Page: 1, BlockID: "b1"},
				Confidence:          m.conf,
				ConfidenceAvailable: true,
			}},
		}},
	}, nil
}

type suffFakeExtractor struct {
	facts extract.DocumentFacts
}

func (m suffFakeExtractor) Name() string    { return "suff-fake-ext" }
func (m suffFakeExtractor) Version() string { return "test" }

func (m suffFakeExtractor) Extract(_ context.Context, _ parser.ParsedDocument) (extract.DocumentFacts, error) {
	return m.facts, nil
}

func suffPresentField(key, val, norm, docID string) extract.ExtractedField {
	return extract.ExtractedField{
		Key: key, Value: val, Normalized: norm,
		Evidence:         extract.EvidenceRef{DocumentID: docID, Page: 1, BlockID: "b1"},
		Status:           extract.StatusPresent,
		Extractor:        "suff-fake-ext",
		ExtractorVersion: "test",
	}
}

func suffMissingField(key string) extract.ExtractedField {
	return extract.ExtractedField{
		Key:              key,
		Status:           extract.StatusMissing,
		Extractor:        "suff-fake-ext",
		ExtractorVersion: "test",
	}
}

// suffSufficientClaimForm returns sufficient CLAIM_FORM evidence: every
// required key PRESENT with verifywrap-mappable normalized values.
func suffSufficientClaimForm(docID string) extract.DocumentFacts {
	return extract.DocumentFacts{
		DocumentID: docID,
		DocType:    "CLAIM_FORM",
		Fields: map[string]extract.ExtractedField{
			"claim_number":       suffPresentField("claim_number", "CLM-1", "CLM-1", docID),
			"policy_number":      suffPresentField("policy_number", "POL-1", "POL-1", docID),
			"patient_name":       suffPresentField("patient_name", "Alice", "alice", docID),
			"hospital_name":      suffPresentField("hospital_name", "City Hospital", "city hospital", docID),
			"admission_date":     suffPresentField("admission_date", "2024-01-01", "2024-01-01", docID),
			"discharge_date":     suffPresentField("discharge_date", "2024-01-05", "2024-01-05", docID),
			"total_amount_paise": suffPresentField("total_amount_paise", "50.00", "5000", docID),
		},
	}
}

func countR8(codes []string) int {
	n := 0
	for _, c := range codes {
		if c == verify.CodeMissingRequiredDocument {
			n++
		}
	}
	return n
}

func hasLowOCR(codes []string) bool {
	for _, c := range codes {
		if c == "LOW_OCR_CONFIDENCE" {
			return true
		}
	}
	return false
}

// hasSufficiencyR8 reports the sufficiency-specific signal in a decoded
// envelope: an R8 at MEDIUM carrying the document as evidence pointer.
func hasSufficiencyR8(env invest.UnresolvedException, docID string) bool {
	for _, f := range env.RuleFindings {
		if f.Code != invest.RuleMissingRequiredDocument {
			continue
		}
		if f.Severity != invest.SeverityMedium {
			continue
		}
		for _, id := range f.EvidenceIDs {
			if id == docID {
				return true
			}
		}
	}
	return false
}

func suffProcessor(facts extract.DocumentFacts, effective string, view ClaimView, pol PolicyData) (*Processor, string) {
	docID := facts.DocumentID
	fetch := &fakeFetcher{fileName: "claim_form.pdf", mime: "application/pdf", content: "dummy content"}
	if effective == "XRAY_REPORT" {
		fetch.fileName = "xray.pdf"
	}
	p := NewProcessor(fetch, &fakeStore{}, &fakeClaimLoader{view: view}, &fakePolicyChecker{data: pol})
	p.Parser = suffFakeParser{conf: 0.95, text: "dummy"}
	p.Extractors = map[string]extract.Extractor{effective: suffFakeExtractor{facts: facts}}
	return p, docID
}

func TestProcessor_Sufficiency_AllMissing_RaisesInsufficient(t *testing.T) {
	docID := "doc-suff-a"
	facts := extract.DocumentFacts{DocumentID: docID, DocType: "CLAIM_FORM", Fields: map[string]extract.ExtractedField{}}
	// Empty claim/policy identity keeps R1 quiet so R8 counts isolate the gate.
	p, _ := suffProcessor(facts, "CLAIM_FORM", ClaimView{}, PolicyData{Active: true})
	out, _ := p.runNewPipeline(context.Background(), "t1", "c1", "abc123", docID, "CLAIM_FORM")
	if out.Status != documents.StProcessed {
		t.Fatalf("status=%q, want PROCESSED (insufficiency is exception/HITL, not FAILED)", out.Status)
	}
	if out.Kind != OutcomeSuccess {
		t.Fatalf("kind=%q, want SUCCESS (exceptions are data)", out.Kind)
	}
	if hasLowOCR(out.ExceptionCodes) {
		t.Fatalf("high-OCR fixture must not carry LOW_OCR_CONFIDENCE, got %v", out.ExceptionCodes)
	}
	if got := countR8(out.ExceptionCodes); got != 3 {
		t.Fatalf("R8 count=%d, want 3 (2 engine for absent docs + 1 sufficiency gate)", got)
	}
	if out.ExceptionEnvelope == nil {
		t.Fatal("insufficient evidence must produce an exception envelope (HITL input)")
	}
	env, err := invest.Decode(out.ExceptionEnvelope)
	if err != nil {
		t.Fatalf("Decode envelope: %v", err)
	}
	if !hasSufficiencyR8(env, docID) {
		t.Fatalf("envelope lacks sufficiency R8 (MEDIUM with doc pointer %q); findings=%v", docID, env.RuleFindings)
	}
}

func TestProcessor_Sufficiency_Sufficient_NoOverBlocking(t *testing.T) {
	docID := "doc-suff-b"
	facts := suffSufficientClaimForm(docID)
	view := ClaimView{PolicyNumber: "POL-1", PatientName: "Alice", HospitalName: "City Hospital"}
	pol := PolicyData{Number: "POL-1", Patient: "Alice", Active: true}
	p, _ := suffProcessor(facts, "CLAIM_FORM", view, pol)
	out, _ := p.runNewPipeline(context.Background(), "t1", "c1", "abc123", docID, "CLAIM_FORM")
	if out.Status != documents.StProcessed {
		t.Fatalf("status=%q, want PROCESSED", out.Status)
	}
	if out.Kind != OutcomeSuccess {
		t.Fatalf("kind=%q, want SUCCESS", out.Kind)
	}
	if hasLowOCR(out.ExceptionCodes) {
		t.Fatalf("high-OCR sufficient fixture must not carry LOW_OCR_CONFIDENCE, got %v", out.ExceptionCodes)
	}
	// Engine R8s for the two absent docs remain (single-doc pipeline); the
	// gate must not add a third. Wiring-stage envelope is nil here (AGREED
	// provenance unresolvable against nil evidence, best-effort) so the
	// count is the no-over-blocking signal.
	if got := countR8(out.ExceptionCodes); got != 2 {
		t.Fatalf("R8 count=%d, want 2 (engine only; gate must not over-block sufficient evidence)", got)
	}
}

func TestProcessor_Sufficiency_BoundaryMinusOne_HITL(t *testing.T) {
	docID := "doc-suff-c"
	facts := suffSufficientClaimForm(docID)
	delete(facts.Fields, "hospital_name")
	facts.Fields["hospital_name"] = suffMissingField("hospital_name")
	view := ClaimView{PolicyNumber: "POL-1", PatientName: "Alice", HospitalName: "City Hospital"}
	pol := PolicyData{Number: "POL-1", Patient: "Alice", Active: true}
	p, _ := suffProcessor(facts, "CLAIM_FORM", view, pol)
	out, _ := p.runNewPipeline(context.Background(), "t1", "c1", "abc123", docID, "CLAIM_FORM")
	if out.Status != documents.StProcessed {
		t.Fatalf("status=%q, want PROCESSED (boundary insufficiency is HITL, not FAILED)", out.Status)
	}
	if hasLowOCR(out.ExceptionCodes) {
		t.Fatalf("high-OCR fixture must not carry LOW_OCR_CONFIDENCE, got %v", out.ExceptionCodes)
	}
	if got := countR8(out.ExceptionCodes); got != 3 {
		t.Fatalf("R8 count=%d, want 3 (2 engine + 1 gate for the single gapped key)", got)
	}
}

func TestProcessor_Sufficiency_UnknownClass_HITL(t *testing.T) {
	docID := "doc-suff-d"
	facts := extract.DocumentFacts{DocumentID: docID, DocType: "XRAY_REPORT", Fields: map[string]extract.ExtractedField{}}
	p, _ := suffProcessor(facts, "XRAY_REPORT", ClaimView{}, PolicyData{Active: true})
	out, _ := p.runNewPipeline(context.Background(), "t1", "c1", "abc123", docID, "XRAY_REPORT")
	if out.Status != documents.StProcessed {
		t.Fatalf("status=%q, want PROCESSED (unknown class fails closed to HITL, not FAILED)", out.Status)
	}
	if out.Kind == OutcomeTerminal {
		t.Fatalf("unknown class must not be terminal FAILED, got kind %q", out.Kind)
	}
	if hasLowOCR(out.ExceptionCodes) {
		t.Fatalf("high-OCR fixture must not carry LOW_OCR_CONFIDENCE, got %v", out.ExceptionCodes)
	}
	if got := countR8(out.ExceptionCodes); got != 4 {
		t.Fatalf("R8 count=%d, want 4 (3 engine for absent required docs + 1 gate for unknown class)", got)
	}
	if out.ExceptionEnvelope == nil {
		t.Fatal("unknown class must produce an exception envelope (HITL input)")
	}
	env, err := invest.Decode(out.ExceptionEnvelope)
	if err != nil {
		t.Fatalf("Decode envelope: %v", err)
	}
	if !hasSufficiencyR8(env, docID) {
		t.Fatalf("envelope lacks sufficiency R8 (MEDIUM with doc pointer %q); findings=%v", docID, env.RuleFindings)
	}
}
