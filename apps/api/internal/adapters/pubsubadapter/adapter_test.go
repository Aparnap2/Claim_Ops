package pubsubadapter

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/pubsub"
	"cloud.google.com/go/pubsub/pstest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// newFakeClient spins up a pstest in-process fake and returns a client wired
// to it. LAYER 1: always runs, no network beyond localhost loopback.
func newFakeClient(t *testing.T) (*pubsub.Client, func()) {
	t.Helper()
	ctx := context.Background()
	srv := pstest.NewServer()
	conn, err := grpc.Dial(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		srv.Close()
		t.Fatalf("dial fake pubsub server: %v", err)
	}
	client, err := pubsub.NewClient(ctx, "test-project", option.WithGRPCConn(conn))
	if err != nil {
		conn.Close()
		srv.Close()
		t.Fatalf("new pubsub client: %v", err)
	}
	cleanup := func() {
		client.Close()
		conn.Close()
		srv.Close()
	}
	return client, cleanup
}

// mustTopicSub provisions a topic and a pull subscription against the given
// client (fake or emulator).
func mustTopicSub(t *testing.T, ctx context.Context, client *pubsub.Client, topicID, subID string) {
	t.Helper()
	topic, err := client.CreateTopic(ctx, topicID)
	if err != nil {
		t.Fatalf("create topic %q: %v", topicID, err)
	}
	_, err = client.CreateSubscription(ctx, subID, pubsub.SubscriptionConfig{Topic: topic})
	if err != nil {
		t.Fatalf("create subscription %q: %v", subID, err)
	}
}

// receiveUntilCancel runs ReceiveEvent until ctx ends (the handler cancels it
// after observing what it needs). A context-closed return is the expected
// shutdown signal, not a failure.
func receiveUntilCancel(sub *Subscriber, ctx context.Context, handle func(context.Context, []byte, map[string]string) error) error {
	err := sub.ReceiveEvent(ctx, handle)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// publishAndReceiveOne publishes a single message then receives exactly one
// delivery, returning what the handler observed.
func publishAndReceiveOne(t *testing.T, ctx context.Context, client *pubsub.Client, topicID, subID, eventType string, payload []byte, attrs map[string]string) (serverID string, gotPayload []byte, gotAttrs map[string]string) {
	t.Helper()
	pub := NewPublisher(client, topicID)
	serverID, err := pub.PublishEvent(ctx, eventType, payload, attrs)
	if err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}
	if serverID == "" {
		t.Fatal("PublishEvent returned empty server message ID")
	}

	sub := NewSubscriber(client, subID)
	recvCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = receiveUntilCancel(sub, recvCtx, func(_ context.Context, p []byte, a map[string]string) error {
		gotPayload = append([]byte(nil), p...)
		gotAttrs = a
		cancel()
		return nil
	})
	if err != nil {
		t.Fatalf("ReceiveEvent: %v", err)
	}
	if gotPayload == nil {
		t.Fatal("timed out waiting for message delivery")
	}
	return serverID, gotPayload, gotAttrs
}

// ---------------------------------------------------------------------------
// LAYER 1: in-process fake — always runs
// ---------------------------------------------------------------------------

func TestPublishReceiveRoundTrip(t *testing.T) {
	client, cleanup := newFakeClient(t)
	defer cleanup()
	ctx := context.Background()
	const topicID, subID = "events", "events-sub"
	mustTopicSub(t, ctx, client, topicID, subID)

	payload := []byte(`{"opaque":"bytes"}`)
	// event_type here is deliberately stale: the eventType argument must win.
	attrs := map[string]string{
		"event_id":   "evt-1",
		"tenant_id":  "tenant-a",
		"event_type": "stale-ignored",
	}

	_, gotPayload, gotAttrs := publishAndReceiveOne(t, ctx, client, topicID, subID, "test.event", payload, attrs)

	if string(gotPayload) != string(payload) {
		t.Fatalf("payload mismatch: got %q want %q", gotPayload, payload)
	}
	want := map[string]string{
		"event_id":   "evt-1",
		"tenant_id":  "tenant-a",
		"event_type": "test.event",
	}
	for k, v := range want {
		if gotAttrs[k] != v {
			t.Fatalf("attribute %q: got %q want %q (all: %v)", k, gotAttrs[k], v, gotAttrs)
		}
	}
	// The caller's map must be untouched (adapter copies before forcing).
	if attrs["event_type"] != "stale-ignored" {
		t.Fatalf("caller attrs map was mutated: %v", attrs)
	}
}

