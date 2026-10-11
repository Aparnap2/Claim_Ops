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


def all_results(**overrides: object) -> dict[str, object]:
    """A healthy result for every required package, with optional overrides."""
    results = {
        package: GATE.PackageResult(0, ["TestX"], []) for package in GATE.REQUIRED_PACKAGES
    }
    results.update(overrides)
    return results  # type: ignore[return-value]


def test_nonzero_exit_with_passing_tests_still_fails_the_gate() -> None:
    """The defect this gate must never have again.

    A package can report passing tests and still exit nonzero because another
    test failed. Counting passes alone classified such a package as healthy and
    let a real Postgres-backed failure through a green gate.
    """
    results = all_results(
        **{"./internal/ingest": GATE.PackageResult(1, ["TestA", "TestB"], [])}
    )
    problems = GATE.evaluate(results)
    assert len(problems) == 1
    assert "./internal/ingest" in problems[0]
    assert "exited 1" in problems[0]


def test_build_error_is_reported_not_swallowed() -> None:
    """A compile error exits nonzero with no passing tests at all."""
    results = all_results(
        **{"./internal/worker": GATE.PackageResult(2, [], [])}
    )
    problems = GATE.evaluate(results)
    assert len(problems) == 1
    assert "./internal/worker" in problems[0]
    assert "exited 2" in problems[0]


def test_all_skipped_package_is_still_reported() -> None:
    """The original failure mode: service down, everything skipped, exit 0."""
    results = all_results(
        **{"./internal/repository/postgres": GATE.PackageResult(0, [], ["TestA"])}
    )
    problems = GATE.evaluate(results)
    assert len(problems) == 1
    assert "executed no tests" in problems[0]


def test_healthy_run_reports_no_problems() -> None:
    assert GATE.evaluate(all_results()) == []


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


def test_a_busy_package_cannot_mask_a_silent_one() -> None:
    """Aggregate counting would pass here; per-package must not."""
    results = all_results()
    results["./internal/repository/postgres"] = GATE.PackageResult(0, [], ["TestA"])
    assert len(GATE.evaluate(results)) == 1


def test_a_missing_package_entry_is_reported() -> None:
    """A package that produced no output must not be assumed healthy."""
    results = all_results()
    del results["./internal/ingest"]
    problems = GATE.evaluate(results)
    assert len(problems) == 1
    assert "./internal/ingest" in problems[0]


def test_missing_dsn_fails_closed(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("TEST_POSTGRES_DSN", raising=False)
    assert GATE.main() == 1


def test_blank_dsn_fails_closed(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("TEST_POSTGRES_DSN", "   ")
    assert GATE.main() == 1
