// Package liteparse adapts the LiteParse PDF parser into the canonical
// parser.Parser boundary (GitHub issue #30).
//
// Scope: application/pdf plus the admission-allowlisted image types
// (jpeg/png/tiff), all of which the vendor handles. Anything else is
// rejected with parser.ErrUnsupportedMediaType before any subprocess
// is spawned.
//
// Provenance: bounding boxes come from vendor LayoutBlocks/TextItems in
// PDF points and are normalized to 0-1 by page dimensions. Anything the
// vendor does not provide is nil/empty, never fabricated.
//
// Confidence: TextItem.confidence is nil in all observed vendor output
// and blocks carry no confidence at all. The mapping assigns 1.0 to
// vendor-silent text. This is a deliberate placeholder, not a
// measurement: the #31 benchmark harness must decide whether
// vendor-silent 1.0 is acceptable or needs calibration (same open
// question as the Docling adapter).
package liteparse

import (
	"context"
	"os"
	"time"

	"claimops-api/internal/parser"
)

const (
	// AdapterName is recorded in artifact metadata.
	AdapterName = "liteparse"
	// AdapterVersion pins the vendor library version for reproducibility.
	AdapterVersion = "2.14.4"
	// DefaultTimeout bounds the shim subprocess. LiteParse is a fast
	// native parser (no model downloads); 120s is generous.
	DefaultTimeout = 120 * time.Second
	// DefaultShimPath is the repo-relative path of the Python shim.
	DefaultShimPath = "tools/parsers/liteparse/shim.py"
)

// Adapter implements parser.Parser by running the Python shim in a
// subprocess and converting its JSON envelope via mapping.go.
type Adapter struct {
	runner  Runner
	timeout time.Duration
}

// New builds an Adapter. runner executes the shim (use NewExecRunner in
// production, a fake in tests); timeout <= 0 selects DefaultTimeout.
func New(runner Runner, timeout time.Duration) *Adapter {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Adapter{runner: runner, timeout: timeout}
}

// Name implements parser.Parser.
func (a *Adapter) Name() string { return AdapterName }

// Version implements parser.Parser.
func (a *Adapter) Version() string { return AdapterVersion }

// supportedMedia is the vendor-handled set. It mirrors the admission
// allowlist: every type admission can pass, LiteParse can parse.
var supportedMedia = map[string]bool{
	"application/pdf": true,
	"image/jpeg":      true,
	"image/png":       true,
	"image/tiff":      true,
}

// Parse implements parser.Parser.
func (a *Adapter) Parse(ctx context.Context, input parser.TrustedDocument) (parser.ParsedDocument, error) {
	// Honor cancellation before doing any work.
	if err := ctx.Err(); err != nil {
		return parser.ParsedDocument{}, wrapCtx(err)
	}
	// Scope gate BEFORE spawning: vendor-handled media only.
	if !supportedMedia[input.MediaType] {
		return parser.ParsedDocument{}, unsupportedMedia(input.MediaType)
	}

	// Write the trusted bytes to a temp file. The shim sees only bytes
	// + filename + media type, never tenant/claim context.
	tmp, err := os.CreateTemp("", "liteparse-*.pdf")
	if err != nil {
		return parser.ParsedDocument{}, wrapTempFile(err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(input.Content); err != nil {
		tmp.Close()
		return parser.ParsedDocument{}, wrapTempFile(err)
	}
	if err := tmp.Close(); err != nil {
		return parser.ParsedDocument{}, wrapTempFile(err)
	}

	// Bound the run when the caller supplied no deadline of its own.
	runCtx := ctx
	cancel := context.CancelFunc(func() {})
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		runCtx, cancel = context.WithTimeout(ctx, a.timeout)
		defer cancel()
	}

	raw, err := a.runner.Run(runCtx, tmpPath)
	if err != nil {
		return parser.ParsedDocument{}, err
	}
	doc, err := Convert(input.DocumentID, input.SHA256, input.MediaType, raw)
	if err != nil {
		return parser.ParsedDocument{}, err
	}
	if err := doc.Validate(); err != nil {
		return parser.ParsedDocument{}, wrapValidate(err)
	}
	return doc, nil
}
