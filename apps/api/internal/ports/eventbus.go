// Package ports defines the outbound port boundaries for the ClaimOps
// application edge. This file adds the event-bus port used by document
// ingestion to emit domain events without coupling handlers to transport.
package ports

import (
	"context"
	"sync"
)

// TopicDocumentUploaded is emitted when a new document is ingested.
const TopicDocumentUploaded = "document.uploaded"

// DocumentUploadedSchemaVersion is the versioned-contract marker for the
// DocumentUploaded event payload (see documentUploaded in the handlers
// package). Convention: every event payload carries a `schema_version`
// string of the form `<event-name>.vN` (e.g. "document-uploaded.v1") so
// consumers can gate on breaking payload changes without inspecting the
// topic name.
const DocumentUploadedSchemaVersion = "document-uploaded.v1"

// EventBus publishes serialized domain events to a topic.
type EventBus interface {
	Publish(ctx context.Context, topic string, event []byte) error
}

// InMemoryBus is a minimal in-process EventBus for edge tests and local
// development. Events are appended per topic in publish order.
type InMemoryBus struct {
	mu     sync.Mutex
	Events map[string][][]byte
}

// NewInMemoryBus builds an InMemoryBus with an initialized topic map.
func NewInMemoryBus() *InMemoryBus {
	return &InMemoryBus{Events: make(map[string][][]byte)}
}

// Publish appends a copy of event to the topic log.
func (b *InMemoryBus) Publish(ctx context.Context, topic string, event []byte) error {
	_ = ctx
	cp := make([]byte, len(event))
	copy(cp, event)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.Events == nil {
		b.Events = make(map[string][][]byte)
	}
	b.Events[topic] = append(b.Events[topic], cp)
	return nil
}
