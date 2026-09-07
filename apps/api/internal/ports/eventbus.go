// Package ports defines the outbound port boundaries for the ClaimOps
// application edge. This file adds the event-bus port used by document
// ingestion to emit domain events without coupling handlers to transport.
package ports

import (
	"context"
	"fmt"
	"sync"
)

// TopicDocumentIngested is emitted when a new document is ingested
// (PRD §10 Stage 2, §34 Event Model: `document.ingested`).
const TopicDocumentIngested = "document.ingested"

// DocumentIngestedSchemaVersion is the versioned-contract marker for the
// DocumentIngested event payload. Convention: every event payload carries
// a `schema_version` string of the form `<event-name>.vN`
// (e.g. "document-ingested.v1") so consumers can gate on breaking payload
// changes without inspecting the topic name.
// EventEnvelope is the canonical envelope every domain event payload
// carries, alongside its event-specific fields:
//
//	schema_version  "<event-name>.vN" — gate on breaking payload changes
//	                without inspecting the topic. Additive fields do NOT
//	                bump the version.
//	event_id        unique per emission; consumers key idempotency on
//	                (tenant, claim, event_id).
//	occurred_at     RFC3339 UTC emission time.
//
// Example: document.ingested carries schema_version "document-ingested.v1".
const DocumentIngestedSchemaVersion = "document-ingested.v1"

// EventBus publishes serialized domain events to a topic.
type EventBus interface {
	Publish(ctx context.Context, topic string, event []byte) error
}

// Subscriber receives serialized domain events from a topic.
//
// Subscribe registers h for topic in registration order and returns an
// unsubscribe closure that removes exactly that registration. Handlers
// must not retain or mutate the event slice.
type Subscriber interface {
	Subscribe(topic string, h func(ctx context.Context, event []byte) error) (unsubscribe func())
}

// subscriberEntry pairs a handler with a registration ID so unsubscribe
// removes exactly one registration (funcs are not comparable).
type subscriberEntry struct {
	id uint64
	h  func(ctx context.Context, event []byte) error
}

// InMemoryBus is a minimal in-process EventBus for edge tests and local
// development. Events are appended per topic in publish order.
type InMemoryBus struct {
	mu     sync.Mutex
	Events map[string][][]byte
	subs   map[string][]subscriberEntry
	nextID uint64
}

// NewInMemoryBus builds an InMemoryBus with an initialized topic map.
func NewInMemoryBus() *InMemoryBus {
	return &InMemoryBus{Events: make(map[string][][]byte)}
}

// compile-time checks: InMemoryBus is both a publisher and a subscriber.
var (
	_ EventBus   = (*InMemoryBus)(nil)
	_ Subscriber = (*InMemoryBus)(nil)
)

// Subscribe registers h for topic and returns an unsubscribe closure
// removing exactly that registration.
func (b *InMemoryBus) Subscribe(topic string, h func(ctx context.Context, event []byte) error) (unsubscribe func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs == nil {
		b.subs = make(map[string][]subscriberEntry)
	}
	b.nextID++
	id := b.nextID
	b.subs[topic] = append(b.subs[topic], subscriberEntry{id: id, h: h})
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		entries := b.subs[topic]
		for i, e := range entries {
			if e.id == id {
				b.subs[topic] = append(entries[:i], entries[i+1:]...)
				return
			}
		}
	}
}

// Publish appends a copy of event to the topic log, then dispatches a
// per-handler copy to subscribers of topic synchronously in registration
// order. The first handler error aborts dispatch and is returned wrapped
// with the topic; the event stays in the log regardless.
func (b *InMemoryBus) Publish(ctx context.Context, topic string, event []byte) error {
	_ = ctx
	cp := make([]byte, len(event))
	copy(cp, event)
	b.mu.Lock()
	if b.Events == nil {
		b.Events = make(map[string][][]byte)
	}
	b.Events[topic] = append(b.Events[topic], cp)
	handlers := make([]func(ctx context.Context, event []byte) error, 0, len(b.subs[topic]))
	for _, e := range b.subs[topic] {
		handlers = append(handlers, e.h)
	}
	b.mu.Unlock()
	for _, h := range handlers {
		hcp := make([]byte, len(cp))
		copy(hcp, cp)
		if err := h(ctx, hcp); err != nil {
			return fmt.Errorf("ports: subscriber for topic %q: %w", topic, err)
		}
	}
	return nil
}
