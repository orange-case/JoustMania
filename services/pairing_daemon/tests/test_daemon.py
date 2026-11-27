"""Tests for psmove_pairing.daemon module."""

import time
from unittest.mock import AsyncMock, create_autospec, patch

import pytest

from psmove_pairing.daemon import _HEALTH_STALENESS_THRESHOLD, PairingDaemon

from .conftest import MockCommandRunner


@pytest.fixture
def daemon(mock_tracer):
    """Provide PairingDaemon instance for tests."""
    return PairingDaemon(mock_tracer)


class TestValidatePrerequisites:
    """Tests for validate_prerequisites()."""

    @pytest.mark.asyncio
    async def test_all_prerequisites_pass(self, daemon):
        """Test when all prerequisites are available."""
        runner = MockCommandRunner()
        runner.add_response(["bluetoothctl", "show"], (0, "Controller ..."))

        with patch("psmove_pairing.daemon.run_command", runner):
            with patch("psmove_pairing.daemon.shutil.which", return_value="/usr/bin/bluetoothctl"):
                result = await daemon.validate_prerequisites()
                assert result is True

    @pytest.mark.asyncio
    async def test_bluetoothctl_not_found(self, daemon):
        """Test when bluetoothctl is not found."""
        runner = MockCommandRunner()

        def mock_which(cmd):
            if cmd == "bluetoothctl":
                return None
            return f"/usr/bin/{cmd}"

        with patch("psmove_pairing.daemon.run_command", runner):
            with patch("psmove_pairing.daemon.shutil.which", side_effect=mock_which):
                result = await daemon.validate_prerequisites()
                assert result is False

    @pytest.mark.asyncio
    async def test_hciconfig_not_found(self, daemon):
        """Test when hciconfig is not found."""
        runner = MockCommandRunner()
        runner.add_response(["bluetoothctl", "show"], (0, "Controller ..."))

        def mock_which(cmd):
            if cmd == "hciconfig":
                return None
            return f"/usr/bin/{cmd}"

        with patch("psmove_pairing.daemon.run_command", runner):
            with patch("psmove_pairing.daemon.shutil.which", side_effect=mock_which):
                result = await daemon.validate_prerequisites()
                assert result is False


class TestIsHealthy:
    """Tests for is_healthy()."""

    def test_healthy_during_startup(self, daemon):
        """Test that daemon is healthy during startup grace period."""
        assert daemon.is_healthy() is True

    def test_healthy_after_polls(self, daemon):
        """Test daemon is healthy after both loops have run."""
        daemon.update_usb_poll_timestamp()
        daemon.update_bt_monitor_timestamp()
        daemon._startup_time = time.time() - _HEALTH_STALENESS_THRESHOLD - 10
        assert daemon.is_healthy() is True

    def test_unhealthy_when_usb_stale(self, daemon):
        """Test daemon is unhealthy when USB poll is stale."""
        daemon._startup_time = time.time() - _HEALTH_STALENESS_THRESHOLD - 10
        daemon.update_bt_monitor_timestamp()
        daemon._last_usb_poll = time.time() - _HEALTH_STALENESS_THRESHOLD - 10
        assert daemon.is_healthy() is False

    def test_unhealthy_when_bt_stale(self, daemon):
        """Test daemon is unhealthy when Bluetooth monitor is stale."""
        daemon._startup_time = time.time() - _HEALTH_STALENESS_THRESHOLD - 10
        daemon.update_usb_poll_timestamp()
        daemon._last_bt_monitor = time.time() - _HEALTH_STALENESS_THRESHOLD - 10
        assert daemon.is_healthy() is False


class TestGetHealthStatus:
    """Tests for get_health_status()."""

    def test_status_includes_all_fields(self, daemon):
        """Test health status includes all expected fields."""
        status = daemon.get_health_status()
        assert "healthy" in status
        assert "uptime_seconds" in status
        assert "last_usb_poll_seconds_ago" in status
        assert "last_bt_monitor_seconds_ago" in status
        assert "usb_poll_count" in status
        assert "bt_monitor_count" in status

    def test_status_reflects_health(self, daemon):
        """Test health status reflects actual health."""
        daemon.update_usb_poll_timestamp()
        daemon.update_bt_monitor_timestamp()
        status = daemon.get_health_status()
        assert status["healthy"] is True

    def test_status_tracks_poll_counts(self, daemon):
        """Test health status tracks poll counts."""
        initial_status = daemon.get_health_status()
        assert initial_status["usb_poll_count"] == 0
        assert initial_status["bt_monitor_count"] == 0


