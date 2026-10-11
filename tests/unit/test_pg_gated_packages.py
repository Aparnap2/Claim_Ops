"""Tests for the Postgres-gated package coverage gate (APA-83 P10).

The gate exists because `go test` exits 0 when an entire suite skips, so a
green process is not evidence that DB-backed tests ran.
"""

from __future__ import annotations

import importlib.util
from pathlib import Path

import pytest

SCRIPT = (
    Path(__file__).resolve().parents[2] / "scripts" / "ci" / "check_pg_gated_packages.py"
)
SPEC = importlib.util.spec_from_file_location("pg_gated_packages", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
GATE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GATE)


def test_required_packages_cover_every_postgres_gated_package() -> None:
    """The set must include every package that gates on TEST_POSTGRES_DSN.

    Guarding the enumeration matters: a package could gate on the DSN without
    appearing here, and then a service outage would skip it undetected.
    """
    repo = SCRIPT.parents[2]
    api = repo / "apps" / "api"
    discovered: set[str] = set()
    for path in api.rglob("*_test.go"):
        if "TEST_POSTGRES_DSN" in path.read_text(encoding="utf-8"):
            package = "./" + str(path.parent.relative_to(api))
            discovered.add(package.rstrip("/."))

    required = {package.rstrip("/.") for package in GATE.REQUIRED_PACKAGES}
    missing = sorted(discovered - required)
    assert not missing, f"Postgres-gated packages absent from the gate: {missing}"


def test_count_results_reads_verbose_pass_and_skip_lines() -> None:
    output = (
        "=== RUN   TestAlpha\n"
        "--- PASS: TestAlpha (0.01s)\n"
        "=== RUN   TestBeta\n"
        "--- SKIP: TestBeta (0.00s)\n"
        "--- FAIL: TestGamma (0.02s)\n"
    )
    passed, skipped = GATE.count_results(output)
    assert passed == ["TestAlpha"]
    assert skipped == ["TestBeta"]


def test_a_package_whose_tests_all_skipped_is_underrun() -> None:
    """The whole point: 0 passes with N skips must not read as covered."""
    counts = {package: 1 for package in GATE.REQUIRED_PACKAGES}
    counts["./internal/repository/postgres"] = 0
    underrun = GATE.underrun_packages(counts)
    assert underrun == ["./internal/repository/postgres"]


def test_a_busy_package_cannot_mask_a_silent_one() -> None:
    """Aggregate counting would pass here; per-package must not.

    532 aggregate passes with Postgres fully down is the measured shape of this
    failure — most packages have unrelated tests that pass regardless.
    """
    counts = {package: 50 for package in GATE.REQUIRED_PACKAGES}
    counts["./internal/repository/postgres"] = 0
    assert sum(counts.values()) >= 100
    assert GATE.underrun_packages(counts) == ["./internal/repository/postgres"]


def test_a_missing_package_entry_is_treated_as_underrun() -> None:
    """A package that produced no output must not be assumed healthy."""
    counts = {
        package: 5
        for package in GATE.REQUIRED_PACKAGES
        if package != "./internal/ingest"
    }
    underrun = GATE.underrun_packages(counts)
    assert underrun == ["./internal/ingest"]


def test_all_packages_covered_means_no_underrun() -> None:
    counts = {package: 3 for package in GATE.REQUIRED_PACKAGES}
    assert GATE.underrun_packages(counts) == []


def test_missing_dsn_fails_closed(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("TEST_POSTGRES_DSN", raising=False)
    assert GATE.main() == 1


def test_blank_dsn_fails_closed(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("TEST_POSTGRES_DSN", "   ")
    assert GATE.main() == 1
