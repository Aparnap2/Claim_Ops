package liteparse

import (
	"encoding/json"
	"testing"

	"claimops-api/internal/parser"
)

func vendorEnvelope(t *testing.T, payload string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"ok":      true,
		"payload": json.RawMessage(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

const tablePayload = `{"total_pages":1,"pages":[{"page_num":1,"width":595.0,"height":842.0,
"blocks":[
{"kind":"heading","text":"Bill","level":2,"ordered":null,"marker":null,
 "bbox":{"x":10,"y":20,"width":100,"height":18},"header":null,"rows":null},
{"kind":"table","text":"","level":null,"ordered":null,"marker":null,"bbox":null,
 "header":[{"text":"Item","bbox":null},{"text":"Amount","bbox":null}],
 "rows":[[{"text":"A","bbox":{"x":10,"y":40,"width":50,"height":12}},{"text":"","bbox":null}]]},
{"kind":"paragraph","text":"Thanks","level":null,"ordered":null,"marker":null,
 "bbox":{"x":10,"y":100,"width":60,"height":12},"header":null,"rows":null}
],"text_items":null}]}`

func TestConvertTable(t *testing.T) {
	doc, err := Convert("doc-1", "sha", "application/pdf", vendorEnvelope(t, tablePayload))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(doc.Pages) != 1 || len(doc.Pages[0].Tables) != 1 {
		t.Fatalf("tables = %+v", doc.Pages)
	}
	tbl := doc.Pages[0].Tables[0]
	if len(tbl.Rows) != 2 || len(tbl.Rows[0].Cells) != 2 {
		t.Fatalf("rows = %+v", tbl.Rows)
	}
	if tbl.Rows[0].Cells[0].Text != "Item" || tbl.Rows[1].Cells[1].Text != "" {
		t.Fatalf("cells = %+v", tbl.Rows)
	}
	// Header row folded in as first row; empty string preserved verbatim.
	if got := tbl.Rows[1].Cells[0].Evidence.Box; got == nil {
		t.Fatal("cell box should be normalized, got nil")
	} else if got.X0 < 0 || got.X1 > 1 {
		t.Fatalf("box not normalized: %+v", got)
	}
	// Blocks: heading + paragraph (table is not a block).
	if len(doc.Pages[0].Blocks) != 2 {
		t.Fatalf("blocks = %+v", doc.Pages[0].Blocks)
	}
	if doc.Pages[0].Blocks[0].Type != parser.BlockHeading {
		t.Fatalf("block0 = %q, want heading", doc.Pages[0].Blocks[0].Type)
	}
	if doc.Metadata.ParserName != AdapterName || doc.Metadata.SourceSHA256 != "sha" {
		t.Fatalf("metadata = %+v", doc.Metadata)
	}
}

func TestConvertListAndFigure(t *testing.T) {
	payload := `{"total_pages":1,"pages":[{"page_num":1,"width":600,"height":800,
"blocks":[
{"kind":"paragraph","text":"Step","level":null,"ordered":true,"marker":null,"bbox":null,"header":null,"rows":null},
{"kind":"rule","text":"","level":null,"ordered":null,"marker":null,"bbox":null,"header":null,"rows":null},
{"kind":"weird-kind","text":"Keep me","level":null,"ordered":null,"marker":null,"bbox":null,"header":null,"rows":null}
],"text_items":null}]}`
	doc, err := Convert("doc-1", "sha", "application/pdf", vendorEnvelope(t, payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got := []parser.BlockType{doc.Pages[0].Blocks[0].Type, doc.Pages[0].Blocks[1].Type, doc.Pages[0].Blocks[2].Type}
	want := []parser.BlockType{parser.BlockListItem, parser.BlockFigure, parser.BlockText}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("blocks = %v, want %v", got, want)
		}
	}
}

func TestConvertTextItemsFallback(t *testing.T) {
	payload := `{"total_pages":1,"pages":[{"page_num":1,"width":600,"height":800,
"blocks":null,"text_items":[
{"text":"Hello","x":10,"y":50,"width":40,"height":12,"confidence":null},
{"text":"world","x":55,"y":50.5,"width":40,"height":12,"confidence":null},
{"text":"Next line","x":10,"y":70,"width":60,"height":12,"confidence":null}
]}]}`
	doc, err := Convert("doc-1", "sha", "application/pdf", vendorEnvelope(t, payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	blocks := doc.Pages[0].Blocks
	if len(blocks) != 2 || blocks[0].Text != "Hello world" || blocks[1].Text != "Next line" {
		t.Fatalf("line grouping = %+v", blocks)
	}
	if blocks[0].Evidence.Box == nil {
		t.Fatal("line box should union item boxes")
	}
}

func TestConvertEnvelopeErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want error
	}{
		{"not json", "nope", parser.ErrParseFailure},
		{"ok false no error", `{"ok":false}`, parser.ErrParseFailure},
		{"unsupported", `{"ok":false,"error":{"code":"unsupported_media","message":"x"}}`, parser.ErrUnsupportedMediaType},
		{"parse failure", `{"ok":false,"error":{"code":"parse_failure","message":"boom"}}`, parser.ErrParseFailure},
		{"unknown code", `{"ok":false,"error":{"code":"weird","message":"x"}}`, parser.ErrParseFailure},
	} {
		if _, err := Convert("d", "s", "m", []byte(tc.raw)); !isErr(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func isErr(err, target error) bool {
	if err == nil {
		return false
	}
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
