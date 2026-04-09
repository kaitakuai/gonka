"""
Model Manifest — integrity contract for models loaded inside TEE.

The manifest is baked into the VM image at build time. At runtime,
the model loader downloads files and verifies each SHA-256 against
the manifest. Any mismatch → hard fail (possible supply chain attack).

Because the manifest is part of the VM image, it's part of the VM
measurement (MR_TD / SNP launch digest). Tampering with the manifest
changes the measurement and breaks attestation.

Spec reference: §10.1 Model manifest
"""

import hashlib
import json
from dataclasses import dataclass, field
from pathlib import Path


@dataclass
class ManifestFile:
    """A single file entry in the manifest (spec §10.1)."""
    filename: str
    sha256: str
    size_bytes: int

    def to_dict(self) -> dict:
        return {
            "filename": self.filename,
            "sha256": self.sha256,
            "size_bytes": self.size_bytes,
        }

    @classmethod
    def from_dict(cls, data: dict) -> "ManifestFile":
        return cls(
            filename=data["filename"],
            sha256=data["sha256"],
            size_bytes=data["size_bytes"],
        )


@dataclass
class ModelManifest:
    """Manifest pinning a specific model revision with per-file SHA-256 hashes.

    Format per spec §10.1:
      - model_id: HuggingFace model identifier
      - revision: pinned git commit hash (NOT "main")
      - files: array of {filename, sha256, size_bytes}
      - total_size_bytes: sum of all file sizes
      - created_at: ISO 8601 timestamp
    """

    model_id: str
    revision: str
    files: list[ManifestFile]
    total_size_bytes: int
    created_at: str

    def save(self, path: Path | str) -> None:
        path = Path(path)
        path.write_text(json.dumps(self.to_dict(), indent=2) + "\n")

    def to_dict(self) -> dict:
        return {
            "model_id": self.model_id,
            "revision": self.revision,
            "files": [f.to_dict() for f in self.files],
            "total_size_bytes": self.total_size_bytes,
            "created_at": self.created_at,
        }

    @classmethod
    def load(cls, path: Path | str) -> "ModelManifest":
        path = Path(path)
        data = json.loads(path.read_text())
        return cls.from_dict(data)

    @classmethod
    def from_dict(cls, data: dict) -> "ModelManifest":
        return cls(
            model_id=data["model_id"],
            revision=data["revision"],
            files=[ManifestFile.from_dict(f) for f in data["files"]],
            total_size_bytes=data["total_size_bytes"],
            created_at=data["created_at"],
        )

    def get_file(self, filename: str) -> ManifestFile | None:
        """Look up a file entry by name."""
        for f in self.files:
            if f.filename == filename:
                return f
        return None

    def verify_file(self, filename: str, file_path: Path) -> bool:
        """Verify a downloaded file against the manifest hash (spec §10.2 MI-1)."""
        entry = self.get_file(filename)
        if entry is None:
            return False
        actual_hex = _sha256_file(file_path)
        return actual_hex == entry.sha256

    @property
    def file_count(self) -> int:
        return len(self.files)

    @property
    def filenames(self) -> list[str]:
        return [f.filename for f in self.files]

    @property
    def weight_files(self) -> list[ManifestFile]:
        """Return only model weight files (safetensors/bin), not config/tokenizer."""
        return [f for f in self.files if f.filename.endswith((".safetensors", ".bin"))]


def _sha256_file(path: Path) -> str:
    """Compute SHA-256 of a file, streaming (handles large files)."""
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while True:
            chunk = f.read(8 * 1024 * 1024)  # 8 MB chunks
            if not chunk:
                break
            h.update(chunk)
    return h.hexdigest()
