"""
Model Manifest — integrity contract for models loaded inside TEE.

The manifest is baked into the VM image at build time. At runtime,
the model loader downloads files and verifies each SHA-256 against
the manifest. Any mismatch → hard fail (possible supply chain attack).

Because the manifest is part of the VM image, it's part of the VM
measurement (MR_TD / SNP launch digest). Tampering with the manifest
changes the measurement and breaks attestation.
"""

import hashlib
import json
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path


@dataclass
class ModelManifest:
    """Manifest pinning a specific model revision with per-file SHA-256 hashes."""

    version: int
    model_id: str
    revision: str
    source: str
    files: dict[str, str]  # filename -> "sha256:hexdigest"
    total_sha256: str
    allowed_mirrors: list[str] = field(default_factory=list)
    generated_at: str = ""

    def save(self, path: Path | str) -> None:
        path = Path(path)
        path.write_text(json.dumps(self.to_dict(), indent=2) + "\n")

    def to_dict(self) -> dict:
        return {
            "version": self.version,
            "model_id": self.model_id,
            "revision": self.revision,
            "source": self.source,
            "files": self.files,
            "total_sha256": self.total_sha256,
            "allowed_mirrors": self.allowed_mirrors,
            "generated_at": self.generated_at,
        }

    @classmethod
    def load(cls, path: Path | str) -> "ModelManifest":
        path = Path(path)
        data = json.loads(path.read_text())
        return cls.from_dict(data)

    @classmethod
    def from_dict(cls, data: dict) -> "ModelManifest":
        if data.get("version", 0) != 1:
            raise ValueError(f"Unsupported manifest version: {data.get('version')}")
        return cls(
            version=data["version"],
            model_id=data["model_id"],
            revision=data["revision"],
            source=data["source"],
            files=data["files"],
            total_sha256=data["total_sha256"],
            allowed_mirrors=data.get("allowed_mirrors", []),
            generated_at=data.get("generated_at", ""),
        )

    def verify_file(self, filename: str, file_path: Path) -> bool:
        """Verify a downloaded file against the manifest hash."""
        expected = self.files.get(filename)
        if not expected:
            return False

        if not expected.startswith("sha256:"):
            raise ValueError(f"Unsupported hash format: {expected}")

        expected_hex = expected[len("sha256:"):]
        actual_hex = _sha256_file(file_path)
        return actual_hex == expected_hex

    def verify_total(self) -> bool:
        """Verify total_sha256 is consistent with individual file hashes."""
        computed = compute_total_sha256(self.files)
        return self.total_sha256 == computed

    @property
    def file_count(self) -> int:
        return len(self.files)

    @property
    def weight_files(self) -> list[str]:
        """Return only model weight files (safetensors/bin), not config/tokenizer."""
        return [f for f in self.files if f.endswith((".safetensors", ".bin"))]


def compute_total_sha256(files: dict[str, str]) -> str:
    """Compute total hash from sorted file hashes.

    Deterministic: sorted by filename, concatenate hex digests, SHA-256 the result.
    """
    sorted_hashes = []
    for filename in sorted(files.keys()):
        h = files[filename]
        if h.startswith("sha256:"):
            h = h[len("sha256:"):]
        sorted_hashes.append(h)

    combined = "".join(sorted_hashes)
    total = hashlib.sha256(combined.encode()).hexdigest()
    return f"sha256:{total}"


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
