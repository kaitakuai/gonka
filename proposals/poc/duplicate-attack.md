# Proposal: Duplicate Nonce Attack Mitigation

This proposal addresses a vulnerability in the PoC v2 off-chain artifact system where a malicious participant could inflate their score by duplicating valid artifacts instead of performing honest computation.

## Problem

In PoC v2, participants generate artifacts off-chain and commit only a Merkle root + count to the chain. Validators sample a small random subset (e.g., 200 out of 100,000) to verify. The current MMR (Merkle Mountain Range) does not enforce any relationship between an artifact's position (leaf index) and its nonce.

**Attack mechanism:**

1. Generate fewer legitimate `(nonce, vector)` pairs than claimed (e.g., 90,000 instead of 100,000)
2. Duplicate some pairs to fill the remaining slots
3. Commit `(root_hash, 100000)` to chain
4. Serve valid proofs when validators request artifacts

Validators have no way to detect that the same `(nonce, vector)` exists at multiple indices unless they happen to sample both.

### Why Duplicates Are Hard to Detect

For comparison, consider invalid artifacts (wrong computation). If a validator samples any invalid artifact, it fails verification immediately. Detection probability follows:

```
P(invalid detected) = 1 - (1 - rate)^k
```

| Fraud Rate | Invalid Artifact Detection | Duplicate Detection |
|:-----------|:---------------------------|:--------------------|
| 1% | 86.6% | 0.2% |
| 10% | ~100% | 2.0% |
| 50% | ~100% | 9.5% |

Invalid artifacts are detected independently when sampled. Duplicates require sampling *both* copies of the same nonce — a collision event with probability proportional to `k²/N`, not `k/N`. This makes duplicate detection fundamentally harder and motivates structural prevention.

### Duplicate Detection Probability

For N=100,000 artifacts with sample size k=200, probability that a single validator's sample contains at least one duplicate pair:

| Duplicate Rate | Duplicate Pairs | Detection Probability |
|:---------------|:----------------|:----------------------|
| 10% | 5,000 | ~2.0% |
| 20% | 10,000 | ~3.9% |
| 30% | 15,000 | ~5.8% |
| 50% | 25,000 | ~9.5% |

Even with 50% of artifacts being duplicates, a single validator has less than 10% chance of detecting fraud.

### Impact of Population Size

The attack becomes more effective with larger artifact counts:

| Total Artifacts | 10% Duplicates | 20% Duplicates | 30% Duplicates | 50% Duplicates |
|:----------------|:---------------|:---------------|:---------------|:---------------|
| 1,000 | 86.4% | 98.1% | 99.7% | 100.0% |
| 5,000 | 32.8% | 54.9% | 69.7% | 86.3% |
| 10,000 | 18.0% | 32.8% | 45.0% | 63.0% |
| 100,000 | 2.0% | 3.9% | 5.8% | 9.5% |
| 1,000,000 | 0.2% | 0.4% | 0.6% | 1.0% |

With large artifact counts (100k+), detection probability drops below 10% even for aggressive duplication.

### Multi-Validator Aggregation

With multiple validators, detection improves but remains limited:

```
P(at least one detects) = 1 - (1 - P_single)^V
```

For 100k artifacts with 10% duplicates and 10 validators, where `P_single = 2.0%` from the table above:

```
P = 1 - (1 - 0.02)^10 ≈ 18%
```

Still an 82% chance of the attack going undetected.

### Non-Response Evasion

Even the low detection probability overstates the risk to attackers. A dishonest participant can evade detection entirely:

1. Receive proof request for leaf index `I`
2. Check if index `I` corresponds to a duplicated nonce
3. If yes, refuse to respond (timeout)

From the validator's perspective, this is indistinguishable from legitimate network issues. Without aggressive non-response penalties (which would punish honest nodes with connectivity problems), attackers can selectively hide evidence of duplication.

This further motivates structural prevention over detection-based approaches.

## Proposal

**Goal**: Enforce `position == nonce` in the commitment structure. If each nonce can only occupy one specific slot, duplicates become impossible by design.

Approach: use a **Sparse Merkle Sum Tree (SMST)** where nonce determines the tree path.

### Constraints

- Commits every 5 seconds during generation
- Generation phase: ~5 minutes, ~5M artifacts
- Max nonce: ~15M (tree depth D=24 optimization target). Larger nonces must work correctly with deeper tree - D is not a hard limit, just optimization target.
- Validation happens after generation completes

