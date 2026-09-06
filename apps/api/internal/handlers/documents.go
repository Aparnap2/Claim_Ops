package handlers

import (
	"encoding/json"
	"strings"

	"claimops-api/internal/ingest"
	"claimops-api/internal/ports"

	"github.com/gofiber/fiber/v2"
)

// documentUploaded is the payload published on ports.TopicDocumentUploaded.
// SchemaVersion carries the versioned-contract marker
// ports.DocumentUploadedSchemaVersion ("document-uploaded.v1").
type documentUploaded struct {
	SchemaVersion string `json:"schema_version"`
	Tenant        string `json:"tenant"`
	Claim         string `json:"claim"`
	DocumentID    string `json:"document_id"`
	SHA256        string `json:"sha256"`
}

// PostDocument ingests one document for claim :id.
//
// Body: {file_name, mime, content_text}. Tenant comes from middleware locals
// tenant_id (401 TENANT_MISSING if absent). Duplicate content for the same
// tenant+claim returns 200 with duplicate:true; new documents publish a
// DocumentUploaded event and return 201. Publish failure keeps the stored
// document and returns 500 INTERNAL_ERROR.
func PostDocument(store *ingest.Store, bus ports.EventBus) fiber.Handler {
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
		doc, created, err := store.Put(tenant, claimID, body.FileName, body.MIME, body.ContentText)
		if err != nil {
			return WriteError(c, fiber.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		}
		if !created {
			return c.Status(fiber.StatusOK).JSON(fiber.Map{
				"ok": true, "duplicate": true, "document": doc,
			})
		}
		payload, _ := json.Marshal(documentUploaded{
			SchemaVersion: ports.DocumentUploadedSchemaVersion,
			Tenant:        string(doc.Tenant),
			Claim:         string(doc.ClaimID),
			DocumentID:    doc.ID,
			SHA256:        doc.SHA256,
		})
		if err := bus.Publish(c.Context(), ports.TopicDocumentUploaded, payload); err != nil {
			return WriteError(c, fiber.StatusInternalServerError, "INTERNAL_ERROR",
				"failed to publish document event")
		}
		return c.Status(fiber.StatusCreated).JSON(fiber.Map{
			"ok": true, "duplicate": false, "document": doc,
		})
	}
}
