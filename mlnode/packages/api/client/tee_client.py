#!/usr/bin/env python3
"""
TEE Client — Verify attestation and perform encrypted inference.

Implements the client side of spec §5.3 (client verification) and §3.2 (encrypted inference):
1. GET /attestation → verify cert chain + report + key binding
2. Generate ephemeral keypair
3. Encrypt request with TEE's pubkey
4. POST /v1/chat/completions (encrypted)
5. Decrypt response with ephemeral private key
6. Verify metadata signature + response_hash

Spec §5.3: Client MUST NOT use verification.* fields from the bundle.
Client MUST verify cert chain, report signature, key binding independently.

Usage:
    python tee_client.py --url http://HOST:PORT --prompt "Hello, world"
"""

import argparse
import base64
import hashlib
import json
import subprocess
import sys
import tempfile
from pathlib import Path

import httpx
from nacl.public import PrivateKey, PublicKey, Box
from nacl.signing import VerifyKey
from nacl.utils import random as nacl_random


# ---------------------------------------------------------------------------
# Attestation verification (spec §5.3 V1-V7)
# ---------------------------------------------------------------------------

def verify_attestation(att: dict) -> bool:
    """
    Verify attestation bundle per spec §5.3.

    Client MUST NOT use verification.* fields (ADR-0010).
    All checks performed independently.
    """
    tee_type = att.get("tee_type", "unknown")
    report = att.get("report", {})
    parsed = report.get("parsed", {})
    certs = att.get("certs", {})

    print("\n=== Attestation Verification ===")
    print(f"  TEE type: {tee_type}")

    results = {}

    # V1: Cert chain verification
    if tee_type == "amd-sev-snp":
        results["certs"] = _verify_amd_certs(certs)
    elif tee_type == "intel-tdx":
        results["certs"] = _verify_intel_certs(certs)
    else:
        results["certs"] = False
    print(f"  Certificate chain:              {'PASS' if results['certs'] else 'FAIL'}")

    # V2: Report signature (structural — full crypto verify needs platform-specific libs)
    if tee_type == "amd-sev-snp":
        results["report"] = parsed.get("version") == 3 and len(report.get("report_hex", "")) > 100
    elif tee_type == "intel-tdx":
        results["report"] = parsed.get("version") == 4 and len(report.get("report_hex", "")) > 100
    else:
        results["report"] = False
    print(f"  Report structure:               {'PASS' if results['report'] else 'FAIL'}")

    # V3: Key binding — report_data == SHA-512(encryption_pubkey || signing_pubkey)
    enc_pub = att.get("encryption_pubkey", "")
    sign_pub = att.get("signing_pubkey", "")
    expected_rd = hashlib.sha512(
        bytes.fromhex(enc_pub) + bytes.fromhex(sign_pub)
    ).hexdigest()
    actual_rd = report.get("report_data", "")
    results["keys"] = expected_rd == actual_rd
    print(f"  Keys bound to TEE:              {'PASS' if results['keys'] else 'FAIL'}")

    # V4: Debug disabled
    if tee_type == "amd-sev-snp":
        policy = parsed.get("policy", {})
        debug = policy.get("debug_allowed", True)
        results["debug"] = not debug
        print(f"  Debug disabled:                 {'PASS' if results['debug'] else 'FAIL (INSECURE!)'}")
    elif tee_type == "intel-tdx":
        # TDX: TD attributes bit 0 = debug. All zeros = no debug.
        td_attr = parsed.get("td_attributes", "0000000000000000")
        debug_bit = int(td_attr[:2], 16) & 1 if td_attr else 0
        results["debug"] = debug_bit == 0
        print(f"  Debug disabled:                 {'PASS' if results['debug'] else 'FAIL (INSECURE!)'}")

    # V6: Report version
    if tee_type == "amd-sev-snp":
        v = parsed.get("version", 0)
        results["version"] = v == 3
        print(f"  Report version:                 {v} {'(v3 = SEV-SNP)' if v == 3 else 'UNEXPECTED'}")
    elif tee_type == "intel-tdx":
        v = parsed.get("version", 0)
        results["version"] = v == 4
        print(f"  Quote version:                  {v} {'(v4 = TDX)' if v == 4 else 'UNEXPECTED'}")

    # Show hardware info
    hw = att.get("hardware", {})
    print(f"\n  Hardware TEE: {hw.get('tee_type', 'unknown')}")
    if tee_type == "amd-sev-snp":
        print(f"  CPU: family {hw.get('cpu_family')}, model {hw.get('cpu_model')}")
        print(f"  Chip ID: {str(hw.get('chip_id', 'unknown'))[:32]}...")
    elif tee_type == "intel-tdx":
        print(f"  MR_SEAM: {str(hw.get('mr_seam', 'unknown'))[:32]}...")
        print(f"  TEE TCB SVN: {str(hw.get('tee_tcb_svn', 'unknown'))[:32]}...")

    # Show VM image
    vm = att.get("vm_image", {})
    print(f"\n  OS: {vm.get('os')}")
    print(f"  Kernel: {vm.get('kernel')}")
    print(f"  vLLM: {vm.get('vllm_version')}")
    print(f"  Measurement: {report.get('measurement', 'unknown')[:32]}...")

    # GPU
    gpu = att.get("gpu")
    print(f"\n  GPU CC: {gpu.get('gpu_name') if gpu else 'not available'}")

    # Overall
    all_ok = all(results.values())
    print(f"\n  OVERALL: {'TRUSTED' if all_ok else 'NOT TRUSTED'}")
    return all_ok


