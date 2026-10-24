"""Unit tests for scripts/ci/release_gate.py (the release-gate witness).

No network: the gh API and run-log fetchers are injected fakes. Evidence
fixtures are shaped on real main-branch runs (55f4d26 / 254c4b92) recorded
in the negative-control plan (docs/ci/RELEASE_CONTRACT.md §8); the
qualification record in the EVIDENCE_COMPLETE fixture is clearly synthetic
(no real qualification run exists yet — that demo is deferred to the owner).
"""

from __future__ import annotations

import importlib.util
import sys
from pathlib import Path
from typing import Any

import pytest

_SCRIPT = Path(__file__).resolve().parents[2] / "scripts" / "ci" / "release_gate.py"
_SPEC = importlib.util.spec_from_file_location("release_gate", _SCRIPT)
assert _SPEC is not None and _SPEC.loader is not None
GATE = importlib.util.module_from_spec(_SPEC)
# Register before exec: the frozen dataclass resolves its module via
# sys.modules during class creation.
sys.modules["release_gate"] = GATE
_SPEC.loader.exec_module(GATE)

REPO = "owner/repo"
FULL_SHA = "55f4d26f4897dda6bee24efe1a735966f31c2711"
SHORT_SHA = "55f4d26"

# Real run ids from main (verified via gh api during the negative-control plan).
RUN_CI = 40073487084
RUN_SECURITY = 40073487102
RUN_INTEGRATION = 40073487133
RUN_SECURITY_FAILED = 39541843041  # control (a): 254c4b92 security failure
RUN_QUAL_SYNTHETIC = 999999  # synthetic fixture — NOT a real run


def _run(
    run_id: int,
    name: str,
    event: str = "push",
    status: str = "completed",
    conclusion: str = "success",
    created: str = "2026-10-20T00:00:00Z",
) -> dict[str, Any]:
    return {
        "id": run_id,
        "name": name,
        "event": event,
        "status": status,
        "conclusion": conclusion,
        "created_at": created,
        "head_sha": FULL_SHA,
        "html_url": f"https://github.com/{REPO}/actions/runs/{run_id}",
    }


def _job(name: str, conclusion: str = "success") -> dict[str, Any]:
    return {"name": name, "conclusion": conclusion}


class FakeGh:
    """Route-table stand-in for the gh API callable."""

    def __init__(self, responses: dict[str, Any]) -> None:
        self._responses = responses
        self.calls: list[str] = []

    def __call__(self, args: list[str]) -> dict[str, Any]:
        path = args[0]
        self.calls.append(path)
        for prefix, response in self._responses.items():
            if path.startswith(prefix):
                return response
        raise GATE.GateError(f"no route for {path}")


def _green_main_responses() -> dict[str, Any]:
    """Control (b) evidence: 55f4d26 — ci/security/integration green, no
    qualification run, no posture issue."""
    return {
        f"repos/{REPO}/commits/": {"sha": FULL_SHA},
        f"repos/{REPO}/actions/runs?head_sha=": {
            "workflow_runs": [
                _run(RUN_CI, "ci"),
                _run(RUN_SECURITY, "security"),
                _run(RUN_INTEGRATION, "integration"),
            ]
        },
        f"repos/{REPO}/actions/runs/{RUN_CI}/jobs": {
            "jobs": [_job("deterministic"), _job("terraform")]
        },
        f"repos/{REPO}/actions/runs/{RUN_SECURITY}/jobs": {
            "jobs": [_job("static"), _job("govulncheck"), _job("pip-audit")]
        },
        f"repos/{REPO}/actions/runs/{RUN_INTEGRATION}/jobs": {"jobs": [_job("services")]},
        "search/issues?q=": {"items": []},
        f"repos/{REPO}/issues?state=all": [],
    }


def _config(**overrides: Any) -> GATE.GateConfig:
    defaults: dict[str, Any] = {
        "repo": REPO,
        "target_sha": FULL_SHA,
        "release_label": "",
        "posture_max_age_hours": 168.0,
        "attestation_statement": "",
        "attested_by": "",
        "attested_date": "",
        "gate_run_id": 0,
        "gate_actor": "tester",
        "gate_sha": "0" * 40,
        "out": None,
    }
    defaults.update(overrides)
    return GATE.GateConfig(**defaults)


def _qual_log(pg: int = 12, measured: int | None = None, expected: int = 14) -> str:
    lines = [
        "qualify: ephemeral database up",
        GATE.QUALIFIED_MARKER,
        f"postgres-backed passing tests: {pg}",
    ]
    if measured is not None:
        lines.append(f"LLM cases measured: {measured}/{expected}")
    return "\n".join(lines)


