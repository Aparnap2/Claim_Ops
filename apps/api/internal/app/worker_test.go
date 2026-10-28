package app

import (
	"context"
	"encoding/json"
	"testing"

	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
	"claimops-api/internal/evidence"
	"claimops-api/internal/ports"
	"claimops-api/internal/repository/postgres"
	"claimops-api/internal/worker"
)

func TestStartDocumentWorkerDispatchAndStop(t *testing.T) {
	bus := ports.NewInMemoryBus()
	var got [][]byte
	stop := StartDocumentWorker(bus, func(_ context.Context, event []byte) error {
		got = append(got, append([]byte(nil), event...))
		return nil
	})
	evt, _ := json.Marshal(map[string]string{"document_id": "doc-1"})
	if err := bus.Publish(context.Background(), ports.TopicDocumentIngested, evt); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 dispatch, got %d", len(got))
	}
	stop()
	if err := bus.Publish(context.Background(), ports.TopicDocumentIngested, evt); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected no dispatch after stop, got %d", len(got))
	}
}

func TestDocumentEventHandlerScopesTenant(t *testing.T) {
	store := &mapStore{docs: map[string]documents.Document{}}
	claimsDB := &stubClaims{}
	proc := worker.NewProcessor(
		&mapFetcher{files: map[string]string{"doc-9": "discharge summary"}},
		store,
		claimsDB,
		stubPolicy{},
	)
	h := DocumentEventHandler(proc)
	evt, _ := json.Marshal(map[string]string{
		"schema_version": ports.DocumentIngestedSchemaVersion,
		"tenant":         "t-scope",
		"claim":          "CLM-9",
		"document_id":    "doc-9",
		"sha256":         "ab",
	})
	if err := h(context.Background(), evt); err != nil {
		t.Fatalf("handle failed: %v", err)
	}
	if store.tenantSeen != "t-scope" {
		t.Fatalf("expected tenant-scoped ctx t-scope, saw %q", store.tenantSeen)
	}
	// Tier-1 (NewProcessor leaves Parser nil) never enters runNewPipeline,
	// where the ADR 010 durable producer lives. This negative control pins
	// that: the durable boundary must stay untouched on the legacy path
	// (a future accidental wiring of the producer into Tier-1 would fail
	// here). The producer's positive path is proven in the worker package
	// against the Parser-gated pipeline.
	if len(claimsDB.recTenants) != 0 {
		t.Fatalf("Tier-1 path must not consult the durable producer, got %v", claimsDB.recTenants)
	}
}

type mapFetcher struct {
	files map[string]string
}

func (m *mapFetcher) Fetch(_ context.Context, _, _, docID string) (string, string, string, error) {
	return "f.txt", "text/plain", m.files[docID], nil
}

type mapStore struct {
	docs       map[string]documents.Document
	tenantSeen string
}

func (m *mapStore) InsertDocument(ctx context.Context, d documents.Document) (bool, error) {
	if t, err := postgres.TenantFrom(ctx); err == nil {
		m.tenantSeen = string(t)
	}
	m.docs[d.ID] = d
	return true, nil
}

func (m *mapStore) ListDocuments(_ context.Context, _ claims.ClaimID) ([]documents.Document, error) {
	return nil, nil
}

func (m *mapStore) InsertEvidence(_ context.Context, _ evidence.FieldEvidence) (bool, error) {
	return true, nil
}

func (m *mapStore) ListEvidence(_ context.Context, _ claims.ClaimID) ([]evidence.FieldEvidence, error) {
	return nil, nil
}

// stubClaims is a tenant-agnostic read model double for the Tier-1 (legacy)
// pipeline. It records producer calls so tests can prove the ADR 010 durable
// boundary is never consulted on the Parser-nil Tier-1 path.
type stubClaims struct {
	recTenants []string
}

func (s *stubClaims) LoadClaim(_ context.Context, _, _ string) (worker.ClaimView, error) {
	return worker.ClaimView{PolicyNumber: "POL-1", ClaimedPaise: 100}, nil
}

// RecordInsufficientEvidenceTransition implements worker.ClaimLoader. A
// call here would mean the Tier-1 path reached the new-pipeline durable
// boundary, which it must not; tests assert the recording stays empty.
func (s *stubClaims) RecordInsufficientEvidenceTransition(_ context.Context, tenant, _ string, _ int, _ string) error {
	s.recTenants = append(s.recTenants, tenant)
	return nil
}

type stubPolicy struct{}

func (stubPolicy) CheckPolicy(_ context.Context, _, _ string) (worker.PolicyData, error) {
	return worker.PolicyData{Number: "POL-1", Active: true}, nil
}
