#!/usr/bin/env python3
"""
TEE Client — Verify attestation and perform encrypted inference.

Implements the client side of proposal #951 inference pipeline:
1. GET /attestation → verify AMD cert chain + SNP report
2. Verify keys are bound to TEE (SHA-512 check)
3. Generate ephemeral keypair
4. Encrypt request with TEE's pubkey
5. POST /v1/chat/completions (encrypted)
6. Decrypt response with ephemeral private key
7. Verify metadata signature

Usage:
    python tee_client.py --url http://HOST:PORT --prompt "Hello, world"
"""

import argparse
import base64
import hashlib
import json
import sys


import httpx
from nacl.public import PrivateKey, PublicKey, Box
from nacl.signing import VerifyKey
from nacl.utils import random as nacl_random


def verify_attestation(att: dict) -> bool:
    """
    Verify attestation bundle.

    In production, steps 1-4 would use AMD's root cert from
    https://download.amd.com/developer/eula/sev/ask_ark_milan.cert
    and cryptographically verify the full chain.

    Here we check:
    - Verification flags (certs + report validated by snpguest on the node)
    - Keys are bound to SNP report (SHA-512 match)
    - Debug mode is disabled
    """
    v = att.get("verification", {})
    tee_type = att.get("tee_type", "unknown")
    report = att.get("report", {})
    parsed = report.get("parsed", {})
    policy = parsed.get("policy", {})

    print("\n=== Attestation Verification ===")
    print(f"  TEE type: {tee_type}")

    # Check cert chain was validated
    certs_ok = v.get("certs_valid", False)
    print(f"  Certificate chain:              {'PASS' if certs_ok else 'FAIL'}")

    # Check report signature
    report_ok = v.get("report_valid", False)
    print(f"  Report signature:               {'PASS' if report_ok else 'FAIL'}")

    # Check keys bound to report
    enc_pub = att.get("encryption_pubkey", "")
    sign_pub = att.get("signing_pubkey", "")
    expected_rd = hashlib.sha512(
        bytes.fromhex(enc_pub) + bytes.fromhex(sign_pub)
    ).hexdigest()
    actual_rd = report.get("report_data", "")
    keys_ok = expected_rd == actual_rd
    print(f"  Keys bound to TEE:              {'PASS' if keys_ok else 'FAIL'}")

    # Platform-specific checks
    if tee_type == "amd-sev-snp":
        debug = policy.get("debug_allowed", True)
        print(f"  Debug disabled:                 {'PASS' if not debug else 'FAIL (INSECURE!)'}")
        report_v = parsed.get("version", 0)
        print(f"  SNP report version:             {report_v} {'(v3 = SEV-SNP)' if report_v == 3 else 'UNEXPECTED'}")
    elif tee_type == "intel-tdx":
        report_v = parsed.get("version", 0)
        print(f"  TDX Quote version:              {report_v} {'(v4 = TDX)' if report_v == 4 else 'UNEXPECTED'}")

    # Show hardware
    hw = att.get("hardware", {})
    print(f"\n  Hardware TEE: {hw.get('tee_type', 'unknown')}")
    if tee_type == "amd-sev-snp":
        print(f"  CPU: family {hw.get('cpu_family')}, model {hw.get('cpu_model')}")
        print(f"  Chip ID: {hw.get('chip_id', 'unknown')[:32]}...")
        print(f"  SEV API: {hw.get('sev_version', {}).get('current', 'unknown')}")
    elif tee_type == "intel-tdx":
        print(f"  MR_SEAM: {(hw.get('mr_seam') or 'unknown')[:32]}...")
        print(f"  TEE TCB SVN: {hw.get('tee_tcb_svn', 'unknown')[:32]}...")

    # Show VM image
    vm = att.get("vm_image", {})
    print(f"\n  OS: {vm.get('os')}")
    print(f"  Kernel: {vm.get('kernel')}")
    print(f"  vLLM: {vm.get('vllm_version')}")
    print(f"  Measurement: {report.get('measurement', 'unknown')[:32]}...")
    if vm.get("image_hash"):
        print(f"  Image hash: {vm['image_hash']}")

    # GPU
    gpu = att.get("gpu")
    if gpu:
        print(f"\n  GPU: {gpu.get('gpu_name')} mode={gpu.get('mode')}")
    else:
        print(f"\n  GPU CC: not available")

    # Determine overall trust
    if tee_type == "amd-sev-snp":
        debug = policy.get("debug_allowed", True)
        all_ok = certs_ok and report_ok and keys_ok and not debug
    else:
        all_ok = certs_ok and report_ok and keys_ok

    print(f"\n  OVERALL: {'TRUSTED' if all_ok else 'NOT TRUSTED'}")
    return all_ok


