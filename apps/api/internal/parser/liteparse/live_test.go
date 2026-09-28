package liteparse

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"claimops-api/internal/parser"
)

// requireEnvEnv makes the live parser gate a hard failure instead of a skip
// when set to "1". CI sets it AFTER Python/uv setup, so a broken production
// parser environment fails the job instead of quietly skipping. Locally it
// stays unset so a contributor without the venv is not blocked; that skip is
// the only legitimate one (see liveShim).
const requireLiveEnv = "CLAIMOPS_REQUIRE_LITEPARSE"

// TestLiveShimSmoke runs the real shim + mapping + Validate path against a
// real corpus PDF (CASE-001, the same bytes eval-v1 scores). It is a smoke
// gate, not a fidelity test: #31 owns corpus scoring.
//
// APA-48: this gate previously never gated anything. In CI it ran BEFORE
// `Set up Python` / `uv sync`, so liteparse was not importable and it
// SKIPPED on every run — a green pipeline while every document terminal-
// FAILED in production. It now runs after Python setup, and with
// CLAIMOPS_REQUIRE_LITEPARSE=1 a missing runtime is a FAILURE. The
// assertions below cover the full evidence surface, because "the shim
// started" is not the same claim as "a usable Parser result came back".
func TestLiveShimSmoke(t *testing.T) {
	py, shim := liveShim(t)
	realPDF, err := os.ReadFile(corpusCase001(t))
	if err != nil {
		requireLive(t, fmt.Sprintf("corpus PDF unavailable: %v", err))
		t.Skipf("corpus PDF unavailable: %v", err)
	}
	in, err := parser.NewTrustedDocument("doc-live-1", "t1", "c1", "bill.pdf",
		"application/pdf", "abc123", int64(len(realPDF)), realPDF)
	if err != nil {
		t.Fatal(err)
	}
	a := New(NewExecRunner(py, shim, 60*time.Second), 60*time.Second)

	doc, err := a.Parse(context.Background(), in)
	if err != nil {
		t.Fatalf("live parse: %v", err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("live artifact invalid: %v", err)
	}

	// --- metadata: the artifact must stay traceable to parser + input ---
	if doc.Metadata.ParserName != AdapterName {
		t.Errorf("ParserName = %q, want %q", doc.Metadata.ParserName, AdapterName)
	}
	if doc.Metadata.ParserVersion != AdapterVersion {
		t.Errorf("ParserVersion = %q, want %q", doc.Metadata.ParserVersion, AdapterVersion)
	}
	if doc.Metadata.SourceSHA256 != "abc123" || doc.Metadata.SourceMedia != "application/pdf" {
		t.Errorf("provenance metadata = %+v, want the trusted input echoed", doc.Metadata)
	}
	if doc.Metadata.PageCount != len(doc.Pages) {
		t.Errorf("PageCount = %d, want %d", doc.Metadata.PageCount, len(doc.Pages))
	}

	// --- pages: present, and numbered 1..N in order ---
	if len(doc.Pages) == 0 {
		t.Fatal("live artifact has no pages")
	}

	blocks, tables, cells := 0, 0, 0
	sawHeading := false
	for _, page := range doc.Pages {
		blocks += len(page.Blocks)
		tables += len(page.Tables)

		// --- block IDs: unique, and sequential in reading order ---
		// Sequential IDs are the evidence that the vendor's stream order
		// survived the conversion instead of being reshuffled.
		firstY := 1.0
		anyY := false
		for i, b := range page.Blocks {
			if want := fmt.Sprintf("b%d", i+1); b.ID != want {
				t.Errorf("page %d block[%d].ID = %q, want %q (reading order not preserved)",
					page.Number, i, b.ID, want)
			}
			if b.Evidence.BlockID != b.ID {
				t.Errorf("page %d block %q evidence BlockID = %q, want it to match",
					page.Number, b.ID, b.Evidence.BlockID)
			}
			switch b.Type {
			case parser.BlockHeading:
				sawHeading = true
			case parser.BlockText, parser.BlockListItem, parser.BlockTableCell, parser.BlockFigure:
			default:
				t.Errorf("page %d block %q has unknown type %q", page.Number, b.ID, b.Type)
			}
			// --- bounding boxes: present and normalized, never fabricated ---
			if b.Evidence.Box == nil {
				t.Errorf("page %d block %q has no bounding box (provenance must not be dropped)", page.Number, b.ID)
				continue
			}
			for name, v := range map[string]float64{
				"x0": b.Evidence.Box.X0, "y0": b.Evidence.Box.Y0,
				"x1": b.Evidence.Box.X1, "y1": b.Evidence.Box.Y1,
			} {
				if v < 0 || v > 1 {
					t.Errorf("page %d block %q box %s = %v, want within [0,1]", page.Number, b.ID, name, v)
				}
			}
			if b.Evidence.Box.X1 < b.Evidence.Box.X0 || b.Evidence.Box.Y1 < b.Evidence.Box.Y0 {
				t.Errorf("page %d block %q box is inverted: %+v", page.Number, b.ID, b.Evidence.Box)
			}
			if !anyY || b.Evidence.Box.Y0 < firstY {
				firstY, anyY = b.Evidence.Box.Y0, true
			}
		}
		// --- reading-order evidence: the stream STARTS at the page top ---
		// Deliberately not a monotonic-Y assertion: observed vendor output
		// is not strictly monotonic (multi-column), and asserting it would
		// test a guarantee the parser never made.
		if anyY && page.Blocks[0].Evidence.Box != nil {
			if got := page.Blocks[0].Evidence.Box.Y0; got > firstY+1e-9 {
				t.Errorf("page %d first block Y0 = %v, want the topmost block (Y0 %v)", page.Number, got, firstY)
			}
		}

		// --- tables: reconstructed, with cell-level evidence ---
		for _, tb := range page.Tables {
			if tb.ID == "" {
				t.Errorf("page %d table has blank id", page.Number)
			}
			if tb.Page != page.Number {
				t.Errorf("table %q page = %d, want %d", tb.ID, tb.Page, page.Number)
			}
			for _, row := range tb.Rows {
				for _, c := range row.Cells {
					cells++
					if c.Evidence.Box == nil {
						t.Errorf("page %d table %q cell has no bounding box", page.Number, tb.ID)
					}
					if c.RowSpan < 1 || c.ColSpan < 1 {
						t.Errorf("page %d table %q cell spans = %dx%d, want >= 1x1",
							page.Number, tb.ID, c.RowSpan, c.ColSpan)
					}
				}
			}
		}
	}

	if tables == 0 || cells == 0 {
		t.Errorf("live artifact reconstructed %d table(s) / %d cell(s); CASE-001 is a hospital bill with a line-item table", tables, cells)
	}
	if !sawHeading {
		t.Error("live artifact has no heading blocks (block-type mapping lost)")
	}
	if blocks == 0 {
		t.Error("live artifact has no content blocks")
	}
	t.Logf("live: %d pages, %d blocks, %d tables, %d cells",
		len(doc.Pages), blocks, tables, cells)
}

// corpusCase001 is the eval-v1 corpus document the live smoke parses. A
// real document, not a synthetic one: the smoke must exercise the same
// bytes production sees.
func corpusCase001(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "..", "..", "..", "fixtures", "parser_eval", "v1", "CASE-001", "document.pdf")
}

