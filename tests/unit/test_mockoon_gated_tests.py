"""Tests for the Mockoon anti-fake-green gate (APA-83 P12).

Mockoon-dependent tests self-skip when the service is unavailable, and `go test`
still exits 0. These tests pin the rule that a Mockoon-gated test must appear as
`--- PASS` — a package's unrelated passing tests are not evidence.
"""

from __future__ import annotations

import importlib.util
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[2] / "scripts" / "ci" / "check_mockoon_gated_tests.py"
SPEC = importlib.util.spec_from_file_location("mockoon_gated_tests", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
GATE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GATE)

PKG_HTTP = "./internal/adapters/http"
PKG_TOOLS = "./internal/investigate/tools"
PKG_APP = "./internal/app"


def healthy() -> dict[str, object]:
    """Every required package reporting each of its Mockoon tests as passing."""
    return {
        package: GATE.PackageResult(
            0, [f"{name}/case" for name in names], []
        )
        for package, names in GATE.REQUIRED_MOCKOON_TESTS.items()
    }


def test_healthy_output_passes() -> None:
    assert GATE.evaluate(healthy()) == []  # type: ignore[arg-type]


def test_all_mockoon_tests_skipped_fails() -> None:
    """The exact P12 failure mode: service down, tests skip, job would be green."""
    results = healthy()
    for package, names in GATE.REQUIRED_MOCKOON_TESTS.items():
        results[package] = GATE.PackageResult(0, ["TestSomethingElse"], list(names))
    problems = GATE.evaluate(results)  # type: ignore[arg-type]
    assert len(problems) == sum(len(n) for n in GATE.REQUIRED_MOCKOON_TESTS.values())
    assert any("did not pass" in problem for problem in problems)


def test_zero_passing_tests_fails() -> None:
    results = {package: GATE.PackageResult(0, [], []) for package in GATE.REQUIRED_MOCKOON_TESTS}
    problems = GATE.evaluate(results)
    assert problems


def test_nonzero_exit_with_passing_tests_still_fails() -> None:
    """Same defect class the P10 review caught: exit code outranks pass counts."""
    results = healthy()
    results[PKG_TOOLS] = GATE.PackageResult(1, ["TestLiveToolMatrix"], [])
    problems = GATE.evaluate(results)  # type: ignore[arg-type]
    assert len(problems) == 1
    assert "exited 1" in problems[0]
    assert PKG_TOOLS in problems[0]


def test_missing_package_output_fails() -> None:
    results = healthy()
    del results[PKG_HTTP]
    problems = GATE.evaluate(results)  # type: ignore[arg-type]
    assert len(problems) == 1
    assert "no result" in problems[0]


def test_a_busy_package_cannot_mask_an_unexecuted_one() -> None:
    """Many unrelated passes must not stand in for the Mockoon-gated test.

    This is the measured shape of the defect: with Mockoon down,
    ./internal/investigate/tools reported 81 passes and exit 0 while
    TestLiveToolMatrix skipped.
    """
    results = healthy()
    noisy = [f"Unrelated{i}" for i in range(200)]  # no Mockoon-gated test among them
    results[PKG_TOOLS] = GATE.PackageResult(0, noisy, ["TestLiveToolMatrix"])
    problems = GATE.evaluate(results)  # type: ignore[arg-type]
    assert len(problems) == 1
    assert PKG_TOOLS in problems[0]
    assert "did not pass" in problems[0]


def test_one_required_test_skipping_is_enough_to_fail() -> None:
    """A package that runs only some of its required Mockoon tests still fails."""
    results = healthy()
    required = list(GATE.REQUIRED_MOCKOON_TESTS[PKG_TOOLS])
    results[PKG_TOOLS] = GATE.PackageResult(0, [], required)
    problems = GATE.evaluate(results)  # type: ignore[arg-type]
    assert len(problems) == 1
    assert required[0] in problems[0]