# --- select_run -------------------------------------------------------------


def test_select_run_prefers_push_over_pull_request() -> None:
    runs = [
        _run(1, "ci", event="pull_request", created="2026-10-21T00:00:00Z"),
        _run(2, "ci", event="push", created="2026-10-20T00:00:00Z"),
    ]
    assert GATE.select_run(runs, "ci")["id"] == 2


def test_select_run_takes_latest_within_pool() -> None:
    runs = [
        _run(1, "ci", created="2026-10-20T00:00:00Z"),
        _run(2, "ci", created="2026-10-22T00:00:00Z"),
    ]
    assert GATE.select_run(runs, "ci")["id"] == 2


def test_select_run_ignores_in_progress_and_other_workflows() -> None:
    runs = [
        _run(1, "ci", status="in_progress"),
        _run(2, "security"),
    ]
    assert GATE.select_run(runs, "ci") is None


def test_select_run_falls_back_to_pull_request_when_alone() -> None:
    runs = [_run(1, "ci", event="pull_request")]
    assert GATE.select_run(runs, "ci")["id"] == 1


# --- resolve_full_sha -------------------------------------------------------


def test_resolve_full_sha_returns_commit_sha() -> None:
    gh = FakeGh({f"repos/{REPO}/commits/": {"sha": FULL_SHA}})
    assert GATE.resolve_full_sha(gh, REPO, SHORT_SHA) == FULL_SHA


def test_resolve_full_sha_fails_closed_on_bad_response() -> None:
    gh = FakeGh({f"repos/{REPO}/commits/": {"sha": "short"}})
    with pytest.raises(GATE.GateError):
        GATE.resolve_full_sha(gh, REPO, SHORT_SHA)


# --- attestation form validation --------------------------------------------


def test_attestation_missing_when_all_blank() -> None:
    record = GATE.validate_attestation("", "", "")
    assert record["status"] == "missing"
    assert record["provided"] is False


def test_attestation_pass_with_all_fields() -> None:
    record = GATE.validate_attestation("reviewed", "alice", "2026-10-24")
    assert record["status"] == "pass"


def test_attestation_malformed_when_partial() -> None:
    record = GATE.validate_attestation("reviewed", "alice", "")
    assert record["status"] == "malformed"


def test_attestation_malformed_when_date_unparseable() -> None:
    record = GATE.validate_attestation("reviewed", "alice", "10/24/2026")
    assert record["status"] == "malformed"


# --- decide -----------------------------------------------------------------


def _passing_evidence() -> list[dict[str, Any]]:
    return [{"criterion": f"M{i}", "status": "pass", "reason": "ok"} for i in range(1, 10)]


def test_decide_blocked_on_any_failure() -> None:
    evidence = _passing_evidence()
    evidence[7]["status"] = "fail"
    verdict, failures = GATE.decide(evidence, {"status": "missing"})
    assert verdict == GATE.VERDICT_BLOCKED
    assert failures[0]["criterion"] == "M8"


def test_decide_blocked_on_indeterminate() -> None:
    evidence = _passing_evidence()
    evidence[8]["status"] = "indeterminate"
    verdict, _failures = GATE.decide(evidence, {"status": "missing"})
    assert verdict == GATE.VERDICT_BLOCKED


def test_decide_blocked_on_malformed_attestation_even_when_green() -> None:
    verdict, failures = GATE.decide(_passing_evidence(), {"status": "malformed"})
    assert verdict == GATE.VERDICT_BLOCKED
    assert failures[0]["criterion"] == "H1-H3"


def test_decide_evidence_complete_without_attestation() -> None:
    verdict, failures = GATE.decide(_passing_evidence(), {"status": "missing"})
    assert verdict == GATE.VERDICT_EVIDENCE_COMPLETE
    assert failures == []


def test_decide_passed_with_attestation() -> None:
    verdict, _failures = GATE.decide(_passing_evidence(), {"status": "pass"})
    assert verdict == GATE.VERDICT_PASSED


# --- qualification log parsing (M8) -----------------------------------------


def test_qualification_pg_only_pass() -> None:
    record, qual = GATE._qualification_criterion(
        [_run(RUN_QUAL_SYNTHETIC, "qualification")],
        lambda run_id: _qual_log(pg=12) if run_id == RUN_QUAL_SYNTHETIC else None,
    )
    assert record["status"] == "pass"
    assert qual["scope"] == "pg-only"
    assert qual["pg_backed_passing_tests"] == 12


