"""
Unit tests for compute_image_hash() — auto-computed image fingerprint.

Spec reference: §3.1.4 (image_hash field in attestation bundle)
Acceptance criteria §12: deterministic, sensitive to changes, symlinks ignored.
"""

import hashlib
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import MagicMock, patch

sys.modules["common"] = MagicMock()
sys.modules["common.logger"] = MagicMock()
sys.modules["common.logger"].create_logger = MagicMock(return_value=MagicMock())


class TestComputeImageHash(unittest.TestCase):
    """§3.1.4: image_hash deterministic computation."""

    def _compute_with_paths(self, paths, pip_output="pkg==1.0\n"):
        """Helper: compute hash with custom _IMAGE_HASH_PATHS."""
        with patch("tee.attestation._IMAGE_HASH_PATHS", paths):
            with patch("tee.attestation._sp") as mock_sp:
                mock_sp.run.return_value = MagicMock(
                    returncode=0, stdout=pip_output
                )
                from tee.attestation import compute_image_hash
                return compute_image_hash()

    def test_deterministic(self):
        """Same files = same hash."""
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "test.py"
            p.write_text("print('hello')")
            h1 = self._compute_with_paths([Path(d)])
            h2 = self._compute_with_paths([Path(d)])
            assert h1 == h2

    def test_sensitive_to_change(self):
        """Any change = different hash."""
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "test.py"
            p.write_text("print('hello')")
            h1 = self._compute_with_paths([Path(d)])

            p.write_text("print('world')")
            h2 = self._compute_with_paths([Path(d)])
            assert h1 != h2

    def test_sensitive_to_new_file(self):
        """Adding a file = different hash."""
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "a.py"
            p.write_text("x = 1")
            h1 = self._compute_with_paths([Path(d)])

            p2 = Path(d) / "b.py"
            p2.write_text("y = 2")
            h2 = self._compute_with_paths([Path(d)])
            assert h1 != h2
            p2.unlink()

    def test_ignores_non_hashed_extensions(self):
        """Files with non-hashed extensions are ignored."""
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "code.py"
            p.write_text("x = 1")
            h1 = self._compute_with_paths([Path(d)])

            # Add a .pyc file — should be ignored
            pyc = Path(d) / "code.pyc"
            pyc.write_bytes(b"\x00\x01\x02")
            h2 = self._compute_with_paths([Path(d)])
            assert h1 == h2
            pyc.unlink()

    def test_pip_freeze_included(self):
        """Different pip freeze = different hash."""
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "test.py"
            p.write_text("x = 1")
            h1 = self._compute_with_paths([Path(d)], pip_output="pkg==1.0\n")
            h2 = self._compute_with_paths([Path(d)], pip_output="pkg==2.0\n")
            assert h1 != h2

    def test_returns_hex_string(self):
        """Hash is a 64-char hex string (SHA-256)."""
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "test.py"
            p.write_text("x = 1")
            h = self._compute_with_paths([Path(d)])
            assert len(h) == 64
            int(h, 16)  # must be valid hex


if __name__ == "__main__":
    unittest.main()
