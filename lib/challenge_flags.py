"""Challenge flag switches on the match trace (FR-255, UC-035 BR-018, docs/specs/flags.md §1.4).

A challenge flag is a key starting with one of :data:`CHALLENGE_FLAG_PREFIXES`. Inside a match
(:func:`match_scope`, opened by the game coordinator around the game root span) the tracing hook
(:class:`lib.openfeature_otel_hooks.DedupingTracingHook`) records its ``feature_flag.evaluation``
event only when the key's outcome changed for that match, and :class:`ChallengeMatch` puts
``orangecase.challenge.start`` / ``.end`` on the match root.
"""

from __future__ import annotations

import contextvars
import logging
from collections.abc import Iterator
from contextlib import contextmanager

from opentelemetry.trace import Span

logger = logging.getLogger(__name__)

# Kept equal to services/agent/flags/challenge.go ChallengeFlagPrefixes (lib/tests checks it).
CHALLENGE_FLAG_PREFIXES: tuple[str, ...] = ("challenge.", "stowaway.")

# (domain, key, code default) evaluated once per gameplay frame of every running match; the
# default's type picks the typed evaluation.
WATCHED_CHALLENGE_FLAGS: tuple[tuple[str, str, object], ...] = (("observability", "stowaway.challenge", "none"),)

EVENT_START = "orangecase.challenge.start"
EVENT_END = "orangecase.challenge.end"
ATTR_NAME = "orangecase.challenge.name"
ATTR_END_REASON = "orangecase.challenge.end_reason"
ATTR_KEY = "feature_flag.key"
ATTR_VARIANT = "feature_flag.result.variant"
END_FLAG_CHANGE = "flag_change"
END_MATCH_END = "match_end"

_INACTIVE_STRINGS = frozenset({"", "off", "none"})
_UNSET = object()
# Flag keys are a finite set; the cap only guards against a caller inventing keys.
_PREFIX_CACHE_MAX = 4096
_prefix_cache: dict[str, bool] = {}
_reported: set[str] = set()


def _report_once(what: str) -> None:
    if what not in _reported:
        _reported.add(what)
        logger.warning("challenge flag tracking failed (%s); the event is skipped", what, exc_info=True)


def is_challenge_flag(flag_key: str) -> bool:
    hit = _prefix_cache.get(flag_key)
    if hit is None:
        hit = flag_key.startswith(CHALLENGE_FLAG_PREFIXES)
        if len(_prefix_cache) < _PREFIX_CACHE_MAX:
            _prefix_cache[flag_key] = hit
    return hit


def set_challenge_prefixes(prefixes: tuple[str, ...]) -> None:
    """Replace the prefix list (tests, a future challenge); clears the lookup cache."""
    global CHALLENGE_FLAG_PREFIXES
    CHALLENGE_FLAG_PREFIXES = tuple(prefixes)
    _prefix_cache.clear()


def challenge_name(flag_key: str, value: object) -> str | None:
    """The challenge's name while the value switches it on, else None."""
    if value is None or value is False:
        return None
    if isinstance(value, str):
        return None if value.strip().lower() in _INACTIVE_STRINGS else value
    return flag_key


class ChallengeMatch:
    """Per-match change state: last outcome per challenge key, and the active challenges."""

    __slots__ = ("root", "_last", "_active")

    def __init__(self, root: Span) -> None:
        self.root = root
        self._last: dict[str, tuple] = {}
        self._active: dict[str, tuple[str, str | None]] = {}

    def observe(self, flag_key: str, variant: str | None, value: object, is_error: bool) -> bool:
        """True when this outcome differs from the key's last one in the match. Never raises."""
        try:
            marker = (variant, value, is_error)
            if self._last.get(flag_key, _UNSET) == marker:
                return False
            self._last[flag_key] = marker
            self._switch(flag_key, challenge_name(flag_key, value), variant)
            return True
        except Exception:
            _report_once("observe")
            return False

    def _switch(self, flag_key: str, name: str | None, variant: str | None) -> None:
        current = self._active.get(flag_key)
        if current is not None and current[0] == name:
            return
        if current is not None:
            self._end(flag_key, current, END_FLAG_CHANGE)
            del self._active[flag_key]
        if name is not None:
            self._active[flag_key] = (name, variant)
            self.root.add_event(EVENT_START, _attributes(flag_key, name, variant))

    def _end(self, flag_key: str, challenge: tuple[str, str | None], reason: str) -> None:
        attributes = _attributes(flag_key, *challenge)
        attributes[ATTR_END_REASON] = reason
        self.root.add_event(EVENT_END, attributes)

    def close(self) -> None:
        """End every active challenge with ``match_end``. Never raises."""
        try:
            for flag_key, challenge in self._active.items():
                self._end(flag_key, challenge, END_MATCH_END)
        except Exception:
            _report_once("close")
        self._active.clear()
        self._last.clear()


def _attributes(flag_key: str, name: str, variant: str | None) -> dict:
    attributes = {ATTR_NAME: name, ATTR_KEY: flag_key}
    if variant:
        attributes[ATTR_VARIANT] = variant
    return attributes


_current: contextvars.ContextVar[ChallengeMatch | None] = contextvars.ContextVar("challenge_match", default=None)


def current_match() -> ChallengeMatch | None:
    return _current.get()


def open_match(root: Span) -> contextvars.Token:
    """Start the running match for this context (and the tasks it starts later)."""
    return _current.set(ChallengeMatch(root))


def close_match(token: contextvars.Token) -> None:
    """End the match :func:`open_match` started: its active challenges end with ``match_end``."""
    match = _current.get()
    if match is not None:
        match.close()
    try:
        _current.reset(token)
    except ValueError:
        _report_once("close_match from another context")


@contextmanager
def match_scope(root: Span) -> Iterator[ChallengeMatch]:
    token = open_match(root)
    try:
        yield _current.get()
    finally:
        close_match(token)


_clients: dict[str, object] = {}


def _client(domain: str):
    client = _clients.get(domain)
    if client is None:
        from openfeature import api

        client = api.get_client(domain=domain)
        _clients[domain] = client
    return client


def _evaluate(client, key: str, default: object) -> None:
    if isinstance(default, bool):
        client.get_boolean_value(key, default)
    elif isinstance(default, str):
        client.get_string_value(key, default)
    elif isinstance(default, int):
        client.get_integer_value(key, default)
    elif isinstance(default, float):
        client.get_float_value(key, default)
    else:
        client.get_object_value(key, default)


def poll_watched_flags(watched: tuple[tuple[str, str, object], ...] | None = None) -> None:
    """Evaluate the watched challenge flags in the current match; the hook records changes.

    Called once per gameplay frame. Nothing to do outside a match or with no watched flag.
    Never raises into the game loop.
    """
    entries = WATCHED_CHALLENGE_FLAGS if watched is None else watched
    if not entries or _current.get() is None:
        return
    for domain, key, default in entries:
        try:
            _evaluate(_client(domain), key, default)
        except Exception:
            _report_once(f"poll {domain}/{key}")
