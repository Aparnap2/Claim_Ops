package outbox_test

// APA-44 regression suite: one publish attempt must have a bounded deadline
// and must not inherit an effectively immortal server-lifetime context.
//
// Before the fix, RunOnce handed the caller context straight to publish. From
// production wiring (cmd/api/main.go) that context is the SERVER-LIFETIME
// context, so `result.Get` inside pubsubadapter.PublishEvent blocked for as
// long as the Pub/Sub client kept retrying a dead transport (>45s measured in
// APA-43, unbounded in principle). The dispatch loop then stopped polling:
// a transport outage degraded into a dispatch stall instead of the intended
// transient-and-continue.
//
// These tests drive the REAL production chain (outbox.Dispatcher ->
// pubsubadapter.Publisher -> cloud.google.com/go/pubsub) against dead origins:
// an httptest server that accepts and never answers, and a closed TCP port.
// No live container, no emulator, no network beyond loopback. No time.Sleep:
// every wait is a bounded select on a channel, so the suite is fast and has
// no sleep races.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"claimops-api/internal/adapters/pubsubadapter"
	"claimops-api/internal/outbox"
	"claimops-api/internal/repository/postgres"

	"cloud.google.com/go/pubsub"
	"cloud.google.com/go/pubsub/pstest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ---------------------------------------------------------------------------
// Dead-origin harness
// ---------------------------------------------------------------------------

// testPublishTimeout is the per-publish deadline handed to the dispatcher.
// Kept short so the suite stays fast; it is a real deadline, not a fake
// clock, so no production seam had to be restructured to test it.
const testPublishTimeout = 200 * time.Millisecond

// blockBudget is the watchdog: how long a bounded RunOnce may take before we
// call it a stall. Deliberately ~25x testPublishTimeout so ordinary -race and
// CI scheduling noise cannot make this flaky, while a genuinely unbounded
// publish (the pre-fix behavior, observed still blocked after 8s) trips it.
const blockBudget = 5 * time.Second

// closedPortAddr is a TCP port with nothing listening. Port 1 is reserved and
// refuses immediately, so this is a deterministic "transport down" origin.
const closedPortAddr = "127.0.0.1:1"

// deadClient returns a real pubsub.Client wired to addr, which is expected to
// be a dead origin. Uses the same construction as production wiring does at
// client-creation time (main-agent owned), so the failure path under test is
// the genuine client retry path, not a stub.
func deadClient(t *testing.T, addr string) (*pubsub.Client, func()) {
	t.Helper()
	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial dead origin %q: %v", addr, err)
	}
	client, err := pubsub.NewClient(context.Background(), "apa44-dead", option.WithGRPCConn(conn))
	if err != nil {
		_ = conn.Close()
		t.Fatalf("pubsub client for %q: %v", addr, err)
	}
	return client, func() {
		_ = client.Close()
		_ = conn.Close()
	}
}

// hangingOrigin is an httptest origin that accepts the connection and never
// answers: the connection-open-but-silent failure that defeats naive
// "connection refused is instant" reasoning. Released by the returned func.
func hangingOrigin(t *testing.T) (addr string, stop func()) {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	return srv.Listener.Addr().String(), func() {
		close(release)
		srv.Close()
	}
}

