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
	for _, code := range []string{"parse_failure", "timeout", "harness", "bogus"} {
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
