package liteparse

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"claimops-api/internal/parser"
)

// TestLiveShimSmoke runs the real shim + mapping + Validate path against
// a minimal generated PDF. Skipped when liteparse is not installed.
// This is a smoke gate, not a fidelity test: #31 owns corpus scoring.
func TestLiveShimSmoke(t *testing.T) {
	py, shim := liveShim(t)
	content := []byte("%PDF-1.4\nlive smoke\n" + string(make([]byte, 600)))
	in, err := parser.NewTrustedDocument("doc-live", "t1", "c1", "live.pdf",
		"application/pdf", "abc123", int64(len(content)), content)
	if err != nil {
		t.Fatal(err)
	}
	a := New(NewExecRunner(py, shim, 60*time.Second), 60*time.Second)
	// CASE-001 is a real corpus PDF; use its bytes for a faithful smoke.
	realPDF, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "fixtures", "parser_eval", "v1", "CASE-001", "document.pdf"))
	if err != nil {
		t.Skipf("corpus PDF unavailable: %v", err)
	}
	in2, err := parser.NewTrustedDocument("doc-live-1", "t1", "c1", "bill.pdf",
		"application/pdf", "abc123", int64(len(realPDF)), realPDF)
	if err != nil {
		t.Fatal(err)
	}
	_ = in
	doc, err := a.Parse(context.Background(), in2)
	if err != nil {
		t.Fatalf("live parse: %v", err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("live artifact invalid: %v", err)
	}
	if len(doc.Pages) == 0 || len(doc.Pages[0].Blocks) == 0 {
		t.Fatal("live artifact has no content")
	}
	t.Logf("live: %d pages, %d blocks, %d tables",
		len(doc.Pages), len(doc.Pages[0].Blocks), len(doc.Pages[0].Tables))
}

// liveShim resolves the venv interpreter + shim path, skipping when the
// vendor library is unavailable.
func liveShim(t *testing.T) (py, shim string) {
	t.Helper()
	root := filepath.Join("..", "..", "..", "..", "..")
	shim = filepath.Join(root, "tools", "parsers", "liteparse", "shim.py")
	if _, err := os.Stat(shim); err != nil {
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
	t.Skip("liteparse not installed")
	return "", ""
}