### SMST Overview

**Structure**: Fixed-depth binary tree where nonce bits determine the path. Empty subtrees use precomputed "empty hash".

**Sum property**: Each node stores `count = left.count + right.count`. Enables sampling dense index `[0, total_count)` in sparse tree:

```
At each node:
  if index < left.count: go left
  else: go right, index -= left.count
```

**Why duplicates are impossible**: Position determined by nonce value. One slot per nonce. Inserting same nonce overwrites, doesn't duplicate.

### Option A: SMST During Generation (Selected)

Maintain SMST incrementally. Commit root every 5 seconds.

- Pro: Single structure, duplicates impossible at all times
- Pro: No equivalence proof needed - SMST is the only structure
- Con: Insert O(24) vs MMR O(1), needs snapshot handling for historical commits

Insert overhead: ~100ms per 5-sec window at 83K inserts - acceptable.

### Option B: MMR + Post-Generation SMST (Rejected)

Rejected due to equivalence proof complexity. Would require proving MMR and SMST contain exactly the same artifacts, with no way to add nonces after last MMR commit. Option A avoids this entirely.

### Complexity

| Operation | Cost |
|:----------|:-----|
| Insert | O(D) = O(24) hashes per artifact |
| 5-sec window (83K inserts) | ~2M hashes, ~100ms |
| Proof generation | O(D) = O(24) |
| Tree rebuild from disk (5M) | ~6s |

## Implementation Plan

### Goal

Minimal changes. Keep existing messages and flows unchanged.

### Scope

**Unchanged**:
- `MsgPoCV2StoreCommit` - same fields `(creator, poc_stage_start_block_height, count, root_hash)`
- `MsgMLNodeWeightDistribution` - unchanged
- `managed_store.go` - works with any store implementation

**New/Modified**:
- Extract `ArtifactStore` interface from current concrete type
- Implement SMST store as alternative implementation
- Add `use_smst` param (default: true) to `PocParams`
- Config manager selects implementation based on param

### Interface

Extract from current `store.go`:

```go
type ArtifactStore interface {
    AddWithNode(nonce int32, vector []byte, nodeId string) error
    GetRoot() []byte
    GetRootAt(snapshotCount uint32) ([]byte, error)
    GetFlushedRoot() (count uint32, root []byte)
    Count() uint32
    GetArtifact(denseIndex uint32) (nonce int32, vector []byte, error)
    GetProof(denseIndex uint32, snapshotCount uint32) ([][]byte, error)
    GetNodeDistribution() map[string]uint32
    Flush() error
    Close() error
}
```

Note: `leafIndex` renamed to `denseIndex`. For MMR it's sequential position, for SMST it's sum-navigated position.

### Persistence

Reuse current `artifacts.data` format - artifacts stored in arrival order. On load, extract nonces to rebuild SMST.

```
Per PoC stage directory:
  artifacts.data    - [len][nonce][vector]... in arrival order (reuse current format!)
  roots.json        - {count: root_hash} for committed snapshots
```

**Critical: roots.json writes must be ATOMIC** (write to temp file, then rename). This is the source of truth for what was committed on-chain.

Arrival-order is implicit in `artifacts.data` - no separate nonces file needed.

### Snapshots

**Why needed**: Multiple commits during generation (every 5 sec). We don't know which commit will be final on-chain. Validators query specific (root, count) pair.

**Approach**:
- Snapshot at count N = SMST built from first N artifacts in `artifacts.data`
- `GetRootAt(count)`: O(1) lookup in `roots.json`
- `GetProof(index, count)`: rebuild SMST from `artifacts[0:count]`, generate proof

**Rebuild cost**: ~6s for 5M. Mitigations:
- **Prebuild on distribution submit**: `submitWeightDistribution()` queries on-chain for final committed count. Trigger SMST build here - before validators request proofs.
- All validators query same count, single prebuild serves all
- Cache remains in memory until store pruned

### Recovery

On restart, rebuild state from `artifacts.data`:

| Scenario | Recovery |
|:---------|:---------|
| During generation | Read artifacts, rebuild SMST, continue accepting inserts |
| After generation, before prebuild | Read artifacts, wait for prebuild trigger |
| After prebuild | Read artifacts, rebuild SMST at committed count |

