"""Shared steps of the Flagsmith link journeys (test_flagsmith_link.py, test_flagsmith_sync.py, #2592).

The link polls the fake every FLAGSMITH_POLL_SECONDS (5 s). Tests wait on ``/state``
(``flagsmith.last_pull_at`` advancing, ``pending``, ``out_of_sync``, ``link``) and on the
fake's recorded requests, never on fixed sleeps.

Rules these helpers encode (architecture note flagsmith-sync.md §4.2, plan #2592 §6, §8):
- after a confirmed push, a pull must see the pushed value before the test changes the
  value back (the value guard skips a document that still shows a value the push replaced);
- the first pull after a start skips the keys the restart rule cleared: allow one more poll;
- each test leaves the link settled: ``settle`` waits until nothing is pending and a pull
  ran after the last push, then ``reset_values`` and one more pull.
"""

from __future__ import annotations

import datetime as dt
import re
import time
import uuid
from collections.abc import Callable

from tests.integration.flag_service import FlagService, FlagServiceError

POLL_SECONDS = 5.0
# A pull every POLL_SECONDS plus the round itself and the reconnect backoff of a short outage.
PULL_TIMEOUT = 3 * POLL_SECONDS + 10.0
RESTART_SECONDS = 45.0
COORDINATOR_INSTANCE = "itest-flagsmith"

_FRACTION = re.compile(r"\.(\d+)")


def wait_for(check: Callable[[], object], timeout: float, what: str, interval: float = 0.25):
    """Poll ``check`` until it returns a truthy value; fail with ``what`` and its last value."""
    deadline = time.monotonic() + timeout
    last = None
    while True:
        try:
            last = check()
        except FlagServiceError as err:  # e.g. 503 no_sync_stream right after a restart
            last = err
        else:
            if last:
                return last
        if time.monotonic() > deadline:
            raise AssertionError(f"timed out after {timeout:.0f}s waiting for {what}; last: {last!r}")
        time.sleep(interval)


def link(flag_service: FlagService) -> dict:
    return flag_service.state().get("flagsmith", {})


def wait_link(flag_service: FlagService, timeout: float = PULL_TIMEOUT, **want) -> dict:
    return wait_for(
        lambda: (lambda s: s if all(s.get(k) == v for k, v in want.items()) else None)(link(flag_service)),
        timeout,
        f"flagsmith link {want}",
    )


def _parse(stamp: str | None) -> float | None:
    if not stamp:
        return None
    stamp = _FRACTION.sub(lambda m: "." + m.group(1)[:6], stamp.replace("Z", "+00:00"))
    return dt.datetime.fromisoformat(stamp).timestamp()


def last_pull(flag_service: FlagService) -> float | None:
    """The wall-clock start of the last finished pull round (``/state.flagsmith.last_pull_at``)."""
    return _parse(link(flag_service).get("last_pull_at"))


def wait_pulls(
    flag_service: FlagService, n: int = 1, after: float | None = None, timeout: float | None = None
) -> float:
    """Wait for ``n`` pull rounds that started after ``after`` (default: now); returns the last start."""
    mark = time.time() if after is None else after
    for i in range(n):
        start = mark
        mark = wait_for(
            lambda start=start: (lambda t: t if t is not None and t > start else None)(last_pull(flag_service)),
            timeout or PULL_TIMEOUT,
            f"pull {i + 1}/{n} after {start:.3f}",
        )
    return mark


def overlay(flag_service: FlagService, flag_set: str, flag: str) -> dict:
    return flag_service.get(flag_set, flag).get("stack_overlay") or {}


def wait_overlay(flag_service: FlagService, flag_set: str, flag: str, timeout: float = PULL_TIMEOUT, **want) -> dict:
    return wait_for(
        lambda: (lambda o: o if o and all(o.get(k) == v for k, v in want.items()) else None)(
            overlay(flag_service, flag_set, flag)
        ),
        timeout,
        f"{flag_set}/{flag} overlay {want}",
    )


def put_when_synced(flag_service: FlagService, flag_set: str, flag: str, **kwargs) -> dict:
    """PUT once flagd has reopened its sync stream after a restart (503 no_sync_stream until then)."""
    return wait_for(lambda: flag_service.put(flag_set, flag, **kwargs), RESTART_SECONDS, f"PUT {flag_set}/{flag}")


def ok_pushes(fake, feature: str) -> list[dict]:
    return [r for r in fake.pushes(feature) if r["status"] // 100 == 2]


def wait_push(
    fake,
    feature: str,
    check: Callable[[dict], bool],
    after: float = 0.0,
    timeout: float = PULL_TIMEOUT,
    flag_service: FlagService | None = None,
):
    """The first accepted push of ``feature`` at or after monotonic ``after`` matching ``check``.

    With ``flag_service``, nudge the sender while waiting: the push is queued by a pull round (#2608)."""

    def found():
        hit = next((r for r in ok_pushes(fake, feature) if r["t"] >= after and check(r)), None)
        if hit is None and flag_service is not None:
            nudge(flag_service)
        return hit

    return wait_for(found, timeout, f"a push of {feature}", interval=2.0 if flag_service else 0.25)


def patch_value(req: dict) -> tuple[bool, str]:
    """(enabled, value text) of a ``__future__`` PATCH body."""
    default = req["body"]["environment_default"]
    return bool(default["enabled"]), str(default["value"]["value"])


def new_game_id() -> str:
    return f"game_{uuid.uuid4().hex[:8]}"


def register_game(flag_service: FlagService, game_id: str, serials: list[str], kind: str = "shadow") -> None:
    flag_service._ok(
        "PUT",
        f"/games/{game_id}",
        {"coordinator_instance": COORDINATOR_INSTANCE, "game_kind": kind, "serials": serials},
    )


def end_game(flag_service: FlagService, game_id: str) -> None:
    flag_service.request("DELETE", f"/games/{game_id}")


def settle(flag_service: FlagService, fake) -> None:
    """Leave the link in a clean state for the next test (plan #2592 §8.3): wait until nothing is
    pending and a pull ran after the last push (value guard), reset the fake, and wait for a
    pull of the reset document."""
    fake.down = False
    fake.delay = 0.0
    fake.clear_refusals()
    wait_link(flag_service, RESTART_SECONDS + 60, link="online")
    # A push a pull round queued waits for the next local change (#2608): nudge until none is pending.
    wait_for(
        lambda: link(flag_service).get("pending") == 0 or nudge(flag_service),
        PULL_TIMEOUT * 2,
        "pending 0",
        interval=2.0,
    )
    wait_pulls(flag_service, 1)
    fake.reset_values()
    wait_pulls(flag_service, 1)
    wait_for(
        lambda: link(flag_service).get("pending") == 0 or nudge(flag_service),
        PULL_TIMEOUT,
        "pending 0 after the reset",
        interval=2.0,
    )


def nudge(flag_service: FlagService) -> None:
    """An unrelated local write that wakes the link's sender.

    #2608: a pull round that queues a push (actKeepLocal: a kill switch kept local, or the
    restart rule removing the link's own game value) does not wake the sender; the push goes
    out with the next local change. Journeys that wait for such a push nudge with this write
    until #2608 lands (then drop the nudge and tighten the wait)."""
    flag_service.put("observability", "flagservice.propagation_probe", value=uuid.uuid4().hex)