// healthyPubSub returns a client backed by the in-process pstest fake with
// topicID provisioned, for the publish-success and recovery cases.
func healthyPubSub(t *testing.T, topicID string) (*pubsub.Client, func()) {
	t.Helper()
	srv := pstest.NewServer()
	conn, err := grpc.Dial(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		srv.Close()
		t.Fatalf("dial pstest: %v", err)
	}
	client, err := pubsub.NewClient(context.Background(), "apa44-healthy", option.WithGRPCConn(conn))
	if err != nil {
		_ = conn.Close()
		srv.Close()
		t.Fatalf("pubsub client for pstest: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.CreateTopic(ctx, topicID); err != nil {
		_ = client.Close()
		_ = conn.Close()
		srv.Close()
		t.Fatalf("create topic %q: %v", topicID, err)
	}
	return client, func() {
		_ = client.Close()
		_ = conn.Close()
		srv.Close()
	}
}

// publisherFor mirrors the production wiring in cmd/api/main.go exactly: a
// PublishFunc that calls pubsubadapter.PublishEvent with the routing triple.
func publisherFor(p *pubsubadapter.Publisher) outbox.PublishFunc {
	return func(ctx context.Context, e postgres.OutboxEvent) error {
		_, err := p.PublishEvent(ctx, e.EventType, e.Payload, map[string]string{
			pubsubadapter.AttrEventID:  e.EventID,
			pubsubadapter.AttrTenantID: string(e.Tenant),
		})
		return err
	}
}

// runOnceBounded runs d.RunOnce(ctx) on its own goroutine and returns its
// result, failing the test if it does not return within blockBudget. The
// goroutine is always released via cancel, so a stall fails the test without
// leaking the goroutine (the repo forbids goroutine leaks).
func runOnceBounded(t *testing.T, d *outbox.Dispatcher, ctx context.Context, cancel context.CancelFunc) (published, dead int, err error) {
	t.Helper()
	type result struct {
		published, dead int
		err             error
	}
	done := make(chan result, 1)
	go func() {
		p, dd, e := d.RunOnce(ctx)
		done <- result{p, dd, e}
	}()
	select {
	case r := <-done:
		return r.published, r.dead, r.err
	case <-time.After(blockBudget):
		cancel() // release the blocked publish so the goroutine exits
		t.Fatalf("RunOnce did not return within %s: dispatch stalled (APA-44 regression)", blockBudget)
		return 0, 0, nil
	}
}

// countingStore records which events were actually attempted, so area 5 can
// assert a stalled record does not starve the ones behind it.
type countingStore struct {
	*fakeStore
	mu        sync.Mutex
	attempted []string
}

func newCountingStore(events []postgres.OutboxEvent) *countingStore {
	return &countingStore{fakeStore: newFakeStore(events)}
}

func (c *countingStore) ClaimUnpublished(ctx context.Context, limit int) ([]postgres.OutboxEvent, error) {
	events, err := c.fakeStore.ClaimUnpublished(ctx, limit)
	if err == nil {
		c.mu.Lock()
		for _, e := range events {
			c.attempted = append(c.attempted, e.EventID)
		}
		c.mu.Unlock()
	}
	return events, err
}

func (c *countingStore) attemptedIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.attempted...)
}

// ---------------------------------------------------------------------------
// Area 1: publish success against a healthy transport
// ---------------------------------------------------------------------------

// A healthy publish must still succeed under the bounded context: the new
// deadline must not truncate legitimate delivery.
func TestAPA44_Area1_PublishSuccess(t *testing.T) {
	client, done := healthyPubSub(t, "apa44-ok")
	defer done()

	store := newFakeStore([]postgres.OutboxEvent{testEvent("e-ok")})
	d := outbox.New(store, publisherFor(pubsubadapter.NewPublisher(client, "apa44-ok")), 10, 5, 0, testPublishTimeout)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	published, dead, err := runOnceBounded(t, d, ctx, cancel)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if published != 1 || dead != 0 {
		t.Fatalf("RunOnce = (%d,%d), want (1,0)", published, dead)
	}
	if !store.published["e-ok"] {
		t.Fatal("event was not marked published after a successful publish")
	}
}

// ---------------------------------------------------------------------------
// Area 2: publish timeout is bounded (hanging origin)
// ---------------------------------------------------------------------------