class TestTimestampUpdates:
    """Tests for timestamp update methods."""

    def test_update_usb_poll_timestamp(self, daemon):
        """Test USB poll timestamp is updated."""
        assert daemon._last_usb_poll == 0.0
        daemon.update_usb_poll_timestamp()
        assert daemon._last_usb_poll > 0

    def test_update_bt_monitor_timestamp(self, daemon):
        """Test Bluetooth monitor timestamp is updated."""
        assert daemon._last_bt_monitor == 0.0
        daemon.update_bt_monitor_timestamp()
        assert daemon._last_bt_monitor > 0


class TestRun:
    """Tests for run()."""

    @pytest.mark.asyncio
    async def test_run_starts_both_loops(self, daemon):
        """Test that run starts both USB and BT loops."""
        import asyncio

        usb_loop_started = False
        bt_loop_started = False

        async def mock_usb_loop():
            nonlocal usb_loop_started
            usb_loop_started = True
            raise asyncio.CancelledError()

        async def mock_bt_loop():
            nonlocal bt_loop_started
            bt_loop_started = True
            raise asyncio.CancelledError()

        daemon._usb_poll_loop = mock_usb_loop
        daemon._bt_monitor_loop = mock_bt_loop

        runner = MockCommandRunner()
        runner.add_response(["bluetoothctl", "show"], (0, "ok"))

        with patch("psmove_pairing.daemon.run_command", runner):
            with patch("psmove_pairing.daemon.shutil.which", return_value="/usr/bin/tool"):
                try:
                    await daemon.run()
                except asyncio.CancelledError:
                    pass

        assert usb_loop_started
        assert bt_loop_started

    @pytest.mark.uc("UC-001", flow="main", steps=[7], br=["BR-010"])
    async def test_shutdown_ends_the_led_cues(self, daemon):
        """#2091: cancelling the daemon cancels the LED cue tasks on the controllers on USB."""
        import asyncio

        from psmove_pairing.usb_led import UsbLedCue

        async def run_forever():
            await asyncio.Event().wait()

        daemon._usb_poll_loop = run_forever
        daemon._bt_monitor_loop = run_forever
        daemon.usb_pairing.led_cue = create_autospec(UsbLedCue, instance=True)

        with (
            patch.object(daemon, "validate_prerequisites", AsyncMock(return_value=True)),
            patch("psmove_pairing.daemon.register_pairing_agent", AsyncMock()),
        ):
            task = asyncio.create_task(daemon.run())
            await asyncio.sleep(0.01)
            task.cancel()
            with pytest.raises(asyncio.CancelledError):
                await task

        daemon.usb_pairing.led_cue.close.assert_awaited_once()


async def _run_until_signal(daemon, sig, when_started):
    """Run daemon.run() with the loop's signal handlers captured, fire ``sig`` once ``when_started()``."""
    import asyncio

    loop = asyncio.get_running_loop()
    handlers = {}
    with (
        patch.object(loop, "add_signal_handler", side_effect=lambda s, cb, *a: handlers.__setitem__(s, (cb, a))),
        patch.object(loop, "remove_signal_handler", side_effect=lambda s: handlers.pop(s, None) is not None),
    ):
        task = asyncio.create_task(daemon.run())
        for _ in range(200):
            if sig in handlers and when_started():
                break
            await asyncio.sleep(0.01)
        callback, args = handlers[sig]
        callback(*args)
        await asyncio.wait_for(task, timeout=2)
    return handlers, task


