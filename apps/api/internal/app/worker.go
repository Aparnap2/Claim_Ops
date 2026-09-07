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
func DocumentEventHandler(proc *worker.Processor) func(ctx context.Context, event []byte) error {
	return func(ctx context.Context, event []byte) error {
		var t documentTenant
		if err := json.Unmarshal(event, &t); err == nil && t.Tenant != "" {
			ctx = postgres.WithTenant(ctx, claims.TenantID(t.Tenant))
		}
		return proc.Handle(ctx, event).Err
	}
}
