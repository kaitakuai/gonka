package artifacts

// ArtifactStore defines the interface for PoC artifact storage with Merkle commitments.
// Implementations must be safe for concurrent use.
type ArtifactStore interface {
	// Add appends an artifact if nonce is not already in the store.
	// Deprecated: Use AddWithNode to track per-node distribution.
	Add(nonce int32, vector []byte) error

	// AddWithNode appends an artifact and tracks which node contributed it.
	// Returns ErrDuplicateNonce if nonce already exists.
	AddWithNode(nonce int32, vector []byte, nodeId string) error

	// GetRoot returns the current root hash, or nil if empty.
	GetRoot() []byte

	// GetRootAt returns the root hash at a specific snapshot count.
	// Returns nil if snapshotCount is 0, error if snapshotCount exceeds current count.
	GetRootAt(snapshotCount uint32) ([]byte, error)

	// GetFlushedRoot returns the root and count of ONLY persisted artifacts.
	// Safe to report externally - survives process crashes.
	GetFlushedRoot() (count uint32, root []byte)

	// Count returns the total number of artifacts (including unflushed).
	Count() uint32

	// GetArtifact retrieves an artifact by its dense index.
	// Dense index is the sequential position [0, count).
	GetArtifact(denseIndex uint32) (nonce int32, vector []byte, err error)

	// GetProof generates a merkle proof for denseIndex at snapshotCount.
	GetProof(denseIndex uint32, snapshotCount uint32) ([][]byte, error)

	// GetNodeDistribution returns a copy of the flushed node distribution.
	GetNodeDistribution() map[string]uint32

	// GetNodeCounts returns a copy of the current (unflushed) node distribution.
	GetNodeCounts() map[string]uint32

	// Flush persists buffered artifacts to disk.
	Flush() error

	// Close flushes and releases resources.
	Close() error

	// PrebuildSnapshot builds and caches tree state at specified count for fast proofs.
	PrebuildSnapshot(count uint32) error
}
