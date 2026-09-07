package ports_test

import (
	"context"
	"errors"
	"testing"

	"claimops-api/internal/ports"
)

func TestSubscribeDispatchOrder(t *testing.T) {
	bus := ports.NewInMemoryBus()
	ctx := context.Background()
	var order []string
	bus.Subscribe("t", func(ctx context.Context, event []byte) error {
		order = append(order, "first:"+string(event))
		return nil
	})
	bus.Subscribe("t", func(ctx context.Context, event []byte) error {
		order = append(order, "second:"+string(event))
		return nil
	})
	if err := bus.Publish(ctx, "t", []byte("ping")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(order) != 2 || order[0] != "first:ping" || order[1] != "second:ping" {
		t.Fatalf("dispatch order = %v, want [first:ping second:ping]", order)
	}
	if len(bus.Events["t"]) != 1 {
		t.Fatalf("event log = %d entries, want 1", len(bus.Events["t"]))
	}
}

func TestUnsubscribe(t *testing.T) {
	bus := ports.NewInMemoryBus()
	ctx := context.Background()
	calls := 0
	unsub := bus.Subscribe("t", func(ctx context.Context, event []byte) error {
		calls++
		return nil
	})
	kept := 0
	bus.Subscribe("t", func(ctx context.Context, event []byte) error {
		kept++
		return nil
	})
	unsub()
	if err := bus.Publish(ctx, "t", []byte("x")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if calls != 0 {
		t.Fatalf("unsubscribed handler called %d times, want 0", calls)
	}
	if kept != 1 {
		t.Fatalf("remaining handler called %d times, want 1", kept)
	}
	// Double-unsubscribe is a no-op, never panics.
	unsub()
}

func TestPublishErrorAbort(t *testing.T) {
	bus := ports.NewInMemoryBus()
	ctx := context.Background()
	sentinel := errors.New("boom")
	var ran []string
	bus.Subscribe("t", func(ctx context.Context, event []byte) error {
		ran = append(ran, "first")
		return sentinel
	})
	bus.Subscribe("t", func(ctx context.Context, event []byte) error {
		ran = append(ran, "second")
		return nil
	})
	err := bus.Publish(ctx, "t", []byte("x"))
	if !errors.Is(err, sentinel) {
		t.Fatalf("Publish err = %v, want wrap of sentinel", err)
	}
	if len(ran) != 1 || ran[0] != "first" {
		t.Fatalf("dispatch ran %v, want [first] (abort on first error)", ran)
	}
	// The event stays in the log even when dispatch fails.
	if len(bus.Events["t"]) != 1 {
		t.Fatalf("event log = %d entries, want 1 despite handler error", len(bus.Events["t"]))
	}
}

func TestSubscribeOtherTopicUntouched(t *testing.T) {
	bus := ports.NewInMemoryBus()
	ctx := context.Background()
	calls := 0
	bus.Subscribe("a", func(ctx context.Context, event []byte) error {
		calls++
		return nil
	})
	if err := bus.Publish(ctx, "b", []byte("x")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if calls != 0 {
		t.Fatalf("handler for topic a fired on topic b publish")
	}
}
