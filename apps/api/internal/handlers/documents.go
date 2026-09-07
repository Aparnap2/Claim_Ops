package handlers

import (
	"context"
	"strings"

	"claimops-api/internal/documents"
	"claimops-api/internal/ingest"

	"github.com/gofiber/fiber/v2"
)

// Uploader ingests one document's opaque bytes and records the RECEIVED
// document row plus its outbox event atomically. It is satisfied by
// ingest.Service.Upload; handlers stay transport-agnostic (no DB, no blob,
// no bus) so unit tests inject a stub.
type Uploader interface {
	Upload(ctx context.Context, tenant, claimID, fileName, mime string, content []byte) (documents.Document, bool, error)
}

// PostDocument ingests one document for claim :id via u.
//
// Body: {file_name, mime, content_text}. Tenant comes from middleware locals
// tenant_id (401 TENANT_MISSING if absent). Duplicate content for the same
// tenant+claim returns 200 with duplicate:true; new documents return 201.
// Request-shape failures (ingest.IsValidation) return 400 VALIDATION_ERROR;
// backend failures return 500 INTERNAL_ERROR. Event emission is owned by
// the transactional outbox inside Upload, never by this handler.
func PostDocument(u Uploader) fiber.Handler {
	return func(c *fiber.Ctx) error {
		tenant, _ := c.Locals("tenant_id").(string)
		if strings.TrimSpace(tenant) == "" {
			return WriteError(c, fiber.StatusUnauthorized, "TENANT_MISSING",
				"X-Tenant-ID header is required")
		}
		claimID := strings.TrimSpace(c.Params("id"))
		if claimID == "" {
			return WriteError(c, fiber.StatusBadRequest, "VALIDATION_ERROR",
				"claim id is required")
		}
		var body struct {
			FileName    string `json:"file_name"`
			MIME        string `json:"mime"`
			ContentText string `json:"content_text"`
		}
		if err := c.BodyParser(&body); err != nil {
			return WriteError(c, fiber.StatusBadRequest, "VALIDATION_ERROR", "malformed JSON body")
		}
		doc, created, err := u.Upload(c.Context(), tenant, claimID, body.FileName, body.MIME, []byte(body.ContentText))
		if err != nil {
			if ingest.IsValidation(err) {
				return WriteError(c, fiber.StatusBadRequest, "VALIDATION_ERROR", err.Error())
			}
			return WriteError(c, fiber.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
		}
		if !created {
			return c.Status(fiber.StatusOK).JSON(fiber.Map{
				"ok": true, "duplicate": true, "document": doc,
			})
		}
		return c.Status(fiber.StatusCreated).JSON(fiber.Map{
			"ok": true, "duplicate": false, "document": doc,
		})
	}
}
