package liteparse

import (
	"context"
	"errors"
	"testing"

	"claimops-api/internal/parser"
)

// fakeRunner serves canned stdout without spawning a subprocess.
type fakeRunner struct {
	raw   []byte
	err   error
	calls int
}

func (f *fakeRunner) Run(ctx context.Context, _ string) ([]byte, error) {
	f.calls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.raw, f.err
}

func testInput(media string, content []byte) parser.TrustedDocument {
	if content == nil {
		content = []byte("%PDF-1.4\ntest\n" + string(make([]byte, 600)))
	}
	in, err := parser.NewTrustedDocument("doc-1", "t1", "c1", "bill.pdf", media,
		"abc123", int64(len(content)), content)
	if err != nil {
		panic(err)
	}
	return in
}

func okEnvelope() []byte {
	return []byte(`{"ok":true,"payload":{"total_pages":1,"pages":[{"page_num":1,"width":600,"height":800,
"blocks":[{"kind":"paragraph","text":"Hi","level":null,"ordered":null,"marker":null,"bbox":null,"header":null,"rows":null}],
"text_items":null}]}}`)
}

func TestAdapterHappyPath(t *testing.T) {
	f := &fakeRunner{raw: okEnvelope()}
	a := New(f, 0)
	doc, err := a.Parse(context.Background(), testInput("application/pdf", nil))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if doc.Metadata.ParserName != "liteparse" || doc.Metadata.ParserVersion != AdapterVersion {
		t.Fatalf("metadata = %+v", doc.Metadata)
	}
	if f.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", f.calls)
	}
}

func TestAdapterUnsupportedMediaShortCircuits(t *testing.T) {
	f := &fakeRunner{raw: okEnvelope()}
	a := New(f, 0)
	_, err := a.Parse(context.Background(), testInput("image/fictional-xyz", nil))
	if !errors.Is(err, parser.ErrUnsupportedMediaType) {
		t.Fatalf("err = %v, want ErrUnsupportedMediaType", err)
	}
	if f.calls != 0 {
		t.Fatal("runner must not spawn for unsupported media")
	}
}

func TestAdapterShimFailure(t *testing.T) {
	f := &fakeRunner{raw: []byte(`{"ok":false,"error":{"code":"parse_failure","message":"boom"}}`)}
	a := New(f, 0)
	_, err := a.Parse(context.Background(), testInput("application/pdf", nil))
	if !errors.Is(err, parser.ErrParseFailure) {
		t.Fatalf("err = %v, want ErrParseFailure", err)
	}
}

func TestAdapterCancelledCtx(t *testing.T) {
	f := &fakeRunner{raw: okEnvelope()}
	a := New(f, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.Parse(ctx, testInput("application/pdf", nil))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestAdapterConformance(t *testing.T) {
	// fakeRunner + REAL mapping path: exercises everything except the
	// vendor binary itself.
	parser.RunConformance(t, New(&fakeRunner{raw: okEnvelope()}, 0))
}
