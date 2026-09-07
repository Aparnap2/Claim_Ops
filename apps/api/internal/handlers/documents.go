package handlers

import (
	"encoding/json"
	"strings"
	"time"

	"claimops-api/internal/ingest"
	"claimops-api/internal/ports"

	"github.com/gofiber/fiber/v2"
)

// documentUploaded is the payload published on ports.TopicDocumentUploaded.
// SchemaVersion carries the versioned-contract marker
// ports.DocumentUploadedSchemaVersion ("document-uploaded.v1").
// EventID is unique per emission ("evt-"+document ID) for end-to-end
// idempotency (tenant, claim, event_id); OccurredAt is RFC3339 UTC.
// Additive envelope fields do not bump the schema version (see ports
// EventEnvelope convention).
type documentUploaded struct {
	SchemaVersion string `json:"schema_version"`
	EventID       string `json:"event_id"`
	OccurredAt    string `json:"occurred_at"`
	Tenant        string `json:"tenant"`
	Claim         string `json:"claim"`
	DocumentID    string `json:"document_id"`
	SHA256        string `json:"sha256"`
}

// PostDocument ingests one document for claim :id.
//
// Body: {file_name, mime, content_text}. Tenant comes from middleware locals
// tenant_id (401 TENANT_MISSING if absent). Duplicate content for the same
// tenant+claim returns 200 with duplicate:true; new documents stage their
// content bytes into blob (for the document worker's fetch bridge), publish
// a DocumentUploaded event and return 201. Publish failure keeps the stored
// document and returns 500 INTERNAL_ERROR.
func PostDocument(store *ingest.Store, blob *ingest.BlobStore, bus ports.EventBus) fiber.Handler {
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
		// Stage content for the worker fetch bridge. BlobStore.Put is
		// infallible in-memory (it only rejects a blank docID, which the
		// store never generates), so a failure here is a programming
		// error surfaced as INTERNAL_ERROR by document choice.
		if err := blob.Put(doc.ID, body.FileName, body.MIME, body.ContentText); err != nil {
			return WriteError(c, fiber.StatusInternalServerError, "INTERNAL_ERROR",
				"failed to stage document content")
		}
		payload, _ := json.Marshal(documentUploaded{
			SchemaVersion: ports.DocumentUploadedSchemaVersion,
			EventID:       "evt-" + doc.ID,
			OccurredAt:    time.Now().UTC().Format(time.RFC3339),
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
