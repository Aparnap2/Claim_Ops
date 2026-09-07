// Command api boots the ClaimOps Go Fiber edge service with its
// document-transport plane: GCS-backed ingest, transactional outbox,
// Pub/Sub publish via the outbox dispatcher, and a pull worker loop.
//
// Degradation is explicit and loud, never silent: without a database DSN
// the API still serves (uploads fail closed); without the worker DSN or
// Pub/Sub reachability the dispatcher/pull loops stay off while ingestion
// keeps recording outbox rows for a later worker-enabled instance.
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"claimops-api/internal/adapters/gcsblob"
	httpadapter "claimops-api/internal/adapters/http"
	"claimops-api/internal/adapters/pubsubadapter"
	workeradapter "claimops-api/internal/adapters/worker"
	"claimops-api/internal/app"
	"claimops-api/internal/claims"
	"claimops-api/internal/config"
	"claimops-api/internal/handlers"
	"claimops-api/internal/ingest"
	"claimops-api/internal/observability"
	"claimops-api/internal/outbox"
	"claimops-api/internal/repository/postgres"
	"claimops-api/internal/worker"

	"cloud.google.com/go/pubsub"
	"cloud.google.com/go/storage"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	shutdown := observability.Init("claimops-api", cfg.OtelEnabled)
	defer shutdown()

	ctx := context.Background()
	var uploader handlers.Uploader = app.PendingUploader{}
	if cfg.DatabaseURL != "" {
		if svc, err := buildIngestService(ctx, cfg); err != nil {
			log.Printf("ingest: degraded (%v); document uploads will fail closed", err)
		} else {
			uploader = svc
			startTransportPlane(ctx, cfg, svc)
		}
	} else {
		log.Print("ingest: no DATABASE_URL; document uploads will fail closed")
	}

	addr := fmt.Sprintf(":%s", cfg.Port)
	log.Printf("claimops-api listening on %s (env=%s)", addr, cfg.AppEnv)
	if err := app.NewWithDeps(uploader).Listen(addr); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

// compile-time check: *ingest.Service satisfies the handler contract.
var _ handlers.Uploader = (*ingest.Service)(nil)

// buildIngestService wires the GCS-backed transactional ingest path.
func buildIngestService(ctx context.Context, cfg config.Config) (*ingest.Service, error) {
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("api pool: %w", err)
	}
	sclient, err := storage.NewClient(ctx)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("storage client: %w", err)
	}
	return ingest.NewService(gcsblob.New(cfg.GCSBucketDocuments, sclient), pool), nil
}

// startTransportPlane launches the outbox dispatcher and the Pub/Sub pull
// worker loop as background goroutines. Failures here degrade to
// serving-only mode: outbox rows accumulate for a later worker.
func startTransportPlane(ctx context.Context, cfg config.Config, svc *ingest.Service) {
	wpool, err := pgxpool.New(ctx, cfg.WorkerDatabaseURL)
	if err != nil {
		log.Printf("transport: worker pool unavailable (%v); dispatcher and pull loop disabled", err)
		return
	}
	pclient, err := pubsub.NewClient(ctx, cfg.GCPProject)
	if err != nil {
		wpool.Close()
		log.Printf("transport: pubsub client unavailable (%v); dispatcher and pull loop disabled", err)
		return
	}
	if err := ensureTopicSubscription(ctx, pclient, cfg); err != nil {
		wpool.Close()
		_ = pclient.Close()
		log.Printf("transport: pubsub topology unavailable (%v); dispatcher and pull loop disabled", err)
		return
	}
	publisher := pubsubadapter.NewPublisher(pclient, cfg.PubSubTopicDocuments)
	disp := outbox.New(
		workeradapter.NewOutboxStore(wpool),
		func(ctx context.Context, e postgres.OutboxEvent) error {
			_, err := publisher.PublishEvent(ctx, e.EventType, e.Payload, map[string]string{
				"event_id":  e.EventID,
				"tenant_id": string(e.Tenant),
			})
			return err
		},
		10, 5, 5*time.Second,
	)
	go runDispatcher(ctx, disp, cfg.OutboxPollMS)

	storeBridge := workeradapter.NewStoreBridge(svc.Pool)
	proc := worker.NewProcessor(
		workeradapter.NewBlobFetchBridge(svc.Blobs, storeBridge),
		storeBridge,
		workeradapter.NewClaimBridge(svc.Pool),
		workeradapter.NewPolicyBridge(httpadapter.NewPolicyClient(
			httpadapter.New(cfg.PolicyBaseURL, 5*time.Second))),
	)
	sub := pubsubadapter.NewSubscriber(pclient, cfg.PubSubSubDocuments)
	handle := app.DocumentEventHandler(proc)
	go func() {
		err := sub.ReceiveEvent(ctx, func(mctx context.Context, payload []byte, attrs map[string]string) error {
			if t, ok := attrs["tenant_id"]; ok && t != "" {
				mctx = postgres.WithTenant(mctx, claims.TenantID(t))
			}
			if err := handle(mctx, payload); err != nil {
				log.Printf("worker: terminal outcome: %v", err)
			}
			// Always ack: outcomes are terminal and idempotent
			// (processed-set + canonical IDs + idempotent inserts), so a
			// Nack-redelivered poison message could only spin.
			return nil
		})
		if err != nil {
			log.Printf("transport: pull loop ended: %v", err)
		}
	}()
	log.Print("transport: dispatcher and pull worker running")
}

// ensureTopicSubscription creates the topic and pull subscription when
// missing (idempotent; works on the emulator and on real GCP with
// pubsub.topics.create / pubsub.subscriptions.create). Terraform owns
// production topology long-term; this keeps local/dev boot self-sufficient.
func ensureTopicSubscription(ctx context.Context, client *pubsub.Client, cfg config.Config) error {
	topic := client.Topic(cfg.PubSubTopicDocuments)
	exists, err := topic.Exists(ctx)
	if err != nil {
		return fmt.Errorf("topic check: %w", err)
	}
	if !exists {
		if _, err := client.CreateTopic(ctx, cfg.PubSubTopicDocuments); err != nil {
			return fmt.Errorf("create topic: %w", err)
		}
	}
	sub := client.Subscription(cfg.PubSubSubDocuments)
	exists, err = sub.Exists(ctx)
	if err != nil {
		return fmt.Errorf("subscription check: %w", err)
	}
	if !exists {
		_, err := client.CreateSubscription(ctx, cfg.PubSubSubDocuments, pubsub.SubscriptionConfig{
			Topic:       topic,
			AckDeadline: 60 * time.Second,
		})
		if err != nil {
			return fmt.Errorf("create subscription: %w", err)
		}
	}
	return nil
}

// runDispatcher polls the outbox until ctx ends (server lifetime scope).
func runDispatcher(ctx context.Context, disp *outbox.Dispatcher, pollMS int) {
	t := time.NewTicker(time.Duration(pollMS) * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			published, dead, err := disp.RunOnce(ctx)
			if err != nil {
				log.Printf("transport: dispatcher error: %v", err)
				continue
			}
			if published+dead > 0 {
				log.Printf("transport: dispatcher published=%d dead=%d", published, dead)
			}
		}
	}
}
