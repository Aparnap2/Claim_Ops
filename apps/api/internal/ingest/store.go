// Package ingest owns idempotent document intake for the edge.
//
// Contract against sibling package documents (apps/api/internal/documents):
//   - type Document struct{ ID, Tenant, ClaimID string; Type DocType;
//     FileName, MIME, SHA256 string; SizeBytes int64; Status string }
//   - func Classify(fileName, mime string) DocType
//   - Status "RECEIVED" is an untyped string constant so it assigns whether
//     Status is a plain string or a named string type.
package ingest

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"

	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
)

// ErrEmptyDocument is returned when content is empty.
var ErrEmptyDocument = errors.New("ingest: empty document content")

// Store is an in-memory idempotent document store keyed by the composite
// tenant|claim|sha256 string key. Re-putting identical content for the same
// tenant+claim returns the original document.
type Store struct {
	mu   sync.Mutex
	docs map[string]documents.Document
}

// New builds an empty Store.
func New() *Store {
	return &Store{docs: make(map[string]documents.Document)}
}

// key builds the composite dedupe key. Callers pass trimmed values.
func key(tenant, claimID, sha string) string {
	return tenant + "|" + claimID + "|" + sha
}

// Put stores content idempotently. It returns (doc, created, err) where
// created is false when identical content was already stored for the same
// tenant+claim (duplicate). New documents get ID "doc-"+12 hex chars,
// Type from documents.Classify, and Status "RECEIVED".
func (s *Store) Put(tenant, claimID, fileName, mime, content string) (documents.Document, bool, error) {
	t := strings.TrimSpace(tenant)
	if t == "" {
		return documents.Document{}, false, errors.New("ingest: blank tenant")
	}
	c := strings.TrimSpace(claimID)
	if c == "" {
		return documents.Document{}, false, errors.New("ingest: blank claim id")
	}
	f := strings.TrimSpace(fileName)
	if f == "" {
		return documents.Document{}, false, errors.New("ingest: blank file name")
	}
	if content == "" {
		return documents.Document{}, false, ErrEmptyDocument
	}
	m := strings.TrimSpace(mime)

	sum := sha256.Sum256([]byte(content))
	sha := hex.EncodeToString(sum[:])
	k := key(t, c, sha)

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.docs[k]; ok {
		return existing, false, nil
	}
	docType, _ := documents.Classify(f, m)
	doc := documents.Document{
		ID:        newID(sha),
		Tenant:    claims.TenantID(t),
		ClaimID:   claims.ClaimID(c),
		Type:      docType,
		FileName:  f,
		MIME:      m,
		SHA256:    sha,
		SizeBytes: int64(len(content)),
		Status:    documents.StReceived,
	}
	if s.docs == nil {
		s.docs = make(map[string]documents.Document)
	}
	s.docs[k] = doc
	return doc, true, nil
}

// newID returns "doc-" + 12 hex chars (6 random bytes), falling back to the
// content-hash prefix when entropy fails so IDs stay non-blank.
func newID(sha string) string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		if len(sha) >= 12 {
			return "doc-" + sha[:12]
		}
		return "doc-" + sha
	}
	return "doc-" + hex.EncodeToString(b[:])
}
