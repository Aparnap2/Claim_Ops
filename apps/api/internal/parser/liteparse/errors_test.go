package liteparse

import (
	"context"
	"errors"
	"testing"

	"claimops-api/internal/parser"
)

func TestMapEnvelopeErrorTaxonomy(t *testing.T) {
	if err := mapEnvelopeError("unsupported_media", "x"); !errors.Is(err, parser.ErrUnsupportedMediaType) {
		t.Fatalf("unsupported_media = %v", err)
	}
	// APA-48: "harness" moved OUT of this list. It reports a runtime/
	// environment fault (shim cannot read input, vendor import failed,
	// serialization failed), not a defect in the document, so it now maps to
	// ErrRuntimeUnavailable; the worker routes that to the existing
	// OutcomeTransient instead of failTerminal, which used to durably record
	// a good customer's file as unparseable whenever our own image was broken.
	// See runtime_errors_test.go for the dedicated coverage.
	for _, code := range []string{"parse_failure", "timeout", "bogus"} {
		if err := mapEnvelopeError(code, "x"); !errors.Is(err, parser.ErrParseFailure) {
			t.Fatalf("%s = %v, want ErrParseFailure", code, err)
		}
	}
}

func TestWrapCtxPreservesCancellation(t *testing.T) {
	if err := wrapCtx(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v", err)
	}
	if err := wrapCtx(context.DeadlineExceeded); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline = %v", err)
	}
}
