package app

// APA-43, Phase-3 plan step 9 remainder, CASE 2: localgcp / PubSub-down
// transient — qualified against the REAL emulator and the REAL dispatcher.
//
// ALREADY PROVEN (not repeated here)
// ---------------------------------
//
//	worker restart / convergence ... internal/worker/restart_tenant_swap_test.go,
//	                              internal/worker/transient_redelivery_test.go
//	duplicate (with doubles) ...... this file's sibling, infra_matrix_live_test.go
//	                              (whose CASES 1a/1b use the live transport but a
//	                               dispatcher that never fails)
//	Poison policy .................. internal/outbox/dispatcher.go (documented
//	                              DECISION: maxAttempts -> next_attempt_at
//	                              = now()+24h, counted dead)
//
// WHAT ONLY A REAL OUTAGE SHOWS
// -----------------------------
//
// With a fake publisher the "transient" classification is whatever the
// test author typed. Here the publisher is the REAL Pub/Sub adapter
// against a REAL emulator that is genuinely not listening, so the failure
// is authored by the network stack, not by the test.
//
// The claims are all about DURABLE Postgres state, never about a returned
// boolean:
//
//	publishes nothing   RunOnce returns published=0 AND the real publisher
//	                    was actually invoked (attempt count) AND
//	                    published_at is still NULL
//	UNPOISONED          next_attempt_at is NOT the 24h poison quarantine
//	claimable           next_attempt_at becomes due on its own, with no
//	                    intervention, inside a bounded wait
//	recovers            after the transport returns, the SAME dispatcher
//	                    publishes, the row is marked published, and the REAL
//	                    subscriber receives the message
//
// MEASURED TOPOLOGY — THE PLAN'S PREMISE IS WRONG, AND SO IS THE BRIEF'S
// ---------------------------------------------------------------------
// docs/specs/10_PHASE3_E2E_QUALIFICATION.md:70 and the task brief both
// describe localgcp as ONE CONTAINER holding Pub/Sub :8085, Storage :4443
// and Cloud SQL. MEASURED on localgcp v0.6.0 (`localgcp up --help`):
// `--services` accepts only spanner, bigtable, cloudsql, memorystore and
// bigquery. Pub/Sub and Cloud Storage are emulated IN-PROCESS by the
// localgcp binary.
//
// So there is no container to `docker stop`: the outage is produced by
// stopping a PROCESS. The brief's "localgcp and PubSub are the SAME
// container, so this is one proof, not two" is right in substance (one
// outage covers both) but wrong in mechanism — there is no container, and
// the operator's stop/restart commands are process commands, which is
// why this test takes them as explicit configuration rather than
// hardcoding a `docker` call.
//
// EMULATOR ARTIFACT, RECORDED NOT WORKED AROUND
// ---------------------------------------------
// A localgcp process restart loses ALL emulator state, topics and
// subscriptions included, where real Pub/Sub would not. The test
// therefore re-provisions the topic after the restart and says so where
// it happens. The property under test — a failed publish leaves the row
// claimable and unpoisoned, and delivery resumes once the transport is
// back — is unaffected: it is a claim about Postgres and about the real
// dispatcher's policy, not about emulator durability.
//
// BLAST RADIUS AND WHY IT IS OPT-IN
// ---------------------------------
// Stopping localgcp interrupts a SHARED local development service, so
// this test refuses to run unless the operator names both the stop and
// the restart command (APA43_LOCALGCP_STOP / APA43_LOCALGCP_RESTART).
// No default is assumed, so the test can never kill a service it was not
// explicitly told it owns. The restart command is launched detached so it
// survives the test process.

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"claimops-api/internal/adapters/pubsubadapter"
)

// infraOutboxRow is the durable outbox state this case asserts on. Every
// field is read from Postgres, never inferred from a return value.
type infraOutboxRow struct {
	Published     bool
	NextAttemptAt time.Time
	LastError     string
}

// readInfraOutboxRow reads one event's durable dispatch state. The
// queries are fixed literals; the only variable input is a bound
// parameter.
func readInfraOutboxRow(t *testing.T, env *infraEnv, eventID string) infraOutboxRow {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var (
		out       infraOutboxRow
		published *time.Time
		next      *time.Time
		lastErr   *string
	)
	err := env.Admin.QueryRow(ctx,
		`SELECT published_at, next_attempt_at, last_error FROM outbox_events WHERE event_id = $1`,
		eventID).Scan(&published, &next, &lastErr)
	if err != nil {
		t.Fatalf("read durable outbox state for %s: %v", eventID, err)
	}
	if published != nil {
		out.Published = true
	}
	if next != nil {
		out.NextAttemptAt = *next
	}
	if lastErr != nil {
		out.LastError = *lastErr
	}
	return out
}