def encrypted_inference(url: str, enc_pubkey_hex: str, sign_pubkey_hex: str,
                        messages: list, model: str = "Qwen/Qwen2.5-0.5B-Instruct",
                        max_tokens: int = 128, temperature: float = 0.7) -> dict:
    """
    Perform encrypted inference against a Confidential MLNode.

    1. Generate ephemeral X25519 keypair
    2. Encrypt OpenAI request with TEE's pubkey
    3. POST encrypted request
    4. Decrypt response
    5. Verify metadata signature
    """
    # Generate ephemeral keypair (one-time, for this request only)
    client_private = PrivateKey.generate()
    client_public = client_private.public_key
    tee_enc_pub = PublicKey(bytes.fromhex(enc_pubkey_hex))

    # Build OpenAI request
    openai_req = {
        "model": model,
        "messages": messages,
        "max_tokens": max_tokens,
        "temperature": temperature,
    }

    # Encrypt with NaCl box
    box = Box(client_private, tee_enc_pub)
    nonce = nacl_random(24)
    plaintext = json.dumps(openai_req).encode()
    ciphertext = box.encrypt(plaintext, nonce).ciphertext

    print("\n=== Encrypted Inference ===")
    print(f"  Client ephemeral pubkey: {bytes(client_public).hex()[:32]}...")
    print(f"  Request size: {len(plaintext)} bytes → {len(ciphertext)} bytes encrypted")

    # Send encrypted request
    payload = {
        "ciphertext": base64.b64encode(ciphertext).decode(),
        "nonce": base64.b64encode(nonce).decode(),
        "sender_pubkey": bytes(client_public).hex(),
    }

    resp = httpx.post(f"{url}/v1/chat/completions", json=payload, timeout=300.0)
    resp.raise_for_status()
    enc_resp = resp.json()

    # Decrypt response
    resp_ct = base64.b64decode(enc_resp["ciphertext"])
    resp_nonce = base64.b64decode(enc_resp["nonce"])
    resp_box = Box(client_private, tee_enc_pub)
    decrypted = resp_box.decrypt(resp_ct, resp_nonce)
    result = json.loads(decrypted)

    print(f"  Response decrypted: {len(decrypted)} bytes")

    # Verify metadata signature
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

    # Verify response_hash binds metadata to ciphertext
    expected_hash = hashlib.sha256(resp_ct).hexdigest()
    actual_hash = metadata.get("response_hash", "")
    hash_ok = expected_hash == actual_hash
    print(f"  Response hash bound: {'VALID' if hash_ok else 'INVALID'}")

    print(f"  Tokens: {metadata.get('prompt_tokens')} prompt + "
          f"{metadata.get('completion_tokens')} completion = "
          f"{metadata.get('total_tokens')} total")

    # Show response
    content = result.get("choices", [{}])[0].get("message", {}).get("content", "")
    print(f"\n  Response: {content}")

    return result


def main():
    parser = argparse.ArgumentParser(description="TEE MLNode Client")
    parser.add_argument("--url", required=True, help="MLNode URL (e.g. http://localhost:8080)")
    parser.add_argument("--prompt", default="What is confidential computing?",
                        help="User prompt")
    parser.add_argument("--model", default="Qwen/Qwen2.5-0.5B-Instruct")
    parser.add_argument("--max-tokens", type=int, default=128)
    parser.add_argument("--skip-verify", action="store_true",
                        help="Skip attestation verification (dev only)")
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

    result = encrypted_inference(
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