class TestSignalShutdown:
    """#2941: the daemon is PID 1, which has no default SIGTERM action."""

    @pytest.mark.parametrize("sig_name", ["SIGTERM", "SIGINT"])
    @pytest.mark.uc("UC-001", flow="main", steps=[7], br=["BR-010"])
    async def test_signal_stops_the_loops_and_returns(self, daemon, sig_name):
        """A SIGTERM (docker stop) cancels both loops, ends the LED cues and run() returns normally."""
        import asyncio
        import signal

        from psmove_pairing.usb_led import UsbLedCue

        sig = getattr(signal, sig_name)
        loops_running = asyncio.Event()

        async def run_forever():
            loops_running.set()
            await asyncio.Event().wait()

        daemon._usb_poll_loop = run_forever
        daemon._bt_monitor_loop = run_forever
        daemon.usb_pairing.led_cue = create_autospec(UsbLedCue, instance=True)

        with (
            patch.object(daemon, "validate_prerequisites", AsyncMock(return_value=True)),
            patch("psmove_pairing.daemon.register_pairing_agent", AsyncMock()),
        ):
            handlers, task = await _run_until_signal(daemon, sig, loops_running.is_set)

        daemon.usb_pairing.led_cue.close.assert_awaited_once()
        assert handlers == {}, "the signal handlers are removed again"
        assert task.cancelling() == 0

    @pytest.mark.uc("UC-001", br=["BR-010"])
    async def test_sigterm_during_boot_stops_before_the_loops(self, daemon):
        """A SIGTERM while the pairing agent registers stops the daemon; the loops never start."""
        import asyncio
        import signal

        registering = asyncio.Event()

        async def slow_register():
            registering.set()
            await asyncio.Event().wait()

        usb_loop = AsyncMock()
        daemon._usb_poll_loop = usb_loop
        with (
            patch.object(daemon, "validate_prerequisites", AsyncMock(return_value=True)),
            patch("psmove_pairing.daemon.register_pairing_agent", side_effect=slow_register),
        ):
            await _run_until_signal(daemon, signal.SIGTERM, registering.is_set)

        usb_loop.assert_not_called()

    async def test_cancel_without_a_signal_still_propagates(self, daemon):
        """An outside cancel (no signal) is not swallowed."""
        import asyncio

        async def run_forever():
            await asyncio.Event().wait()

        daemon._usb_poll_loop = run_forever
        daemon._bt_monitor_loop = run_forever
        with (
            patch.object(daemon, "validate_prerequisites", AsyncMock(return_value=True)),
            patch("psmove_pairing.daemon.register_pairing_agent", AsyncMock()),
            patch.object(daemon.usb_pairing.led_cue, "close", AsyncMock()),
        ):
            task = asyncio.create_task(daemon.run())
            await asyncio.sleep(0.01)
            task.cancel()
            with pytest.raises(asyncio.CancelledError):
                await task


@pytest.mark.uc("UC-001", br=["BR-009"])
def test_pairing_agent_answers_for_the_daemons_usb_pairings(daemon):
    from psmove_pairing.bluez_agent import agent

    assert agent.is_known == daemon.usb_pairing.paired_this_session


@pytest.mark.uc("UC-001", br=["BR-009"])
async def test_monitor_cycle_makes_sure_the_agent_is_registered(daemon):
    """Every Bluetooth monitor cycle re-registers the agent if BlueZ has not confirmed it, also after an error."""
    import asyncio
    from unittest.mock import AsyncMock

    daemon.bt_monitor.monitor = AsyncMock(side_effect=RuntimeError("monitor failed"))
    ensure = AsyncMock(return_value=False)
    with (
        patch("psmove_pairing.daemon.ensure_pairing_agent", ensure),
        patch("psmove_pairing.daemon.asyncio.sleep", AsyncMock(side_effect=[None, asyncio.CancelledError])),
        pytest.raises(asyncio.CancelledError),
    ):
        await daemon._bt_monitor_loop()

    assert ensure.await_count == 2


class TestStartupAndStaleness:
    """#1770: bounded startup commands, sane staleness before the first poll."""

    @pytest.mark.asyncio
    async def test_bluetoothctl_check_has_short_timeout(self, daemon):
        """bluetoothctl show must not use run_command's 60 s default."""
        seen = {}

        async def fake_run(cmd, **kwargs):
            seen["timeout"] = kwargs.get("timeout_seconds", 60)
            return -1, "timeout"

        with patch("psmove_pairing.daemon.run_command", fake_run):
            with patch("psmove_pairing.daemon.shutil.which", return_value="/usr/bin/x"):
                assert await daemon.validate_prerequisites() is False
        assert seen["timeout"] <= 10

    @pytest.mark.asyncio
    async def test_run_command_honours_timeout(self):
        """A hanging command is killed after the given timeout, not 60 s."""
        from psmove_pairing.utils import run_command

        start = time.monotonic()
        code, output = await run_command(["sleep", "30"], timeout_seconds=0.2)
        assert (code, output) == (-1, "timeout")
        assert time.monotonic() - start < 5

    def test_stale_log_never_reports_epoch(self, daemon, caplog):
        """Before the first poll, staleness counts from daemon start."""
        daemon._startup_time = time.time() - _HEALTH_STALENESS_THRESHOLD - 10
        with caplog.at_level("WARNING", logger="psmove-pairing"):
            assert daemon.is_healthy() is False
        assert "never polled" in caplog.text
