"""
Model Loader — download and verify model files inside TEE VM.

Spec §10.2:
  [MI-1] When a model manifest is present, the MLNode MUST verify
         SHA-256 hash of each model file at load time.
  [MI-2] Hash mismatch MUST prevent the model from loading.

Flow (PLAN.md §7.2):
  1. Read model_manifest.json (baked into VM image)
  2. Download from HuggingFace (or mirror via HF_ENDPOINT)
  3. SHA-256 verify each file against manifest
  4. Hash mismatch → FATAL ERROR, refuse to start
  5. Download failure → retry with mirror, then fail
  6. Return model path for vLLM to load

Storage (§7.2.3):
  Models are stored on the VM disk (qcow2 overlay), which is
  encrypted by SEV-SNP/TDX hardware. The host sees only ciphertext.
"""

import hashlib
import os
import sys
from pathlib import Path

from common.logger import create_logger

from .manifest import ModelManifest

logger = create_logger(__name__)

# Default model storage inside TEE VM (on encrypted disk, not tmpfs).
# qcow2 overlay is encrypted by SEV-SNP/TDX — host sees ciphertext.
DEFAULT_MODEL_DIR = Path("/data/models")

# Manifest location (baked into VM image at build time)
DEFAULT_MANIFEST_PATH = Path("/app/model_manifest.json")


class ModelLoadError(Exception):
    """Fatal error during model loading. MLNode MUST NOT start."""
    pass


class HashMismatchError(ModelLoadError):
    """SHA-256 hash mismatch — possible supply chain attack (spec §10.2 MI-2)."""
    pass


def load_model(
    manifest_path: Path | str | None = None,
    model_dir: Path | str | None = None,
) -> tuple[Path, ModelManifest]:
    """Download and verify model per manifest.

    Args:
        manifest_path: Path to model_manifest.json. Default: /app/model_manifest.json
        model_dir: Directory to store model files. Default: /data/models

    Returns:
        (model_path, manifest) — path to verified model dir and loaded manifest.

    Raises:
        ModelLoadError: manifest missing, download failed, etc.
        HashMismatchError: SHA-256 mismatch (spec §10.2 MI-2).
    """
    manifest_path = Path(manifest_path or DEFAULT_MANIFEST_PATH)
    model_dir = Path(model_dir or DEFAULT_MODEL_DIR)

    # Step 1: Load manifest
    if not manifest_path.exists():
        logger.warning(f"No model manifest at {manifest_path} — skipping model verification")
        return model_dir, None

    logger.info(f"Loading model manifest: {manifest_path}")
    manifest = ModelManifest.load(manifest_path)
    logger.info(
        f"  Model: {manifest.model_id}@{manifest.revision[:12]}, "
        f"{manifest.file_count} files, {manifest.total_size_bytes / (1024**3):.1f} GB"
    )

    # Step 2: Prepare model directory
    model_path = model_dir / manifest.model_id.replace("/", "--")
    model_path.mkdir(parents=True, exist_ok=True)

    # Step 3: Check if already downloaded and verified
    if _all_files_verified(manifest, model_path):
        logger.info("All model files already present and verified")
        return model_path, manifest

    # Step 4: Download from HuggingFace (or mirror)
    logger.info(f"Downloading model to {model_path}...")
    _download_model(manifest, model_path)

    # Step 5: Verify ALL files (spec §10.2 MI-1)
    logger.info("Verifying SHA-256 hashes...")
    _verify_all_files(manifest, model_path)

    logger.info(f"Model loaded and verified: {manifest.model_id}")
    return model_path, manifest


def _all_files_verified(manifest: ModelManifest, model_path: Path) -> bool:
    """Check if all files exist and have correct hashes (cached download)."""
    for entry in manifest.files:
        file_path = model_path / entry.filename
        if not file_path.exists():
            return False
        if not manifest.verify_file(entry.filename, file_path):
            logger.warning(f"Cached file hash mismatch: {entry.filename} — re-downloading")
            file_path.unlink()
            return False
    return True


def _download_model(manifest: ModelManifest, model_path: Path) -> None:
    """Download model files from HuggingFace Hub.

    Uses HF_ENDPOINT env var for mirror support (spec §7.2.2).
    Mirrors are safe because every file is SHA-256 verified.
    """
    from huggingface_hub import hf_hub_download

    endpoint = os.getenv("HF_ENDPOINT")
    if endpoint:
        logger.info(f"Using HF mirror: {endpoint}")

    for entry in manifest.files:
        file_path = model_path / entry.filename
        if file_path.exists():
            # Already downloaded (partial resume)
            if manifest.verify_file(entry.filename, file_path):
                logger.info(f"  {entry.filename}: cached, verified")
                continue
            else:
                logger.warning(f"  {entry.filename}: cached but hash mismatch, re-downloading")
                file_path.unlink()

        logger.info(f"  {entry.filename}: downloading ({entry.size_bytes / (1024**2):.1f} MB)...")

        try:
            downloaded = hf_hub_download(
                manifest.model_id,
                entry.filename,
                revision=manifest.revision,
                local_dir=str(model_path),
                local_dir_use_symlinks=False,
                endpoint=endpoint,
            )
            logger.info(f"  {entry.filename}: done")
        except Exception as e:
            raise ModelLoadError(
                f"Failed to download {entry.filename} from "
                f"{endpoint or 'huggingface.co'}: {e}"
            )


def _verify_all_files(manifest: ModelManifest, model_path: Path) -> None:
    """Verify SHA-256 of every file against manifest.

    Spec §10.2:
      [MI-1] MUST verify SHA-256 hash of each model file at load time.
      [MI-2] Hash mismatch MUST prevent the model from loading.
    """
    for entry in manifest.files:
        file_path = model_path / entry.filename
        if not file_path.exists():
            raise ModelLoadError(f"File missing after download: {entry.filename}")

        actual = _sha256_file(file_path)
        if actual != entry.sha256:
            raise HashMismatchError(
                f"SHA-256 MISMATCH for {entry.filename}: "
                f"expected {entry.sha256[:16]}..., got {actual[:16]}... "
                f"Possible supply chain attack. Refusing to load model."
            )
        logger.info(f"  {entry.filename}: SHA-256 OK")


def _sha256_file(path: Path) -> str:
    """Compute SHA-256 of a file, streaming."""
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while True:
            chunk = f.read(8 * 1024 * 1024)
            if not chunk:
                break
            h.update(chunk)
    return h.hexdigest()
