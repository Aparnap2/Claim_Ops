// Contract conformance runner: every parser adapter (#30) must pass
// RunConformance against its Parser implementation. The runner exercises
// the vendor-neutral guarantees only — input validation, artifact
// validation, cancellation, error taxonomy, and metadata reproducibility.
// Adapter-specific behavior (fidelity, tables, provenance depth) is
// measured by the benchmark harness (#31), not here.
package parser

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// conformanceInput returns a minimal valid TrustedDocument for runner use.
func conformanceInput(t *testing.T) TrustedDocument {
	t.Helper()
	content := []byte("%PDF-1.4\nconformance fixture\n" + strings.Repeat(" ", 600))
	in, err := NewTrustedDocument(
		"doc-conformance", "t-conf", "c-conf",
		"bill.pdf", "application/pdf",
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		int64(len(content)), content,
	)
	if err != nil {
		t.Fatalf("conformance input: %v", err)
	}
	return in
}

// RunConformance asserts the Parser contract. Adapter tests call it as:
//
//	func TestLiteParseConforms(t *testing.T) {
//		parser.RunConformance(t, liteparse.New(...))
//	}
//
// Failures mean the adapter violates the canonical boundary, not that
// extraction quality is poor (quality is #31's job).
func RunConformance(t *testing.T, p Parser) {
	t.Helper()
	if strings.TrimSpace(p.Name()) == "" {
		t.Fatal("contract: adapter Name must be non-blank")
	}
	if strings.TrimSpace(p.Version()) == "" {
		t.Fatal("contract: adapter Version must be non-blank")
	}

	// Happy path: valid input yields a valid artifact with matching
	// identity and reproducibility metadata.
	in := conformanceInput(t)
	out, err := p.Parse(context.Background(), in)
	if err != nil {
		t.Fatalf("contract: Parse(valid) = %v, want artifact", err)
	}
	if err := out.Validate(); err != nil {
		t.Fatalf("contract: artifact invalid: %v", err)
	}
	if out.DocumentID != in.DocumentID {
		t.Fatalf("contract: artifact id = %q, want %q", out.DocumentID, in.DocumentID)
	}
	if out.Metadata.ParserName != p.Name() {
		t.Fatalf("contract: metadata name = %q, want adapter %q", out.Metadata.ParserName, p.Name())
	}
	if out.Metadata.ParserVersion != p.Version() {
		t.Fatalf("contract: metadata version = %q, want adapter %q", out.Metadata.ParserVersion, p.Version())
	}
	if out.Metadata.SourceSHA256 != in.SHA256 {
		t.Fatalf("contract: metadata sha = %q, want input %q", out.Metadata.SourceSHA256, in.SHA256)
	}

	// Cancellation: a cancelled context must fail, never hang or succeed
	// silently on unparseable-cancellation grounds.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Parse(cctx, in); err == nil {
		t.Fatal("contract: Parse(cancelled ctx) = nil, want error")
	}

	// Error taxonomy: unsupported media must classify as
	// ErrUnsupportedMediaType via errors.Is.
	img := in
	img.MediaType = "image/fictional-xyz"
	img.FileName = "scan.fxyz"
	if _, err := p.Parse(context.Background(), img); !errors.Is(err, ErrUnsupportedMediaType) {
		t.Fatalf("contract: Parse(unsupported media) = %v, want ErrUnsupportedMediaType", err)
	}
}

// stubParser is the in-package fake proving the runner accepts a
// conforming adapter and rejects broken ones (used by parser_test.go;
// real adapters live in their own packages per the vendor rule).
type stubParser struct {
	name    string
	version string
	parse   func(ctx context.Context, in TrustedDocument) (ParsedDocument, error)
}

func (s stubParser) Name() string    { return s.name }
func (s stubParser) Version() string { return s.version }
func (s stubParser) Parse(ctx context.Context, in TrustedDocument) (ParsedDocument, error) {
	return s.parse(ctx, in)
}

// conformingStub returns a minimal valid artifact for input.
func conformingStub(name, version string) stubParser {
	return stubParser{name: name, version: version, parse: func(ctx context.Context, in TrustedDocument) (ParsedDocument, error) {
		if err := ctx.Err(); err != nil {
			return ParsedDocument{}, err
		}
		if in.MediaType != "application/pdf" {
			return ParsedDocument{}, ErrUnsupportedMediaType
		}
		return ParsedDocument{
			DocumentID: in.DocumentID,
			Pages: []ParsedPage{{Number: 1, Blocks: []ContentBlock{{
				ID:         "b1",
				Type:       BlockText,
				Text:       "stub",
				Evidence:   EvidenceLocation{DocumentID: in.DocumentID, Page: 1, BlockID: "b1"},
				Confidence: 1.0,
			}}}},
			Metadata: DocumentMetadata{
				ParserName: name, ParserVersion: version,
				SourceSHA256: in.SHA256, SourceMedia: in.MediaType, PageCount: 1,
			},
		}, nil
	}}
}