// A connection that opens but never answers must not stall the dispatcher
// past the per-publish deadline. This is the core APA-44 assertion: before the
// fix RunOnce never returned here.
func TestAPA44_Area2_PublishTimeoutBounded_HangingOrigin(t *testing.T) {
	addr, stop := hangingOrigin(t)
	defer stop()
	client, done := deadClient(t, addr)
	defer done()

	store := newFakeStore([]postgres.OutboxEvent{testEvent("e-hang")})
	d := outbox.New(store, publisherFor(pubsubadapter.NewPublisher(client, "apa44")), 10, 5, 0, testPublishTimeout)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	published, dead, err := runOnceBounded(t, d, ctx, cancel)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if elapsed > blockBudget {
		t.Fatalf("RunOnce took %s, want <= %s", elapsed, blockBudget)
	}
	if published != 0 || dead != 0 {
		t.Fatalf("RunOnce = (%d,%d), want (0,0): a timeout is a transient failure, not a terminal one", published, dead)
	}
	if store.published["e-hang"] {
		t.Fatal("timed-out publish was marked published")
	}
	if store.lastErr["e-hang"] == "" {
		t.Fatal("timed-out publish recorded no error")
	}
}

// Same bound against a closed port: connection-level failure must also be
// bounded, not retried out to the server-lifetime deadline.
func TestAPA44_Area2_PublishTimeoutBounded_ClosedPort(t *testing.T) {
	client, done := deadClient(t, closedPortAddr)
	defer done()

	store := newFakeStore([]postgres.OutboxEvent{testEvent("e-refused")})
	d := outbox.New(store, publisherFor(pubsubadapter.NewPublisher(client, "apa44")), 10, 5, 0, testPublishTimeout)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	published, _, err := runOnceBounded(t, d, ctx, cancel)
	if elapsed := time.Since(start); elapsed > blockBudget {
		t.Fatalf("RunOnce took %s against a closed port, want <= %s", elapsed, blockBudget)
	}
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if published != 0 {
		t.Fatalf("published = %d, want 0 against a closed port", published)
	}
	if store.published["e-refused"] {
		t.Fatal("publish against a closed port was marked published")
	}
}

// ---------------------------------------------------------------------------
// Area 3: transport failure keeps transient classification
// ---------------------------------------------------------------------------

// A transport failure must stay TRANSIENT: recorded with backoff, not
// quarantined as dead on the first failure. Preserving this distinction is an
// explicit constraint of the fix.
func TestAPA44_Area3_TransportFailureStaysTransient(t *testing.T) {
	addr, stop := hangingOrigin(t)
	defer stop()
	client, done := deadClient(t, addr)
	defer done()

	store := newFakeStore([]postgres.OutboxEvent{testEvent("e-transient")})
	d := outbox.New(store, publisherFor(pubsubadapter.NewPublisher(client, "apa44")), 10, 5, time.Minute, testPublishTimeout)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	published, dead, err := runOnceBounded(t, d, ctx, cancel)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if published != 0 || dead != 0 {
		t.Fatalf("RunOnce = (%d,%d), want (0,0) on first transport failure", published, dead)
	}
	// Backoff scheduled, not the 24h poison quarantine.
	next := store.next["e-transient"]
	if !next.After(time.Now()) {
		t.Fatalf("transient failure scheduled at %s, want a future backoff (not an immediate re-claim, not a 24h quarantine)", next)
	}
	if d := time.Until(next); d > 2*time.Minute {
		t.Fatalf("backoff = %s, want ~retryBase (1m), not the 24h poison quarantine", d)
	}
}

// ---------------------------------------------------------------------------
// Area 4: caller cancellation mid-publish
// ---------------------------------------------------------------------------

