"""
Unit tests for tee/attestation.py — attestation bundle assembly.

Spec references:
  §3.1: GET /attestation response format
  §3.1.6: verification object gating (TEE_DEBUG_ATTESTATION)
  §3.1.4: vm_image.image_hash
"""

import os
import sys
import unittest
from unittest.mock import MagicMock, patch

sys.modules["common"] = MagicMock()
sys.modules["common.logger"] = MagicMock()
sys.modules["common.logger"].create_logger = MagicMock(return_value=MagicMock())

from tee.types import TEEInfo, TEEType


class MockBackend:
    def tee_type(self): return "amd-sev-snp"
    def generate_report(self, rd): return b"\x00" * 1184
    def fetch_certs(self): return {"ark": "pem1", "ask": "pem2", "vcek": "pem3"}
    def verify_certs(self): return True
    def verify_report(self): return True
    def parse_report(self, raw): return {
        "version": 3, "report_data": "00" * 64, "measurement": "aa" * 48,
        "policy": {"debug_allowed": False},
    }


class MockKeys:
    enc_public_hex = "00" * 32
    sign_public_hex = "00" * 32
    def compute_report_data(self): return b"\x00" * 64


class TestAttestationBundle(unittest.TestCase):
    """§3.1: Attestation bundle structure."""

    @patch("tee.attestation._get_backend", return_value=MockBackend())
    @patch("tee.attestation._collect_vm_metadata", return_value={
        "os": "Ubuntu", "kernel": "6.8", "arch": "x86_64", "vllm_version": "0.15.1"
    })
    def test_bundle_has_required_fields(self, mock_meta, mock_backend):
        """Bundle contains all spec §3.1 required fields."""
        from tee.attestation import generate_attestation
        tee_info = TEEInfo(cpu_tee=TEEType.AMD_SEV_SNP, device_path="/dev/sev-guest")

        bundle = generate_attestation(MockKeys(), tee_info, image_hash="abc123")

        assert "tee_type" in bundle
        assert "encryption_pubkey" in bundle
        assert "signing_pubkey" in bundle
        assert "report" in bundle
        assert "hardware" in bundle
        assert "vm_image" in bundle
        assert "certs" in bundle
        assert bundle["tee_type"] == "amd-sev-snp"

    @patch("tee.attestation._get_backend", return_value=MockBackend())
    @patch("tee.attestation._collect_vm_metadata", return_value={
        "os": "Ubuntu", "kernel": "6.8", "arch": "x86_64", "vllm_version": "0.15.1"
    })
    def test_report_fields(self, mock_meta, mock_backend):
        """§3.1.1: Report object has report_hex, report_data, measurement, parsed."""
        from tee.attestation import generate_attestation
        tee_info = TEEInfo(cpu_tee=TEEType.AMD_SEV_SNP, device_path="/dev/sev-guest")

        bundle = generate_attestation(MockKeys(), tee_info)
        report = bundle["report"]

        assert "report_hex" in report
        assert "report_data" in report
        assert "measurement" in report
        assert "parsed" in report

    @patch("tee.attestation._get_backend", return_value=MockBackend())
    @patch("tee.attestation._collect_vm_metadata", return_value={
        "os": "Ubuntu", "kernel": "6.8", "arch": "x86_64", "vllm_version": "0.15.1"
    })
    def test_vm_image_fields(self, mock_meta, mock_backend):
        """§3.1.4: vm_image object has required fields."""
        from tee.attestation import generate_attestation
        tee_info = TEEInfo(cpu_tee=TEEType.AMD_SEV_SNP, device_path="/dev/sev-guest")

        bundle = generate_attestation(MockKeys(), tee_info, image_hash="test_hash")
        vm = bundle["vm_image"]

        assert "measurement" in vm
        assert "os" in vm
        assert "kernel" in vm
        assert "arch" in vm
        assert "vllm_version" in vm
        assert vm.get("image_hash") == "test_hash"

    @patch("tee.attestation._get_backend", return_value=MockBackend())
    @patch("tee.attestation._collect_vm_metadata", return_value={
        "os": "Ubuntu", "kernel": "6.8", "arch": "x86_64", "vllm_version": "0.15.1"
    })
    def test_gpu_null_when_absent(self, mock_meta, mock_backend):
        """§3.1.3: gpu is null when no GPU CC."""
        from tee.attestation import generate_attestation
        tee_info = TEEInfo(cpu_tee=TEEType.AMD_SEV_SNP, device_path="/dev/sev-guest")

        bundle = generate_attestation(MockKeys(), tee_info)
        assert bundle["gpu"] is None


class TestVerificationGating(unittest.TestCase):
    """§3.1.6: verification object MUST be absent in production."""

    @patch("tee.attestation._get_backend", return_value=MockBackend())
    @patch("tee.attestation._collect_vm_metadata", return_value={
        "os": "Ubuntu", "kernel": "6.8", "arch": "x86_64", "vllm_version": "0.15.1"
    })
    def test_verification_absent_by_default(self, mock_meta, mock_backend):
        """Production: verification field MUST be absent."""
        os.environ.pop("TEE_DEBUG_ATTESTATION", None)
        from tee.attestation import generate_attestation
        tee_info = TEEInfo(cpu_tee=TEEType.AMD_SEV_SNP, device_path="/dev/sev-guest")

        bundle = generate_attestation(MockKeys(), tee_info)
        assert "verification" not in bundle

    @patch("tee.attestation._get_backend", return_value=MockBackend())
    @patch("tee.attestation._collect_vm_metadata", return_value={
        "os": "Ubuntu", "kernel": "6.8", "arch": "x86_64", "vllm_version": "0.15.1"
    })
    def test_verification_present_with_debug_flag(self, mock_meta, mock_backend):
        """Debug: verification present when TEE_DEBUG_ATTESTATION=1."""
        os.environ["TEE_DEBUG_ATTESTATION"] = "1"
        try:
            from tee.attestation import generate_attestation
            tee_info = TEEInfo(cpu_tee=TEEType.AMD_SEV_SNP, device_path="/dev/sev-guest")

            bundle = generate_attestation(MockKeys(), tee_info)
            assert "verification" in bundle
            v = bundle["verification"]
            assert "certs_valid" in v
            assert "report_valid" in v
            assert "keys_bound" in v
        finally:
            os.environ.pop("TEE_DEBUG_ATTESTATION", None)


if __name__ == "__main__":
    unittest.main()
