"""TempoMixin: the music tempo / intensity driver of BaseGameMode (#2633).

The tempo driver of Phase 70 and #1833 (UC-031 BR-005) moved here unchanged from
``games/base.py``: the music start and stop, the natural slow/fast schedule, the
intensity ramps and their audio glides, the agent ``music_tempo_override`` and
``ramp_tempo`` handling, and the 100 ms ``_music_loop``. ``BaseGameMode`` inherits
them, so every call site and test still goes through the game object. The state
they use is set up by :meth:`TempoMixin._init_tempo_state`, which
``BaseGameMode.__init__`` calls at the point where that state used to be set.
``_lerp``, ``_game_state_labels`` and ``_emit_sensitivity_metric`` stay in
``BaseGameMode`` (``_lerp`` runs per frame).

Import order: ``games/base.py`` imports this module after its module-level
constants and just before ``class BaseGameMode``, and this module imports those
constants back from it. Import ``BaseGameMode`` from ``games/base.py``; do not
import this module on its own first.

Module globals: the logger and the constants are base.py's own objects, so log
records and the metric labels are unchanged. ``tracer`` and ``read_float_flag``
are looked up on the base module at call time (``_base.``), so a test that
patches them on ``games.base`` still reaches this code and the span
instrumentation scope stays base.py's tracer; never import those two by value.
``metrics`` is imported by value (as in ``games/shields.py``), so the telemetry
contract inventory sees the gauges recorded here; patch ``games.tempo.metrics``
in a test that needs to intercept them.
"""

import asyncio

from opentelemetry import context as otel_context
from opentelemetry import trace

from services.game_coordinator import game_clock, metrics
from services.game_coordinator.games import base as _base
from services.game_coordinator.games.base import (
    AUDIO_PROBE_INTERVAL_SECONDS,
    AUDIO_TEMPO_RETRY_MAX_SECONDS,
    AUDIO_TEMPO_RETRY_MIN_SECONDS,
    AUDIO_TEMPO_RPC_TIMEOUT_SECONDS,
    DEFAULT_RAMP_DOWN_SECONDS,
    DEFAULT_RAMP_UP_SECONDS,
    MIN_GLIDE_SECONDS,
    MUSIC_REFUSAL_RESTARTS_MAX,
    MUSIC_TICK_LOG_INTERVAL_SECONDS,
    MUTED_TRACK_ID,
    RAMP_CURVE_LINEAR,
    RAMP_TEMPO_STEP_TRANSITION,
    SLOW_MUSIC_SPEED,
    IntensityRamp,
    RampDescriptor,
    intensity_from_tempo,
    logger,
    resolve_ramp_seconds,
    tempo_from_intensity,
)
from services.game_coordinator.grpc_clients import PROBE_DOWN, PROBE_OK


