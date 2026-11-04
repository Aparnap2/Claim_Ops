#!/usr/bin/env python3
"""Fail closed unless every emulator-gated package actually executed tests.

Why this exists: `make localgcp-test` and the Pub/Sub/GCS adapter suites
self-skip when the emulator is unreachable, and the command still exits 0.
A green result is therefore meaningless unless at least one designated
emulator-backed test actually ran. This check reads the verbose per-test
output and requires the designated tests to appear as `--- PASS`, with no
`--- SKIP` for them, so a missing or unhealthy emulator fails the gate.

It is the emulator analogue of check_pg_gated_packages.py. It does NOT
prove production GCP behaviour — emulator integration is not production
integration (see docs/KNOWN_LIMITATIONS.md KL-15).
"""

from __future__ import annotations

import os
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
API = ROOT / "apps" / "api"

# Packages that gate on PUBSUB_EMULATOR_HOST / STORAGE_EMULATOR_HOST and
# self-skip when the emulator is absent. Enumerated explicitly so a newly
# gated package cannot silently join the set without a decision here.
REQUIRED_PACKAGES: tuple[str, ...] = (
    "./internal/adapters/pubsubadapter",
    "./internal/adapters/gcsblob",
)

# The designated tests that prove a real emulator-backed operation occurred.
# These must appear as `--- PASS` (never `--- SKIP`).
REQUIRED_NAMED_TESTS: dict[str, tuple[str, ...]] = {
    "./internal/adapters/pubsubadapter": ("TestEmulatorPublishReceiveRoundTrip",),
    "./internal/adapters/gcsblob": ("TestStoreRoundTrip",),
}

MIN_PASSES = 1

PASS_RE = re.compile(r"^--- PASS: (\S+)", re.MULTILINE)
SKIP_RE = re.compile(r"^--- SKIP: (\S+)", re.MULTILINE)


def run_package(package: str, env: dict[str, str]) -> tuple[int, list[str], list[str]]:
    result = subprocess.run(
        ["go", "test", "-v", "-count=1", package],
        cwd=API,
        env=env,
        check=False,
        text=True,
        capture_output=True,
    )
    output = result.stdout + result.stderr
    passed = PASS_RE.findall(output)
    skipped = SKIP_RE.findall(output)
    return result.returncode, passed, skipped


def main() -> int:
    env = os.environ.copy()
    env.setdefault("PUBSUB_EMULATOR_HOST", "localhost:8085")
    env.setdefault("STORAGE_EMULATOR_HOST", "localhost:4443")

    print("Emulator-gated package coverage (designated tests must PASS, not SKIP)")
    problems: list[str] = []
    for package in REQUIRED_PACKAGES:
        rc, passed, skipped = run_package(package, env)
        status = "ok" if (rc == 0 and len(passed) >= MIN_PASSES) else "FAILED"
        print(f"  {status:>6}  {package}  pass={len(passed)} skip={len(skipped)} exit={rc}")
        if rc != 0:
            problems.append(f"{package}: go test exited {rc}")
        if len(passed) < MIN_PASSES:
            problems.append(f"{package}: executed no tests (pass={len(passed)} skip={len(skipped)})")
        for name in REQUIRED_NAMED_TESTS.get(package, ()):
            if not any(c == name or c.startswith(name + "/") for c in passed):
                problems.append(
                    f"{package}: required emulator test {name} did not PASS "
                    f"(pass={len(passed)} skip={len(skipped)}) — "
                    "emulator may be down or test self-skipped"
                )
            if any(c == name or c.startswith(name + "/") for c in skipped):
                problems.append(
                    f"{package}: required emulator test {name} SKIPPED — "
                    "this must never happen in a green CI gate"
                )

    if problems:
        print("\nEmulator-gated coverage FAILED:", file=sys.stderr)
        for p in problems:
            print(f"  - {p}", file=sys.stderr)
        return 1
    print("\nEmulator-gated coverage OK: all designated tests passed.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
