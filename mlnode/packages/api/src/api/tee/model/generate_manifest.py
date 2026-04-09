#!/usr/bin/env python3
"""
Generate a model manifest from a HuggingFace repository.

Fetches file list + SHA-256 hashes via HF API without downloading weights.
Pins the exact revision (commit hash) for reproducibility.

Output format per spec §10.1:
  {model_id, revision, files: [{filename, sha256, size_bytes}], total_size_bytes, created_at}

Usage:
    python3 generate_manifest.py Qwen/Qwen2.5-7B-Instruct
    python3 generate_manifest.py Qwen/Qwen2.5-7B-Instruct --revision main
    python3 generate_manifest.py Qwen/Qwen2.5-7B-Instruct --output manifest.json
"""

import argparse
import hashlib
import json
import sys
from datetime import datetime, timezone

from huggingface_hub import HfApi, hf_hub_download
from huggingface_hub.utils import RepositoryNotFoundError, RevisionNotFoundError

from .manifest import ModelManifest, ManifestFile


def generate(
    model_id: str,
    revision: str = "main",
) -> ModelManifest:
    """Generate a ModelManifest from HuggingFace Hub API.

    Args:
        model_id: HuggingFace model identifier (e.g. "Qwen/Qwen2.5-7B-Instruct")
        revision: Branch, tag, or commit hash (default "main")

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

    files: list[ManifestFile] = []
    skipped: list[tuple[str, int]] = []  # (filename, size)

    for sibling in model_info.siblings or []:
        filename = sibling.rfilename

        # Skip non-essential files
        if filename.startswith(".") or filename in ("README.md", ".gitattributes"):
            continue

        size = sibling.size or 0

        # Get SHA-256 from LFS metadata
        lfs = sibling.lfs
        if lfs and lfs.get("sha256"):
            files.append(ManifestFile(
                filename=filename,
                sha256=lfs["sha256"],
                size_bytes=lfs.get("size", size),
            ))
        else:
            skipped.append((filename, size))

    # For small non-LFS files (config.json, tokenizer.json etc), download and hash
    if skipped:
        for filename, size in skipped:
            try:
                path = hf_hub_download(model_id, filename, revision=pinned_revision)
                h = hashlib.sha256()
                file_size = 0
                with open(path, "rb") as f:
                    while True:
                        chunk = f.read(8 * 1024 * 1024)
                        if not chunk:
                            break
                        h.update(chunk)
                        file_size += len(chunk)
                files.append(ManifestFile(
                    filename=filename,
                    sha256=h.hexdigest(),
                    size_bytes=file_size,
                ))
            except Exception as e:
                print(f"  Warning: skipping {filename}: {e}", file=sys.stderr)

    if not files:
        raise SystemExit(
            f"No files with SHA-256 hashes found for {model_id}@{revision}. "
            "The model may not use LFS for its files."
        )

    # Sort by filename for deterministic output
    files.sort(key=lambda f: f.filename)

    total_size = sum(f.size_bytes for f in files)

    return ModelManifest(
        model_id=model_id,
        revision=pinned_revision,
        files=files,
        total_size_bytes=total_size,
        created_at=datetime.now(timezone.utc).isoformat(),
    )


def main():
    parser = argparse.ArgumentParser(
        description="Generate a model manifest from HuggingFace Hub"
    )
    parser.add_argument("model_id", help="HuggingFace model ID (e.g. Qwen/Qwen2.5-7B-Instruct)")
    parser.add_argument("--revision", default="main", help="Branch, tag, or commit hash")
    parser.add_argument("--output", "-o", help="Output file (default: stdout)")
    args = parser.parse_args()

    print(f"Generating manifest for {args.model_id}@{args.revision}...", file=sys.stderr)

    manifest = generate(args.model_id, args.revision)

    output = json.dumps(manifest.to_dict(), indent=2) + "\n"

    if args.output:
        with open(args.output, "w") as f:
            f.write(output)
        print(f"Manifest written to {args.output}", file=sys.stderr)
    else:
        sys.stdout.write(output)

    print(f"  Model: {manifest.model_id}", file=sys.stderr)
    print(f"  Revision: {manifest.revision}", file=sys.stderr)
    print(f"  Files: {manifest.file_count} ({len(manifest.weight_files)} weights)", file=sys.stderr)
    print(f"  Total size: {manifest.total_size_bytes / (1024**3):.2f} GB", file=sys.stderr)


if __name__ == "__main__":
    main()
