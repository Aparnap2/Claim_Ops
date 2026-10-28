package postgres_test

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestIntegrationSentinelPostgresRoundTrip is the strict-mode sentinel for
// `make integration`.
//
// Why it exists: every other Postgres-backed test skips when the database is
// unreachable (requirePool), which is correct for unit CI (`make test` runs
// with no services) but is exactly the false-green shape APA-83 closed: a
// reachable-but-unusable Postgres — wrong password, wrong database, a
// proxy that accepts TCP and nothing else — made every gated test skip while
// `go test` exited 0, so a "green" strict run proved nothing about the
// database layer.
//
// Contract:
//   - Inert unless TEST_POSTGRES_STRICT=1. Strict `make integration` and
//     integration.yml set it; `make test`/`make check` and the degraded
//     escape never do, so unit CI stays green with no services.
//   - When armed, every step is t.Fatalf — connect, DDL, insert, select.
//     It must NEVER skip: skipping here is the failure mode it exists to
//     catch. A connect failure names the redacted DSN so the operator can
//     see which credential/endpoint was unusable.
//   - The round-trip uses a TEMP TABLE on a single connection, so it leaves
//     no rows and needs no teardown; the table dies with the connection.
func TestIntegrationSentinelPostgresRoundTrip(t *testing.T) {
	if os.Getenv("TEST_POSTGRES_STRICT") != "1" {
		t.Skip("sentinel enforces strict integration mode only (armed by TEST_POSTGRES_STRICT=1 in strict `make integration` and integration.yml)")
	}
	dsn := testDSN()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// pgx.Connect performs dial AND authentication: a reachable server with
	// wrong credentials fails here, not at first query.
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf(
			"sentinel: connect failed — strict mode requires a USABLE Postgres, not merely a reachable one (dsn %s): %v",
			redactDSN(dsn), err,
		)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	if _, err := conn.Exec(ctx, `CREATE TEMP TABLE sentinel_roundtrip (k text PRIMARY KEY, v integer)`); err != nil {
		t.Fatalf("sentinel: CREATE TEMP TABLE: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO sentinel_roundtrip (k, v) VALUES ($1, $2)`, "sentinel", 42); err != nil {
		t.Fatalf("sentinel: INSERT: %v", err)
	}
	var got int
	if err := conn.QueryRow(ctx, `SELECT v FROM sentinel_roundtrip WHERE k = $1`, "sentinel").Scan(&got); err != nil {
		t.Fatalf("sentinel: SELECT: %v", err)
	}
	if got != 42 {
		t.Fatalf("sentinel: round-trip read %d, want 42", got)
	}
}

// redactDSN strips the password from a postgres DSN before it appears in
// error output. Credentials here are throwaway local values, but error logs
// travel (CI logs, terminals) — never print a password, even a fake one.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "<unparseable dsn>"
	}
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	return u.String()
}
