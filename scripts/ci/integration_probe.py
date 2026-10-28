#!/usr/bin/env python3
"""Probe integration services (Postgres, Mockoon) for `make integration`.

Extracted from the Makefile's inline probe so the strict/degraded logic has a
single, unit-tested definition. Prints one status line per service (same
wording as the previous inline probe) and exits:

  0 — every service reachable
  3 — at least one service unreachable (strict mode must fail; degraded mode
      may proceed because the gated tests self-skip)

Exit 3 (not 1) so callers can distinguish "service down" from a probe bug.

`--down-list` is the informational mode used by the degraded banner: it
prints the comma-joined names of the unreachable services (empty line when
everything is up) and ALWAYS exits 0 — it reports, it does not gate.
Stdlib only — runs with plain `python3`, no uv env required.
"""

from __future__ import annotations

import argparse
import os
import socket
import sys
import urllib.parse
import urllib.request

DEFAULT_TEST_POSTGRES_DSN = "postgres://claimops_app:claimops_app@localhost:5433/claimops"
DEFAULT_MOCKOON_URL = "http://localhost:3001"
MOCKOON_PROBE_PATH = "/v1/policies/POL-001"

EXIT_OK = 0
EXIT_SERVICE_DOWN = 3


def postgres_reachable(dsn: str) -> tuple[bool, str]:
    """TCP-probe the Postgres host:port parsed from a DSN."""
    parsed = urllib.parse.urlparse(dsn)
    host = parsed.hostname or "localhost"
    port = parsed.port or 5433
    sock = socket.socket()
    sock.settimeout(2)
    try:
        up = sock.connect_ex((host, port)) == 0
    finally:
        sock.close()
    if up:
        return True, f"integration: Postgres {host}:{port} reachable"
    return (
        False,
        f"SKIP: Postgres unreachable at {host}:{port} — postgres-gated tests will self-skip",
    )


def mockoon_reachable(url: str) -> tuple[bool, str]:
    """HTTP-probe the Mockoon gate endpoint."""
    try:
        response = urllib.request.urlopen(url, timeout=3)
        if response.status == 200:
            return True, f"integration: Mockoon {url} reachable (status {response.status})"
        return (
            False,
            f"SKIP: Mockoon gate returned status {response.status} — "
            "mockoon-gated tests will self-skip",
        )
    except Exception as exc:  # any failure means unreachable; message is diagnostic only
        return (
            False,
            f"SKIP: Mockoon unreachable at {url} ({exc}) — mockoon-gated tests will self-skip",
        )


def down_services(dsn: str, mockoon_url: str) -> list[str]:
    """Return the names of the services that are currently unreachable."""
    pg_ok, _ = postgres_reachable(dsn)
    mk_ok, _ = mockoon_reachable(mockoon_url + MOCKOON_PROBE_PATH)
    down: list[str] = []
    if not pg_ok:
        down.append("postgres")
    if not mk_ok:
        down.append("mockoon")
    return down


def main() -> int:
    parser = argparse.ArgumentParser(description="Probe integration services (Postgres, Mockoon).")
    parser.add_argument(
        "--down-list",
        action="store_true",
        help="print comma-joined names of unreachable services and always exit 0 "
        "(informational; used for the degraded-mode banner)",
    )
    args = parser.parse_args()

    dsn = os.environ.get("TEST_POSTGRES_DSN", DEFAULT_TEST_POSTGRES_DSN)
    mockoon_url = os.environ.get("MOCKOON_URL", DEFAULT_MOCKOON_URL)

    if args.down_list:
        print(",".join(down_services(dsn, mockoon_url)))
        return EXIT_OK

    pg_ok, pg_msg = postgres_reachable(dsn)
    mk_ok, mk_msg = mockoon_reachable(mockoon_url + MOCKOON_PROBE_PATH)
    print(pg_msg)
    print(mk_msg)
    return EXIT_OK if (pg_ok and mk_ok) else EXIT_SERVICE_DOWN


if __name__ == "__main__":
    sys.exit(main())
