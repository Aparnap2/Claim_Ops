// T3 (get_documents) backed by investigate.DocumentMetaReader.
//
// Logging: none (see policy.go). Metadata rows carry IDs, types, hashes,
// and statuses only — never bytes — and nothing here is logged regardless.
//
// Envelope: implements investigate.ToolFunc via the tool.go New*
// constructors only (NewGetDocumentsRequest for paging validation,
// NewGetDocumentsResponse + ToResponse for the result). Limit clamps to
// 1..MaxRowsGetDocuments (<=0 selects the cap); the cursor is the opaque
// last-seen document ID ("" opens the first page).
package tools

import (
	"context"
	"fmt"
	"strings"

	"claimops-api/internal/claims"
	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/ports"
)

// DocumentsRequest is the T3 input: investigation envelope identity plus
// paging. Limit <= 0 selects the cap; above-cap clamps (never fails
// closed on count — a model-asked 200 still returns a bounded page).
type DocumentsRequest struct {
	TenantID        string
	ClaimID         string
	InvestigationID string
	RequestID       string
	Limit           int
	Cursor          string
}

// validate rejects blank identity, malformed investigation IDs, and
// untrimmed cursors, and clamps the limit into 1..MaxRowsGetDocuments.
// Every rejection wraps ports.ErrContract (fail closed, permanent).
func (r *DocumentsRequest) validate() error {
	if err := claims.TenantID(r.TenantID).Validate(); err != nil {
		return fmt.Errorf("%w: documents: %v", ports.ErrContract, err)
	}
	if err := claims.ClaimID(r.ClaimID).Validate(); err != nil {
		return fmt.Errorf("%w: documents: %v", ports.ErrContract, err)
	}
	if err := invest.ValidateID(invest.InvestigationIDPrefix, r.InvestigationID); err != nil {
		return fmt.Errorf("%w: documents: %v", ports.ErrContract, err)
	}
	if strings.TrimSpace(r.RequestID) == "" {
		return fmt.Errorf("%w: documents: blank request id", ports.ErrContract)
	}
	if r.Limit <= 0 {
		r.Limit = investigate.MaxRowsGetDocuments
	}
	if r.Limit > investigate.MaxRowsGetDocuments {
		r.Limit = investigate.MaxRowsGetDocuments
	}
	if r.Cursor != "" && r.Cursor != strings.TrimSpace(r.Cursor) {
		return fmt.Errorf("%w: documents: cursor must be trimmed", ports.ErrContract)
	}
	return nil
}

// NewDocumentsTool returns the T3 ToolFunc: re-validate the generic
// envelope, clamp paging, page metadata via the reader, and respond with
// the metadata-only page. Reader errors propagate untouched.
func NewDocumentsTool(reader investigate.DocumentMetaReader) investigate.ToolFunc {
	return func(ctx context.Context, req investigate.Request) (investigate.Response, error) {
		if req.Tool != invest.ToolGetDocuments {
			return investigate.Response{}, fmt.Errorf("%w: documents: dispatched %q, want get_documents", ports.ErrContract, string(req.Tool))
		}
		if err := req.Validate(); err != nil {
			return investigate.Response{}, err
		}
		in := DocumentsRequest{
			TenantID:        req.TenantID,
			ClaimID:         req.ClaimID,
			InvestigationID: req.InvestigationID,
			RequestID:       req.RequestID,
			Limit:           req.Limit,
			Cursor:          req.Cursor,
		}
		if err := in.validate(); err != nil {
			return investigate.Response{}, err
		}
		if reader == nil {
			return investigate.Response{}, fmt.Errorf("%w: documents: nil reader", ports.ErrContract)
		}
		page, err := reader.ListDocuments(ctx, req.TenantID, req.ClaimID, in.Limit, in.Cursor)
		if err != nil {
			return investigate.Response{}, err
		}
		resp, err := investigate.NewGetDocumentsResponse(page.Documents, page.Truncated, page.NextCursor)
		if err != nil {
			return investigate.Response{}, err
		}
		return resp.ToResponse(), nil
	}
}