Load time: ~6s for 5M artifacts (matches current MMR approach).

### Proof Format

MMR and SMST proofs are structurally different. Verifier needs to know which type.

Recommendation: At upgrade block, all new commits use SMST. Old commits already validated. No runtime switch needed - just code change at upgrade.

### Files

**DAPI (decentralized-api):**

| File | Change |
|:-----|:-------|
| `artifacts/store.go` | Extract interface, rename to `mmr_store.go` |
| `artifacts/smst.go` | New SMST tree + `VerifyProof()` |
| `artifacts/smst_store.go` | New store using SMST |
| `artifacts/interface.go` | Interface definition |
| `artifacts/managed_store.go` | Use interface instead of concrete type |
| `poc/proof_client.go` | Call SMST `VerifyProof()` based on config |
| `poc/commit_worker.go` | Trigger SMST prebuild in `submitWeightDistribution()` |
| `apiconfig/config_manager.go` | Select implementation based on param |

**Chain (inference-chain):**

| File | Change |
|:-----|:-------|
| `proto/inference/inference/params.proto` | Add `bool use_smst = 13` to `PocParams` |

**Testermint:**

| File | Change |
|:-----|:-------|
| `src/main/kotlin/data/AppExport.kt` | Add `useSmst` field to `PocParams` data class |
| `src/main/kotlin/Main.kt` | Set `useSmst = true` in test setup |

### Development Process

**Phase 1: Benchmark current MMR**

Create stress tests measuring:
- Write throughput: inserts/sec (in-memory and with flush)
- Read throughput: proofs/sec
- Proof size: bytes per proof
- Load from disk: time to rebuild tree from `artifacts.data`
- Document results in `proposals/poc/duplicate-attack-perf.md`

Expected proof sizes:
- MMR (N=5M): ~1.5KB (path + peaks)
- SMST (D=24): ~900 bytes (fixed depth, no peaks)
- Artifact: ~28 bytes (nonce + vector)

**Phase 2: Extract interface**

- Create `interface.go` with `ArtifactStore` interface
- Rename `store.go` -> `mmr_store.go`
- Update `managed_store.go` to use interface
- Verify existing tests pass

**Phase 3: Implement SMST**

- Create `smst.go` with core tree operations
- Create `smst_store.go` implementing interface
- Unit tests for correctness
- **Dynamic depth**: Tree depth determined by max nonce seen, not hardcoded. D=24 covers nonces up to 16.7M. If nonce > 2^24, depth increases automatically. No failures, just slightly more hashes per operation.
- **Verifier depth**: Verifier infers depth from proof length (number of sibling entries). No hardcoded depth needed for validation.

**Phase 4: Benchmark SMST**

- Run same stress tests as Phase 1
- Additional SMST-specific tests:
  - Load 5M tree from disk to memory (target: <10s)
  - Reset 5M tree to 4.9M state by rebuild (measure time)
- Compare throughput with MMR
- Document results

**Phase 5: Integration**

- Add `use_smst` param
- Config manager implementation selection
- Integration tests with testermint

### Expected Throughput

Pure engine (in-memory, no disk):
- MMR: millions of inserts/sec (O(1) append)
- SMST: 100K-500K inserts/sec (O(24) per insert)

With disk persistence, both bottleneck on I/O.

## Design Notes

### Proof Verification Location

Verified: proof verification is **off-chain only**, in DAPI:
- `decentralized-api/poc/proof_client.go` - `FetchAndVerifyProofs()`
- `decentralized-api/poc/artifacts/mmr.go` - `VerifyProof()`

No on-chain verification in `inference-chain`. This simplifies the upgrade - only DAPI code needs SMST verifier.

### Cache Eviction

Reuse existing `managed_store.go` cleanup (already implemented for MMR):
- `retainCount` = 10 (keeps last 10 PoC stages)
- `cleanupLoop` runs every 30 seconds, prunes oldest stores
- SMST store evicted same as MMR store - no changes needed

### Migration

No migration needed. At upgrade:
- Old epochs: already validated, data pruned
- New epochs: use SMST from start

The `use_smst` param could be compile-time constant rather than runtime config.

### Stress Test Scope

Test both:
- In-memory: measures pure engine speed
- With persistence: measures realistic throughput including disk I/O and lock contention
