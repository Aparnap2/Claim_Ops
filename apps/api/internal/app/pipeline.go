package app

import (
	"time"

	httpadapter "claimops-api/internal/adapters/http"
	workeradapter "claimops-api/internal/adapters/worker"
	"claimops-api/internal/ports"
	"claimops-api/internal/worker"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ProcessorDeps bundles what the deterministic document pipeline needs.
// Both entrypoints (cmd/api pull loop, cmd/worker push endpoint) build
// the identical processor from these deps — the only difference between
// runtimes is transport, never application behavior.
type ProcessorDeps struct {
	Blobs         ports.BlobStore
	Pool          *pgxpool.Pool
	PolicyBaseURL string
	PolicyTimeout time.Duration
}

// BuildProcessor wires the Tier-1 pipeline: GCS-backed fetch, idempotent
// postgres store, claim loader, Mockoon-backed policy check.
func BuildProcessor(d ProcessorDeps) *worker.Processor {
	store := workeradapter.NewStoreBridge(d.Pool)
	timeout := d.PolicyTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return worker.NewProcessor(
		workeradapter.NewBlobFetchBridge(d.Blobs, store),
		store,
		workeradapter.NewClaimBridge(d.Pool),
		workeradapter.NewPolicyBridge(httpadapter.NewPolicyClient(
			httpadapter.New(d.PolicyBaseURL, timeout))),
	)
}
