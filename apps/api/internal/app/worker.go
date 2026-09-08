package app

import (
	"context"
	"encoding/json"

	"claimops-api/internal/claims"
	"claimops-api/internal/ports"
	"claimops-api/internal/repository/postgres"
	"claimops-api/internal/worker"
)

// documentTenant carries just the tenant field of a DocumentIngested event
// for request-scoped tenant propagation into the processor.
type documentTenant struct {
	Tenant string `json:"tenant"`
}

// StartDocumentWorker subscribes handle to document.ingested events and
// returns a stop func (unsubscribe). The bus owns delivery semantics;
// this is registration only.
func StartDocumentWorker(bus ports.Subscriber, handle func(ctx context.Context, event []byte) error) func() {
	return bus.Subscribe(ports.TopicDocumentIngested, handle)
}

// DocumentEventHandler adapts a worker.Processor to the subscriber
// signature, scoping each event's context to its tenant before handling.
// A blank tenant is passed through untouched: downstream tenant checks
// (postgres.ErrNoTenant) then fail the outcome transiently, ending
// terminal FAILED after retries rather than crashing the consumer.
//
// Pull-path delivery semantics (#23): only TRANSIENT outcomes return a
// non-nil error. Bus finding (verified in code):
//   - ports.InMemoryBus.Publish dispatches synchronously; the first
//     handler error aborts dispatch and is returned wrapped to the
//     Publish caller. There is no redelivery.
//   - pubsubadapter.Subscriber.ReceiveEvent Nacks (redelivers) on a
//     non-nil handler error and Acks on nil.
//
// Returning nil for TERMINAL/SUCCESS/DUPLICATE is what keeps poison
// messages from spinning forever on a Nack-based consumer, while a
// TRANSIENT error lets that same consumer redeliver. NOTE: cmd/api's pull
// loop currently wraps this handler and always returns nil (always Ack),
// so pull redelivery is disabled at the wiring layer regardless; the
// classification here is what makes a Nack-propagating wiring correct.
func DocumentEventHandler(proc *worker.Processor) func(ctx context.Context, event []byte) error {
	return func(ctx context.Context, event []byte) error {
		out := DocumentOutcomeHandler(proc)(ctx, event)
		if out.Kind == worker.OutcomeTransient {
			return out.Err
		}
		return nil
	}
}

// DocumentOutcomeHandler adapts a worker.Processor to the push-handler
// signature, returning the full worker.Outcome so the caller (Pub/Sub
// push endpoint) can branch on Outcome.Kind. Tenant scoping matches
// DocumentEventHandler; both share this single scoping site.
func DocumentOutcomeHandler(proc *worker.Processor) func(ctx context.Context, event []byte) worker.Outcome {
	return func(ctx context.Context, event []byte) worker.Outcome {
		var t documentTenant
		if err := json.Unmarshal(event, &t); err == nil && t.Tenant != "" {
			ctx = postgres.WithTenant(ctx, claims.TenantID(t.Tenant))
		}
		return proc.Handle(ctx, event)
	}
}