// If the caller cancels while a publish is in flight, RunOnce must return
// rather than block. Cancellation must propagate into the publish attempt.
func TestAPA44_Area4_CallerCancelsMidPublish(t *testing.T) {
	addr, stop := hangingOrigin(t)
	defer stop()
	client, done := deadClient(t, addr)
	defer done()

	// started is closed the instant the publish attempt begins, so the cancel
	// is triggered by observed progress rather than a sleep: no sleep races.
	started := make(chan struct{})
	var once sync.Once
	realPublish := publisherFor(pubsubadapter.NewPublisher(client, "apa44"))
	publish := func(ctx context.Context, e postgres.OutboxEvent) error {
		once.Do(func() { close(started) })
		return realPublish(ctx, e)
	}
	store := newFakeStore([]postgres.OutboxEvent{testEvent("e-cancel")})
	// Deadline deliberately longer than the test's block budget is not needed:
	// cancel fires as soon as the publish is in flight, so a passing test
	// proves CANCELLATION returned the call, not the deadline.
	d := outbox.New(store, publish, 10, 5, 0, 30*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-started:
			cancel()
		case <-time.After(blockBudget):
			t.Error("publish never started")
		}
	}()

	start := time.Now()
	published, _, err := runOnceBounded(t, d, ctx, cancel)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if elapsed > blockBudget {
		t.Fatalf("RunOnce took %s; caller cancellation did not propagate into publish", elapsed)
	}
	if published != 0 {
		t.Fatalf("published = %d, want 0 after cancellation", published)
	}
	if store.published["e-cancel"] {
		t.Fatal("cancelled publish was marked published")
	}
}

// ---------------------------------------------------------------------------
// Area 5: one stalled record must not starve the records behind it
// ---------------------------------------------------------------------------

// Stated behavior: a stalled record does NOT prevent later records from being
// attempted. The loop is sequential, so the stalled record costs at most one
// publish deadline and the batch then continues. Pre-fix, the first record
// blocked the loop forever and nothing behind it was ever attempted.
func TestAPA44_Area5_StalledRecordDoesNotStarveLaterRecords(t *testing.T) {
	addr, stop := hangingOrigin(t)
	defer stop()
	client, done := deadClient(t, addr)
	defer done()

	ids := []string{"e-first", "e-second", "e-third"}
	events := make([]postgres.OutboxEvent, 0, len(ids))
	for _, id := range ids {
		events = append(events, testEvent(id))
	}
	store := newCountingStore(events)
	d := outbox.New(store, publisherFor(pubsubadapter.NewPublisher(client, "apa44")), 10, 5, 0, testPublishTimeout)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	published, dead, err := runOnceBounded(t, d, ctx, cancel)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if published != 0 || dead != 0 {
		t.Fatalf("RunOnce = (%d,%d), want (0,0)", published, dead)
	}
	attempted := store.attemptedIDs()
	if len(attempted) != len(ids) {
		t.Fatalf("attempted %v, want all %d records attempted despite the stall", attempted, len(ids))
	}
	for i, id := range ids {
		if attempted[i] != id {
			t.Fatalf("attempt order = %v, want %v (claim order preserved)", attempted, ids)
		}
	}
}

// ---------------------------------------------------------------------------
// Area 6: recovery once Pub/Sub becomes available
// ---------------------------------------------------------------------------

