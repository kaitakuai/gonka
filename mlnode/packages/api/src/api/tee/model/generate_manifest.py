#!/usr/bin/env python3
"""
Generate a model manifest from a HuggingFace repository.

Fetches file list + SHA-256 hashes via HF API without downloading weights.
Pins the exact revision (commit hash) for reproducibility.

Usage:
    python3 generate_manifest.py Qwen/Qwen2.5-7B-Instruct
    python3 generate_manifest.py Qwen/Qwen2.5-7B-Instruct --revision main
    python3 generate_manifest.py Qwen/Qwen2.5-7B-Instruct --output manifest.json
    python3 generate_manifest.py Qwen/Qwen2.5-7B-Instruct --mirrors https://hf-mirror.com
"""

import argparse
import json
import sys
from datetime import datetime, timezone

from huggingface_hub import HfApi
from huggingface_hub.utils import RepositoryNotFoundError, RevisionNotFoundError

from .manifest import ModelManifest, compute_total_sha256


def generate(
    model_id: str,
    revision: str = "main",
    mirrors: list[str] | None = None,
) -> ModelManifest:
    """Generate a ModelManifest from HuggingFace Hub API.

    Args:
        model_id: HuggingFace model identifier (e.g. "Qwen/Qwen2.5-7B-Instruct")
        revision: Branch, tag, or commit hash (default "main")
        mirrors: Additional allowed download mirrors

    Returns:
        ModelManifest with per-file SHA-256 hashes and pinned revision.
    """
    api = HfApi()

    try:
        model_info = api.model_info(model_id, revision=revision, files_metadata=True)
    except RepositoryNotFoundError:
        raise SystemExit(f"Model not found: {model_id}")
    except RevisionNotFoundError:
        raise SystemExit(f"Revision not found: {model_id}@{revision}")

    # Pin to exact commit hash (not "main" which drifts)
    pinned_revision = model_info.sha

    # Collect file hashes
    files: dict[str, str] = {}
    skipped: list[str] = []

    for sibling in model_info.siblings or []:
        filename = sibling.rfilename

        # Skip non-essential files
        if filename.startswith(".") or filename in ("README.md", ".gitattributes"):
            continue

        # Get SHA-256 from LFS metadata
        lfs = sibling.lfs
        if lfs and lfs.get("sha256"):
            files[filename] = f"sha256:{lfs['sha256']}"
        elif hasattr(sibling, "blob_id") and sibling.blob_id:
            # Non-LFS files: blob_id is git SHA-1, not SHA-256
            # We need to fetch the actual SHA-256
            skipped.append(filename)
        else:
            skipped.append(filename)

    if not files:
        raise SystemExit(
            f"No files with SHA-256 hashes found for {model_id}@{revision}. "
            "The model may not use LFS for its files."
        )

    # For small non-LFS files (config.json, tokenizer.json etc), download and hash
    if skipped:
        from huggingface_hub import hf_hub_download
        import hashlib as _hashlib

        for filename in skipped:
            try:
                path = hf_hub_download(
                    model_id, filename, revision=pinned_revision
                )
                h = _hashlib.sha256()
                with open(path, "rb") as f:
                    while True:
                        chunk = f.read(8 * 1024 * 1024)
                        if not chunk:
                            break
                        h.update(chunk)
                files[filename] = f"sha256:{h.hexdigest()}"
            except Exception as e:
                print(f"  Warning: skipping {filename}: {e}", file=sys.stderr)

    total = compute_total_sha256(files)

    allowed_mirrors = ["https://huggingface.co"]
    if mirrors:
        allowed_mirrors.extend(mirrors)

    return ModelManifest(
        version=1,
        model_id=model_id,
        revision=pinned_revision,
        source="huggingface",
        files=files,
        total_sha256=total,
        allowed_mirrors=allowed_mirrors,
        generated_at=datetime.now(timezone.utc).isoformat(),
    )


def main():
    parser = argparse.ArgumentParser(
        description="Generate a model manifest from HuggingFace Hub"
    )
    parser.add_argument("model_id", help="HuggingFace model ID (e.g. Qwen/Qwen2.5-7B-Instruct)")
    parser.add_argument("--revision", default="main", help="Branch, tag, or commit hash")
    parser.add_argument("--output", "-o", help="Output file (default: stdout)")
    parser.add_argument(
        "--mirrors", nargs="*", default=[],
        help="Additional allowed download mirrors (e.g. https://hf-mirror.com)"
    )
    args = parser.parse_args()

    print(f"Generating manifest for {args.model_id}@{args.revision}...", file=sys.stderr)

    manifest = generate(args.model_id, args.revision, args.mirrors)

    output = json.dumps(manifest.to_dict(), indent=2) + "\n"

    if args.output:
        with open(args.output, "w") as f:
            f.write(output)
        print(f"Manifest written to {args.output}", file=sys.stderr)
        print(f"  Model: {manifest.model_id}", file=sys.stderr)
        print(f"  Revision: {manifest.revision}", file=sys.stderr)
        print(f"  Files: {manifest.file_count} ({len(manifest.weight_files)} weights)", file=sys.stderr)
        print(f"  Total SHA-256: {manifest.total_sha256}", file=sys.stderr)
    else:
        sys.stdout.write(output)


if __name__ == "__main__":
    main()