// infraPoisonQuarantineFloor is the boundary between "parked for a retry"
// and "poisoned". The dispatcher's deadQuarantine is 24h (dispatcher.go),
// so any next_attempt_at under an hour is unambiguously a retry backoff
// and not the poison path. One hour leaves a wide margin over the
// production retryBase (5s) while staying nowhere near 24h.
const infraPoisonQuarantineFloor = time.Hour

// infraRequireLocalGCPControl returns the operator-declared stop and
// restart commands, or skips. The test never invents them: stopping a
// shared local service is opt-in by construction.
func infraRequireLocalGCPControl(t *testing.T) (stop, restart string) {
	t.Helper()
	stop = strings.TrimSpace(os.Getenv("APA43_LOCALGCP_STOP"))
	restart = strings.TrimSpace(os.Getenv("APA43_LOCALGCP_RESTART"))
	switch {
	case stop == "" && restart == "":
		t.Skip("APA43_LOCALGCP_STOP / APA43_LOCALGCP_RESTART unset: this case must stop a SHARED local service and may only do so when the operator names the commands")
	case stop == "":
		t.Skip("APA43_LOCALGCP_STOP unset: refusing to stop localgcp without a declared stop command")
	case restart == "":
		t.Skip("APA43_LOCALGCP_RESTART unset: refusing to stop localgcp without a declared restart command, which would leave the operator's emulator down")
	}
	return stop, restart
}

// infraRunShell runs cmd through the shell and returns its combined
// output.
//
// The shell is an ACTUATOR, not an assertion: a stop or start command's
// exit status does not establish whether the emulator is down or up.
// Only the port probe does. So a non-zero exit here is reported and
// returned, never fatal, and the caller gates on infraWaitPortClosed /
// infraWaitPortOpen instead.
//
// That tolerance is load-bearing for the common `pkill -f <pattern>`
// stop command: pkill matches its own `sh -c` command line and signals
// itself, so it legitimately exits non-zero (or is killed) even though it
// did stop the emulator. A stop command that genuinely fails is still
// caught, by the port never closing.
func infraRunShell(t *testing.T, cmd string) error {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "sh", "-c", cmd).CombinedOutput() // #nosec G204 -- operator-declared command, documented above
	text := strings.TrimSpace(string(out))
	if err != nil {
		t.Logf("command %q exited %v (tolerated; the port probe is the assertion): %s", cmd, err, text)
		return err
	}
	if text != "" {
		t.Logf("command %q: %s", cmd, text)
	}
	return nil
}

// infraPubSubHost returns the Pub/Sub emulator host, required to be set
// because this case reconfigures the emulator lifecycle around it.
func infraPubSubHost(t *testing.T) string {
	t.Helper()
	host := strings.TrimSpace(os.Getenv("PUBSUB_EMULATOR_HOST"))
	if host == "" {
		t.Skip("PUBSUB_EMULATOR_HOST unset: this case needs to stop and restart the emulator behind it")
	}
	return host
}

// infraLaunchDetached starts the operator-declared restart command
// detached from this process, so the emulator outlives the test binary.
// Output is discarded because the emulators are quiet (-q) and a full
// pipe would eventually block them.
func infraLaunchDetached(t *testing.T, cmd string) error {
	t.Helper()
	// #nosec G204 -- operator-declared command (see infraRequireLocalGCPControl).
	// Launch is an actuator: a failure is tolerated here and gated by the
	// port probe that follows, for the same reason infraRunShell is.
	if err := exec.Command("sh", "-c", "setsid "+cmd+" >/dev/null 2>&1 &").Run(); err != nil {
		t.Logf("launch detached %q exited %v (tolerated; the port probe is the assertion)", cmd, err)
		return err
	}
	return nil
}

