"""
Unit tests for tee/detect.py — TEE hardware auto-detection.

All tests mock device files and subprocess calls,
so they run on any machine (no TEE hardware required).
"""

import subprocess
import sys
import unittest
from unittest.mock import MagicMock, patch

# Mock common.logger before importing detect
sys.modules["common"] = MagicMock()
sys.modules["common.logger"] = MagicMock()
sys.modules["common.logger"].create_logger = MagicMock(return_value=MagicMock())

from tee.types import GPUCCMode, TEEType
from tee.detect import (
    _check_device,
    _check_sysfs,
    _detect_cpu_tee,
    _detect_gpu_cc,
    _validate_compatibility,
    detect_tee,
)


class TestCheckDevice(unittest.TestCase):
    """Test _check_device() with various file states."""

    @patch("builtins.open")
    def test_device_exists_and_opens(self, mock_open):
        mock_open.return_value.__enter__ = MagicMock()
        mock_open.return_value.__exit__ = MagicMock()
        self.assertTrue(_check_device("/dev/sev-guest"))

    @patch("builtins.open", side_effect=FileNotFoundError)
    def test_device_not_found(self, _):
        self.assertFalse(_check_device("/dev/sev-guest"))

    @patch("builtins.open", side_effect=PermissionError)
    def test_device_permission_denied(self, _):
        self.assertFalse(_check_device("/dev/sev-guest"))

    @patch("builtins.open", side_effect=OSError("device error"))
    def test_device_os_error(self, _):
        self.assertFalse(_check_device("/dev/sev-guest"))


class TestCheckSysfs(unittest.TestCase):
    """Test _check_sysfs() kernel module confirmation."""

    @patch("os.path.isdir", return_value=True)
    def test_sysfs_exists(self, _):
        self.assertTrue(_check_sysfs(TEEType.AMD_SEV_SNP))

    @patch("os.path.isdir", return_value=False)
    def test_sysfs_missing(self, _):
        self.assertFalse(_check_sysfs(TEEType.AMD_SEV_SNP))


class TestDetectCpuTee(unittest.TestCase):
    """Test _detect_cpu_tee() with various device combinations."""

    @patch("tee.detect._check_sysfs", return_value=True)
    @patch("tee.detect._check_device")
    def test_amd_detected(self, mock_device, _):
        mock_device.side_effect = lambda p: p == "/dev/sev-guest"
        result = _detect_cpu_tee()
        self.assertIsNotNone(result)
        self.assertEqual(result[0], TEEType.AMD_SEV_SNP)
        self.assertEqual(result[1], "/dev/sev-guest")

    @patch("tee.detect._check_sysfs", return_value=True)
    @patch("tee.detect._check_device")
    def test_intel_detected(self, mock_device, _):
        mock_device.side_effect = lambda p: p == "/dev/tdx_guest"
        result = _detect_cpu_tee()
        self.assertIsNotNone(result)
        self.assertEqual(result[0], TEEType.INTEL_TDX)
        self.assertEqual(result[1], "/dev/tdx_guest")

    @patch("tee.detect._check_sysfs", return_value=True)
    @patch("tee.detect._check_device")
    def test_both_exist_amd_wins(self, mock_device, _):
        """AMD is checked first, so it wins when both are present."""
        mock_device.return_value = True
        result = _detect_cpu_tee()
        self.assertEqual(result[0], TEEType.AMD_SEV_SNP)

    @patch("tee.detect._check_device", return_value=False)
    def test_nothing_found(self, _):
        result = _detect_cpu_tee()
        self.assertIsNone(result)

    @patch("tee.detect._check_sysfs", return_value=True)
    @patch("tee.detect._check_device")
    def test_amd_fails_intel_ok(self, mock_device, _):
        """AMD device fails to open, falls through to Intel."""
        mock_device.side_effect = lambda p: p == "/dev/tdx_guest"
        result = _detect_cpu_tee()
        self.assertEqual(result[0], TEEType.INTEL_TDX)


