"""Gesture state machine of the operator pad (UC-015 BR-007, A4, A6; UC-016 BR-008, BR-011, BR-014, BR-015).

Pure: no I/O, no timers, no clock. Every event carries its own timestamp (ms) and the caller
passes the timings in force; the timings at a key's press are that key's snapshot for its whole
hold, so a test drives it with fake events and an injected clock.

The page keys (UC-016 BR-011):

    K1 edit  | K2 reset | knob
    K3 edit  | K4 reset | K5 agent
    K6 edit  | K7 reset | K8 game

- tap or hold is decided at release from release time minus press time only;
- edit keys (K1, K3, K6): a knob turn while one of them is the only key down counts toward it and
  marks it used (``layer_step`` per completed step, ``layer`` with the net at release); unused: a
  tap, a release from ``hold_agent_ms`` on the ``transition`` (UC-015's K3 grip), else a cancel;
  a knob press while it is held is the confirmation (``confirm``), taken only when the value stood
  ``CONFIRM_STABLE_MS``; a later step clears it; turns while the knob is down are ignored;
- reset keys (K2, K4, K7): tap, cancel, ``reset`` from ``hold_reset_ms``; K4 from ``hold_lock_ms``
  the ``lock`` instead, unless the reset was confirmed (a knob press past the reset stage); a
  release within ``GUARD_MS`` after a stage boundary cancels; a turn does nothing;
- K8: tap; ``hold`` from ``hold_end_ms`` to below ``hold_lock_ms``; at or after it a cancel;
- K5: ``agent_hold`` once K5 has been down ``hold_agent_ms`` (from ``due`` or at the release),
  whatever else is down or locked; ``clean`` says no other key, no knob press, no lock touched the
  hold. Its release after that is a ``hold`` carrying the same ``clean``; the caller decides;
- a chord (a key goes down while another key, or the knob press, is down) blocks every key until
  all keys are up, except the K5 ``agent_hold``;
- locked: only the K5 ``agent_hold`` and the K4 hold past ``hold_lock_ms`` (unlock) act;
- a key with no event for ``STALE_KEY_MS`` counts as up (its release was lost);
- the knob alone: volume (``knob_turn``, ``knob_press`` mute) or with the pages a ``page_step``.
"""

from __future__ import annotations

from dataclasses import dataclass

from services.operator_pad.config import (
    CONFIRM_STABLE_MS,
    GUARD_MS,
    K1,
    K2,
    K3,
    K4,
    K5,
    K6,
    K7,
    KEYS,
    KNOB_PRESS,
    KNOB_TURN_A,
    KNOB_TURN_B,
    STALE_KEY_MS,
    Timings,
)

EDIT_KEYS = frozenset({K1, K3, K6})
RESET_KEYS = frozenset({K2, K4, K7})
KNOB = "KNOB"

PRESS, RELEASE, REPEAT = 1, 0, 2

KNOB_PAUSE_MS = 250  # BR-005: a pause this long in turning drops the volume remainder
KNOB_PUSH_JITTER_MS = 150  # UC-016 BR-008: raw turns this soon after a knob press are the push, not a turn

TAP = "tap"
HOLD = "hold"
CANCEL = "cancel"
LAYER = "layer"
LAYER_STEP = "layer_step"  # BR-017: a completed step of a held edit key (preview only, never a write)
KNOB_TURN = "knob_turn"
KNOB_PRESS_GESTURE = "knob_press"
LOCK = "lock"
PAGE_STEP = "page_step"  # UC-016 BR-008: a knob step with the pages; never a command
RESET = "reset"  # UC-016 A8: a reset key released past its reset stage
CONFIRM = "confirm"  # UC-016 A9: the knob press confirming the value showing (never a command)
AGENT_HOLD = "agent_hold"  # UC-016 A10: K5 reached its hold time while still down
TRANSITION = "transition"  # UC-016 BR-011: an edit key held past hold_agent_ms without a turn


@dataclass(frozen=True)
class KeyEvent:
    """One key event already mapped to a pad key name (``K1`` .. ``K8``, ``KNOB_*``)."""

    name: str
    value: int  # 1 press, 0 release, 2 auto-repeat
    t_ms: int


