"""
Unit tests for tee/model/loader.py — Model download and verification.

Spec references:
  §10.2 MI-1: MUST verify SHA-256 hash of each model file at load time
  §10.2 MI-2: Hash mismatch MUST prevent model from loading
  Security: path traversal, model_id validation, disk space check
"""

import hashlib
import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import MagicMock, patch

sys.modules["common"] = MagicMock()
sys.modules["common.logger"] = MagicMock()
sys.modules["common.logger"].create_logger = MagicMock(return_value=MagicMock())

from tee.model.manifest import ManifestFile, ModelManifest
from tee.model.loader import (
    ModelLoadError,
    HashMismatchError,
    _safe_path,
    _MODEL_ID_RE,
)


class TestSafePath(unittest.TestCase):
    """Path traversal protection."""

    def test_normal_filename(self):
        """Normal filename resolves correctly."""
        with tempfile.TemporaryDirectory() as d:
            base = Path(d)
            result = _safe_path(base, "model.safetensors")
            assert str(result).startswith(str(base.resolve()))

    def test_path_traversal_rejected(self):
        """../../etc/passwd MUST be rejected."""
        with tempfile.TemporaryDirectory() as d:
            base = Path(d)
            with self.assertRaises(ModelLoadError):
                _safe_path(base, "../../etc/passwd")

    def test_dot_dot_in_middle(self):
        """subdir/../../../etc/shadow MUST be rejected."""
        with tempfile.TemporaryDirectory() as d:
            base = Path(d)
            with self.assertRaises(ModelLoadError):
                _safe_path(base, "subdir/../../../etc/shadow")

    def test_subdirectory_allowed(self):
        """Subdirectory filenames are allowed."""
        with tempfile.TemporaryDirectory() as d:
            base = Path(d)
            result = _safe_path(base, "subdir/file.py")
            assert str(result).startswith(str(base.resolve()))


class TestModelIdValidation(unittest.TestCase):
    """model_id regex validation."""

    def test_valid_model_ids(self):
        """Standard HuggingFace model IDs pass."""
        assert _MODEL_ID_RE.match("Qwen/Qwen2.5-7B-Instruct")
        assert _MODEL_ID_RE.match("meta-llama/Llama-3.1-8B")
        assert _MODEL_ID_RE.match("mistralai/Mistral-7B-v0.3")

    def test_invalid_model_ids(self):
        """Malicious model IDs rejected."""
        assert not _MODEL_ID_RE.match("../../etc/passwd")
        assert not _MODEL_ID_RE.match("single-name")
        assert not _MODEL_ID_RE.match("org/name/extra")
        assert not _MODEL_ID_RE.match("org/na me")
        assert not _MODEL_ID_RE.match("")


class TestLoadModelMissingManifest(unittest.TestCase):
    """§10.2: Missing manifest = FATAL."""

    def test_missing_manifest_raises(self):
        """ModelLoadError when manifest file doesn't exist."""
        from tee.model.loader import load_model

        with self.assertRaises(ModelLoadError) as ctx:
            load_model(manifest_path="/nonexistent/manifest.json")
        assert "not found" in str(ctx.exception).lower()


class TestHashMismatch(unittest.TestCase):
    """§10.2 MI-2: Hash mismatch MUST prevent loading."""

    def test_verify_correct_hash(self):
        """Correct hash passes verification."""
        with tempfile.NamedTemporaryFile(delete=False) as f:
            f.write(b"model weights data")
            path = Path(f.name)

        expected = hashlib.sha256(b"model weights data").hexdigest()
        manifest = ModelManifest(
            model_id="test/model", revision="abc123",
            files=[ManifestFile(filename="weights.bin", sha256=expected, size_bytes=18)],
            total_size_bytes=18, created_at="2026-01-01",
        )

        assert manifest.verify_file("weights.bin", path)
        path.unlink()

    def test_verify_wrong_hash(self):
        """Wrong hash fails verification."""
        with tempfile.NamedTemporaryFile(delete=False) as f:
            f.write(b"model weights data")
            path = Path(f.name)

        manifest = ModelManifest(
            model_id="test/model", revision="abc123",
            files=[ManifestFile(filename="weights.bin", sha256="wrong" * 16, size_bytes=18)],
            total_size_bytes=18, created_at="2026-01-01",
        )

        assert not manifest.verify_file("weights.bin", path)
        path.unlink()


if __name__ == "__main__":
    unittest.main()