def test_qualification_llm_matrix_pass() -> None:
    record, qual = GATE._qualification_criterion(
        [_run(RUN_QUAL_SYNTHETIC, "qualification")],
        lambda run_id: (
            _qual_log(pg=12, measured=14, expected=14) if run_id == RUN_QUAL_SYNTHETIC else None
        ),
    )
    assert record["status"] == "pass"
    assert qual["scope"] == "llm-matrix"
    assert qual["llm_measured"] == 14


def test_qualification_incomplete_llm_matrix_fails() -> None:
    record, _qual = GATE._qualification_criterion(
        [_run(RUN_QUAL_SYNTHETIC, "qualification")],
        lambda run_id: (
            _qual_log(pg=12, measured=9, expected=14) if run_id == RUN_QUAL_SYNTHETIC else None
        ),
    )
    assert record["status"] == "fail"
    assert "incomplete" in record["reason"]


def test_qualification_missing_marker_is_indeterminate() -> None:
    record, _qual = GATE._qualification_criterion(
        [_run(RUN_QUAL_SYNTHETIC, "qualification")],
        lambda run_id: (
            "postgres-backed passing tests: 12" if run_id == RUN_QUAL_SYNTHETIC else None
        ),
    )
    assert record["status"] == "indeterminate"


def test_qualification_missing_pg_count_is_indeterminate() -> None:
    record, _qual = GATE._qualification_criterion(
        [_run(RUN_QUAL_SYNTHETIC, "qualification")],
        lambda run_id: GATE.QUALIFIED_MARKER if run_id == RUN_QUAL_SYNTHETIC else None,
    )
    assert record["status"] == "indeterminate"


def test_qualification_zero_pg_tests_is_indeterminate() -> None:
    record, _qual = GATE._qualification_criterion(
        [_run(RUN_QUAL_SYNTHETIC, "qualification")],
        lambda run_id: _qual_log(pg=0) if run_id == RUN_QUAL_SYNTHETIC else None,
    )
    assert record["status"] == "indeterminate"
    assert "self-skipped" in record["reason"]


def test_qualification_unreadable_log_is_indeterminate() -> None:
    record, _qual = GATE._qualification_criterion(
        [_run(RUN_QUAL_SYNTHETIC, "qualification")], lambda run_id: None
    )
    assert record["status"] == "indeterminate"


def test_qualification_no_run_fails() -> None:
    record, _qual = GATE._qualification_criterion([], lambda run_id: None)
    assert record["status"] == "fail"


# --- posture (M9) -----------------------------------------------------------


def _posture_gh(comment_at: str | None) -> tuple[GATE.GhApi, int]:
    responses: dict[str, Any] = {
        "search/issues?q=": {"items": [{"title": "Security posture runs", "number": 7}]},
        f"repos/{REPO}/issues/7/comments": (
            [{"created_at": comment_at, "body": "posture summary"}] if comment_at else []
        ),
    }
    return FakeGh(responses), 7


def test_posture_fresh_passes() -> None:
    gh, _ = _posture_gh("2026-10-20T00:00:00Z")
    record, posture = GATE._posture_criterion(
        gh, REPO, 168.0, GATE._parse_gh_time("2026-10-24T00:00:00Z")
    )
    assert record["status"] == "pass"
    assert posture["status"] == "pass"
    assert posture["age_hours"] == pytest.approx(96.0)


def test_posture_stale_fails() -> None:
    gh, _ = _posture_gh("2026-10-20T00:00:00Z")
    record, posture = GATE._posture_criterion(
        gh, REPO, 0.0, GATE._parse_gh_time("2026-10-24T00:00:00Z")
    )
    assert record["status"] == "fail"
    assert posture["status"] == "fail"


def test_posture_missing_issue_is_indeterminate() -> None:
    gh = FakeGh({"search/issues?q=": {"items": []}, f"repos/{REPO}/issues?state=all": []})
    record, _posture = GATE._posture_criterion(
        gh, REPO, 168.0, GATE._parse_gh_time("2026-10-24T00:00:00Z")
    )
    assert record["status"] == "indeterminate"


def test_posture_issue_without_comments_is_indeterminate() -> None:
    gh, _ = _posture_gh(None)
    record, _posture = GATE._posture_criterion(
        gh, REPO, 168.0, GATE._parse_gh_time("2026-10-24T00:00:00Z")
    )
    assert record["status"] == "indeterminate"


# --- security-delta (M6) ----------------------------------------------------