class TestDetectGpuCC(unittest.TestCase):
    """Test _detect_gpu_cc() with various nvidia-smi states."""

    @patch("shutil.which", return_value=None)
    def test_no_nvidia_smi(self, _):
        result = _detect_gpu_cc()
        self.assertIsNone(result)

    @patch("shutil.which", return_value="/usr/bin/nvidia-smi")
    @patch("subprocess.run")
    def test_cc_mode_on(self, mock_run, _):
        # First call: conf-compute -grs
        cc_result = MagicMock()
        cc_result.returncode = 0
        cc_result.stdout = "CC status: ON"
        # Second call: query GPU name/driver
        query_result = MagicMock()
        query_result.returncode = 0
        query_result.stdout = "NVIDIA H100 SXM, 535.129.03"
        mock_run.side_effect = [cc_result, query_result]

        result = _detect_gpu_cc()
        self.assertIsNotNone(result)
        self.assertEqual(result.mode, GPUCCMode.SPT)
        self.assertEqual(result.gpu_name, "NVIDIA H100 SXM")
        self.assertEqual(result.driver_version, "535.129.03")

    @patch("shutil.which", return_value="/usr/bin/nvidia-smi")
    @patch("subprocess.run")
    def test_cc_mode_off(self, mock_run, _):
        cc_result = MagicMock()
        cc_result.returncode = 0
        cc_result.stdout = "CC status: OFF"
        mock_run.return_value = cc_result

        result = _detect_gpu_cc()
        self.assertIsNone(result)

    @patch("shutil.which", return_value="/usr/bin/nvidia-smi")
    @patch("subprocess.run")
    def test_conf_compute_not_supported(self, mock_run, _):
        """Older driver or non-CC GPU — conf-compute returns error."""
        cc_result = MagicMock()
        cc_result.returncode = 1
        mock_run.return_value = cc_result

        result = _detect_gpu_cc()
        self.assertIsNone(result)

    @patch("shutil.which", return_value="/usr/bin/nvidia-smi")
    @patch("subprocess.run", side_effect=subprocess.TimeoutExpired(cmd="", timeout=10))
    def test_nvidia_smi_timeout(self, *_):
        result = _detect_gpu_cc()
        self.assertIsNone(result)

    @patch("shutil.which", return_value="/usr/bin/nvidia-smi")
    @patch("subprocess.run")
    def test_mpt_mode(self, mock_run, _):
        cc_result = MagicMock()
        cc_result.returncode = 0
        cc_result.stdout = "CC status: ON (MPT mode)"
        query_result = MagicMock()
        query_result.returncode = 0
        query_result.stdout = "NVIDIA B200, 550.54.15"
        mock_run.side_effect = [cc_result, query_result]

        result = _detect_gpu_cc()
        self.assertIsNotNone(result)
        self.assertEqual(result.mode, GPUCCMode.MPT)


class TestValidateCompatibility(unittest.TestCase):
    """Test compatibility warnings for CPU+GPU combos."""

    def test_amd_plus_gpu_no_warnings(self):
        from tee.types import GPUCCInfo
        gpu = GPUCCInfo(mode=GPUCCMode.SPT, gpu_name="H100", driver_version="535")
        warnings = _validate_compatibility(TEEType.AMD_SEV_SNP, gpu)
        self.assertEqual(len(warnings), 0)

    def test_intel_plus_gpu_tdx_connect_warning(self):
        from tee.types import GPUCCInfo
        gpu = GPUCCInfo(mode=GPUCCMode.SPT, gpu_name="H100", driver_version="535")
        warnings = _validate_compatibility(TEEType.INTEL_TDX, gpu)
        self.assertEqual(len(warnings), 1)
        self.assertIn("TDX Connect", warnings[0])

    def test_no_gpu_no_warnings(self):
        warnings = _validate_compatibility(TEEType.AMD_SEV_SNP, None)
        self.assertEqual(len(warnings), 0)


class TestDetectTee(unittest.TestCase):
    """Test detect_tee() end-to-end."""

    @patch("tee.detect._detect_gpu_cc", return_value=None)
    @patch("tee.detect._detect_cpu_tee")
    def test_amd_no_gpu(self, mock_cpu, _):
        mock_cpu.return_value = (TEEType.AMD_SEV_SNP, "/dev/sev-guest")
        info = detect_tee()
        self.assertEqual(info.cpu_tee, TEEType.AMD_SEV_SNP)
        self.assertEqual(info.device_path, "/dev/sev-guest")
        self.assertIsNone(info.gpu_cc)
        self.assertEqual(len(info.warnings), 0)

    @patch("tee.detect._detect_gpu_cc")
    @patch("tee.detect._detect_cpu_tee")
    def test_amd_with_gpu_cc(self, mock_cpu, mock_gpu):
        from tee.types import GPUCCInfo
        mock_cpu.return_value = (TEEType.AMD_SEV_SNP, "/dev/sev-guest")
        mock_gpu.return_value = GPUCCInfo(
            mode=GPUCCMode.SPT, gpu_name="H100", driver_version="535"
        )
        info = detect_tee()
        self.assertEqual(info.cpu_tee, TEEType.AMD_SEV_SNP)
        self.assertIsNotNone(info.gpu_cc)
        self.assertEqual(info.gpu_cc.gpu_name, "H100")
        self.assertEqual(len(info.warnings), 0)

    @patch("tee.detect._detect_gpu_cc")
    @patch("tee.detect._detect_cpu_tee")
    def test_intel_with_gpu_cc_has_warning(self, mock_cpu, mock_gpu):
        from tee.types import GPUCCInfo
        mock_cpu.return_value = (TEEType.INTEL_TDX, "/dev/tdx_guest")
        mock_gpu.return_value = GPUCCInfo(
            mode=GPUCCMode.SPT, gpu_name="H100", driver_version="535"
        )
        info = detect_tee()
        self.assertEqual(info.cpu_tee, TEEType.INTEL_TDX)
        self.assertIsNotNone(info.gpu_cc)
        self.assertEqual(len(info.warnings), 1)
        self.assertIn("TDX Connect", info.warnings[0])

    @patch("tee.detect._detect_cpu_tee", return_value=None)
    def test_no_tee_exits(self, _):
        with self.assertRaises(SystemExit) as ctx:
            detect_tee()
        self.assertEqual(ctx.exception.code, 1)


if __name__ == "__main__":
    unittest.main()
