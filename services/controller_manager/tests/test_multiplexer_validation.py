"""
Unit tests for multiplexer backend combination validation.

Tests that only safe backend combinations are allowed:
- Single backends: mock, python, rust
- Dual/triple: mock+python, mock+rust, python+rust, mock+python+rust
- Rejects: duplicates, unknown backends
"""

import sys
from pathlib import Path

import pytest

# Setup paths for imports
test_dir = Path(__file__).parent
service_dir = test_dir.parent
project_root = service_dir.parent.parent
sys.path.insert(0, str(project_root))

from services.controller_manager.multiplexer.validation import validate_backend_combination


class TestValidSingleBackends:
    """Single backends should always be valid."""

    @pytest.mark.parametrize("name", ["mock", "rust"])
    def test_single_backend_passes(self, name):
        validate_backend_combination([name])


class TestValidMultiBackends:
    """Valid multi-backend combinations."""

    def test_mock_rust(self):
        validate_backend_combination(["mock", "rust"])

    def test_rejects_removed_python_backend(self):
        with pytest.raises(ValueError, match="Unsupported backend combination"):
            validate_backend_combination(["mock", "python"])

    def test_order_does_not_matter(self):
        validate_backend_combination(["rust", "mock"])


class TestInvalidCombinations:
    """Conflicting backend combinations should raise ValueError."""

    def test_rejects_duplicate_rust(self):
        with pytest.raises(ValueError, match="Unsupported backend combination"):
            validate_backend_combination(["rust", "rust"])

    def test_rejects_unknown_backend(self):
        with pytest.raises(ValueError, match="Unsupported backend combination"):
            validate_backend_combination(["mock", "windows"])

    def test_rejects_bluetooth(self):
        with pytest.raises(ValueError, match="Unsupported backend combination"):
            validate_backend_combination(["bluetooth"])
