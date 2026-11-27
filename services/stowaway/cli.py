"""``stowaway run [--challenge NAME] ...``: the command line and the container entry (UC-106 BR-007)."""

from __future__ import annotations

import argparse
import asyncio
import logging
import os
import signal
import sys

from services.stowaway.config import CHALLENGES, MODES, Overrides, Settings, SettingsSource

logger = logging.getLogger("stowaway")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="stowaway", description="Synthetic load of the Orange Case (UC-106).")
    sub = parser.add_subparsers(dest="command", required=True)
    run = sub.add_parser("run", help="generate the load; flags decide what a missing option does")
    run.add_argument("--challenge", choices=CHALLENGES, help="overrides stowaway.challenge")
    run.add_argument("--mode", choices=MODES, help="overrides stowaway.mode")
    run.add_argument("--intensity", type=float, help="overrides stowaway.intensity (clamped to the ceiling)")
    run.add_argument("--span-target", type=int, help="overrides stowaway.span_target")
    run.add_argument("--endpoint", help="OTLP gRPC endpoint for development (default: the case's collector)")
    run.add_argument(
        "--no-flagd",
        action="store_true",
        help="development only, needs --endpoint: ignore flagd, the options apply and Ctrl-C is the kill switch",
    )
    return parser


def parse(argv: list[str] | None = None) -> argparse.Namespace:
    parser = build_parser()
    args = parser.parse_args(argv)
    if args.no_flagd and not args.endpoint:
        parser.error("--no-flagd is only allowed with --endpoint (the case's kill switch is a flag)")
    return args


def settings_source(args: argparse.Namespace) -> SettingsSource:
    overrides = Overrides(args.challenge, args.mode, args.intensity, args.span_target)
    if args.no_flagd:
        return SettingsSource(read=lambda: Settings(enabled=True), overrides=overrides)
    return SettingsSource(overrides=overrides)


async def run(args: argparse.Namespace) -> None:
    from lib.grpc_utils import create_channel
    from lib.otel_metrics import init_metrics
    from proto import controller_manager_mock_pb2_grpc, game_coordinator_pb2_grpc
    from services.stowaway.challenges import build_generators
    from services.stowaway.runner import Stowaway
    from services.stowaway.synthetic import init_providers

    tracer_provider, meter_provider = init_providers(args.endpoint)
    # The delivered count is unmarked, so it reaches VictoriaMetrics (telemetry.md §3.5).
    init_metrics(set_global=False)
    coordinator_addr = (
        f"{os.getenv('GAME_COORDINATOR_HOST', 'game-coordinator')}:{os.getenv('GAME_COORDINATOR_PORT', '50053')}"
    )
    mock_addr = os.getenv("MOCK_CONTROLLER_ADDR", "controller-manager:50062")
    coordinator_channel = create_channel(coordinator_addr, enable_tracing=False, enable_metrics=False)
    mock_channel = create_channel(mock_addr, enable_tracing=False, enable_metrics=False)
    stowaway = Stowaway(
        settings_source(args),
        game_coordinator_pb2_grpc.GameCoordinatorServiceStub(coordinator_channel),
        build_generators(),
        mock=controller_manager_mock_pb2_grpc.MockControllerServiceStub(mock_channel),
    )
    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGTERM, signal.SIGINT):
        loop.add_signal_handler(sig, stop.set)
    logger.info("stowaway started (coordinator %s, mock %s)", coordinator_addr, mock_addr)
    try:
        await stowaway.run(stop)
    finally:
        await coordinator_channel.close()
        await mock_channel.close()
        tracer_provider.shutdown()
        meter_provider.shutdown()
        logger.info("stowaway stopped")


def main(argv: list[str] | None = None) -> int:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
    os.environ.setdefault("OTEL_SERVICE_NAME", "stowaway")
    args = parse(argv)
    asyncio.run(run(args))
    return 0


if __name__ == "__main__":
    sys.exit(main())
