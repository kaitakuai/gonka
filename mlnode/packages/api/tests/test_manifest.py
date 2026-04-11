"""
Unit tests for tee/model/manifest.py — Model manifest format.

Spec reference: §10.1 Model manifest
"""

import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import MagicMock

sys.modules["common"] = MagicMock()
sys.modules["common.logger"] = MagicMock()
sys.modules["common.logger"].create_logger = MagicMock(return_value=MagicMock())

from tee.model.manifest import ManifestFile, ModelManifest, _sha256_file


class TestManifestFormat(unittest.TestCase):
    """§10.1: Manifest structure compliance."""

    def _make_manifest(self):
        return ModelManifest(
            model_id="Qwen/Qwen2.5-7B-Instruct",
            revision="abc123def456",
            files=[
                ManifestFile(filename="model.safetensors", sha256="aabb" * 16, size_bytes=14_000_000_000),
                ManifestFile(filename="config.json", sha256="ccdd" * 16, size_bytes=1024),
            ],
            total_size_bytes=14_000_001_024,
            created_at="2026-04-10T00:00:00Z",
        )

    def test_to_dict_spec_fields(self):
        """Manifest dict has all spec §10.1 required fields."""
        m = self._make_manifest()
        d = m.to_dict()

        assert "model_id" in d
        assert "revision" in d
        assert "files" in d
        assert "total_size_bytes" in d
        assert "created_at" in d

    def test_files_is_array(self):
        """§10.1: files MUST be array of {filename, sha256, size_bytes}."""
        m = self._make_manifest()
        d = m.to_dict()

        assert isinstance(d["files"], list)
        for f in d["files"]:
            assert "filename" in f
            assert "sha256" in f
            assert "size_bytes" in f

    def test_roundtrip_save_load(self):
        """Manifest save → load produces identical object."""
        m = self._make_manifest()
        with tempfile.NamedTemporaryFile(suffix=".json", mode="w", delete=False) as f:
            path = Path(f.name)

        m.save(path)
        loaded = ModelManifest.load(path)

        assert loaded.model_id == m.model_id
        assert loaded.revision == m.revision
        assert loaded.total_size_bytes == m.total_size_bytes
        assert len(loaded.files) == len(m.files)
        assert loaded.files[0].filename == m.files[0].filename
        path.unlink()

    def test_verify_file(self):
        """verify_file checks SHA-256 correctly."""
        m = self._make_manifest()

        with tempfile.NamedTemporaryFile(delete=False) as f:
            f.write(b"hello world")
            path = Path(f.name)

        import hashlib
        expected = hashlib.sha256(b"hello world").hexdigest()

        # Replace manifest hash with correct one
        m.files[1] = ManifestFile(filename="config.json", sha256=expected, size_bytes=11)

        assert m.verify_file("config.json", path)
        assert not m.verify_file("config.json", Path("/nonexistent"))
        path.unlink()

    def test_weight_files_filter(self):
        """weight_files returns only .safetensors/.bin files."""
        m = self._make_manifest()
        weights = m.weight_files
        assert len(weights) == 1
        assert weights[0].filename == "model.safetensors"


if __name__ == "__main__":
    unittest.main()