def test_required_set_matches_repository_evidence() -> None:
    """A newly introduced Mockoon-gated test must not silently escape the gate.

    Re-discovers Mockoon gate points from source and reports any that are
    neither required nor explicitly excluded with a documented reason. The
    exclusions matter: a test that also needs localgcp or the GCW emulator
    cannot be asserted here without making the job permanently red.
    """
    api = SCRIPT.parents[2] / "apps" / "api"
    discovered: set[str] = set()

    for path in api.rglob("*_test.go"):
        text = path.read_text(encoding="utf-8")
        # A test file that self-skips when a Mockoon origin is unreachable.
        if "Mockoon unreachable" in text or "Mockoon gate" in text:
            discovered.update(_test_names(text))
        # internal/app gates through infraEnvFor(t, _, true).
        if "infraEnvFor(t," in text:
            for name in _test_names(text):
                if _calls_mockoon_gate(text, name):
                    discovered.add(name)

    required = {name for names in GATE.REQUIRED_MOCKOON_TESTS.values() for name in names}
    accounted_for = required | set(GATE.EXCLUDED_MOCKOON_TESTS)
    unaccounted = sorted(discovered - accounted_for)
    assert not unaccounted, f"Mockoon-gated tests absent from the gate: {unaccounted}"


def test_excluded_tests_still_exist_and_state_a_reason() -> None:
    """Every documented exclusion must name a real test and give a reason."""
    for name, reason in GATE.EXCLUDED_MOCKOON_TESTS.items():
        assert name.startswith("Test"), name
        assert reason.strip(), f"{name} is excluded without a documented reason"


def test_exclusions_are_actually_mockoon_gated() -> None:
    """An exclusion must not smuggle a genuinely Mockoon-only test out of the gate."""
    api = SCRIPT.parents[2] / "apps" / "api"
    sources = {
        path: path.read_text(encoding="utf-8")
        for path in api.rglob("*_test.go")
        if "Mockoon unreachable" in path.read_text(encoding="utf-8")
        or "infraEnvFor(t," in path.read_text(encoding="utf-8")
    }
    for name in GATE.EXCLUDED_MOCKOON_TESTS:
        assert any(name in text for text in sources.values()), (
            f"{name} is listed as excluded but no Mockoon gate was found for it"
        )


def _test_names(text: str) -> list[str]:
    import re

    return re.findall(r"^func (Test[A-Za-z0-9_]*)\(", text, re.MULTILINE)


def _calls_mockoon_gate(text: str, name: str) -> bool:
    """True when the named test's body calls infraEnvFor with needMockoon=true."""
    import re

    pattern = r"^func " + re.escape(name) + r"\(t \*testing\.T\) \{(.*?)^\}"
    match = re.search(pattern, text, re.MULTILINE | re.DOTALL)
    if not match:
        return False
    return bool(re.search(r"infraEnvFor\(t,\s*[^,]+,\s*true\)", match.group(1)))


def test_unavailable_or_blank_endpoint_fails_closed(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("MOCKOON_URL", raising=False)
    assert GATE.main() == 1
    monkeypatch.setenv("MOCKOON_URL", "   ")
    assert GATE.main() == 1


def test_count_results_reads_verbose_lines() -> None:
    output = (
        "--- PASS: TestLiveToolMatrix/case_a (0.01s)\n"
        "--- SKIP: TestLiveToolMatrix/case_b (0.00s)\n"
    )
    passed, skipped = GATE.count_results(output)
    assert passed == ["TestLiveToolMatrix/case_a"]
    assert skipped == ["TestLiveToolMatrix/case_b"]


def test_passed_required_matches_top_level_and_subtests() -> None:
    assert GATE.passed_required(["TestLiveToolMatrix"], ("TestLiveToolMatrix",)) == {
        "TestLiveToolMatrix"
    }
    assert GATE.passed_required(["TestLiveToolMatrix/x"], ("TestLiveToolMatrix",)) == {
        "TestLiveToolMatrix"
    }
    # A different test with a shared prefix must not satisfy the requirement.
    assert GATE.passed_required(["TestLiveToolMatrixExtra"], ("TestLiveToolMatrix",)) == set()
