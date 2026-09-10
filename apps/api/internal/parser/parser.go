// Package parser defines the application-owned document parser boundary
// for the ClaimOps evaluation phase (#28, child of #26).
//
// The parser sits downstream of the #16 trust boundary: it never sees
// untrusted bytes. Its input is a TrustedDocument — bytes that already
// passed admission (size, MIME allowlist, content sniffing, filename
// validation) and integrity verification (SHA256 on fetch). Its output
// is a ParsedDocument in the canonical artifact schema owned by ClaimOps.
//
// Vendor rule: no LiteParse, Docling, or other parser-vendor types may
// appear outside adapter packages. Adapters implement Parser and convert
// vendor output into the canonical types here. Downstream code
// (normalization, deterministic extraction, verification) depends only
// on this package.
//
// Provenance rule: page/block/bounding-box provenance is preserved where
// the vendor provides it and represented explicitly (nil BoundingBox,
// empty BlockID) where it does not. Adapters must never fabricate
// provenance.
//
// Purity: stdlib-only (plus the claims value objects), no HTTP, no I/O,
// no logging, no clock reads. Timing and measurement belong to the
// benchmark harness (#31), not to artifacts — ParsedDocument carries no
// timestamps so golden artifacts stay deterministic.
package parser

import (
	"context"
	"errors"
	"strings"

	"claimops-api/internal/claims"
)

// Parser errors. Adapters wrap vendor failures with ErrParseFailure;
// callers use errors.Is to classify without importing vendor packages.
var (
	// ErrBlankDocumentID is returned when a document identifier is empty.
	ErrBlankDocumentID = errors.New("parser: blank document id")
	// ErrEmptyContent is returned when the trusted bytes are empty.
	ErrEmptyContent = errors.New("parser: empty content")
	// ErrUnsupportedMediaType is returned when the adapter cannot parse
	// the trusted media type (e.g. a text-only adapter given an image).
	ErrUnsupportedMediaType = errors.New("parser: unsupported media type")
	// ErrParseFailure wraps a vendor-side parse failure. Adapters must
	// wrap with %w so errors.Is still classifies it.
	ErrParseFailure = errors.New("parser: parse failure")
)

// TrustedDocument is the parser input: bytes that crossed the trust
// boundary. SHA256 is the hash verified at fetch time; SizeBytes is the
// verified byte length. Content is never empty.
type TrustedDocument struct {
	DocumentID string
	Tenant     claims.TenantID
	ClaimID    claims.ClaimID
	FileName   string
	MediaType  string
	SHA256     string
	SizeBytes  int64
	Content    []byte
}

// NewTrustedDocument builds a TrustedDocument, enforcing the boundary
// invariants: non-blank document id, tenant/claim valid per the claims
// value objects, non-blank media type, non-empty content whose length
// matches sizeBytes, and a non-blank expected SHA256. The constructor
// does not hash: hashing belongs to admission/integrity, not to parsing.
func NewTrustedDocument(docID string, tenant claims.TenantID, claimID claims.ClaimID, fileName, mediaType, sha256 string, sizeBytes int64, content []byte) (TrustedDocument, error) {
	if strings.TrimSpace(docID) == "" {
		return TrustedDocument{}, ErrBlankDocumentID
	}
	if err := tenant.Validate(); err != nil {
		return TrustedDocument{}, err
	}
	if err := claimID.Validate(); err != nil {
		return TrustedDocument{}, err
	}
	if strings.TrimSpace(mediaType) == "" {
		return TrustedDocument{}, ErrUnsupportedMediaType
	}
	if len(content) == 0 {
		return TrustedDocument{}, ErrEmptyContent
	}
	if int64(len(content)) != sizeBytes {
		return TrustedDocument{}, errors.New("parser: size mismatch (want len(content) == sizeBytes)")
	}
	if strings.TrimSpace(sha256) == "" {
		return TrustedDocument{}, errors.New("parser: blank sha256")
	}
	return TrustedDocument{
		DocumentID: strings.TrimSpace(docID),
		Tenant:     tenant,
		ClaimID:    claimID,
		FileName:   fileName,
		MediaType:  strings.TrimSpace(mediaType),
		SHA256:     strings.TrimSpace(sha256),
		SizeBytes:  sizeBytes,
		Content:    content,
	}, nil
}

