package liteparse

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"claimops-api/internal/parser"
)

// errShimOverflow is an internal sentinel for unbounded shim output. It
// surfaces wrapped in parser.ErrParseFailure (an infra-shaped vendor
// failure, not a caller media error).
var errShimOverflow = errors.New("liteparse: shim output overflow")

// unsupportedMedia classifies a non-vendor-handled input. Returned BEFORE
// spawning the shim so unsupported media never pays subprocess cost.
func unsupportedMedia(mediaType string) error {
	return fmt.Errorf("%w: liteparse handles pdf/jpeg/png/tiff only, got %q",
		parser.ErrUnsupportedMediaType, mediaType)
}

// wrapCtx preserves cancellation semantics: callers using errors.Is can
// still see context.Canceled / context.DeadlineExceeded via Unwrap.
func wrapCtx(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("liteparse: context deadline exceeded: %w", context.DeadlineExceeded)
	}
	return fmt.Errorf("liteparse: context canceled: %w: %v", context.Canceled, err)
}

// wrapTempFile maps temp-file I/O failures (infra, not vendor output).
func wrapTempFile(err error) error {
	return fmt.Errorf("%w: liteparse temp file: %v", parser.ErrParseFailure, err)
}

// wrapValidate maps a canonical Validate rejection of our own converted
// artifact. This is a bug signal (mapping produced an invalid artifact),
// classified as a parse failure.
func wrapValidate(err error) error {
	return fmt.Errorf("%w: liteparse artifact invalid: %v", parser.ErrParseFailure, err)
}

// wrapExec maps process-lifecycle failures (start/pipe/read). When ctx
// was cancelled, the cancellation error takes precedence.
func wrapExec(stage string, err error, stderr []byte, ctx context.Context) error {
	if ctx.Err() != nil {
		return wrapCtx(ctx.Err())
	}
	if len(stderr) > 0 {
		return fmt.Errorf("%w: liteparse shim %s: %v: %s",
			parser.ErrParseFailure, stage, err, truncate(string(stderr), 500))
	}
	return fmt.Errorf("%w: liteparse shim %s: %v", parser.ErrParseFailure, stage, err)
}

// mapExitError maps a non-zero shim exit (or signal kill) to a typed
// error. ctx cancellation (including the timeout kill) maps to the ctx
// error; anything else is a shim crash -> ErrParseFailure with a stderr
// snippet. Raw (non-envelope) stdout is NOT parsed here: a crashed shim
// cannot be trusted, and envelope parsing belongs to mapping.go.
func mapExitError(runCtx context.Context, waitErr error, stderr []byte) error {
	if runCtx.Err() != nil {
		// Covers both caller cancellation and our timeout kill, plus
		// the race where the shim exits just as ctx fires: prefer the
		// cancellation signal, it is the actionable cause.
		return wrapCtx(runCtx.Err())
	}
	if len(stderr) > 0 {
		return fmt.Errorf("%w: liteparse shim crashed: %v: %s",
			parser.ErrParseFailure, waitErr, truncate(string(stderr), 500))
	}
	return fmt.Errorf("%w: liteparse shim crashed: %v", parser.ErrParseFailure, waitErr)
}

// mapEnvelopeError maps an ok:false envelope code to the parser error
// taxonomy. Envelope codes are fixed by the seam spec.
func mapEnvelopeError(code, message string) error {
	switch code {
	case "unsupported_media":
		// Shim-side media refusal (e.g. magic mismatch on bytes the Go
		// gate let through). Same taxonomy as the Go pre-spawn gate.
		return fmt.Errorf("%w: liteparse shim: %s",
			parser.ErrUnsupportedMediaType, truncate(message, 500))
	case "parse_failure", "timeout", "harness":
		return fmt.Errorf("%w: liteparse shim %s: %s",
			parser.ErrParseFailure, code, truncate(message, 500))
	default:
		return fmt.Errorf("%w: liteparse shim unknown error code %q: %s",
			parser.ErrParseFailure, code, truncate(message, 500))
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