// requireLive turns a skip into a failure when CI demands a real parser
// environment, so a misbuilt production artifact cannot pass green.
func requireLive(t *testing.T, msg string) {
	t.Helper()
	if os.Getenv(requireLiveEnv) == "1" {
		t.Fatalf("%s: %s is set, so the parser environment must be real; the live shim gate must not skip in CI", requireLiveEnv, msg)
	}
}

// liveShim resolves a working interpreter + shim path. Skips when the
// optional local Python environment is absent (the only legitimate skip:
// the venv is a contributor convenience, not part of the Go build). Under
// CLAIMOPS_REQUIRE_LITEPARSE=1 a missing runtime fails instead — that is
// the mode CI uses, so an uninstalled liteparse can never be mistaken for
// a healthy pipeline.
func liveShim(t *testing.T) (py, shim string) {
	t.Helper()
	root := filepath.Join("..", "..", "..", "..", "..")
	shim = filepath.Join(root, "tools", "parsers", "liteparse", "shim.py")
	if _, err := os.Stat(shim); err != nil {
		requireLive(t, fmt.Sprintf("shim unavailable: %v", err))
		t.Skipf("shim unavailable: %v", err)
	}
	for _, cand := range []string{filepath.Join(root, ".venv", "bin", "python"), "python3"} {
		if err := exec.Command(cand, "-c", "import liteparse").Run(); err == nil {
			abs, err := filepath.Abs(shim)
			if err != nil {
				t.Fatal(err)
			}
			return cand, abs
		}
	}
	requireLive(t, "liteparse is not importable by any candidate interpreter")
	t.Skip("liteparse not installed (set CLAIMOPS_REQUIRE_LITEPARSE=1 to make this a failure)")
	return "", ""
}
