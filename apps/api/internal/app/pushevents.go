package app

import (
	"context"
	"strings"

	"claimops-api/internal/claims"
	"claimops-api/internal/observability"
	"claimops-api/internal/repository/postgres"
	"claimops-api/internal/worker"

	"github.com/gofiber/fiber/v2"
	"google.golang.org/api/idtoken"
)

// PushAuth configures Pub/Sub push authentication. Mode "none" accepts
// unauthenticated delivery (local only — rejected with APP_ENV=prod at
// startup); mode "oidc" validates the Bearer token's audience and sender.
type PushAuth struct {
	Mode           string
	Audience       string
	ServiceAccount string
}

// TokenVerifier validates an OIDC bearer token, returning the sender email.
type TokenVerifier func(ctx context.Context, token, audience string) (string, error)

// OIDCVerifier is the production TokenVerifier over Google's certs.
func OIDCVerifier(ctx context.Context, token, audience string) (string, error) {
	p, err := idtoken.Validate(ctx, token, audience)
	if err != nil {
		return "", err
	}
	email, _ := p.Claims["email"].(string)
	return email, nil
}

// pushEnvelope is the Pub/Sub push delivery body. Data arrives base64 and
// is decoded by encoding/json into raw event bytes automatically.
type pushEnvelope struct {
	Message struct {
		Data       []byte            `json:"data"`
		Attributes map[string]string `json:"attributes"`
		MessageID  string            `json:"messageId"`
	} `json:"message"`
	Subscription string `json:"subscription"`
}

// DocumentPushHandler serves POST /events/document-ingested for Pub/Sub
// push delivery. Contract: 2xx = ack, anything else = redeliver (DLQ after
// max_delivery_attempts server-side). Delivery semantics branch on
// worker.Outcome.Kind (#23): SUCCESS and DUPLICATE ack silently, TERMINAL
// outcomes ack (200) after a worker.push.terminal Warn — they are logged,
// persisted as FAILED, and idempotency makes redelivery a no-op — while
// TRANSIENT outcomes return 503 so Pub/Sub redelivers. Malformed envelopes
// and auth failures return 4xx.
func DocumentPushHandler(handle func(ctx context.Context, event []byte) worker.Outcome, verify TokenVerifier, auth PushAuth) fiber.Handler {
	return func(c *fiber.Ctx) error {
		switch auth.Mode {
		case "oidc":
			email, err := verify(c.Context(), bearerToken(c.Get("Authorization")), auth.Audience)
			if err != nil {
				return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
					"ok": false, "code": "PUSH_UNAUTHORIZED", "message": "invalid push token",
				})
			}
			if email != auth.ServiceAccount {
				return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
					"ok": false, "code": "PUSH_FORBIDDEN", "message": "unexpected push sender",
				})
			}
		case "none":
		default:
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"ok": false, "code": "PUSH_MISCONFIGURED", "message": "unknown push auth mode",
			})
		}
		var env pushEnvelope
		if err := c.BodyParser(&env); err != nil || len(env.Message.Data) == 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"ok": false, "code": "PUSH_MALFORMED", "message": "invalid push envelope",
			})
		}
		ctx := context.Context(c.Context())
		if t := env.Message.Attributes["tenant_id"]; t != "" {
			ctx = postgres.WithTenant(ctx, claims.TenantID(t))
		}
		out := handle(ctx, env.Message.Data)
		errStr := "unknown transient failure"
		if out.Err != nil {
			errStr = out.Err.Error()
		}
		switch out.Kind {
		case worker.OutcomeSuccess, worker.OutcomeDuplicate:
			return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
		case worker.OutcomeTerminal:
			observability.With(ctx).Warn("worker.push.terminal",
				"tenant", env.Message.Attributes["tenant_id"],
				"message_id", env.Message.MessageID,
				"error", errStr,
			)
			return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
		case worker.OutcomeTransient:
			observability.With(ctx).Warn("worker.push.transient",
				"tenant", env.Message.Attributes["tenant_id"],
				"message_id", env.Message.MessageID,
				"error", errStr,
			)
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"ok": false, "code": "PUSH_TRANSIENT", "message": "transient failure, will retry",
			})
		default:
			// Unknown/zero Kind: fail open toward retry. Acking an
			// unclassified outcome would silently drop the event (#23);
			// a spurious redelivery is idempotent by design.
			observability.With(ctx).Warn("worker.push.transient",
				"tenant", env.Message.Attributes["tenant_id"],
				"message_id", env.Message.MessageID,
				"error", "unclassified outcome kind, treating as transient",
			)
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"ok": false, "code": "PUSH_TRANSIENT", "message": "transient failure, will retry",
			})
		}
	}
}

// bearerToken strips the "Bearer " scheme prefix.
func bearerToken(h string) string {
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return h[7:]
	}
	return h
}
