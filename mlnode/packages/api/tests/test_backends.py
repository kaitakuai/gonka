"""
Unit tests for tee/backends/ — TEE attestation backends.

Tests AMD SEV-SNP and Intel TDX backends with mocked hardware.
"""

import struct
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import MagicMock, patch, mock_open

# Mock common.logger before importing
sys.modules["common"] = MagicMock()
sys.modules["common.logger"] = MagicMock()
sys.modules["common.logger"].create_logger = MagicMock(return_value=MagicMock())

from tee.backends.base import TEEBackend
from tee.backends.amd_sev_snp import AmdSevSnpBackend
from tee.backends.intel_tdx import IntelTdxBackend


class TestTEEBackendInterface(unittest.TestCase):
    """Verify TEEBackend is abstract and cannot be instantiated."""

    def test_cannot_instantiate(self):
        with self.assertRaises(TypeError):
            TEEBackend(Path("/tmp/test"))

    def test_required_methods(self):
        methods = ["tee_type", "generate_report", "fetch_certs",
                    "verify_certs", "verify_report", "parse_report"]
        for m in methods:
            self.assertTrue(hasattr(TEEBackend, m), f"Missing method: {m}")


# ---------------------------------------------------------------------------
# AMD SEV-SNP Backend
# ---------------------------------------------------------------------------

class TestAmdSevSnpBackend(unittest.TestCase):
    """Test AmdSevSnpBackend with mocked snpguest CLI."""

    def setUp(self):
        self._tmpdir = tempfile.TemporaryDirectory()
        self.tmp = Path(self._tmpdir.name) / "amd-attestation"
        self.backend = AmdSevSnpBackend(attestation_dir=self.tmp)

    def tearDown(self):
        self._tmpdir.cleanup()

    def test_tee_type(self):
        self.assertEqual(self.backend.tee_type(), "amd-sev-snp")

    @patch("tee.backends.amd_sev_snp._run")
    def test_generate_report_success(self, mock_run):
        mock_run.return_value = MagicMock(returncode=0, stderr="")
        fake_report = b"\x00" * 1184

        with patch.object(Path, "write_bytes"):
            with patch.object(Path, "read_bytes", return_value=fake_report):
                result = self.backend.generate_report(b"\x01" * 64)

        self.assertEqual(len(result), 1184)

    @patch("tee.backends.amd_sev_snp._run")
    def test_generate_report_failure(self, mock_run):
        mock_run.return_value = MagicMock(returncode=1, stderr="snpguest error")
        with patch.object(Path, "write_bytes"):
            with self.assertRaises(RuntimeError) as ctx:
                self.backend.generate_report(b"\x01" * 64)
        self.assertIn("snpguest", str(ctx.exception))

    @patch("tee.backends.amd_sev_snp._run")
    def test_fetch_certs(self, mock_run):
        mock_run.return_value = MagicMock(returncode=0)
        with patch.object(Path, "exists", return_value=True):
            with patch.object(Path, "read_text", return_value="-----BEGIN CERTIFICATE-----"):
                certs = self.backend.fetch_certs()
        self.assertIn("vcek", certs)
        self.assertIn("ask", certs)
        self.assertIn("ark", certs)

    @patch("tee.backends.amd_sev_snp._run")
    def test_verify_certs_ok(self, mock_run):
        mock_run.return_value = MagicMock(returncode=0)
        self.assertTrue(self.backend.verify_certs())

    @patch("tee.backends.amd_sev_snp._run")
    def test_verify_certs_fail(self, mock_run):
        mock_run.return_value = MagicMock(returncode=1)
        self.assertFalse(self.backend.verify_certs())

    @patch("tee.backends.amd_sev_snp._run")
    def test_verify_report_ok(self, mock_run):
        mock_run.return_value = MagicMock(returncode=0)
        self.assertTrue(self.backend.verify_report())

    @patch("tee.backends.amd_sev_snp._run")
    def test_verify_report_fail(self, mock_run):
        mock_run.return_value = MagicMock(returncode=1)
        self.assertFalse(self.backend.verify_report())

    def test_parse_report_valid(self):
        """Build a minimal valid SNP report v3 and parse it."""
        raw = bytearray(1184)
        # version=3 at offset 0
        struct.pack_into("<I", raw, 0x00, 3)
        # guest_svn=1
        struct.pack_into("<I", raw, 0x04, 1)
        # policy
        struct.pack_into("<Q", raw, 0x08, 0x30000)
        # vmpl=0
        struct.pack_into("<I", raw, 0x30, 0)
        # sig_algo=1 (ECDSA P-384)
        struct.pack_into("<I", raw, 0x34, 1)
        # report_data: first 64 bytes of known pattern
        raw[0x50:0x90] = b"\xAB" * 64

        parsed = self.backend.parse_report(bytes(raw))
        self.assertEqual(parsed["version"], 3)
        self.assertEqual(parsed["guest_svn"], 1)
        self.assertEqual(parsed["vmpl"], 0)
        self.assertEqual(parsed["signature_algo"], 1)
        self.assertEqual(parsed["report_data"], ("ab" * 64))

    def test_parse_report_too_short(self):
        parsed = self.backend.parse_report(b"\x00" * 100)
        self.assertIn("parse_error", parsed)