// BlockType is the canonical content-block category.
type BlockType string

const (
	// BlockText is a plain text block (paragraph, line, caption).
	BlockText BlockType = "text"
	// BlockHeading is a section heading or title.
	BlockHeading BlockType = "heading"
	// BlockListItem is one item of a bulleted/numbered list.
	BlockListItem BlockType = "list_item"
	// BlockTableCell is a cell surfaced outside a reconstructed table
	// (adapters that cannot rebuild tables still expose cell text here).
	BlockTableCell BlockType = "table_cell"
	// BlockFigure is a non-text figure/diagram reference with its caption.
	BlockFigure BlockType = "figure"
)

// BoundingBox is a normalized (0-1) rectangle on a page. Coordinates are
// fractions of page width/height so artifacts stay comparable across
// render resolutions.
type BoundingBox struct {
	X0 float64 `json:"x0"`
	Y0 float64 `json:"y0"`
	X1 float64 `json:"x1"`
	Y1 float64 `json:"y1"`
}

// EvidenceLocation pins a block or cell to its source position. A nil
// BoundingBox and/or empty BlockID mean the vendor did not provide that
// level of provenance — never a fabricated zero box.
type EvidenceLocation struct {
	DocumentID string       `json:"document_id"`
	Page       int          `json:"page"`
	BlockID    string       `json:"block_id,omitempty"`
	Box        *BoundingBox `json:"box,omitempty"`
}

// ContentBlock is one canonical unit of page content.
type ContentBlock struct {
	ID         string           `json:"id"`
	Type       BlockType        `json:"type"`
	Text       string           `json:"text"`
	Evidence   EvidenceLocation `json:"evidence"`
	Confidence float64          `json:"confidence"`
}

// TableCell is one canonical table cell. RowSpan/ColSpan are >= 1;
// merged cells are represented once with spans > 1.
type TableCell struct {
	Text     string           `json:"text"`
	Evidence EvidenceLocation `json:"evidence"`
	RowSpan  int              `json:"row_span"`
	ColSpan  int              `json:"col_span"`
}

// TableRow is one canonical table row.
type TableRow struct {
	Cells []TableCell `json:"cells"`
}

// Table is one reconstructed table on a page.
type Table struct {
	ID   string     `json:"id"`
	Rows []TableRow `json:"rows"`
	Page int        `json:"page"`
}

// ParsedPage is one canonical page: 1-based Number, content blocks in
// reading order, and reconstructed tables.
type ParsedPage struct {
	Number int            `json:"number"`
	Blocks []ContentBlock `json:"blocks"`
	Tables []Table        `json:"tables"`
}

// DocumentMetadata carries reproducibility fields. No timestamps: the
// harness records timing externally so golden artifacts stay byte-stable.
type DocumentMetadata struct {
	ParserName    string `json:"parser_name"`
	ParserVersion string `json:"parser_version"`
	SourceSHA256  string `json:"source_sha256"`
	SourceMedia   string `json:"source_media"`
	PageCount     int    `json:"page_count"`
}

// ParsedDocument is the canonical artifact every adapter must produce.
// DocumentID and Metadata.SourceSHA256 tie it back to the trusted input.
type ParsedDocument struct {
	DocumentID string           `json:"document_id"`
	Pages      []ParsedPage     `json:"pages"`
	Metadata   DocumentMetadata `json:"metadata"`
}