def test_security_delta_na_on_push_run() -> None:
    run = _run(RUN_SECURITY, "security", event="push")
    record = GATE._security_delta_criterion(run, [_job("static")])
    assert record["status"] == "na"


def test_security_delta_pass_on_pull_request_run() -> None:
    run = _run(RUN_SECURITY, "security", event="pull_request")
    record = GATE._security_delta_criterion(run, [_job("security-delta")])
    assert record["status"] == "pass"


def test_security_delta_indeterminate_when_job_absent_on_pr_run() -> None:
    run = _run(RUN_SECURITY, "security", event="pull_request")
    record = GATE._security_delta_criterion(run, [_job("static")])
    assert record["status"] == "indeterminate"


def test_security_delta_fail_when_job_failed_on_pr_run() -> None:
    run = _run(RUN_SECURITY, "security", event="pull_request")
    record = GATE._security_delta_criterion(run, [_job("security-delta", "failure")])
    assert record["status"] == "fail"


# --- end-to-end evaluate (negative controls) --------------------------------


def test_evaluate_control_a_security_failure_blocks() -> None:
    """Control (a): 254c4b92 — security run failed; gate must BLOCKED naming it."""
    responses = _green_main_responses()
    responses[f"repos/{REPO}/actions/runs?head_sha="] = {
        "workflow_runs": [
            _run(39541843042, "ci"),
            _run(RUN_SECURITY_FAILED, "security", conclusion="failure"),
            _run(39541843043, "integration"),
        ]
    }
    responses[f"repos/{REPO}/actions/runs/39541843042/jobs"] = {
        "jobs": [_job("deterministic"), _job("terraform")]
    }
    responses[f"repos/{REPO}/actions/runs/39541843043/jobs"] = {"jobs": [_job("services")]}
    decision, exit_code = GATE.evaluate(FakeGh(responses), lambda run_id: None, _config())
    assert exit_code == GATE.EXIT_BLOCKED
    assert decision["verdict"] == GATE.VERDICT_BLOCKED
    assert decision["release_decision"] is None
    named = {failure["criterion"] for failure in decision["failures"]}
    assert "M3" in named
    assert "M4" in named
    assert "M5" in named


def test_evaluate_control_b_green_checks_without_qualification_blocks() -> None:
    """Control (b): 55f4d26 — ci/security/integration green, but no
    qualification run and no posture record; gate must BLOCKED on M8 + M9."""
    decision, exit_code = GATE.evaluate(
        FakeGh(_green_main_responses()), lambda run_id: None, _config()
    )
    assert exit_code == GATE.EXIT_BLOCKED
    assert decision["verdict"] == GATE.VERDICT_BLOCKED
    named = {failure["criterion"] for failure in decision["failures"]}
    assert "M8" in named
    assert "M9" in named
    assert decision["target_sha"] == FULL_SHA


def test_evaluate_evidence_complete_without_attestation() -> None:
    """EVIDENCE_COMPLETE path: real 55f4d26 evidence + a clearly synthetic
    qualification record + a fresh posture record, no attestation."""
    responses = _green_main_responses()
    responses[f"repos/{REPO}/actions/runs?head_sha="] = {
        "workflow_runs": [
            _run(RUN_CI, "ci"),
            _run(RUN_SECURITY, "security"),
            _run(RUN_INTEGRATION, "integration"),
            _run(RUN_QUAL_SYNTHETIC, "qualification"),
        ]
    }
    responses[f"repos/{REPO}/actions/runs/{RUN_QUAL_SYNTHETIC}/jobs"] = {"jobs": [_job("qualify")]}
    responses["search/issues?q="] = {"items": [{"title": "Security posture runs", "number": 7}]}
    responses[f"repos/{REPO}/issues/7/comments"] = [
        {"created_at": "2026-10-20T00:00:00Z", "body": "posture summary"}
    ]
    logs = {RUN_QUAL_SYNTHETIC: _qual_log(pg=12, measured=14, expected=14)}
    decision, exit_code = GATE.evaluate(
        FakeGh(responses), lambda run_id: logs.get(run_id), _config()
    )
    assert exit_code == GATE.EXIT_PASS
    assert decision["verdict"] == GATE.VERDICT_EVIDENCE_COMPLETE
    assert decision["release_decision"] is None
    assert decision["disclaimer"]
    assert all(criterion["status"] in ("pass", "na") for criterion in decision["evidence"])


