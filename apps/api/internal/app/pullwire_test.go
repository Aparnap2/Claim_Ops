package app

// APA-30: pull-wiring delivery contract (Option A). The pull callback must
// Nack (non-nil) exactly the TRANSIENT class so Pub/Sub redelivers failed
// work, and Ack (nil) TERMINAL/SUCCESS/DUPLICATE so poison messages can
// never spin. Classification itself stays frozen in the worker package and
// DocumentEventHandler; these tests pin the wiring propagation only.
// UNIT phase: fakes only, no server, no real APIs.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"claimops-api/internal/documents"
	"claimops-api/internal/ports"
	"claimops-api/internal/repository/postgres"
	"claimops-api/internal/worker"
)

type transientFetcher struct{ err error }

func (f *transientFetcher) Fetch(_ context.Context, _, _, _ string) (string, string, string, error) {
	return "", "", "", f.err
}

func transientProc(claimsDB *stubClaims) *worker.Processor {
	return worker.NewProcessor(
		&transientFetcher{err: errors.New("boom: store unavailable")},
		&mapStore{docs: map[string]documents.Document{}},
		claimsDB,
		stubPolicy{},
	)
}

func successProc(claimsDB *stubClaims) *worker.Processor {
	return worker.NewProcessor(
		&mapFetcher{files: map[string]string{"doc-apa30": "discharge summary"}},
		&mapStore{docs: map[string]documents.Document{}},
		claimsDB,
		stubPolicy{},
	)
}

func validEvent(t *testing.T) []byte {
	t.Helper()
	evt, _ := json.Marshal(map[string]string{
		"schema_version": ports.DocumentIngestedSchemaVersion,
		"tenant":         "t-apa30",
		"claim":          "CLM-APA30",
		"document_id":    "doc-apa30",
		"sha256":         "ab",
	})
	return evt
}

func TestPullCallbackTransientNacks(t *testing.T) {
	claimsDB := &stubClaims{}
	cb := PullCallback(DocumentEventHandler(transientProc(claimsDB)))
	if err := cb(context.Background(), validEvent(t), map[string]string{"tenant_id": "t-apa30"}); err == nil {
		t.Fatalf("TRANSIENT outcome returned nil from pull wiring: work silently dropped, want non-nil (Nack/redeliver)")
	}
	// Tier-1 (Parser nil) never enters runNewPipeline, so the ADR 010
	// durable producer stays untouched on the TRANSIENT fetch path.
	if len(claimsDB.recTenants) != 0 {
		t.Fatalf("producer consulted on Tier-1 TRANSIENT path: %v, want none", claimsDB.recTenants)
	}
}

func TestPullCallbackTerminalAcks(t *testing.T) {
	cb := PullCallback(DocumentEventHandler(transientProc(&stubClaims{})))
	if err := cb(context.Background(), []byte("{malformed"), map[string]string{"tenant_id": "t-apa30"}); err != nil {
		t.Fatalf("TERMINAL outcome returned non-nil from pull wiring: poison risk, want nil (Ack), got %v", err)
	}
}

func TestPullCallbackSuccessAcks(t *testing.T) {
	claimsDB := &stubClaims{}
	cb := PullCallback(DocumentEventHandler(successProc(claimsDB)))
	if err := cb(context.Background(), validEvent(t), map[string]string{"tenant_id": "t-apa30"}); err != nil {
		t.Fatalf("SUCCESS outcome returned non-nil from pull wiring, want nil (Ack), got %v", err)
	}
	// Tier-1 success path: the durable producer must not be consulted
	// (Parser nil skips runNewPipeline, where the ADR 010 boundary lives).
	if len(claimsDB.recTenants) != 0 {
		t.Fatalf("producer consulted on Tier-1 SUCCESS path: %v, want none", claimsDB.recTenants)
	}
}

func TestPullCallbackPropagatesHandlerError(t *testing.T) {
	sentinel := errors.New("transient: downstream unavailable")
	nack := PullCallback(func(_ context.Context, _ []byte) error { return sentinel })
	if err := nack(context.Background(), []byte("{}"), nil); !errors.Is(err, sentinel) {
		t.Fatalf("handler error not propagated, got %v", err)
	}
	ack := PullCallback(func(_ context.Context, _ []byte) error { return nil })
	if err := ack(context.Background(), []byte("{}"), nil); err != nil {
		t.Fatalf("nil handler error must stay nil (Ack), got %v", err)
	}
}

func TestPullCallbackBindsTenantFromAttrs(t *testing.T) {
	var seen string
	cb := PullCallback(func(ctx context.Context, _ []byte) error {
		if tenant, err := postgres.TenantFrom(ctx); err == nil {
			seen = string(tenant)
		}
		return nil
	})
	if err := cb(context.Background(), []byte("{}"), map[string]string{"tenant_id": "t-bind"}); err != nil {
		t.Fatal(err)
	}
	if seen != "t-bind" {
		t.Fatalf("tenant ctx = %q, want t-bind", seen)
	}
}
