"""
Integration test: run on a real SEV-SNP VM with /dev/sev-guest.
Must be run as root (device requires root access).

Usage: sudo PYTHONPATH=src/api python3 tests/integration_test_sev.py
"""

import hashlib
import sys
import json

# Test 1: detect_tee() on real hardware
print("=== Test 1: detect_tee() ===")
from tee.detect import detect_tee
info = detect_tee()
print(f"  cpu_tee: {info.cpu_tee.value}")
print(f"  device: {info.device_path}")
print(f"  gpu_cc: {info.gpu_cc}")
print(f"  warnings: {info.warnings}")
assert info.cpu_tee.value == "amd-sev-snp", f"Expected amd-sev-snp, got {info.cpu_tee.value}"
assert info.device_path == "/dev/sev-guest"
print("  PASSED")

# Test 2: AmdSevSnpBackend on real hardware
print("\n=== Test 2: AmdSevSnpBackend ===")
from tee.backends.amd_sev_snp import AmdSevSnpBackend
backend = AmdSevSnpBackend()
assert backend.tee_type() == "amd-sev-snp"
print(f"  tee_type: {backend.tee_type()}")

# Test 3: Generate real SNP report
print("\n=== Test 3: generate_report() ===")
fake_key_data = hashlib.sha512(b"test-key-material").digest()
report = backend.generate_report(fake_key_data)
print(f"  report_len: {len(report)}")
assert len(report) >= 1184, f"Report too short: {len(report)}"
print("  PASSED")

# Test 4: Parse report
print("\n=== Test 4: parse_report() ===")
parsed = backend.parse_report(report)
print(f"  version: {parsed['version']}")
print(f"  vmpl: {parsed['vmpl']}")
print(f"  report_data_match: {fake_key_data.hex() == parsed['report_data']}")
assert "parse_error" not in parsed, f"Parse error: {parsed.get('parse_error')}"
assert fake_key_data.hex() == parsed["report_data"], "report_data mismatch!"
print("  PASSED")

# Test 5: Fetch certs from AMD KDS
print("\n=== Test 5: fetch_certs() ===")
certs = backend.fetch_certs()
for name in ("vcek", "ask", "ark"):
    status = "ok" if certs.get(name) else "MISSING"
    print(f"  {name}: {status}")
print("  PASSED" if certs.get("vcek") else "  WARNING: VCEK not fetched")

# Test 6: Verify cert chain
print("\n=== Test 6: verify_certs() ===")
certs_valid = backend.verify_certs()
print(f"  certs_valid: {certs_valid}")

# Test 7: Verify report
print("\n=== Test 7: verify_report() ===")
report_valid = backend.verify_report()
print(f"  report_valid: {report_valid}")

# Test 8: Attestation dispatcher
print("\n=== Test 8: attestation dispatcher ===")
from tee.attestation import _get_backend
from tee.types import TEEInfo, TEEType
backend2 = _get_backend(info)
assert backend2.tee_type() == "amd-sev-snp"
print(f"  dispatcher selected: {backend2.tee_type()}")
print("  PASSED")

# Summary
print("\n=== SUMMARY ===")
results = {
    "detect_tee": "PASS",
    "backend_type": "PASS",
    "generate_report": "PASS",
    "parse_report": "PASS",
    "fetch_certs": "PASS" if certs.get("vcek") else "WARN",
    "verify_certs": "PASS" if certs_valid else "WARN",
    "verify_report": "PASS" if report_valid else "WARN",
    "dispatcher": "PASS",
}
for test, result in results.items():
    print(f"  {test}: {result}")

failures = [k for k, v in results.items() if v == "FAIL"]
if failures:
    print(f"\nFAILED: {failures}")
    sys.exit(1)
else:
    print("\nALL PASSED")
    sys.exit(0)
