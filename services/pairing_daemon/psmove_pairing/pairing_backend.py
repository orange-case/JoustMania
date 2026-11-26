"""Pairing backend abstraction for PS Move controllers.

Defines a PairingBackend protocol and its implementation, the rust-hid gRPC
PairingService (UC-001 BR-003).
"""

from __future__ import annotations

import logging
from dataclasses import dataclass
from typing import NamedTuple, Protocol, runtime_checkable

logger = logging.getLogger("psmove-pairing")


class UsbController(NamedTuple):
    """A PS Move on USB, as enumerated by a pairing backend."""

    device_path: bytes  # opaque identifier for pair_controller()
    serial: str  # controller Bluetooth address
    product_id: int = 0  # USB product id; 0 = not reported (older rust-hid)


@dataclass
class PairingResult:
    """Result of a pairing operation, returned by PairingBackend.pair_controller().

    Carries success status plus metadata for observability (span attributes).
    """

    success: bool
    already_paired: bool = False
    previous_host: str = ""


@runtime_checkable
class PairingBackend(Protocol):
    """Protocol for pairing backend implementations.

    Each backend handles USB controller enumeration and BT address
    writing using a different I/O strategy. The USBPairing class
    delegates to whichever backend is selected by feature flags.
    """

    async def get_usb_controllers(self) -> list[UsbController]:
        """Enumerate USB-connected PS Move controllers.

        Returns:
            One UsbController (device_path, serial, product_id) per controller.
            device_path is an opaque identifier used by pair_controller().
        """
        ...

    async def pair_controller(self, device_path: bytes, serial: str, adapter_address: str) -> PairingResult:
        """Write the host BT address to a controller.

        Args:
            device_path: Opaque device identifier from get_usb_controllers().
            serial: Controller MAC address.
            adapter_address: Target Bluetooth adapter address to write.

        Returns:
            PairingResult with success status and metadata.
        """
        ...


# gRPC deadline for rust-hid calls: a hung service must not stall the poll loop
# or hold the pairing lock (UC-005 A6)
RUST_HID_RPC_TIMEOUT_SECONDS = 15.0


class RustServiceBackend:
    """Pairing backend that delegates to the Rust HID gRPC service.

    Connects to the rust-hid service (port 50058) for USB controller
    enumeration and BT address writing. The Rust service handles HID
    device I/O directly.
    """

    def __init__(self):
        import os

        from proto import psmove_hid_pb2_grpc

        from lib.grpc_utils import create_channel

        host = os.getenv("RUST_HID_HOST", "localhost")
        port = os.getenv("RUST_HID_PORT", "50058")
        self._channel = create_channel(f"{host}:{port}")
        self._stub = psmove_hid_pb2_grpc.PairingServiceStub(self._channel)

    async def get_usb_controllers(self) -> list[UsbController]:
        """Enumerate USB-connected PS Move controllers via Rust HID service."""
        from proto import psmove_hid_pb2

        response = await self._stub.GetUSBControllers(
            psmove_hid_pb2.GetUSBControllersRequest(), timeout=RUST_HID_RPC_TIMEOUT_SECONDS
        )
        return [UsbController(c.device_path.encode(), c.serial, c.product_id) for c in response.controllers]

    async def pair_controller(self, device_path: bytes, serial: str, adapter_address: str) -> PairingResult:
        """Write the host BT address via Rust HID service."""
        from proto import psmove_hid_pb2

        path_str = device_path.decode() if isinstance(device_path, bytes) else device_path
        response = await self._stub.PairController(
            psmove_hid_pb2.PairControllerRequest(
                device_path=path_str,
                serial=serial,
                adapter_address=adapter_address,
            ),
            timeout=RUST_HID_RPC_TIMEOUT_SECONDS,
        )
        return PairingResult(
            success=response.success,
            already_paired=response.already_paired,
            previous_host=response.previous_host,
        )
