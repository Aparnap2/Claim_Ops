// Package handlers holds HTTP handlers and the API error contract.
package handlers

import (
	"github.com/gofiber/fiber/v2"
)

// ErrorBody is the stable error envelope. Clients must only rely on
// ok/code/message/request_id — never on raw stack traces.
type ErrorBody struct {
	OK        bool   `json:"ok"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// WriteError renders a stable error envelope with the request ID attached.
func WriteError(c *fiber.Ctx, status int, code, message string) error {
	rid, _ := c.Locals("request_id").(string)
	return c.Status(status).JSON(ErrorBody{OK: false, Code: code, Message: message, RequestID: rid})
}

// Health returns liveness for the test healthcheck gate.
func Health(c *fiber.Ctx) error {
	return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true, "service": "claimops-api"})
}

// ClaimSubmit is an edge stub: strict JSON validation only.
// Authoritative lifecycle, persistence, and workflow dispatch arrive in
// later phases; this stub proves malformed rejection + tenant gating.
func ClaimSubmit(c *fiber.Ctx) error {
	var body struct {
		ClaimReference string `json:"claim_reference"`
		PolicyID       string `json:"policy_id"`
	}
	if err := c.BodyParser(&body); err != nil {
		return WriteError(c, fiber.StatusBadRequest, "VALIDATION_ERROR", "malformed JSON body")
	}
	if body.ClaimReference == "" || body.PolicyID == "" {
		return WriteError(c, fiber.StatusBadRequest, "VALIDATION_ERROR",
			"claim_reference and policy_id are required")
	}
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"ok": true, "status": "RECEIVED"})
}
