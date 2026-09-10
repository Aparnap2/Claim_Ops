package liteparse

import (
	"encoding/json"
	"fmt"
	"sort"

	"claimops-api/internal/parser"
)

// vendorSilentConfidence is assigned to every block because LiteParse
// TextItem.confidence is nil in all observed output and LayoutBlocks
// carry no confidence at all. This is a deliberate placeholder, not a
// measurement: the #31 benchmark harness must decide whether
// vendor-silent 1.0 is acceptable or needs calibration (same open
// question as the Docling adapter). Changing it later is a one-line,
// metadata-visible change confined to this file.
const vendorSilentConfidence = 1.0

// lineGroupTolPt groups text items into lines when their y origins differ
// by at most this many PDF points.
const lineGroupTolPt = 2.0

// envelope is the shim stdout contract: exactly one JSON object with
// either ok:true + payload or ok:false + error.
type envelope struct {
	OK      bool            `json:"ok"`
	Payload json.RawMessage `json:"payload"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// vendorPayload is the adapter-specific payload produced by shim.py.
type vendorPayload struct {
	TotalPages int          `json:"total_pages"`
	Pages      []vendorPage `json:"pages"`
}

type vendorPage struct {
	PageNum   int           `json:"page_num"`
	Width     float64       `json:"width"`
	Height    float64       `json:"height"`
	Blocks    []vendorBlock `json:"blocks"`
	TextItems []vendorItem  `json:"text_items"`
}

type vendorBlock struct {
	Kind    string         `json:"kind"`
	Text    string         `json:"text"`
	Level   *int           `json:"level"`
	Ordered *bool          `json:"ordered"`
	Marker  *string        `json:"marker"`
	BBox    *vendorRect    `json:"bbox"`
	Header  []vendorCell   `json:"header"`
	Rows    [][]vendorCell `json:"rows"`
}

type vendorCell struct {
	Text string      `json:"text"`
	BBox *vendorRect `json:"bbox"`
}

type vendorRect struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type vendorItem struct {
	Text       string   `json:"text"`
	X          float64  `json:"x"`
	Y          float64  `json:"y"`
	Width      float64  `json:"width"`
	Height     float64  `json:"height"`
	Confidence *float64 `json:"confidence"`
}

// Convert maps raw shim stdout to the canonical artifact. docID, sha,
// and media come from the trusted input (never from the vendor), so
// identity metadata stays reproducible. The returned document still
// needs Validate (applied by Adapter.Parse); Convert itself only
// guarantees structural assembly, and wraps vendor-shape problems with
// parser.ErrParseFailure.
func Convert(docID, sha, media string, raw []byte) (parser.ParsedDocument, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return parser.ParsedDocument{}, fmt.Errorf("%w: liteparse envelope not JSON: %v", parser.ErrParseFailure, err)
	}
	if !env.OK {
		if env.Error == nil {
			return parser.ParsedDocument{}, fmt.Errorf("%w: liteparse shim ok:false without error", parser.ErrParseFailure)
		}
		return parser.ParsedDocument{}, mapEnvelopeError(env.Error.Code, env.Error.Message)
	}
	var payload vendorPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return parser.ParsedDocument{}, fmt.Errorf("%w: liteparse payload decode: %v", parser.ErrParseFailure, err)
	}
	doc := parser.ParsedDocument{
		DocumentID: docID,
		Metadata: parser.DocumentMetadata{
			ParserName: AdapterName, ParserVersion: AdapterVersion,
			SourceSHA256: sha, SourceMedia: media, PageCount: len(payload.Pages),
		},
	}
	for _, vp := range payload.Pages {
		page, err := convertPage(docID, vp)
		if err != nil {
			return parser.ParsedDocument{}, err
		}
		doc.Pages = append(doc.Pages, page)
	}
	return doc, nil
}

func convertPage(docID string, vp vendorPage) (parser.ParsedPage, error) {
	page := parser.ParsedPage{Number: vp.PageNum}
	blockSeq, tableSeq := 0, 0
	nextBlockID := func() string { blockSeq++; return fmt.Sprintf("b%d", blockSeq) }
	nextTableID := func() string { tableSeq++; return fmt.Sprintf("t%d", tableSeq) }

	if vp.Blocks != nil {
		for _, vb := range vp.Blocks {
			if vb.Kind == "table" {
				t := parser.Table{ID: nextTableID(), Page: vp.PageNum}
				if vb.Header != nil {
					t.Rows = append(t.Rows, convertRow(docID, vp, vb.Header))
				}
				for _, vr := range vb.Rows {
					t.Rows = append(t.Rows, convertRow(docID, vp, vr))
				}
				page.Tables = append(page.Tables, t)
				continue
			}
			page.Blocks = append(page.Blocks, parser.ContentBlock{
				ID:         nextBlockID(),
				Type:       blockType(vb),
				Text:       vb.Text,
				Evidence:   parser.EvidenceLocation{DocumentID: docID, Page: vp.PageNum, BlockID: fmt.Sprintf("b%d", blockSeq), Box: normalizeBox(vp, vb.BBox)},
				Confidence: vendorSilentConfidence,
			})
		}
		return page, nil
	}
	// Fallback: vendor emitted no blocks; group text items into lines.
	for _, line := range groupLines(vp.TextItems) {
		page.Blocks = append(page.Blocks, parser.ContentBlock{
			ID:         nextBlockID(),
			Type:       parser.BlockText,
			Text:       line.text,
			Evidence:   parser.EvidenceLocation{DocumentID: docID, Page: vp.PageNum, BlockID: fmt.Sprintf("b%d", blockSeq), Box: normalizeBox(vp, line.box)},
			Confidence: vendorSilentConfidence,
		})
	}
	return page, nil
}

// blockType maps vendor layout kinds to canonical block types. Unknown
// kinds fall back to BlockText: preserving the text beats dropping it,
// and the kind information loss is confined to this function.
func blockType(vb vendorBlock) parser.BlockType {
	switch vb.Kind {
	case "heading":
		return parser.BlockHeading
	case "table":
		return parser.BlockTableCell // unreachable (tables handled above); defensive
	case "rule", "figure":
		return parser.BlockFigure
	}
	if (vb.Ordered != nil && *vb.Ordered) || (vb.Marker != nil && *vb.Marker != "") {
		return parser.BlockListItem
	}
	return parser.BlockText
}

func convertRow(docID string, vp vendorPage, cells []vendorCell) parser.TableRow {
	row := parser.TableRow{}
	for _, vc := range cells {
		row.Cells = append(row.Cells, parser.TableCell{
			Text:     vc.Text, // verbatim, including empty strings
			Evidence: parser.EvidenceLocation{DocumentID: docID, Page: vp.PageNum, Box: normalizeBox(vp, vc.BBox)},
			RowSpan:  1,
			ColSpan:  1,
		})
	}
	return row
}

// normalizeBox converts a vendor rect in PDF points to a normalized
// 0-1 box using the page dimensions. A nil rect or degenerate page dims
// yield nil: never fabricate provenance. Coordinates clamp to [0,1].
func normalizeBox(vp vendorPage, r *vendorRect) *parser.BoundingBox {
	if r == nil || vp.Width <= 0 || vp.Height <= 0 {
		return nil
	}
	return &parser.BoundingBox{
		X0: clamp01(r.X / vp.Width),
		Y0: clamp01(r.Y / vp.Height),
		X1: clamp01((r.X + r.Width) / vp.Width),
		Y1: clamp01((r.Y + r.Height) / vp.Height),
	}
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

type line struct {
	text string
	box  *vendorRect
}

// groupLines clusters text items into lines by y origin (±tol), orders
// lines top-to-bottom and items left-to-right, and unions each line's
// bbox. Purely spatial: no semantic inference.
func groupLines(items []vendorItem) []line {
	if len(items) == 0 {
		return nil
	}
	sorted := append([]vendorItem(nil), items...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Y != sorted[j].Y {
			return sorted[i].Y < sorted[j].Y
		}
		return sorted[i].X < sorted[j].X
	})
	var lines []line
	for _, it := range sorted {
		placed := false
		for i := range lines {
			// Compare against the line's running y-origin: reuse box.Y.
			if abs(lines[i].box.Y-it.Y) <= lineGroupTolPt {
				lines[i].text += lines[i].sep() + it.Text
				lines[i].box = unionRect(lines[i].box, itRect(it))
				placed = true
				break
			}
		}
		if !placed {
			lines = append(lines, line{text: it.Text, box: itRect(it)})
		}
	}
	return lines
}

func (l line) sep() string {
	if l.text == "" {
		return ""
	}
	return " "
}

func itRect(it vendorItem) *vendorRect {
	return &vendorRect{X: it.X, Y: it.Y, Width: it.Width, Height: it.Height}
}

func unionRect(a, b *vendorRect) *vendorRect {
	x0 := min(a.X, b.X)
	y0 := min(a.Y, b.Y)
	x1 := max(a.X+a.Width, b.X+b.Width)
	y1 := max(a.Y+a.Height, b.Y+b.Height)
	return &vendorRect{X: x0, Y: y0, Width: x1 - x0, Height: y1 - y0}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
