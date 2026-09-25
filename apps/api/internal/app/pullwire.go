package app

import (
	"context"

	"claimops-api/internal/claims"
	"claimops-api/internal/observability"
	"claimops-api/internal/repository/postgres"
)

// PullCallback adapts a subscriber handler (typically DocumentEventHandler)
// to the Pub/Sub pull signature used by cmd/api's pull loop.
//
// APA-30, Option A (chosen): propagate the handler error (Nack) for
// TRANSIENT only at the pull wiring. DocumentEventHandler already maps
// TERMINAL/SUCCESS/DUPLICATE to nil, so returning its error Nacks exactly
// the redeliverable class and Acks everything else — poison-message
// protection stays at the frozen classification layer, untouched here.
//
// Rationale over Option B (pin always-Ack as the intentional local path):
// the Code Standards require that a retryable worker failure allow the
// transport to redeliver and forbid swallowing errors; always-Ack silently
// drops failed work at the wiring layer. Poison-spin cannot follow from
// this change because only TRANSIENT is non-nil (permanent outcomes Ack),
// the push path already redelivers TRANSIENT the same way (503), and a
// stuck TRANSIENT on the local emulator surfaces loudly via the Warn below
// plus Pub/Sub retry/DLQ instead of vanishing.
func PullCallback(handle func(ctx context.Context, event []byte) error) func(ctx context.Context, payload []byte, attrs map[string]string) error {
	return func(mctx context.Context, payload []byte, attrs map[string]string) error {
		tenant := ""
		if t, ok := attrs["tenant_id"]; ok && t != "" {
			mctx = postgres.WithTenant(mctx, claims.TenantID(t))
			tenant = t
		}
		if err := handle(mctx, payload); err != nil {
			observability.With(mctx).Warn("worker.pull.transient",
				"tenant", tenant,
				"error", err.Error(),
			)
			return err
		}
		return nil
	}
}