# ---------------------------------------------------------------------------
# Intel TDX Backend
# ---------------------------------------------------------------------------

class TestIntelTdxBackend(unittest.TestCase):
    """Test IntelTdxBackend with mocked /dev/tdx_guest."""

    def setUp(self):
        self._tmpdir = tempfile.TemporaryDirectory()
        self.tmp = Path(self._tmpdir.name) / "tdx-attestation"
        self.backend = IntelTdxBackend(attestation_dir=self.tmp)

    def tearDown(self):
        self._tmpdir.cleanup()

    def test_tee_type(self):
        self.assertEqual(self.backend.tee_type(), "intel-tdx")

    def test_parse_report_valid(self):
        """Build a minimal valid TDX Quote v4 and parse it."""
        raw = bytearray(700)
        # Header: version=4
        struct.pack_into("<H", raw, 0, 4)
        # att_key_type=2 (ECDSA-256)
        struct.pack_into("<H", raw, 2, 2)
        # tee_type=0x81 (TDX)
        struct.pack_into("<I", raw, 4, 0x81)
        # qe_svn=5
        struct.pack_into("<H", raw, 8, 5)
        # pce_svn=12
        struct.pack_into("<H", raw, 10, 12)
        # report_data at td_offset(48) + 520 = 568
        raw[568:632] = b"\xCD" * 64
        # mr_td at td_offset(48) + 136 = 184
        raw[184:232] = b"\xEF" * 48

        parsed = self.backend.parse_report(bytes(raw))
        self.assertEqual(parsed["version"], 4)
        self.assertEqual(parsed["attestation_key_type"], 2)
        self.assertEqual(parsed["tee_type_raw"], 0x81)
        self.assertEqual(parsed["qe_svn"], 5)
        self.assertEqual(parsed["pce_svn"], 12)
        self.assertEqual(parsed["report_data"], "cd" * 64)
        self.assertEqual(parsed["mr_td"], "ef" * 48)
        self.assertEqual(parsed["measurement"], parsed["mr_td"])

    def test_parse_report_too_short(self):
        parsed = self.backend.parse_report(b"\x00" * 100)
        self.assertIn("parse_error", parsed)

    @patch("os.path.isfile", return_value=False)
    def test_verify_report_no_dcap_structural(self, _):
        """Without DCAP, falls back to structural validation."""
        raw = bytearray(700)
        struct.pack_into("<H", raw, 0, 4)
        self.backend._quote_bytes = bytes(raw)
        with patch.object(Path, "exists", return_value=True):
            result = self.backend.verify_report()
        self.assertTrue(result)

    def test_verify_report_no_quote(self):
        """No quote available — returns False."""
        self.backend._quote_bytes = None
        with patch.object(Path, "exists", return_value=False):
            result = self.backend.verify_report()
        self.assertFalse(result)

    @patch("subprocess.run")
    def test_verify_certs_ok(self, mock_run):
        mock_run.return_value = MagicMock(returncode=0)
        with patch.object(Path, "exists", return_value=True):
            result = self.backend.verify_certs()
        self.assertTrue(result)

    @patch("subprocess.run")
    def test_verify_certs_missing(self, mock_run):
        with patch.object(Path, "exists", return_value=False):
            result = self.backend.verify_certs()
        self.assertFalse(result)


# ---------------------------------------------------------------------------
# Attestation Dispatcher
# ---------------------------------------------------------------------------

class TestAttestationDispatcher(unittest.TestCase):
    """Test attestation.py dispatcher selects correct backend."""

    @patch("tee.backends.amd_sev_snp.AmdSevSnpBackend.__init__", return_value=None)
    def test_amd_backend_selected(self, _):
        from tee.attestation import _get_backend
        from tee.types import TEEInfo, TEEType
        info = TEEInfo(cpu_tee=TEEType.AMD_SEV_SNP, device_path="/dev/sev-guest")
        backend = _get_backend(info)
        self.assertIsInstance(backend, AmdSevSnpBackend)

    @patch("tee.backends.intel_tdx.IntelTdxBackend.__init__", return_value=None)
    def test_intel_backend_selected(self, _):
        from tee.attestation import _get_backend
        from tee.types import TEEInfo, TEEType
        info = TEEInfo(cpu_tee=TEEType.INTEL_TDX, device_path="/dev/tdx_guest")
        backend = _get_backend(info)
        self.assertIsInstance(backend, IntelTdxBackend)


if __name__ == "__main__":
    unittest.main()
