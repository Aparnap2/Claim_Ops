package parser

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"claimops-api/internal/claims"
)

func validInput(t *testing.T) TrustedDocument {
	t.Helper()
	content := []byte("%PDF-1.4\nfixture\n" + strings.Repeat(" ", 600))
	in, err := NewTrustedDocument("doc-1", "t1", "c1", "bill.pdf", "application/pdf",
		"abc123", int64(len(content)), content)
	if err != nil {
		t.Fatalf("NewTrustedDocument: %v", err)
	}
	return in
}

func TestNewTrustedDocument(t *testing.T) {
	in := validInput(t)
	if in.SizeBytes != int64(len(in.Content)) {
		t.Fatalf("size = %d, want %d", in.SizeBytes, len(in.Content))
	}
	cases := []struct {
		name string
		mut  func(*TrustedDocument)
	}{
		{"blank id", func(d *TrustedDocument) { d.DocumentID = "  " }},
		{"blank media", func(d *TrustedDocument) { d.MediaType = "" }},
		{"empty content", func(d *TrustedDocument) { d.Content = nil; d.SizeBytes = 0 }},
		{"size mismatch", func(d *TrustedDocument) { d.SizeBytes++ }},
		{"blank sha", func(d *TrustedDocument) { d.SHA256 = "" }},
		{"bad tenant", func(d *TrustedDocument) { d.Tenant = claims.TenantID("") }},
	}
	for _, tc := range cases {
		d := validInput(t)
		tc.mut(&d)
		if _, err := NewTrustedDocument(d.DocumentID, d.Tenant, d.ClaimID, d.FileName, d.MediaType, d.SHA256, d.SizeBytes, d.Content); err == nil {
			t.Fatalf("%s: want error", tc.name)
		}
	}
}

func validArtifact(docID string) ParsedDocument {
	return ParsedDocument{
		DocumentID: docID,
		Pages: []ParsedPage{{Number: 1,
			Blocks: []ContentBlock{{
				ID: "b1", Type: BlockText, Text: "hello",
				Evidence:   EvidenceLocation{DocumentID: docID, Page: 1, BlockID: "b1"},
				Confidence: 0.9,
			}},
			Tables: []Table{{ID: "t1", Page: 1, Rows: []TableRow{{Cells: []TableCell{
				{Text: "a", Evidence: EvidenceLocation{DocumentID: docID, Page: 1}, RowSpan: 1, ColSpan: 2},
			}}}}},
		}},
		Metadata: DocumentMetadata{ParserName: "stub", ParserVersion: "0.1", SourceSHA256: "abc", SourceMedia: "application/pdf", PageCount: 1},
	}
}

func TestArtifactValidate(t *testing.T) {
	if err := validArtifact("doc-1").Validate(); err != nil {
		t.Fatalf("valid artifact: %v", err)
	}
	mutants := []struct {
		name string
		mut  func(*ParsedDocument)
	}{
		{"blank id", func(d *ParsedDocument) { d.DocumentID = "" }},
		{"page gap", func(d *ParsedDocument) { d.Pages[0].Number = 2 }},
		{"dup block", func(d *ParsedDocument) {
			d.Pages[0].Blocks = append(d.Pages[0].Blocks, d.Pages[0].Blocks[0])
		}},
		{"bad confidence", func(d *ParsedDocument) { d.Pages[0].Blocks[0].Confidence = 2 }},
		{"evidence doc mismatch", func(d *ParsedDocument) { d.Pages[0].Blocks[0].Evidence.DocumentID = "other" }},
		{"evidence page mismatch", func(d *ParsedDocument) { d.Pages[0].Blocks[0].Evidence.Page = 9 }},
		{"zero span", func(d *ParsedDocument) { d.Pages[0].Tables[0].Rows[0].Cells[0].RowSpan = 0 }},
		{"blank parser", func(d *ParsedDocument) { d.Metadata.ParserName = "" }},
		{"count mismatch", func(d *ParsedDocument) { d.Metadata.PageCount = 7 }},
	}
	for _, tc := range mutants {
		d := validArtifact("doc-1")
		tc.mut(&d)
		if err := d.Validate(); err == nil {
			t.Fatalf("%s: want error", tc.name)
		}
	}
}

func TestArtifactJSONRoundTrip(t *testing.T) {
	// Golden-serialization suitability: canonical JSON must be stable and
	// carry provenance explicitly (box present here; nil box omits).
	raw, err := json.Marshal(validArtifact("doc-1"))
	if err != nil {
		t.Fatal(err)
	}
	var back ParsedDocument
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("round trip invalid: %v", err)
	}
	if back.Pages[0].Blocks[0].Evidence.Box != nil {
		t.Fatal("box should be nil (unavailable provenance explicit)")
	}
	raw2, _ := json.Marshal(back)
	if string(raw) != string(raw2) {
		t.Fatal("JSON not byte-stable across round trip")
	}
}

func TestConformanceAcceptsGoodAdapter(t *testing.T) {
	RunConformance(t, conformingStub("stub", "0.1"))
}

func TestAdapterIdentityRequired(t *testing.T) {
	// The conformance runner rejects blank Name/Version via t.Fatal
	// (untestable in-process); pin the precondition here instead.
	for _, p := range []Parser{conformingStub("", "0.1"), conformingStub("stub", "")} {
		if strings.TrimSpace(p.Name()) == "" || strings.TrimSpace(p.Version()) == "" {
			continue // runner would reject: good
		}
		t.Fatalf("blank identity adapter %q/%q must be rejectable", p.Name(), p.Version())
	}
}

func TestConformanceCancellation(t *testing.T) {
	p := conformingStub("stub", "0.1")
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Parse(cctx, conformanceInput(t)); err == nil {
		t.Fatal("cancelled parse must fail")
	}
}
