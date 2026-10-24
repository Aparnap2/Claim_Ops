"""Unit tests for scripts/ci/integration_probe.py (the `make integration` probe).

No network: reachability helpers are exercised with monkeypatched
socket/urlopen; main()'s exit-code logic is exercised with stubbed helpers.
"""

from __future__ import annotations

import importlib.util
import socket
import urllib.request
from pathlib import Path
from typing import Any

import pytest

_SCRIPT = Path(__file__).resolve().parents[2] / "scripts" / "ci" / "integration_probe.py"
_SPEC = importlib.util.spec_from_file_location("integration_probe", _SCRIPT)
assert _SPEC is not None and _SPEC.loader is not None
PROBE = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(PROBE)


class _FakeSocket:
    """Minimal socket stand-in whose connect_ex returns a canned errno."""

    def __init__(self, errno: int) -> None:
        self._errno = errno
        self.closed = False

    def settimeout(self, value: float) -> None:
        assert value == 2

    def connect_ex(self, address: tuple[str, int]) -> int:
        assert address == ("localhost", 5433)
        return self._errno

    def close(self) -> None:
        self.closed = True


class _FakeResponse:
    def __init__(self, status: int) -> None:
        self.status = status


def test_main_all_services_up_exits_zero(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(PROBE, "postgres_reachable", lambda dsn: (True, "pg up"))
    monkeypatch.setattr(PROBE, "mockoon_reachable", lambda url: (True, "mk up"))
    assert PROBE.main() == PROBE.EXIT_OK


def test_main_postgres_down_exits_three(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(PROBE, "postgres_reachable", lambda dsn: (False, "pg down"))
    monkeypatch.setattr(PROBE, "mockoon_reachable", lambda url: (True, "mk up"))
    assert PROBE.main() == PROBE.EXIT_SERVICE_DOWN


def test_main_mockoon_down_exits_three(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(PROBE, "postgres_reachable", lambda dsn: (True, "pg up"))
    monkeypatch.setattr(PROBE, "mockoon_reachable", lambda url: (False, "mk down"))
    assert PROBE.main() == PROBE.EXIT_SERVICE_DOWN


def test_postgres_reachable_reports_up(monkeypatch: pytest.MonkeyPatch) -> None:
    fake = _FakeSocket(0)
    monkeypatch.setattr(socket, "socket", lambda *args: fake)
    ok, msg = PROBE.postgres_reachable(
        "postgres://claimops_app:claimops_app@localhost:5433/claimops"
    )
    assert ok is True
    assert "reachable" in msg
    assert fake.closed is True


def test_postgres_reachable_reports_down(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(socket, "socket", lambda *args: _FakeSocket(111))
    ok, msg = PROBE.postgres_reachable(
        "postgres://claimops_app:claimops_app@localhost:5433/claimops"
    )
    assert ok is False
    assert msg.startswith("SKIP: Postgres unreachable")


def test_mockoon_reachable_reports_200(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(urllib.request, "urlopen", lambda url, timeout: _FakeResponse(200))
    ok, msg = PROBE.mockoon_reachable("http://localhost:3001/v1/policies/POL-001")
    assert ok is True
    assert "reachable" in msg


def test_mockoon_reachable_reports_non_200(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(urllib.request, "urlopen", lambda url, timeout: _FakeResponse(500))
    ok, msg = PROBE.mockoon_reachable("http://localhost:3001/v1/policies/POL-001")
    assert ok is False
    assert msg.startswith("SKIP: Mockoon gate returned status 500")


def test_mockoon_reachable_reports_exception(monkeypatch: pytest.MonkeyPatch) -> None:
    def _raise(url: str, timeout: Any) -> None:
        raise ConnectionError("refused")

    monkeypatch.setattr(urllib.request, "urlopen", _raise)
    ok, msg = PROBE.mockoon_reachable("http://localhost:3001/v1/policies/POL-001")
    assert ok is False
    assert msg.startswith("SKIP: Mockoon unreachable")