def test_evaluate_passed_with_attestation_still_null_release_decision() -> None:
    responses = _green_main_responses()
    responses[f"repos/{REPO}/actions/runs?head_sha="] = {
        "workflow_runs": [
            _run(RUN_CI, "ci"),
            _run(RUN_SECURITY, "security"),
            _run(RUN_INTEGRATION, "integration"),
            _run(RUN_QUAL_SYNTHETIC, "qualification"),
        ]
    }
    responses[f"repos/{REPO}/actions/runs/{RUN_QUAL_SYNTHETIC}/jobs"] = {"jobs": [_job("qualify")]}
    responses["search/issues?q="] = {"items": [{"title": "Security posture runs", "number": 7}]}
    responses[f"repos/{REPO}/issues/7/comments"] = [
        {"created_at": "2026-10-20T00:00:00Z", "body": "posture summary"}
    ]
    logs = {RUN_QUAL_SYNTHETIC: _qual_log(pg=12, measured=14, expected=14)}
    decision, exit_code = GATE.evaluate(
        FakeGh(responses),
        lambda run_id: logs.get(run_id),
        _config(
            attestation_statement="Human review of H1-H4 recorded",
            attested_by="alice",
            attested_date="2026-10-24",
        ),
    )
    assert exit_code == GATE.EXIT_PASS
    assert decision["verdict"] == GATE.VERDICT_PASSED
    assert decision["release_decision"] is None
    assert decision["attestation"]["attested_by"] == "alice"


def test_evaluate_malformed_attestation_blocks_despite_green_evidence() -> None:
    responses = _green_main_responses()
    responses[f"repos/{REPO}/actions/runs?head_sha="] = {
        "workflow_runs": [
            _run(RUN_CI, "ci"),
            _run(RUN_SECURITY, "security"),
            _run(RUN_INTEGRATION, "integration"),
            _run(RUN_QUAL_SYNTHETIC, "qualification"),
        ]
    }
    responses[f"repos/{REPO}/actions/runs/{RUN_QUAL_SYNTHETIC}/jobs"] = {"jobs": [_job("qualify")]}
    responses["search/issues?q="] = {"items": [{"title": "Security posture runs", "number": 7}]}
    responses[f"repos/{REPO}/issues/7/comments"] = [
        {"created_at": "2026-10-20T00:00:00Z", "body": "posture summary"}
    ]
    logs = {RUN_QUAL_SYNTHETIC: _qual_log(pg=12, measured=14, expected=14)}
    decision, exit_code = GATE.evaluate(
        FakeGh(responses),
        lambda run_id: logs.get(run_id),
        _config(attestation_statement="reviewed", attested_by="alice"),
    )
    assert exit_code == GATE.EXIT_BLOCKED
    assert decision["verdict"] == GATE.VERDICT_BLOCKED


def test_evaluate_stale_posture_blocks() -> None:
    responses = _green_main_responses()
    responses[f"repos/{REPO}/actions/runs?head_sha="] = {
        "workflow_runs": [
            _run(RUN_CI, "ci"),
            _run(RUN_SECURITY, "security"),
            _run(RUN_INTEGRATION, "integration"),
            _run(RUN_QUAL_SYNTHETIC, "qualification"),
        ]
    }
    responses[f"repos/{REPO}/actions/runs/{RUN_QUAL_SYNTHETIC}/jobs"] = {"jobs": [_job("qualify")]}
    responses["search/issues?q="] = {"items": [{"title": "Security posture runs", "number": 7}]}
    responses[f"repos/{REPO}/issues/7/comments"] = [
        {"created_at": "2026-10-20T00:00:00Z", "body": "posture summary"}
    ]
    logs = {RUN_QUAL_SYNTHETIC: _qual_log(pg=12, measured=14, expected=14)}
    decision, exit_code = GATE.evaluate(
        FakeGh(responses),
        lambda run_id: logs.get(run_id),
        _config(posture_max_age_hours=0.0),
    )
    assert exit_code == GATE.EXIT_BLOCKED
    named = {failure["criterion"] for failure in decision["failures"]}
    assert "M9" in named


def test_main_api_failure_fails_closed(monkeypatch: pytest.MonkeyPatch) -> None:
    """Any evidence-collection error must produce a BLOCKED artifact (the
    catch lives in main(), so this exercises the real entry point)."""
    monkeypatch.setattr(GATE, "make_gh_api", lambda: FakeGh({}))  # no routes
    monkeypatch.setattr(GATE, "make_run_log_fetcher", lambda: lambda run_id: None)
    exit_code = GATE.main(["--repo", REPO, "--target-sha", SHORT_SHA])
    assert exit_code == GATE.EXIT_BLOCKED
