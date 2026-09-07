// Command api boots the ClaimOps Go Fiber edge service.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	httpadapter "claimops-api/internal/adapters/http"
	workeradapter "claimops-api/internal/adapters/worker"
	"claimops-api/internal/app"
	"claimops-api/internal/config"
	"claimops-api/internal/ingest"
	"claimops-api/internal/observability"
	"claimops-api/internal/ports"
	"claimops-api/internal/worker"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	shutdown := observability.Init("claimops-api", cfg.OtelEnabled)
	defer shutdown()

	docStore := ingest.New()
	blobStore := ingest.NewBlobStore()
	bus := ports.NewInMemoryBus()
	stopWorker := startDocumentWorker(blobStore, bus)
	defer stopWorker()

	addr := fmt.Sprintf(":%s", cfg.Port)
	log.Printf("claimops-api listening on %s (env=%s)", addr, cfg.AppEnv)
	if err := app.NewWithDeps(docStore, blobStore, bus).Listen(addr); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

// startDocumentWorker subscribes the deterministic document pipeline to
// the in-process bus. It degrades to a no-op (serving continues) when no
// database DSN is configured or the pool cannot be created: ingestion
// still accepts documents, they just await a worker-enabled instance.
// The Pub/Sub transport adapter is a tracked follow-up; delivery here is
// in-process synchronous dispatch (see ports.Subscriber).
func startDocumentWorker(blob *ingest.BlobStore, bus *ports.InMemoryBus) func() {
	noop := func() {}
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		log.Print("worker: no DSN configured, document worker disabled")
		return noop
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("worker: pool unavailable (%v), document worker disabled", err)
		return noop
	}
	policyBase := os.Getenv("POLICY_BASE_URL")
	if policyBase == "" {
		policyBase = "http://localhost:3001"
	}
	policyClient := httpadapter.NewPolicyClient(httpadapter.New(policyBase, 5*time.Second))
	proc := worker.NewProcessor(
		workeradapter.NewFetchBridge(blob),
		workeradapter.NewStoreBridge(pool),
		workeradapter.NewClaimBridge(pool),
		workeradapter.NewPolicyBridge(policyClient),
	)
	log.Print("worker: document pipeline subscribed")
	handle := app.DocumentEventHandler(proc)
	return app.StartDocumentWorker(bus, func(ctx context.Context, event []byte) error {
		// Async boundary: subscriber errors would abort Publish and fail
		// the ingestion request that is already accepted (document stored,
		// blob staged). The worker's terminal Outcome is the record — log
		// it here and always acknowledge delivery.
		if err := handle(ctx, event); err != nil {
			log.Printf("worker: terminal outcome: %v", err)
		}
		return nil
	})
}