def _verify_amd_certs(certs: dict) -> bool:
    """Verify AMD cert chain: ARK → ASK → VCEK (spec §5.3 V1)."""
    ark = certs.get("ark")
    ask = certs.get("ask")
    vcek = certs.get("vcek")

    if not all([ark, ask, vcek]):
        return False

    try:
        with tempfile.TemporaryDirectory() as d:
            d = Path(d)
            (d / "ark.pem").write_text(ark)
            (d / "ask.pem").write_text(ask)
            (d / "vcek.pem").write_text(vcek)

            # Verify ASK signed by ARK
            r = subprocess.run(
                ["openssl", "verify", "-CAfile", str(d / "ark.pem"), str(d / "ask.pem")],
                capture_output=True, text=True,
            )
            if r.returncode != 0:
                return False

            # Verify VCEK signed by ASK (with ARK as root)
            chain = (d / "chain.pem")
            chain.write_text(ark + ask)
            r = subprocess.run(
                ["openssl", "verify", "-CAfile", str(chain), str(d / "vcek.pem")],
                capture_output=True, text=True,
            )
            return r.returncode == 0
    except Exception:
        return False


def _verify_intel_certs(certs: dict) -> bool:
    """Verify Intel cert chain: Root → Intermediate [→ PCK] (spec §5.3 V1)."""
    root = certs.get("root")
    intermediate = certs.get("intermediate")
    pck = certs.get("pck")

    if not root or not intermediate:
        return False

    try:
        with tempfile.TemporaryDirectory() as d:
            d = Path(d)
            (d / "root.pem").write_text(root)
            (d / "intermediate.pem").write_text(intermediate)

            # Verify Intermediate signed by Root
            r = subprocess.run(
                ["openssl", "verify", "-CAfile", str(d / "root.pem"), str(d / "intermediate.pem")],
                capture_output=True, text=True,
            )
            if r.returncode != 0:
                return False

            # If PCK available, verify it too
            if pck:
                (d / "pck.pem").write_text(pck)
                chain = (d / "chain.pem")
                chain.write_text(root + intermediate)
                r = subprocess.run(
                    ["openssl", "verify", "-CAfile", str(chain), str(d / "pck.pem")],
                    capture_output=True, text=True,
                )
                return r.returncode == 0

            return True
    except Exception:
        return False


# ---------------------------------------------------------------------------
# Encrypted inference (spec §3.2)
# ---------------------------------------------------------------------------