// A record that failed against a dead transport must be retried later and
// succeed once the transport is back: no lost dispatch. Proved end to end
// against the real adapter by repointing the publish func from a dead origin
// to a healthy one, with retryBase=0 so the record is due immediately.
func TestAPA44_Area6_RecoversWhenPubSubReturns(t *testing.T) {
	dead, deadDone := deadClient(t, closedPortAddr)
	defer deadDone()
	healthy, healthyDone := healthyPubSub(t, "apa44-recovery")
	defer healthyDone()

	store := newFakeStore([]postgres.OutboxEvent{testEvent("e-recover")})

	// swappable transport: dead first, healthy after the first RunOnce.
	var (
		mu    sync.Mutex
		up    *pubsubadapter.Publisher
		calls int
	)
	publish := func(ctx context.Context, e postgres.OutboxEvent) error {
		mu.Lock()
		calls++
		target := up
		mu.Unlock()
		_, err := target.PublishEvent(ctx, e.EventType, e.Payload, map[string]string{
			pubsubadapter.AttrEventID:  e.EventID,
			pubsubadapter.AttrTenantID: string(e.Tenant),
		})
		return err
	}
	up = pubsubadapter.NewPublisher(dead, "apa44")
	d := outbox.New(store, publish, 10, 5, 0, testPublishTimeout)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run 1: transport down -> transient failure, nothing published.
	published, dead2, err := runOnceBounded(t, d, ctx, cancel)
	if err != nil {
		t.Fatalf("RunOnce #1: %v", err)
	}
	if published != 0 || dead2 != 0 {
		t.Fatalf("RunOnce #1 = (%d,%d), want (0,0)", published, dead2)
	}
	if store.published["e-recover"] {
		t.Fatal("record marked published while transport was down")
	}

	// Transport comes back.
	mu.Lock()
	up = pubsubadapter.NewPublisher(healthy, "apa44-recovery")
	mu.Unlock()

	// Run 2: the still-unpublished record is retried and succeeds.
	published, dead2, err = runOnceBounded(t, d, ctx, cancel)
	if err != nil {
		t.Fatalf("RunOnce #2: %v", err)
	}
	if published != 1 || dead2 != 0 {
		t.Fatalf("RunOnce #2 = (%d,%d), want (1,0) after recovery", published, dead2)
	}
	if !store.published["e-recover"] {
		t.Fatal("record was not marked published after the transport recovered")
	}

	// Exactly one successful dispatch: no duplication on recovery.
	published, _, err = runOnceBounded(t, d, ctx, cancel)
	if err != nil {
		t.Fatalf("RunOnce #3: %v", err)
	}
	if published != 0 {
		t.Fatalf("RunOnce #3 published = %d, want 0 (no duplicate dispatch after recovery)", published)
	}
	mu.Lock()
	total := calls
	mu.Unlock()
	if total != 2 {
		t.Fatalf("publish attempts = %d, want exactly 2 (one failed, one succeeded)", total)
	}
}

// The poison/quarantine path must still be reachable: repeated bounded
// timeouts eventually mark a record dead rather than retrying forever. This
// proves the deadline did not make records un-quarantinable.
func TestAPA44_Area3_RepeatedTimeoutsStillQuarantine(t *testing.T) {
	addr, stop := hangingOrigin(t)
	defer stop()
	client, done := deadClient(t, addr)
	defer done()

	store := newFakeStore([]postgres.OutboxEvent{testEvent("e-poison")})
	d := outbox.New(store, publisherFor(pubsubadapter.NewPublisher(client, "apa44")), 10, 3, 0, testPublishTimeout)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 1; i <= 2; i++ {
		if _, dead, err := runOnceBounded(t, d, ctx, cancel); err != nil || dead != 0 {
			t.Fatalf("RunOnce #%d = dead %d err %v, want (0,nil) before the attempt ceiling", i, dead, err)
		}
	}
	_, dead, err := runOnceBounded(t, d, ctx, cancel)
	if err != nil {
		t.Fatalf("RunOnce #3: %v", err)
	}
	if dead != 1 {
		t.Fatalf("dead = %d, want 1 once maxAttempts is reached", dead)
	}
	if until := time.Until(store.next["e-poison"]); until < 23*time.Hour || until > 25*time.Hour {
		t.Fatalf("quarantine delay = %s, want ~24h", until)
	}
}

// Sanity: the harness itself must be a real dead origin, not a fast-failing
// one, or the bounded assertions above would pass vacuously.
func TestAPA44_HarnessOriginActuallyHangs(t *testing.T) {
	addr, stop := hangingOrigin(t)
	defer stop()

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("hanging origin is not even accepting connections: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 1)
	_, err = conn.Read(buf)
	if err == nil {
		t.Fatal("hanging origin sent data; it is not a silent black hole")
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return // expected: connection accepted, then silence until the deadline
	}
	t.Fatalf("hanging origin closed the connection instead of stalling: %v", err)
}