func TestNackRedelivers(t *testing.T) {
	client, cleanup := newFakeClient(t)
	defer cleanup()
	ctx := context.Background()
	const topicID, subID = "events-nack", "events-nack-sub"
	mustTopicSub(t, ctx, client, topicID, subID)

	pub := NewPublisher(client, topicID)
	payload := []byte(`{"nack":"me"}`)
	attrs := map[string]string{"event_id": "evt-nack", "tenant_id": "tenant-a"}
	if _, err := pub.PublishEvent(ctx, "test.nack", payload, attrs); err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}

	sub := NewSubscriber(client, subID)
	var attempts atomic.Int32

	// Delivery 1: fail the handler -> Nack, then stop receiving.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel1()
	if err := receiveUntilCancel(sub, ctx1, func(_ context.Context, p []byte, _ map[string]string) error {
		attempts.Add(1)
		defer cancel1()
		if string(p) != string(payload) {
			t.Errorf("delivery 1 payload: got %q want %q", p, payload)
		}
		return errors.New("boom")
	}); err != nil {
		t.Fatalf("ReceiveEvent (nack): %v", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("expected 1 delivery so far, got %d", attempts.Load())
	}

	// Delivery 2: the Nacked message must be redelivered -> Ack it.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel2()
	if err := receiveUntilCancel(sub, ctx2, func(_ context.Context, p []byte, _ map[string]string) error {
		attempts.Add(1)
		defer cancel2()
		if string(p) != string(payload) {
			t.Errorf("delivery 2 payload: got %q want %q", p, payload)
		}
		return nil
	}); err != nil {
		t.Fatalf("ReceiveEvent (ack): %v", err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("expected Nacked message to be redelivered (2 attempts), got %d", attempts.Load())
	}
}

func TestPublishEventMissingAttrs(t *testing.T) {
	client, cleanup := newFakeClient(t)
	defer cleanup()
	ctx := context.Background()
	pub := NewPublisher(client, "events-missing")

	full := map[string]string{"event_id": "evt-1", "tenant_id": "tenant-a"}
	cases := map[string]struct {
		eventType string
		attrs     map[string]string
	}{
		"empty event type":  {eventType: "", attrs: full},
		"missing event_id":  {eventType: "test.event", attrs: map[string]string{"tenant_id": "tenant-a"}},
		"missing tenant_id": {eventType: "test.event", attrs: map[string]string{"event_id": "evt-1"}},
		"empty event_id":    {eventType: "test.event", attrs: map[string]string{"event_id": "", "tenant_id": "tenant-a"}},
		"nil attrs":         {eventType: "test.event", attrs: nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := pub.PublishEvent(ctx, tc.eventType, []byte(`{}`), tc.attrs); err == nil {
				t.Fatal("expected fail-fast error, got nil")
			}
		})
	}
}

func TestNilGuards(t *testing.T) {
	ctx := context.Background()
	if _, err := NewPublisher(nil, "events").PublishEvent(ctx, "test.event", []byte(`{}`), map[string]string{"event_id": "e", "tenant_id": "t"}); err == nil {
		t.Fatal("expected nil-client publish error, got nil")
	}
	client, cleanup := newFakeClient(t)
	defer cleanup()
	if err := NewSubscriber(client, "no-such-sub").ReceiveEvent(ctx, nil); err == nil {
		t.Fatal("expected nil-handle receive error, got nil")
	}
	if err := NewSubscriber(nil, "no-such-sub").ReceiveEvent(ctx, func(context.Context, []byte, map[string]string) error {
		return nil
	}); err == nil {
		t.Fatal("expected nil-client receive error, got nil")
	}
}

// ---------------------------------------------------------------------------
// LAYER 2: real localgcp emulator — gated, skips when unavailable
// ---------------------------------------------------------------------------

func TestEmulatorPublishReceiveRoundTrip(t *testing.T) {
	host := os.Getenv("PUBSUB_EMULATOR_HOST")
	if host == "" {
		t.Skip("PUBSUB_EMULATOR_HOST unset: skipping emulator round-trip")
	}
	// Dial probe (2s): the emulator was probed Up on :8085, but gate anyway.
	probe, err := net.DialTimeout("tcp", host, 2*time.Second)
	if err != nil {
		t.Skipf("emulator at %q unreachable: %v", host, err)
	}
	probe.Close()

	ctx := context.Background()
	// pubsub.NewClient honors PUBSUB_EMULATOR_HOST (no credentials needed).
	client, err := pubsub.NewClient(ctx, "test-project")
	if err != nil {
		t.Fatalf("emulator client: %v", err)
	}
	defer client.Close()

	suffix := time.Now().UnixNano()
	topicID := fmt.Sprintf("adapter-test-topic-%d", suffix)
	subID := fmt.Sprintf("adapter-test-sub-%d", suffix)
	mustTopicSub(t, ctx, client, topicID, subID)

	payload := []byte(`{"via":"emulator"}`)
	attrs := map[string]string{"event_id": "evt-emu", "tenant_id": "tenant-emu"}
	_, gotPayload, gotAttrs := publishAndReceiveOne(t, ctx, client, topicID, subID, "test.emulator", payload, attrs)

	if string(gotPayload) != string(payload) {
		t.Fatalf("payload mismatch: got %q want %q", gotPayload, payload)
	}
	for k, v := range map[string]string{"event_id": "evt-emu", "tenant_id": "tenant-emu", "event_type": "test.emulator"} {
		if gotAttrs[k] != v {
			t.Fatalf("attribute %q: got %q want %q (all: %v)", k, gotAttrs[k], v, gotAttrs)
		}
	}
}