@dataclass(frozen=True)
class Gesture:
    kind: str
    key: str
    steps: int = 0  # knob_turn: +1 / -1; layer / layer_step / confirm: net turns while the key was down
    held_ms: int = 0
    locked: bool = False  # agent_hold / hold of K5: the pad was locked; lock: the new state
    confirmed: bool = False  # layer / reset: the value released was confirmed with the knob (A9)
    clean: bool = True  # agent_hold / hold of K5: no other key, knob press or lock during the hold


class GestureMachine:
    def __init__(self) -> None:
        self.locked = False
        self._down: dict[str, int] = {}
        self._seen: dict[str, int] = {}  # key -> its last event (press or auto-repeat)
        self._snap: dict[str, Timings] = {}  # key -> the timings at its press
        self._used: set[str] = set()
        self._layer_steps: dict[str, int] = {}
        self._layer_events: dict[str, tuple[int, int]] = {}  # edit key -> (direction, raw events counted)
        self._last_step_ms: dict[str, int] = {}
        self._confirmed: dict[str, int] = {}  # key -> the net steps confirmed (reset keys: 0)
        self._volume_events: tuple[int, int] = (0, 0)
        self._last_volume_turn_ms: int | None = None
        self._chord = False
        self._knob_down = False
        self._knob_pages = False
        self._knob_pressed_ms = 0
        self._knob_turned = False
        self._page_events: tuple[int, int] = (0, 0)
        self._k5_clean = True
        self._k5_fired = False

    def reset(self) -> None:
        """Device gone (UC-015 A4 step 2): every gesture in progress is dropped; the lock stays."""
        locked = self.locked
        self.__init__()
        self.locked = locked

    def keys_down(self) -> bool:
        return bool(self._down) or self._knob_down

    def down(self, key: str) -> bool:
        return key in self._down

    def press_ms(self, key: str) -> int | None:
        return self._down.get(key)

    def confirmed(self, key: str) -> bool:
        return key in self._confirmed

    def preview_key(self) -> str | None:
        """The edit key whose live preview may run (BR-017): the only key down, used, no chord, unlocked."""
        if self._chord or self.locked:
            return None
        key = self._single_edit_key()
        return key if key is not None and key in self._used else None

    def feed(self, event: KeyEvent, timings: Timings, pages: bool = False) -> list[Gesture]:
        """``pages``: the display link shows the pages (UC-016 BR-008); False is the knob of UC-015."""
        out = self._expire(event.t_ms, keep=event.name)
        if event.value == REPEAT:
            if event.name in self._down:
                self._seen[event.name] = event.t_ms
            return out
        if event.name in KEYS:
            if event.value == PRESS:
                return out + self._key_press(event, timings)
            return out + self._key_release(event)
        if event.name in (KNOB_TURN_A, KNOB_TURN_B):
            if event.value != PRESS:
                return out  # one detent = one press; its release carries nothing
            return out + self._knob_turn(-1 if event.name == KNOB_TURN_A else 1, event.t_ms, timings, pages)
        if event.name == KNOB_PRESS:
            if event.value == PRESS:
                return out + self._knob_press(event.t_ms, pages)
            return out + self._knob_release(event.t_ms, timings)
        return out

    def due(self, t_ms: int) -> list[Gesture]:
        """The K5 hold time reached while K5 is down (A10): the caller's timer calls this."""
        pressed = self._down.get(K5)
        if pressed is None or self._k5_fired or t_ms - pressed < self._snap[K5].hold_agent_ms:
            return []
        self._k5_fired = True
        return [Gesture(AGENT_HOLD, K5, held_ms=t_ms - pressed, locked=self.locked, clean=self._k5_is_clean())]

    # --- keys ---------------------------------------------------------------------------

    def _k5_is_clean(self) -> bool:
        return self._k5_clean and not self._chord and not self.locked

    def _expire(self, t_ms: int, keep: str) -> list[Gesture]:
        """BR-011: a key with no event for STALE_KEY_MS is up; nothing fires for it (a K5 threshold already did)."""
        for key in [k for k, seen in self._seen.items() if k != keep and t_ms - seen >= STALE_KEY_MS]:
            self._drop(key)
        if not self._down:
            self._clear_chord_when_idle()
        return []

    def _drop(self, key: str) -> None:
        self._down.pop(key, None)
        self._seen.pop(key, None)
        self._snap.pop(key, None)
        self._used.discard(key)
        self._layer_steps.pop(key, None)
        self._layer_events.pop(key, None)
        self._last_step_ms.pop(key, None)
        self._confirmed.pop(key, None)

    def _key_press(self, event: KeyEvent, timings: Timings) -> list[Gesture]:
        key = event.name
        if key in self._down:
            self._drop(key)  # its release was lost (BR-007): re-armed, timed from this press
        if self.keys_down():
            self._chord = True
            self._k5_clean = False  # K5 down before, or K5 pressed into a chord
        self._down[key] = event.t_ms
        self._seen[key] = event.t_ms
        self._snap[key] = timings
        if key == K5:
            self._k5_fired = False
            self._k5_clean = not self._chord and not self.locked
        return []

    def _key_release(self, event: KeyEvent) -> list[Gesture]:
        key = event.name
        pressed_at = self._down.get(key)
        if pressed_at is None:
            return []
        out = self.due(event.t_ms) if key == K5 else []
        snap = self._snap[key]
        used = key in self._used
        steps = self._layer_steps.get(key, 0)
        confirmed = key in self._confirmed and (key in RESET_KEYS or self._confirmed[key] == steps)
        self._drop(key)
        if self._chord:
            self._clear_chord_when_idle()
            return out
        held = max(0, event.t_ms - pressed_at)
        if key == K5:
            if self._k5_fired:
                return [*out, Gesture(HOLD, K5, held_ms=held, locked=self.locked, clean=self._k5_is_clean())]
            if self.locked:
                return out
            return [Gesture(TAP if held <= snap.tap_ms else CANCEL, key, held_ms=held)]
        if key == K4 and self.locked:
            if held >= snap.hold_lock_ms + GUARD_MS:
                self.locked = False
                return [Gesture(LOCK, key, held_ms=held, locked=False)]
            return []
        if self.locked:
            return []
        if key in EDIT_KEYS:
            if used:
                return [Gesture(LAYER, key, steps=steps, held_ms=held, confirmed=confirmed)]
            if held <= snap.tap_ms:
                return [Gesture(TAP, key, held_ms=held)]
            return [Gesture(TRANSITION if held >= snap.hold_agent_ms else CANCEL, key, held_ms=held)]
        if key in RESET_KEYS:
            return self._reset_release(key, held, snap, used, confirmed)
        # K8
        if held <= snap.tap_ms:
            return [Gesture(TAP, key, held_ms=held)]
        if snap.hold_end_ms + GUARD_MS <= held < snap.hold_lock_ms:
            return [Gesture(HOLD, key, held_ms=held)]
        return [Gesture(CANCEL, key, held_ms=held)]  # the old 3 s lock grip on K8 never force-ends

    def _reset_release(self, key: str, held: int, snap: Timings, used: bool, confirmed: bool) -> list[Gesture]:
        if held <= snap.tap_ms:
            return [] if used else [Gesture(TAP, key, held_ms=held)]
        if held < snap.hold_reset_ms + GUARD_MS:
            return [Gesture(CANCEL, key, held_ms=held)]
        if key != K4 or confirmed or held < snap.hold_lock_ms:
            return [Gesture(RESET, key, held_ms=held, confirmed=confirmed)]
        if held < snap.hold_lock_ms + GUARD_MS:
            return [Gesture(CANCEL, key, held_ms=held)]
        self.locked = True
        self._confirmed.clear()
        return [Gesture(LOCK, key, held_ms=held, locked=True)]

    # --- knob ---------------------------------------------------------------------------

    def _single_edit_key(self) -> str | None:
        if len(self._down) != 1:
            return None
        (key,) = self._down
        return key if key in EDIT_KEYS else None

    @staticmethod
    def _count(events: tuple[int, int], direction: int, per_step: int) -> tuple[tuple[int, int], int]:
        """One raw event: (new accumulator, steps emitted 0 or +-1); a direction change restarts the count."""
        last_direction, count = events
        count = count + 1 if direction == last_direction else 1
        if count >= per_step:
            return (direction, 0), direction
        return (direction, count), 0

    def _knob_turn(self, direction: int, t_ms: int, timings: Timings, pages: bool = False) -> list[Gesture]:
        if self._chord:
            return []
        layer = None if self.locked else self._single_edit_key()
        if layer is not None:
            if self._knob_down:
                return []  # O8: the confirmation press jitters; it never moves the value
            self._volume_events = (0, 0)
            self._used.add(layer)
            events, step = self._count(self._layer_events.get(layer, (0, 0)), direction, timings.knob_events_per_step)
            self._layer_events[layer] = events
            if not step:
                return []
            self._layer_steps[layer] = self._layer_steps.get(layer, 0) + step
            self._last_step_ms[layer] = t_ms
            self._confirmed.pop(layer, None)  # A9: a later step clears the confirmation
            return [Gesture(LAYER_STEP, layer, steps=self._layer_steps[layer])]
        if self._down:
            return []  # a turn with a reset key, K5 or K8 down does nothing (also with the pages)
        if pages and not self._knob_down:
            self._page_events, step = self._count(self._page_events, direction, timings.knob_events_per_step)
            return [Gesture(PAGE_STEP, KNOB, steps=step)] if step else []
        if self.locked:
            return []
        if self._knob_pages and t_ms - self._knob_pressed_ms < KNOB_PUSH_JITTER_MS:
            return []  # one stray detent of the push would be a whole step at one event per step
        previous_ms, self._last_volume_turn_ms = self._last_volume_turn_ms, t_ms
        if previous_ms is None or t_ms - previous_ms >= KNOB_PAUSE_MS:
            self._volume_events = (0, 0)  # the volume commit pause: a half step never writes
        self._volume_events, step = self._count(self._volume_events, direction, timings.knob_events_per_step)
        if step and self._knob_pages:
            self._knob_turned = True  # a completed step, not a raw event: push jitter never blocks the mute
        return [Gesture(KNOB_TURN, KNOB, steps=step)] if step else []

    def _knob_press(self, t_ms: int = 0, pages: bool = False) -> list[Gesture]:
        if self._knob_down:
            return []
        self._knob_pages = False
        self._knob_turned = False
        self._knob_down = True
        if K5 in self._down:
            self._k5_clean = False  # a knob press never blocks the K5 stop; it only makes the hold unclean
        if self._chord or self.locked:
            return []
        if len(self._down) == 1:
            (key,) = self._down
            if key in EDIT_KEYS:
                self._used.add(key)
                base = self._last_step_ms.get(key, self._down[key])
                if t_ms - base < CONFIRM_STABLE_MS:
                    return []  # O8: the value moved just before the press: no confirmation
                net = self._layer_steps.get(key, 0)
                self._confirmed[key] = net
                return [Gesture(CONFIRM, key, steps=net)]
            if key in RESET_KEYS:
                self._used.add(key)
                snap = self._snap[key]
                held = t_ms - self._down[key]
                if held >= snap.hold_reset_ms and (key != K4 or held < snap.hold_lock_ms):
                    self._confirmed[key] = 0
                    return [Gesture(CONFIRM, key)]
                return []  # before the reset stage (or in the lock stage): ignored, never a mute
        if self._down:
            self._chord = True  # K5 or K8 with the knob: blocked (K5's stop still fires)
            return []
        self._volume_events = (0, 0)  # the mute wins over a turn not yet a step
        if pages:
            self._knob_pages = True
            self._knob_pressed_ms = t_ms
            return []
        return [Gesture(KNOB_PRESS_GESTURE, KNOB)]

    def _knob_release(self, t_ms: int = 0, timings: Timings | None = None) -> list[Gesture]:
        if not self._knob_down:
            return []
        self._knob_down = False
        bound, self._knob_pages = self._knob_pages, False
        out: list[Gesture] = []
        if bound and not self._knob_turned and not self._chord and not self.locked and timings is not None:
            held = max(0, t_ms - self._knob_pressed_ms)
            if held <= timings.tap_ms:
                out.append(Gesture(KNOB_PRESS_GESTURE, KNOB, held_ms=held))
        self._clear_chord_when_idle()
        return out

    def _clear_chord_when_idle(self) -> None:
        if not self.keys_down():
            self._chord = False