// isContextEnd reports whether err is the normal end of a bounded pull
// (the case cancels deliberately once it has what it needs), not a real
// subscriber failure.
func isContextEnd(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// infraPortAccepts reports whether hostPort currently accepts a TCP
// connection. It is the primitive both waits are built on, and the only
// thing that decides whether the emulator is up or down in this case.
func infraPortAccepts(hostPort string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", hostPort, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// infraWaitPortUp polls until hostPort accepts, reporting whether it came
// up within d. The non-fatal form exists for use inside t.Cleanup, where
// a Fatalf would Goexit and mask the run's real failure.
func infraWaitPortUp(hostPort string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if infraPortAccepts(hostPort, 500*time.Millisecond) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// infraWaitPortClosed waits until nothing accepts on hostPort, proving
// the emulator is really down rather than assumed down.
func infraWaitPortClosed(t *testing.T, hostPort string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !infraPortAccepts(hostPort, 300*time.Millisecond) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("port %s still accepting after %s: the outage was never established, so this run would prove nothing", hostPort, d)
}

// infraWaitPortOpen waits until the emulator accepts again, proving the
// transport is genuinely back before delivery is asserted.
func infraWaitPortOpen(t *testing.T, hostPort string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if infraPortAccepts(hostPort, 500*time.Millisecond) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("port %s never reopened within %s: the transport did not recover", hostPort, d)
}

// infraWaitClaimable waits for the row to become claimable on its own
// (next_attempt_at <= now()), which is the claim under test: the row
// heals into a retryable state with no intervention from this test.
func infraWaitClaimable(t *testing.T, env *infraEnv, eventID string, d time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	deadline := start.Add(d)
	var last time.Duration
	for time.Now().Before(deadline) {
		row := readInfraOutboxRow(t, env, eventID)
		if !row.Published && !row.NextAttemptAt.After(time.Now()) {
			return time.Since(start)
		}
		last = row.NextAttemptAt.Sub(time.Now())
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("row %s did not become claimable within %s (last next_attempt_at was %s in the future): a failed publish must leave the row retryable, not parked", eventID, d, last)
	return 0
}

// TestLiveInfraMatrix_PubSubDown_TransientRowStaysClaimable is CASE 2 of
// the infrastructure failure matrix, qualified live: with the REAL Pub/Sub
// emulator stopped, the REAL dispatcher must publish nothing, leave the
// row UNPUBLISHED, CLAIMABLE and UNPOISONED, and after the transport
// returns must publish it and deliver it through the REAL subscriber.
//
// The scope is TRANSPORT, not pipeline: this case proves an event survives
// a transport outage intact. What the consumer does with the delivered
// payload is CASE 1a's subject, so the recording handler below returns nil
// without invoking a processor. That is a deliberate scope boundary, not a
// stub: the production Subscriber, its streaming pull, the attribute
// triple and the Ack decision are all the real ones.
func TestLiveInfraMatrix_PubSubDown_TransientRowStaysClaimable(t *testing.T) {
	stopCmd, restartCmd := infraRequireLocalGCPControl(t)

	env := infraEnvFor(t, false, false)
	infraRequirePubSub(t, env)

	pubsubHost := infraPubSubHost(t)

	// Restore the emulator even if an assertion fails part-way: the
	// operator's shared local service must not be left down by a red run.
	// This cleanup reports with Errorf and never Fatalf: a Fatalf (Goexit)
	// inside t.Cleanup aborts the run in a way that hides the real failure.
	// It also never Skips, so infraPubSubHost is resolved BEFORE registration.
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			return
		}
		t.Logf("cleanup: restoring the emulator after the case")
		_ = infraLaunchDetached(t, restartCmd)
		// MEASURED: localgcp needs roughly 10s to bind :8085/:4443, so a
		// single probe reports a false "left down". Poll for the real bind
		// window before calling it a cleanup failure.
		if !infraWaitPortUp(pubsubHost, 90*time.Second) {
			t.Errorf("cleanup: %s did not come back up; the operator's localgcp is left DOWN by this run (restart command was %q)", pubsubHost, restartCmd)
			return
		}
		t.Logf("cleanup: %s is accepting again", pubsubHost)
	})

	id := infraMintIdentity(t, env, infraMockoonTenant)
	infraSeedClaim(t, env, id)
	infraUpload(t, env, id, "hospital_bill.pdf", "application/pdf", infraTier1PDF(t))
	infraTargetOutboxClaim(t, env, id.EventID)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	infraProvisionTopic(t, ctx, env.Pubsub, env.PubSubIDs.Topic, env.PubSubIDs.Early)

	// Production dispatcher over the real cross-tenant outbox store and the
	// real Pub/Sub publisher, with the real cmd/api policy (batch 1 so the
	// claim is unambiguous in a shared volume — see infraTargetOutboxClaim).
	publisher := &infraCountingPublish{inner: pubsubadapter.NewPublisher(env.Pubsub, env.PubSubIDs.Topic)}
	disp := infraNewDispatcher(env, publisher, 1, 5, 5*time.Second)

	// ---- ESTABLISH THE OUTAGE -------------------------------------------
	infraWaitPortOpen(t, pubsubHost, 30*time.Second)
	t.Logf("stopping the emulators: %s", stopCmd)
	// stopped is set BEFORE the command runs, so the restore-cleanup is
	// armed even if the stop aborts the case: an interrupted run must
	// never leave the operator's shared emulator down.
	stopped = true
	_ = infraRunShell(t, stopCmd)
	infraWaitPortClosed(t, pubsubHost, 30*time.Second)
	t.Logf("OUTAGE ESTABLISHED: %s refuses connections", pubsubHost)

	// ---- RUNONCE AGAINST A DEAD TRANSPORT -------------------------------
	// MEASURED, AND IT SHAPES EVERY ASSERTION BELOW: the real Pub/Sub
	// client does not fail fast on a dead emulator. PublishEvent ends in
	// result.Get(ctx) (pubsubadapter/adapter.go:85), which blocks for
	// confirmed receipt, and the client library retries the resulting
	// Unavailable until the CALLER's context ends. So RunOnce blocks; it
	// does not return published=0/dead=0 through the per-event
	// classification path. A 45s budget was not enough (the run returned
	// context.DeadlineExceeded, i.e. it was still retrying at 45s).
	//
	// DEFECT REPORTED, NOT FIXED (frozen policy in this slice): the
	// dispatcher applies no per-publish deadline (outbox/dispatcher.go:96
	// has none) and production calls it with the SERVER-LIFETIME context
	// (cmd/api/main.go:203, runDispatcher). A dead Pub/Sub therefore
	// blocks the whole dispatch loop for as long as the client keeps
	// retrying, instead of being classified transient. "PubSub
	// unavailable transient" degrades into a total dispatch stall. The
	// short bound used here is the TEST's, which is the only reason this
	// case terminates; the exact retry ceiling was not isolated, so no
	// stronger claim is made.
	downCtx, downCancel := context.WithTimeout(context.Background(), 45*time.Second)
	published, dead, err := disp.RunOnce(downCtx)
	downCancel()
	if published != 0 {
		t.Errorf("published = %d, want 0 with the transport down: nothing may reach a dead peer", published)
	}
	if dead != 0 {
		t.Errorf("dead = %d, want 0: one outage must not poison the row (dispatcher maxAttempts is %d)", dead, 5)
	}
	// The failure is surfaced, not swallowed. With no server-lifetime
	// budget here the run ends on the caller's deadline, so the error is
	// expected; the test asserts only that it is non-nil and that the
	// caller CAN observe a failure (i.e. the dispatcher does not report
	// a clean sweep while nothing was published).
	if err == nil {
		t.Errorf("RunOnce against a dead transport returned err=nil with published=%d: a failed publish must be observable, not reported as a clean sweep", published)
	}
	if publisher.calls.Load() != 1 {
		t.Errorf("real publish attempts = %d, want exactly 1: the dispatcher must have TRIED and failed, not skipped the row", publisher.calls.Load())
	}
	t.Logf("DOWN: RunOnce published=%d dead=%d attempts=%d err=%v", published, dead, publisher.calls.Load(), err)

	// ---- THE DURABLE CLAIMS (the actual requirements) ------------------
	// Everything here is read from Postgres, never from a return value.
	row := readInfraOutboxRow(t, env, id.EventID)
	if row.Published {
		t.Errorf("published_at is set with the transport down: the row must stay unpublished (this is the 'publishes nothing' claim, read from durable state)")
	}
	delay := row.NextAttemptAt.Sub(time.Now())
	if delay >= infraPoisonQuarantineFloor {
		t.Errorf("next_attempt_at is %s in the future (>= the %s poison boundary): a single outage parked the row instead of leaving it retryable",
			delay.Round(time.Second), infraPoisonQuarantineFloor)
	}
	if !row.NextAttemptAt.After(time.Now()) {
		t.Logf("row is claimable IMMEDIATELY (next_attempt_at %s is in the past): MarkFailed never ran because the run ended on the caller's deadline, so the row was left untouched rather than backoff-scheduled",
			row.NextAttemptAt.Format(time.RFC3339Nano))
	}
	t.Logf("DOWN row state: published_at IS NULL=%v next_attempt_at=%s (%.1fs ahead, poison boundary %s) last_error=%q",
		!row.Published, row.NextAttemptAt.Format(time.RFC3339Nano), delay.Seconds(), infraPoisonQuarantineFloor, row.LastError)

	// Claimable, proven by the row satisfying the REAL claim predicate
	// (published_at IS NULL AND next_attempt_at <= now()) with no
	// intervention from this test.
	waited := infraWaitClaimable(t, env, id.EventID, 90*time.Second)
	t.Logf("row is claimable under the production predicate after %s (no intervention)", waited.Round(time.Millisecond))

	// ---- RESTART THE TRANSPORT ------------------------------------------
	t.Logf("restarting the emulators: %s", restartCmd)
	_ = infraLaunchDetached(t, restartCmd)
	infraWaitPortOpen(t, pubsubHost, 90*time.Second)
	stopped = false
	t.Logf("TRANSPORT BACK: %s accepts connections again", pubsubHost)

	// EMULATOR ARTIFACT (documented in the file header): the process
	// restart dropped the topic and subscription, so they are
	// re-provisioned. Real Pub/Sub would have retained them; nothing about
	// the property under test depends on this.
	upCtx, upCancel := context.WithTimeout(context.Background(), 60*time.Second)
	infraProvisionTopic(t, upCtx, env.Pubsub, env.PubSubIDs.Topic, env.PubSubIDs.Early)
	upCancel()
	t.Logf("topic %s and subscription %s re-provisioned after the process restart (localgcp keeps emulator state in memory only)",
		env.PubSubIDs.Topic, env.PubSubIDs.Early)

	// ---- THE SAME DISPATCHER MUST NOW PUBLISH ---------------------------
	upCtx2, upCancel2 := context.WithTimeout(context.Background(), 90*time.Second)
	published2, dead2, err2 := disp.RunOnce(upCtx2)
	upCancel2()
	if err2 != nil {
		t.Fatalf("RunOnce after the transport recovered: %v", err2)
	}
	if published2 != 1 {
		t.Errorf("published after recovery = %d, want 1: the row survived the outage and must publish once the transport is back", published2)
	}
	if dead2 != 0 {
		t.Errorf("dead after recovery = %d, want 0", dead2)
	}
	row2 := readInfraOutboxRow(t, env, id.EventID)
	if !row2.Published {
		t.Errorf("published_at is still NULL after a successful publish: MarkPublished did not commit")
	}
	t.Logf("RECOVERED: published=%d dead=%d | row published=%v", published2, dead2, row2.Published)

	// ---- DELIVERY THROUGH THE REAL SUBSCRIBER ---------------------------
	// The production Subscriber and its streaming pull; the handler only
	// records, because this case qualifies transport survival, not the
	// pipeline (CASE 1a qualifies the pipeline).
	sub := pubsubadapter.NewSubscriber(env.Pubsub, env.PubSubIDs.Early)
	var (
		mu       sync.Mutex
		gotEvent []string
		gotBytes int
	)
	deliverCtx, deliverCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer deliverCancel()
	rerr := sub.ReceiveEvent(deliverCtx, func(hctx context.Context, payload []byte, attrs map[string]string) error {
		mu.Lock()
		gotEvent = append(gotEvent, attrs[pubsubadapter.AttrEventID])
		gotBytes += len(payload)
		n := len(gotEvent)
		mu.Unlock()
		if n >= 1 {
			deliverCancel()
		}
		return nil
	})
	if rerr != nil && !isContextEnd(rerr) {
		t.Fatalf("ReceiveEvent: %v", rerr)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gotEvent) == 0 {
		t.Fatal("no delivery observed after the transport recovered: the outage must not have silently dropped the event")
	}
	for _, ev := range gotEvent {
		if ev != id.EventID {
			t.Errorf("delivered event_id = %q, want the test-owned %q: the count must belong to THIS event", ev, id.EventID)
		}
	}
	if gotBytes == 0 {
		t.Errorf("delivered payload is empty: the real transport carried no bytes")
	}
	t.Logf("DELIVERED through the real subscriber after recovery: event_id=%v payload_bytes=%d", gotEvent, gotBytes)
}
