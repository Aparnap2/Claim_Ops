// Package pubsubadapter is the production Google Cloud Pub/Sub
// implementation of the ports.EventBus publish + Subscriber contracts.
//
// Transport note: messages are opaque. The adapter moves payload bytes plus
// string attributes and never inspects, parses, or classifies content.
//
// Client construction (pubsub.NewClient with project ID) is main-agent wiring
// scope, not this package's: PUBSUB_EMULATOR_HOST is honored by the client
// library itself, so no custom endpoint or auth code lives here.
package pubsubadapter

import (
	"context"
	"fmt"

	"cloud.google.com/go/pubsub"
)

// Forced attribute keys. Every published message carries all three so the
// outbox dispatcher and downstream consumers can attribute, route, and
// deduplicate without inspecting the opaque payload.
const (
	AttrEventID   = "event_id"
	AttrEventType = "event_type"
	AttrTenantID  = "tenant_id"
)

// Publisher publishes opaque events to one preconfigured topic.
type Publisher struct {
	client  *pubsub.Client
	topicID string
}

// NewPublisher returns a Publisher bound to topicID. The client is owned by
// the caller (main wiring); topic provisioning is a deploy/wiring concern.
func NewPublisher(client *pubsub.Client, topicID string) *Publisher {
	return &Publisher{client: client, topicID: topicID}
}

// PublishEvent publishes payload to the configured topic with attributes
// forced to include event_id, event_type, and tenant_id. event_id and
// tenant_id must be present in attrs; event_type is taken from the eventType
// argument and forced into the outgoing attributes (overriding any
// caller-supplied value so wire state can never disagree with the call).
// Any missing value is a fail-fast error: unattributable messages are never
// published.
//
// Publish is async-batched by the client library; result.Get(ctx) blocks
// until the server confirms receipt, making delivery synchronous. The outbox
// dispatcher relies on this: a message is marked published only after
// confirmed publish, which is what keeps at-least-once redelivery safe.
//
// It returns the server-assigned message ID.
func (p *Publisher) PublishEvent(ctx context.Context, eventType string, payload []byte, attrs map[string]string) (string, error) {
	if p.client == nil {
		return "", fmt.Errorf("pubsubadapter: publisher has nil client")
	}
	if eventType == "" {
		return "", fmt.Errorf("pubsubadapter: publish requires event type (%q attribute)", AttrEventType)
	}
	eventID := attrs[AttrEventID]
	if eventID == "" {
		return "", fmt.Errorf("pubsubadapter: publish requires %q attribute", AttrEventID)
	}
	tenantID := attrs[AttrTenantID]
	if tenantID == "" {
		return "", fmt.Errorf("pubsubadapter: publish requires %q attribute", AttrTenantID)
	}

	// Copy: never mutate the caller's map, and force the routing triple.
	out := make(map[string]string, len(attrs)+1)
	for k, v := range attrs {
		out[k] = v
	}
	out[AttrEventID] = eventID
	out[AttrEventType] = eventType
	out[AttrTenantID] = tenantID

	data := make([]byte, len(payload))
	copy(data, payload)

	result := p.client.Topic(p.topicID).Publish(ctx, &pubsub.Message{
		Data:       data,
		Attributes: out,
	})
	// Synchronous confirm: see the doc comment above.
	serverID, err := result.Get(ctx)
	if err != nil {
		return "", fmt.Errorf("pubsubadapter: publish event %q (id %q): %w", eventType, eventID, err)
	}
	return serverID, nil
}

// Subscriber receives opaque events from one preconfigured subscription.
type Subscriber struct {
	client         *pubsub.Client
	subscriptionID string
}

// NewSubscriber returns a Subscriber bound to subscriptionID. The client is
// owned by the caller (main wiring).
func NewSubscriber(client *pubsub.Client, subscriptionID string) *Subscriber {
	return &Subscriber{client: client, subscriptionID: subscriptionID}
}

// ReceiveEvent blocks receiving from the configured subscription until ctx is
// done, dispatching each message to handle. A nil handle error acknowledges
// the message (Ack); a non-nil error negative-acknowledges it (Nack) for
// redelivery. Payload bytes and attributes are handed to handle as-is;
// handle must not retain or mutate them.
func (s *Subscriber) ReceiveEvent(ctx context.Context, handle func(context.Context, []byte, map[string]string) error) error {
	if s.client == nil {
		return fmt.Errorf("pubsubadapter: subscriber has nil client")
	}
	if handle == nil {
		return fmt.Errorf("pubsubadapter: receive requires non-nil handle")
	}
	sub := s.client.Subscription(s.subscriptionID)
	// Upstream guidance: a single streaming-pull stream keeps ordering and
	// resource use predictable for the outbox consumer.
	sub.ReceiveSettings.NumGoroutines = 1
	return sub.Receive(ctx, func(ctx context.Context, msg *pubsub.Message) {
		if err := handle(ctx, msg.Data, msg.Attributes); err != nil {
			msg.Nack()
			return
		}
		msg.Ack()
	})
}
