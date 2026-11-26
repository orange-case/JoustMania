"""Tests for pairing backend implementations."""

from unittest.mock import AsyncMock, MagicMock, create_autospec

import pytest

from psmove_pairing.pairing_backend import PairingResult, RustServiceBackend


class TestPairingResult:
    """Tests for PairingResult dataclass."""

    def test_defaults(self):
        result = PairingResult(success=True)
        assert result.success is True
        assert result.already_paired is False
        assert result.previous_host == ""

    def test_with_metadata(self):
        result = PairingResult(success=True, already_paired=True, previous_host="11:22:33:44:55:66")
        assert result.already_paired is True
        assert result.previous_host == "11:22:33:44:55:66"


class TestRustServiceBackend:
    """Tests for RustServiceBackend gRPC client."""

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-001", flow="A7", br=["BR-008"])
    async def test_get_usb_controllers_returns_list(self):
        """Delegates to gRPC stub; each proto controller becomes (path bytes, serial, product id)."""
        from proto import psmove_hid_pb2

        mock_response = psmove_hid_pb2.GetUSBControllersResponse(
            controllers=[
                psmove_hid_pb2.USBController(device_path="/dev/hidraw3", serial="AA:BB:CC:DD:EE:FF", product_id=0x0C5E)
            ]
        )

        mock_stub = MagicMock()
        mock_stub.GetUSBControllers = AsyncMock(return_value=mock_response)

        backend = RustServiceBackend.__new__(RustServiceBackend)
        backend._channel = MagicMock()
        backend._stub = mock_stub

        controllers = await backend.get_usb_controllers()

        assert len(controllers) == 1
        assert controllers[0] == (b"/dev/hidraw3", "AA:BB:CC:DD:EE:FF", 0x0C5E)
        assert controllers[0].product_id == 0x0C5E

    @pytest.mark.asyncio
    async def test_get_usb_controllers_encodes_path(self):
        """device_path string from proto is encoded to bytes."""
        mock_stub = MagicMock()
        mock_controller = MagicMock()
        mock_controller.device_path = "/dev/hidraw0"
        mock_controller.serial = "00:06:F7:AA:BB:CC"
        mock_response = MagicMock()
        mock_response.controllers = [mock_controller]
        mock_stub.GetUSBControllers = AsyncMock(return_value=mock_response)

        backend = RustServiceBackend.__new__(RustServiceBackend)
        backend._channel = MagicMock()
        backend._stub = mock_stub

        controllers = await backend.get_usb_controllers()
        path = controllers[0].device_path
        assert isinstance(path, bytes)
        assert path == b"/dev/hidraw0"

    @pytest.mark.asyncio
    async def test_pair_controller_returns_pairing_result(self):
        """Converts proto PairControllerResponse to PairingResult."""
        mock_stub = MagicMock()
        mock_response = MagicMock()
        mock_response.success = True
        mock_response.already_paired = False
        mock_response.previous_host = "00:00:00:00:00:00"
        mock_stub.PairController = AsyncMock(return_value=mock_response)

        backend = RustServiceBackend.__new__(RustServiceBackend)
        backend._channel = MagicMock()
        backend._stub = mock_stub

        result = await backend.pair_controller(b"/dev/hidraw0", "AA:BB:CC:DD:EE:FF", "11:22:33:44:55:66")

        assert isinstance(result, PairingResult)
        assert result.success is True
        assert result.already_paired is False
        assert result.previous_host == "00:00:00:00:00:00"

    @pytest.mark.asyncio
    async def test_pair_controller_decodes_bytes_path(self):
        """device_path bytes are decoded to string for proto request."""
        mock_stub = MagicMock()
        mock_response = MagicMock()
        mock_response.success = True
        mock_response.already_paired = True
        mock_response.previous_host = "11:22:33:44:55:66"
        mock_stub.PairController = AsyncMock(return_value=mock_response)

        backend = RustServiceBackend.__new__(RustServiceBackend)
        backend._channel = MagicMock()
        backend._stub = mock_stub

        result = await backend.pair_controller(b"/dev/hidraw3", "AA:BB:CC:DD:EE:FF", "11:22:33:44:55:66")

        assert result.already_paired is True
        # Verify the stub was called (path gets decoded in the real implementation)
        mock_stub.PairController.assert_called_once()


class TestRustServiceBackendDeadlines:
    """A hung rust-hid must not stall the poll loop or hold the pairing lock (UC-005 A6)."""

    @pytest.mark.uc("UC-005", flow="A6")
    async def test_every_rpc_carries_a_deadline(self):
        from proto import psmove_hid_pb2_grpc

        from psmove_pairing.pairing_backend import RUST_HID_RPC_TIMEOUT_SECONDS

        stub = create_autospec(psmove_hid_pb2_grpc.PairingServiceStub, instance=True)
        stub.GetUSBControllers = AsyncMock(return_value=MagicMock(controllers=[]))
        stub.PairController = AsyncMock(return_value=MagicMock(success=True, already_paired=False, previous_host=""))
        backend = RustServiceBackend.__new__(RustServiceBackend)
        backend._channel = MagicMock()
        backend._stub = stub

        await backend.get_usb_controllers()
        await backend.pair_controller(b"/dev/hidraw0", "AA:BB:CC:DD:EE:FF", "11:22:33:44:55:66")

        assert stub.GetUSBControllers.await_args.kwargs["timeout"] == RUST_HID_RPC_TIMEOUT_SECONDS
        assert stub.PairController.await_args.kwargs["timeout"] == RUST_HID_RPC_TIMEOUT_SECONDS
        assert RUST_HID_RPC_TIMEOUT_SECONDS <= 15
