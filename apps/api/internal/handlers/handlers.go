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