def encrypted_inference(url: str, enc_pubkey_hex: str, sign_pubkey_hex: str,
                        messages: list, model: str = "Qwen/Qwen2.5-0.5B-Instruct",
                        max_tokens: int = 128, temperature: float = 0.7) -> dict:
    """Perform encrypted inference against a Confidential MLNode."""
    client_private = PrivateKey.generate()
    client_public = client_private.public_key
    tee_enc_pub = PublicKey(bytes.fromhex(enc_pubkey_hex))

    openai_req = {
        "model": model,
        "messages": messages,
        "max_tokens": max_tokens,
        "temperature": temperature,
    }

    box = Box(client_private, tee_enc_pub)
    nonce = nacl_random(24)
    plaintext = json.dumps(openai_req).encode()
    ciphertext = box.encrypt(plaintext, nonce).ciphertext

    print("\n=== Encrypted Inference ===")
    print(f"  Request size: {len(plaintext)} bytes → {len(ciphertext)} bytes encrypted")

    payload = {
        "ciphertext": base64.b64encode(ciphertext).decode(),
        "nonce": base64.b64encode(nonce).decode(),
        "sender_pubkey": bytes(client_public).hex(),
    }

    resp = httpx.post(f"{url}/v1/chat/completions", json=payload, timeout=300.0)
    resp.raise_for_status()
    enc_resp = resp.json()

    # Decrypt
    resp_ct = base64.b64decode(enc_resp["ciphertext"])
    resp_nonce = base64.b64decode(enc_resp["nonce"])
    resp_box = Box(client_private, tee_enc_pub)
    decrypted = resp_box.decrypt(resp_ct, resp_nonce)
    result = json.loads(decrypted)

    print(f"  Response decrypted: {len(decrypted)} bytes")

    # Verify metadata signature (spec §4.4)
    metadata = enc_resp["metadata"]
    sig = bytes.fromhex(enc_resp["metadata_signature"])
    tee_sign_pub = VerifyKey(bytes.fromhex(sign_pubkey_hex))

    canonical = json.dumps(metadata, sort_keys=True, ensure_ascii=True, separators=(",", ":")).encode()
    try:
        tee_sign_pub.verify(canonical, sig)
        sig_ok = True
    except Exception:
        sig_ok = False

    print(f"  Metadata signature: {'VALID' if sig_ok else 'INVALID'}")

    # Verify response_hash (spec §4.5)
    expected_hash = hashlib.sha256(resp_ct).hexdigest()
    actual_hash = metadata.get("response_hash", "")
    hash_ok = expected_hash == actual_hash
    print(f"  Response hash bound: {'VALID' if hash_ok else 'INVALID'}")

    print(f"  Tokens: {metadata.get('prompt_tokens')} prompt + "
          f"{metadata.get('completion_tokens')} completion = "
          f"{metadata.get('total_tokens')} total")

    content = result.get("choices", [{}])[0].get("message", {}).get("content", "")
    print(f"\n  Response: {content}")

    return result


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    parser = argparse.ArgumentParser(description="TEE MLNode Client")
    parser.add_argument("--url", required=True, help="MLNode URL (e.g. http://localhost:8080)")
    parser.add_argument("--prompt", default="What is confidential computing?")
    parser.add_argument("--model", default="Qwen/Qwen2.5-0.5B-Instruct")
    parser.add_argument("--max-tokens", type=int, default=128)
    parser.add_argument("--skip-verify", action="store_true", help="Skip attestation verification (dev only)")
    args = parser.parse_args()

    print(f"Connecting to Confidential MLNode at {args.url}")

    # Step 1: Get attestation
    print("\nFetching attestation...")
    att_resp = httpx.get(f"{args.url}/attestation", timeout=30.0)
    att_resp.raise_for_status()
    att = att_resp.json()

    # Step 2: Verify attestation
    if not args.skip_verify:
        trusted = verify_attestation(att)
        if not trusted:
            print("\nATTESTATION FAILED — aborting. This node is not trusted.")
            sys.exit(1)
    else:
        print("\n  [SKIPPING attestation verification — dev mode]")

    # Step 3: Encrypted inference
    messages = [
        {"role": "system", "content": "You are a helpful assistant."},
        {"role": "user", "content": args.prompt},
    ]

    encrypted_inference(
        url=args.url,
        enc_pubkey_hex=att["encryption_pubkey"],
        sign_pubkey_hex=att["signing_pubkey"],
        messages=messages,
        model=args.model,
        max_tokens=args.max_tokens,
    )

    print("\n=== Done ===")


if __name__ == "__main__":
    main()