class TempoMixin:
    """Music tempo / intensity driver of every game mode, mixed into ``BaseGameMode`` (#2633)."""

    def _init_tempo_state(self) -> None:
        """Set up the tempo driver state; ``BaseGameMode.__init__`` calls it once."""
        # active ramp_tempo CURVE descriptor, or None
        # for no ramp (the default-safe state — behavior unchanged). When set, the
        # single tempo owner (``_check_music_speed``) interpolates the live tempo
        # toward the descriptor's target over its duration and holds there. Set by
        # the ramp_tempo handler; cleared by its revert handler. Lives ONLY on the
        # game object (bounded delta — never persisted to game.json, ownership §1).
        self.tempo_ramp: RampDescriptor | None = None

        # Phase 70: Music tempo control state
        self.music_track_id = None
        # the tempo intensity (UC-031 BR-005) is the source; music_speed is
        # derived from it (property below). Starts held at 0 (1.0x, slow table).
        self._intensity = IntensityRamp.hold(0.0)
        # False while the audio service has not accepted the current glide; the
        # music loop resends the remaining glide at _audio_retry_at.
        self._audio_synced = True
        self._audio_retry_at = 0.0
        self._audio_retry_backoff = AUDIO_TEMPO_RETRY_MIN_SECONDS
        # Music resume (UC-031 A7, BR-009): set once the game sent its music start.
        self._music_wanted = False
        # The audio service failed a call or a probe since it last answered.
        self._audio_down = False
        # The game's track is gone (failed start, refused glide): start it again.
        self._music_lost = False
        self._audio_probe_at = 0.0
        self._music_refusal_restarts = 0
        # Last volume the game sent, so a music restart plays at the same level.
        self._audio_volume: float | None = None
        # Rate limit for the failing-tick warning of _music_loop.
        self._tick_error_text: str | None = None
        self._tick_error_logged_at = 0.0
        self._tick_error_suppressed = 0
        # The linear ramp_tempo descriptor already turned into an intensity ramp
        # (identity check, so one descriptor sends exactly one glide).
        self._adopted_tempo_ramp: RampDescriptor | None = None
        # True while the override or ramp_tempo owns the intensity; when control
        # returns to the schedule the running segment is held where it is.
        self._agent_tempo_owned = False
        # True while a ramp ran at the last music tick, so the tick after it ends
        # still emits the final tempo / threshold gauges.
        self._ramp_metrics_open = False
        self.speed_up = True  # True = next change will speed up, False = slow down
        self.change_time = 0.0  # Time of next tempo change
        self.music_loop_task = None
        # Agent tempo override. When set to a float (1.0-1.3), the
        # music loop adopts this tempo and SUSPENDS its own speed-up/slow-down
        # schedule (resolves the E1 race in 722-intervention-surface §3.1).
        # None = no override; the natural schedule runs. Set/cleared by the
        # music_tempo_override intervention handler; consumed in _check_music_speed.
        self.tempo_override: float | None = None

    @property
    def music_speed(self) -> float:
        """Music ratio derived from the tempo intensity now (#1833, UC-031 BR-005)."""
        return tempo_from_intensity(self._intensity.value_at(game_clock.now()))

    @music_speed.setter
    def music_speed(self, tempo: float) -> None:
        """Pin the intensity at the given tempo's value (no ramp); used by fixtures and resets."""
        self._intensity = IntensityRamp.hold(intensity_from_tempo(tempo), game_clock.now())

    @property
    def tempo_intensity(self) -> float:
        """The tempo intensity in [0, 1] now (0 = slow table, 1 = fast table)."""
        return self._intensity.value_at(game_clock.now())

    def _emit_threshold_metrics(self):
        """Emit effective threshold metrics for the current tempo intensity (Phase 80, #1833)."""
        sens_idx = self.sensitivity.value
        speed_percent = self._intensity.value_at(game_clock.now())

        effective_warn = self._lerp(self.slow_warning[sens_idx], self.fast_warning[sens_idx], speed_percent)
        effective_death = self._lerp(self.slow_max[sens_idx], self.fast_max[sens_idx], speed_percent)

        labels = self._game_state_labels()
        metrics.effective_warning_threshold.labels(**labels).set(effective_warn)
        metrics.effective_death_threshold.labels(**labels).set(effective_death)

    def _emit_music_tempo_metric(self) -> None:
        """Emit this game's current music tempo (Phase 80)."""
        metrics.music_tempo.labels(**self._game_state_labels()).set(self.music_speed)

    async def _start_game_music(self, volume: float | None = None):
        """
        Start game music with tempo control (Phase 70).

        Sets volume and starts music at normal speed. When ``volume`` is None,
        uses the per-game configured game volume (F7, #766).
        Note: play_audio setting is checked centrally in audio service.

        #1833 (UC-031 BR-005, D1): the tempo schedule starts here whether or not
        music plays (no audio client, muted, PlayMusic failed): the intensity and
        the thresholds move without a music track.
        """
        self.music_speed = SLOW_MUSIC_SPEED
        self.speed_up = True
        self._audio_synced = True
        self._audio_retry_backoff = AUDIO_TEMPO_RETRY_MIN_SECONDS
        self.change_time = self._get_music_change_time()
        self._emit_music_tempo_metric()
        self._emit_threshold_metrics()

        if self._is_shadow_game():
            # Shadow games are silent (UC-031 BR-006): PlayMusic on the shared audio
            # service would replace the real game's track. The schedule above still runs (D1).
            trace.get_current_span().set_attribute("game.music", "shadow")
            logger.info(f"Shadow game {self.game_id}: no music, tempo schedule runs silently")
            return

        if not self.audio_client:
            logger.info(
                f"No audio client: tempo schedule runs without music, next change at +{self._next_change_in():.1f}s"
            )
            return

        if volume is None:
            volume = self.game_volume

        self._music_wanted = True
        self._music_lost = False
        self._audio_down = False
        self._music_refusal_restarts = 0
        if await self._send_music_start(volume, SLOW_MUSIC_SPEED):
            logger.info(f"Game music started: {self.music_track_id}, next change at +{self._next_change_in():.1f}s")

    async def _send_music_start(self, volume: float, tempo: float) -> bool:
        """SetVolume, then PlayMusic at ``tempo``; True when a track plays (UC-031 step 2, A7).

        A failed call marks the audio service down and the music lost, so the
        music loop starts it again once the service answers. A refusal (no
        playable file) is final: repeating it would not help.
        """
        from proto import audio_pb2

        self._audio_volume = volume
        try:
            # Set initial volume (may be low during countdown, then ramped up)
            await self.audio_client.SetVolume(audio_pb2.SetVolumeRequest(volume=volume))
            response = await self.audio_client.PlayMusic(
                audio_pb2.PlayMusicRequest(
                    file_pattern="Joust/music/*.ogg",
                    loop=True,
                    tempo=tempo,
                    priority=audio_pb2.AudioPriority.MEDIUM,
                )
            )
        except Exception as e:
            logger.warning(f"Failed to start game music (tempo schedule runs without it, retried): {e}")
            self.music_track_id = None
            self._music_lost = True
            self._audio_down = True
            return False

        self._music_lost = False
        if not response.success:
            logger.warning(f"Failed to start game music (tempo schedule runs without it): {response.error}")
            self.music_track_id = None
            return False
        self.music_track_id = response.track_id
        return True

    def _next_change_in(self) -> float:
        """Seconds until the next scheduled tempo change (for logs)."""
        return self.change_time - game_clock.now()

    async def _stop_game_music(self):
        """Stop game music (Phase 70). A shadow game never started any (#1835)."""
        if not self.audio_client or self._is_shadow_game():
            return

        try:
            from proto import audio_pb2

            self._music_wanted = False
            await self.audio_client.StopMusic(audio_pb2.StopMusicRequest(track_id=""))
            self.music_track_id = None
            logger.info("Game music stopped")

        except Exception as e:
            logger.warning(f"Failed to stop game music: {e}")

    def _get_music_change_time(self) -> float:
        """
        Calculate time of next tempo change based on game progression.

        As more players die, tempo changes become more frequent.
        Returns absolute time (game_clock.now() + delay).
        """
        return game_clock.now() + self._decide_next_change_delay()

    def _decide_next_change_delay(self) -> float:
        """Pacing rule: the random-window schedule (relative delay in seconds).

        A ``_rng.uniform`` draw within the (game-progression-lerped, F6-swappable)
        ``self.music_windows``. The only pacing source since the dead "agent"
        tempo mode was removed (#2361); the agent shapes tempo through the
        ``music_tempo_override`` and ``tempo_ramp`` interventions instead.
        """
        # Calculate game progression (0.0 = start, 1.0 = near end)
        min_moves = len(self.players) - 2
        if min_moves <= 0:
            min_moves = 1
        game_percent = min(1.0, self.dead_count / min_moves)

        # Interpolate between normal and end-game timing using the (init-frozen,
        # F6-swappable) per-instance windows. Read once into a local so a
        # concurrent F6 pacing_profile swap can't tear within a single change.
        windows = self.music_windows
        if self.speed_up:
            # Currently slow, will speed up - use slow timing
            min_t = self._lerp(windows.slow_min, windows.end_slow_min, game_percent)
            max_t = self._lerp(windows.slow_max, windows.end_slow_max, game_percent)
        else:
            # Currently fast, will slow down - use fast timing
            min_t = self._lerp(windows.fast_min, windows.end_fast_min, game_percent)
            max_t = self._lerp(windows.fast_max, windows.end_fast_max, game_percent)

        return self._rng.uniform(min_t, max_t)

    def _read_ramp_seconds(self, rising: bool) -> float:
        """Seconds for a full 0 -> 1 (``rising``) or 1 -> 0 swing (#1833).

        Read at each ramp start from ``game.tempo.ramp_up_seconds`` /
        ``game.tempo.ramp_down_seconds`` ("live per ramp", flags.md §4), never per
        frame. A flagd failure or a malformed value gives the default; the value is
        clamped to [RAMP_SECONDS_MIN, RAMP_SECONDS_MAX] (:func:`resolve_ramp_seconds`).
        """
        # Literal flag keys, so the contract scan resolves both reads.
        if rising:
            value = _base.read_float_flag(
                "game", "tempo.ramp_up_seconds", DEFAULT_RAMP_UP_SECONDS, game_id=self.game_id
            )
            return resolve_ramp_seconds(value, DEFAULT_RAMP_UP_SECONDS)
        value = _base.read_float_flag(
            "game", "tempo.ramp_down_seconds", DEFAULT_RAMP_DOWN_SECONDS, game_id=self.game_id
        )
        return resolve_ramp_seconds(value, DEFAULT_RAMP_DOWN_SECONDS)

    async def _start_intensity_ramp(self, target: float, *, scheduled: bool) -> None:
        """Start an intensity segment toward ``target`` from the intensity now (#1833, UC-031 BR-005).

        The segment lasts ``|target - i(now)|`` times the flag for its direction, so
        a half swing takes half the time and a change of target mid-ramp restarts
        from where the intensity is (no jump). The intensity is updated BEFORE the
        audio send, so thresholds never wait for the audio service.

        ``scheduled`` (the natural slow/fast schedule) flips ``speed_up`` and sets
        the next change ``window`` after the ramp END (as the original JoustMania
        did); an override leaves the schedule state alone.
        """
        now = game_clock.now()
        old_intensity = self._intensity.value_at(now)
        target = max(0.0, min(1.0, target))
        delta = target - old_intensity
        duration = abs(delta) * self._read_ramp_seconds(rising=delta > 0) if delta else 0.0
        self._intensity = IntensityRamp(start=old_intensity, target=target, start_time=now, duration=duration)

        if scheduled:
            # Flip first: the default rule's window depends on the NEXT direction.
            self.speed_up = not self.speed_up
            self.change_time = now + duration + self._decide_next_change_delay()

        old_tempo = tempo_from_intensity(old_intensity)
        new_tempo = tempo_from_intensity(target)
        if delta > 0:
            direction = "speed_up"
        elif delta < 0:
            direction = "slow_down"
        else:
            direction = "hold"

        # Child span of game.cycle; the ChangeTempo call inherits it, so the audio
        # service span links back to the game trace.
        with _base.tracer.start_as_current_span("game.music.tempo_change") as span:
            span.set_attribute("old_tempo", old_tempo)
            span.set_attribute("new_tempo", new_tempo)
            span.set_attribute("dead_count", self.dead_count)
            span.set_attribute("direction", direction)
            span.set_attribute("transition_seconds", duration)
            await self._push_audio(target, duration)

        logger.info(f"Tempo intensity {old_intensity:.2f} -> {target:.2f} over {duration:.2f}s ({direction})")

        # Add timeline marker event on gameplay_span (visible in trace overview)
        if self.gameplay_span:
            self.gameplay_span.add_event(
                "music_tempo_change",
                attributes={
                    "old_tempo": old_tempo,
                    "new_tempo": new_tempo,
                    "direction": direction,
                },
            )

        self._emit_music_tempo_metric()
        self._emit_threshold_metrics()
        if scheduled:
            logger.debug(f"Next tempo change at +{self._next_change_in():.1f}s")

    async def _push_audio(self, target: float, remaining: float, *, after_outage: bool = False) -> str:
        """Send one ChangeTempo glide toward ``target`` over ``remaining`` seconds (#1833).

        Skipped (nothing to keep in sync) without an audio client or a playing track
        (none, failed PlayMusic, or ``muted``): the intensity moves anyway (D1).
        An error or a timeout marks the audio unsynced, so the music loop resends
        the remaining glide after a back-off (0.5 s doubling to 5 s). A refusal
        (``success=False``: the track was replaced, stopped, or the service
        restarted) drops the track; the music loop starts the music again
        (UC-031 A7), for refusals outside an outage at most
        ``MUSIC_REFUSAL_RESTARTS_MAX`` times per game.

        Returns ``accepted``, ``refused``, ``failed`` or ``skipped`` (no track).
        """
        if not self.audio_client or self.music_track_id in (None, MUTED_TRACK_ID):
            self._audio_synced = True
            return "skipped"

        from proto import audio_pb2

        request = audio_pb2.ChangeTempoRequest(
            track_id=self.music_track_id,
            new_tempo=tempo_from_intensity(target),
            transition_duration=max(remaining, MIN_GLIDE_SECONDS),
        )
        try:
            response = await asyncio.wait_for(
                self.audio_client.ChangeTempo(request), timeout=AUDIO_TEMPO_RPC_TIMEOUT_SECONDS
            )
        except Exception as e:
            if self._audio_synced or self._audio_retry_backoff >= AUDIO_TEMPO_RETRY_MAX_SECONDS:
                # First failure of a streak, then once per maximum back-off: no log flood.
                logger.warning(f"Failed to change music tempo (intensity continues, resending): {e!r}")
            self._audio_synced = False
            self._audio_down = True
            self._audio_retry_at = game_clock.now() + self._audio_retry_backoff
            self._audio_retry_backoff = min(self._audio_retry_backoff * 2, AUDIO_TEMPO_RETRY_MAX_SECONDS)
            return "failed"

        self._audio_retry_backoff = AUDIO_TEMPO_RETRY_MIN_SECONDS
        self._audio_synced = True
        if response is not None and not response.success:
            track = self.music_track_id
            self.music_track_id = None
            outage = after_outage or self._audio_down
            if outage or self._music_refusal_restarts < MUSIC_REFUSAL_RESTARTS_MAX:
                if not outage:
                    self._music_refusal_restarts += 1
                self._music_lost = self._music_wanted
                logger.warning(
                    f"Audio refused tempo change for track {track} ({response.error}); starting the game music again"
                )
            else:
                logger.warning(
                    f"Audio refused tempo change for track {track} ({response.error}); "
                    "no further tempo changes are sent for it"
                )
            return "refused"
        return "accepted"

    async def _apply_ramp_tempo(self, target_tempo: float) -> None:
        """Apply one interpolated step of an ``ease`` ramp_tempo curve (#1117).

        The ease curve keeps its per-tick staircase: each 100 ms tick pins the
        intensity at the curve's value and sends a short glide over about one tick
        (RAMP_TEMPO_STEP_TRANSITION), not a full ramp per step. It does NOT flip
        ``speed_up`` or reset ``change_time``, so the natural schedule resumes from
        the held intensity once the ramp is reverted.
        """
        old_tempo = self.music_speed
        self._intensity = IntensityRamp.hold(intensity_from_tempo(target_tempo), game_clock.now())
        with _base.tracer.start_as_current_span("game.music.tempo_ramp") as span:
            span.set_attribute("old_tempo", old_tempo)
            span.set_attribute("new_tempo", target_tempo)
            span.set_attribute("dead_count", self.dead_count)
            await self._push_audio(self._intensity.target, RAMP_TEMPO_STEP_TRANSITION)

        self._emit_music_tempo_metric()
        self._emit_threshold_metrics()

    async def _adopt_linear_tempo_ramp(self, ramp: RampDescriptor) -> None:
        """Turn a linear ramp_tempo descriptor into ONE intensity segment and one glide (#1833).

        The segment starts at the intensity NOW and ends at the descriptor's end
        time, so a descriptor adopted late (armed under an override that has
        since cleared) never makes the thresholds or the music jump (BR-005). It
        never runs faster than the bounded rate of step 3: when the descriptor's
        end is already near or past, the segment takes |delta i| x the flag for
        its direction instead.
        """
        now = game_clock.now()
        old_intensity = self._intensity.value_at(now)
        target = intensity_from_tempo(ramp.target_tempo)
        delta = target - old_intensity
        bounded = abs(delta) * self._read_ramp_seconds(rising=delta > 0) if delta else 0.0
        remaining = max(ramp.start_time + ramp.duration - now, bounded, 0.0)
        self._intensity = IntensityRamp(start=old_intensity, target=target, start_time=now, duration=remaining)
        with _base.tracer.start_as_current_span("game.music.tempo_ramp") as span:
            span.set_attribute("old_tempo", tempo_from_intensity(old_intensity))
            span.set_attribute("new_tempo", ramp.target_tempo)
            span.set_attribute("dead_count", self.dead_count)
            await self._push_audio(target, remaining)
        self._emit_music_tempo_metric()
        self._emit_threshold_metrics()

    async def _hold_intensity_now(self) -> None:
        """End an agent-owned segment: hold the intensity reached now and stop the glide there."""
        self._agent_tempo_owned = False
        now = game_clock.now()
        held = self._intensity.value_at(now)
        if not self._intensity.is_complete(now):
            logger.info(f"Agent tempo control ended mid-ramp: holding intensity {held:.2f}")
            self._intensity = IntensityRamp.hold(held, now)
            await self._push_audio(held, MIN_GLIDE_SECONDS)
            self._emit_music_tempo_metric()
            self._emit_threshold_metrics()

    async def _check_music_speed(self):
        """Drive the tempo intensity and keep the music in sync (Phase 70, #1833).

        Called every 100 ms from the music loop; the single tempo owner. Order:
        agent override, then ramp_tempo, then the natural schedule. Runs without a
        music track too (UC-031 BR-005, D1): only the audio send is skipped.
        Every audio failure is handled in :meth:`_push_audio`; this never raises
        for an audio error.
        """
        now = game_clock.now()

        if self.tempo_override is not None:
            # Agent tempo override: ramp to the override at the bounded
            # rate and hold; the natural schedule is suspended (speed_up and
            # change_time untouched), and resumes from the held intensity when the
            # override clears. A new override value restarts from i(now).
            self._agent_tempo_owned = True
            self._adopted_tempo_ramp = None
            override_target = intensity_from_tempo(self.tempo_override)
            if abs(self._intensity.target - override_target) >= 1e-6:
                await self._start_intensity_ramp(override_target, scheduled=False)
        elif self.tempo_ramp is not None:
            # #1117 ramp_tempo CURVE: a linear curve is one intensity segment (one
            # glide per descriptor, detected by identity); an ease curve steps per
            # tick. change_time is left untouched (mirrors the override).
            self._agent_tempo_owned = True
            ramp = self.tempo_ramp
            if ramp.curve == RAMP_CURVE_LINEAR:
                if ramp is not self._adopted_tempo_ramp:
                    self._adopted_tempo_ramp = ramp
                    await self._adopt_linear_tempo_ramp(ramp)
            else:
                self._adopted_tempo_ramp = None
                desired = ramp.tempo_at(now)
                if abs(self.music_speed - desired) > 1e-3:
                    await self._apply_ramp_tempo(desired)
        else:
            self._adopted_tempo_ramp = None
            if self._agent_tempo_owned:
                # The override cleared or the ramp was reverted (A3): stop the agent's
                # segment where it is now, so a reverted action stops acting at once.
                # The schedule resumes from the held intensity (at once if due).
                await self._hold_intensity_now()
            # Natural schedule: the next change is due once its window has passed
            # AND the previous segment has finished.
            if now >= self.change_time and self._intensity.is_complete(now):
                await self._start_intensity_ramp(1.0 if self.speed_up else 0.0, scheduled=True)

        # Resend the remaining glide after a failed send (back-off in _push_audio).
        now = game_clock.now()
        if not self._audio_synced and now >= self._audio_retry_at:
            await self._push_audio(self._intensity.target, self._intensity.end_time - now)

        await self._keep_music_alive()

        # Gauges follow the intensity while a ramp runs, plus once after it ends.
        ramp_running = not self._intensity.is_complete(now)
        if ramp_running or self._ramp_metrics_open:
            self._emit_music_tempo_metric()
            self._emit_threshold_metrics()
        self._ramp_metrics_open = ramp_running

    async def _keep_music_alive(self) -> None:
        """Start the game music again after the audio service lost it (UC-031 A7, BR-009).

        Once per ``AUDIO_PROBE_INTERVAL_SECONDS`` while the game music should play.
        After an outage the first answer is followed by a glide for the track: an
        accepted glide means the track survived (a slow service, not a restart).
        A refused glide, or a track that was lost already, restarts the music at the
        game's volume and the tempo of the intensity now.
        """
        if not self._music_wanted or not self.audio_client:
            return
        now = game_clock.now()
        if now < self._audio_probe_at:
            return
        self._audio_probe_at = now + AUDIO_PROBE_INTERVAL_SECONDS

        status = await self._probe_audio()
        if status == PROBE_DOWN:
            if not self._audio_down:
                logger.warning("Audio service unreachable during the game; the music starts again when it is back")
            self._audio_down = True
            return
        if status == PROBE_OK and not self._audio_down and not self._music_lost:
            return

        if self._audio_down or status != PROBE_OK:
            self._audio_down = False
            logger.info("Audio service answers again: checking the game music")
            if not self._music_lost and self.music_track_id not in (None, MUTED_TRACK_ID):
                outcome = await self._push_audio(
                    self._intensity.target, self._intensity.end_time - game_clock.now(), after_outage=True
                )
                if outcome in ("failed", "accepted"):
                    return
        if self._music_lost:
            await self._restart_game_music()

    async def _probe_audio(self) -> str:
        """``PROBE_OK`` / ``PROBE_DOWN`` / ``PROBE_RECONNECTED`` from the session's audio probe."""
        probe = getattr(self, "audio_health_client", None)
        if probe is None:
            return PROBE_OK
        try:
            return await probe.check()
        except Exception as e:
            logger.debug(f"Audio probe failed: {e!r}")
            return PROBE_DOWN

    async def _restart_game_music(self) -> None:
        """Send the music start again at the game's volume and the tempo of the intensity now."""
        now = game_clock.now()
        tempo = tempo_from_intensity(self._intensity.value_at(now))
        volume = self._audio_volume if self._audio_volume is not None else self.game_volume
        if not await self._send_music_start(volume, tempo):
            return
        logger.info(f"Game music started again: {self.music_track_id} at {tempo:.2f}x")
        if not self._intensity.is_complete(now):
            await self._push_audio(self._intensity.target, self._intensity.end_time - now)

    def _log_tick_failure(self, error: Exception) -> None:
        """Warn about a failed tempo tick: first, on a new error text, then every interval."""
        text = repr(error)
        now = game_clock.now()
        if text != self._tick_error_text:
            self._tick_error_suppressed = 0  # a stale count must not leak into a new error
        if text == self._tick_error_text and now - self._tick_error_logged_at < MUSIC_TICK_LOG_INTERVAL_SECONDS:
            self._tick_error_suppressed += 1
            return
        suffix = f" ({self._tick_error_suppressed} similar suppressed)" if self._tick_error_suppressed else ""
        logger.warning(f"Tempo tick failed, retrying next tick: {text}{suffix}")
        self._tick_error_text = text
        self._tick_error_logged_at = now
        self._tick_error_suppressed = 0

    async def _music_loop(self):
        """
        Background task to manage music tempo changes (Phase 70).

        Runs alongside the main game loop and periodically checks
        if tempo should change based on game progression.

        Sets game.cycle as active context so each game.music.tempo_change span
        appears directly under game.cycle in traces.
        """
        logger.info("Music loop started")

        try:
            token = otel_context.attach(self.game_cycle_context) if self.game_cycle_context else None
            try:
                while self.running:
                    try:
                        await self._check_music_speed()
                    except Exception as e:
                        # A non-audio error (pacing seam, metrics, tracing) must not end
                        # the tempo loop for the rest of the game: log it, retry next tick.
                        self._log_tick_failure(e)
                    await game_clock.sleep(0.1)  # Check every 100ms
            finally:
                if token is not None:
                    otel_context.detach(token)
        except asyncio.CancelledError:
            logger.info("Music loop cancelled")
            raise  # Re-raise to properly propagate cancellation
        except Exception as e:
            logger.warning(f"Music loop error: {e}")
        finally:
            logger.info("Music loop ended")
