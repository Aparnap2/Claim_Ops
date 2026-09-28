package liteparse

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"claimops-api/internal/parser"
)

// APA-48: a runtime/environment fault is NOT a bad document.
//
// The deployed worker image shipped without an interpreter and without the
// shim, so every ingested document died terminal-FAILED at parse. The
// envelope code for exactly that condition ("harness", shim.py:99/109/148)
// was mapped into parser.ErrParseFailure, which is the document-fault
// class: the worker failTerminal()s it, persists a FAILED document row, and
// blames the customer's file for our broken image. These tests pin the
// separation so the two classes cannot collapse back together.

func TestHarnessEnvelopeIsRuntimeNotDocumentFault(t *testing.T) {
	err := mapEnvelopeError("harness", "liteparse not installed: No module named 'liteparse'")
	if !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("harness envelope = %v, want ErrRuntimeUnavailable", err)
	}
	if errors.Is(err, parser.ErrParseFailure) {
		t.Fatalf("harness envelope = %v, must NOT be classified as a document fault", err)
	}
	if errors.Is(err, parser.ErrUnsupportedMediaType) {
		t.Fatalf("harness envelope = %v, must NOT be classified as a media fault", err)
	}
}

// A genuine document fault must keep its document classification. This
// guards against over-correcting every parse error into "runtime".
func TestDocumentFaultsKeepDocumentClassification(t *testing.T) {
	cases := map[string]error{
		"parse_failure":     parser.ErrParseFailure,
		"timeout":           parser.ErrParseFailure,
		"unsupported_media": parser.ErrUnsupportedMediaType,
		"weird":             parser.ErrParseFailure,
	}
	for code, want := range cases {
		err := mapEnvelopeError(code, "x")
		if !errors.Is(err, want) {
			t.Fatalf("%s = %v, want %v", code, err, want)
		}
		if errors.Is(err, ErrRuntimeUnavailable) {
			t.Fatalf("%s = %v, must not be a runtime fault", code, err)
		}
	}
}

// The exact production failure: DefaultPythonBin cannot be resolved
// because the image has no interpreter. exec.CommandContext fails at
// Start with exec.ErrNotFound.
func TestMissingInterpreterIsRuntimeFault(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "shim.py")
	if err := os.WriteFile(shim, []byte("import sys\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewExecRunner("python3-not-installed-in-image", shim, 10*time.Second)

	_, err := r.Run(context.Background(), shim)
	if err == nil {
		t.Fatal("Run with a missing interpreter = nil, want error")
	}
	if !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("missing interpreter = %v, want ErrRuntimeUnavailable", err)
	}
	if errors.Is(err, parser.ErrParseFailure) {
		t.Fatalf("missing interpreter = %v, must NOT be a document fault", err)
	}
}

// A missing shim is the same class of fault: the image is misbuilt, the
// document is fine. Detected before spawning so it cannot be misread as a
// shim crash (which would surface as a document fault via mapExitError).
func TestMissingShimIsRuntimeFault(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-shipped.py")
	r := NewExecRunner("python3", missing, 10*time.Second)

	_, err := r.Run(context.Background(), filepath.Join(t.TempDir(), "doc.pdf"))
	if err == nil {
		t.Fatal("Run with a missing shim = nil, want error")
	}
	if !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("missing shim = %v, want ErrRuntimeUnavailable", err)
	}
	if errors.Is(err, parser.ErrParseFailure) {
		t.Fatalf("missing shim = %v, must NOT be a document fault", err)
	}
}

// A shim that runs and reports a runtime fault (e.g. the dependency import
// blew up inside the subprocess) must also surface as runtime, never as a
// bad document.
func TestHarnessEnvelopeFromRunnerIsRuntimeFault(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "shim.py")
	// Stands in for a real interpreter that cannot import the vendor lib.
	body := "import json,sys\nprint(json.dumps({'ok':False,'error':{'code':'harness','message':'liteparse not installed'}}))\n"
	if err := os.WriteFile(shim, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := NewExecRunner(pythonForTest(t), shim, 10*time.Second)

	raw, err := runner.Run(context.Background(), shim)
	if err != nil {
		t.Fatalf("Run = %v, want nil (exit 0 + envelope is the contract)", err)
	}
	_, err = Convert("doc-1", "sha", "application/pdf", raw)
	if !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("Convert(harness envelope) = %v, want ErrRuntimeUnavailable", err)
	}
	if errors.Is(err, parser.ErrParseFailure) {
		t.Fatalf("Convert(harness envelope) = %v, must NOT be a document fault", err)
	}
}

// pythonForTest resolves an interpreter for tests that actually spawn one.
func pythonForTest(t *testing.T) string {
	t.Helper()
	if py := os.Getenv("CLAIMOPS_TEST_PYTHON"); py != "" {
		return py
	}
	for _, cand := range []string{"python3", "python"} {
		if _, err := os.Stat(filepath.Join("/usr/bin", cand)); err == nil {
			return cand
		}
	}
	t.Skip("no interpreter available for subprocess test")
	return ""
}