// Validate enforces the canonical structural invariants:
//
//   - document id non-blank, pages numbered 1..N ascending with no gaps,
//   - block ids unique within a page, table ids unique within a page,
//   - every block/table evidence page matches its parent page number,
//   - every evidence document id matches the artifact document id,
//   - confidence in [0,1] and non-NaN, row/col spans >= 1,
//   - metadata parser name/version and source SHA non-blank,
//   - metadata page count matches len(pages).
func (d ParsedDocument) Validate() error {
	if strings.TrimSpace(d.DocumentID) == "" {
		return ErrBlankDocumentID
	}
	for i, p := range d.Pages {
		if p.Number != i+1 {
			return errors.New("parser: pages must be numbered 1..N ascending")
		}
		seenBlocks := make(map[string]bool, len(p.Blocks))
		for _, b := range p.Blocks {
			if strings.TrimSpace(b.ID) == "" {
				return errors.New("parser: blank block id")
			}
			if seenBlocks[b.ID] {
				return errors.New("parser: duplicate block id on page")
			}
			seenBlocks[b.ID] = true
			if err := checkConfidence(b.Confidence); err != nil {
				return err
			}
			if err := d.checkEvidence(b.Evidence, p.Number); err != nil {
				return err
			}
		}
		seenTables := make(map[string]bool, len(p.Tables))
		for _, t := range p.Tables {
			if strings.TrimSpace(t.ID) == "" {
				return errors.New("parser: blank table id")
			}
			if seenTables[t.ID] {
				return errors.New("parser: duplicate table id on page")
			}
			seenTables[t.ID] = true
			if t.Page != p.Number {
				return errors.New("parser: table page must match parent page")
			}
			for _, r := range t.Rows {
				for _, c := range r.Cells {
					if c.RowSpan < 1 || c.ColSpan < 1 {
						return errors.New("parser: table spans must be >= 1")
					}
					if err := d.checkEvidence(c.Evidence, p.Number); err != nil {
						return err
					}
				}
			}
		}
	}
	if strings.TrimSpace(d.Metadata.ParserName) == "" {
		return errors.New("parser: blank parser name")
	}
	if strings.TrimSpace(d.Metadata.ParserVersion) == "" {
		return errors.New("parser: blank parser version")
	}
	if strings.TrimSpace(d.Metadata.SourceSHA256) == "" {
		return errors.New("parser: blank source sha256")
	}
	if d.Metadata.PageCount != len(d.Pages) {
		return errors.New("parser: metadata page count must match pages")
	}
	return nil
}

func (d ParsedDocument) checkEvidence(e EvidenceLocation, page int) error {
	if e.DocumentID != d.DocumentID {
		return errors.New("parser: evidence document id must match artifact")
	}
	if e.Page != page {
		return errors.New("parser: evidence page must match parent page")
	}
	if e.Page < 1 {
		return errors.New("parser: evidence page must be >= 1")
	}
	return nil
}

func checkConfidence(c float64) error {
	if c != c || c < 0 || c > 1 {
		return errors.New("parser: confidence must be in [0,1]")
	}
	return nil
}

// Parser converts trusted bytes into the canonical artifact. ctx carries
// cancellation/deadline only; adapters must honor it and return ctx.Err
// (wrapped, so errors.Is still sees context.Cause semantics via Unwrap).
// Implementations are vendor adapters in their own packages; this package
// must never import them (the dependency points adapter -> parser).
type Parser interface {
	// Parse converts input into the canonical artifact. The returned
	// document must pass Validate. Vendor failures are wrapped with
	// ErrParseFailure; unsupported media with ErrUnsupportedMediaType.
	Parse(ctx context.Context, input TrustedDocument) (ParsedDocument, error)
	// Name is the stable adapter name recorded in metadata (e.g.
	// "liteparse", "docling").
	Name() string
	// Version is the vendor library/model version for reproducibility
	// (e.g. "docling 2.31.0", "liteparse 0.4.2").
	Version() string
}
