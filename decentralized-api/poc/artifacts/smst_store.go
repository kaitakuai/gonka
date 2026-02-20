package artifacts

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

const (
	// MaxLeafCount caps artifacts to prevent overflow in size calculations.
	MaxLeafCount = (1 << 30) - 1 // 1,073,741,823
)

var (
	ErrDuplicateNonce      = errors.New("duplicate nonce")
	ErrLeafIndexOutOfRange = errors.New("leaf index out of range")
	ErrStoreClosed         = errors.New("store is closed")
	ErrCapacityExceeded    = errors.New("store capacity exceeded")
)

type bufferedArtifact struct {
	nonce  int32
	vector []byte
	nodeId string
}

// SMSTArtifactStore provides artifact storage with SMST commitments.
// Nonce determines tree position, making duplicates impossible by design.
type SMSTArtifactStore struct {
	mu     sync.RWMutex
	dir    string
	closed bool

	dataFile *os.File

	buffer        []bufferedArtifact
	offsets       []uint64         // arrival order -> disk offset
	nonceToOffset map[int32]uint64 // nonce -> disk offset (for fast lookup)
	smst          *SMST

	flushedLeafCount  uint32
	flushedDataOffset uint64
	flushedRoots      map[uint32][]byte

	nodeCounts        map[string]uint32
	flushedNodeCounts map[string]uint32

	prebuiltSnapshot *SMST
	prebuiltCount    uint32
}

var _ ArtifactStore = (*SMSTArtifactStore)(nil)

// OpenSMST opens or creates an SMST artifact store in the given directory.
func OpenSMST(dir string) (*SMSTArtifactStore, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create dir: %w", err)
	}

	dataPath := filepath.Join(dir, "artifacts.data")

	dataFile, err := os.OpenFile(dataPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("open data file: %w", err)
	}

	s := &SMSTArtifactStore{
		dir:               dir,
		dataFile:          dataFile,
		buffer:            make([]bufferedArtifact, 0, 1024),
		offsets:           make([]uint64, 0, 1024),
		nonceToOffset:     make(map[int32]uint64),
		smst:              NewSMST(smstDefaultDepth),
		flushedRoots:      make(map[uint32][]byte),
		nodeCounts:        make(map[string]uint32),
		flushedNodeCounts: make(map[string]uint32),
	}

	if err := s.recover(); err != nil {
		s.dataFile.Close()
		return nil, fmt.Errorf("recover: %w", err)
	}

	return s, nil
}

func (s *SMSTArtifactStore) recover() error {
	info, err := s.dataFile.Stat()
	if err != nil {
		return fmt.Errorf("stat data file: %w", err)
	}

	if info.Size() == 0 {
		return s.recoverNodeCounts()
	}

	if _, err := s.dataFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek data file: %w", err)
	}

	var offset uint64
	for {
		nonce, vector, n, err := readArtifact(s.dataFile)
		if err == io.EOF {
			break
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			if truncErr := s.dataFile.Truncate(int64(offset)); truncErr != nil {
				return fmt.Errorf("truncate after partial record: %w", truncErr)
			}
			break
		}
		if err != nil {
			return fmt.Errorf("read artifact at offset %d: %w", offset, err)
		}

		leafData := encodeLeaf(nonce, vector)
		leafHash := smstHashLeaf(leafData)

		if _, err := s.smst.Insert(nonce, leafHash); err != nil {
			return fmt.Errorf("insert nonce %d: %w", nonce, err)
		}

		s.offsets = append(s.offsets, offset)
		s.nonceToOffset[nonce] = offset
		offset += uint64(n)
	}

	s.flushedLeafCount = s.smst.Count()
	s.flushedDataOffset = offset

	rootHash, _ := s.smst.GetRoot()
	s.flushedRoots[s.flushedLeafCount] = rootHash

	if err := s.recoverNodeCounts(); err != nil {
		return fmt.Errorf("recover node counts: %w", err)
	}

	return nil
}

func (s *SMSTArtifactStore) recoverNodeCounts() error {
	nodesPath := filepath.Join(s.dir, "nodes.json")

	f, err := os.Open(nodesPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open nodes file: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat nodes file: %w", err)
	}
	if info.Size() == 0 {
		return nil
	}

	decoder := json.NewDecoder(f)
	if err := decoder.Decode(&s.flushedNodeCounts); err != nil {
		return fmt.Errorf("decode nodes file: %w", err)
	}

	for k, v := range s.flushedNodeCounts {
		s.nodeCounts[k] = v
	}

	return nil
}

func (s *SMSTArtifactStore) Add(nonce int32, vector []byte) error {
	return s.AddWithNode(nonce, vector, "")
}

func (s *SMSTArtifactStore) AddWithNode(nonce int32, vector []byte, nodeId string) error {
	leafData := encodeLeaf(nonce, vector)
	leafHash := smstHashLeaf(leafData)

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}

	if s.smst.Count() >= MaxLeafCount {
		return ErrCapacityExceeded
	}

	if _, err := s.smst.Insert(nonce, leafHash); err != nil {
		return err
	}

	s.buffer = append(s.buffer, bufferedArtifact{nonce: nonce, vector: vector, nodeId: nodeId})

	if nodeId != "" {
		s.nodeCounts[nodeId]++
	}

	return nil
}

func (s *SMSTArtifactStore) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}

	return s.flushLocked()
}

func (s *SMSTArtifactStore) flushLocked() error {
	if len(s.buffer) == 0 {
		return nil
	}

	if _, err := s.dataFile.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek data file: %w", err)
	}

	w := bufio.NewWriter(s.dataFile)
	offset := s.flushedDataOffset

	for _, art := range s.buffer {
		s.offsets = append(s.offsets, offset)
		s.nonceToOffset[art.nonce] = offset

		n, err := writeArtifact(w, art.nonce, art.vector)
		if err != nil {
			return fmt.Errorf("write artifact: %w", err)
		}
		offset += uint64(n)
	}

	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush buffer: %w", err)
	}
	if err := s.dataFile.Sync(); err != nil {
		return fmt.Errorf("sync data file: %w", err)
	}

	if err := s.flushNodeCountsLocked(); err != nil {
		return fmt.Errorf("flush node counts: %w", err)
	}

	s.flushedLeafCount = s.smst.Count()
	s.flushedDataOffset = offset
	s.buffer = s.buffer[:0]

	rootHash, _ := s.smst.GetRoot()
	s.flushedRoots[s.flushedLeafCount] = rootHash

	return nil
}

func (s *SMSTArtifactStore) flushNodeCountsLocked() error {
	for k, v := range s.nodeCounts {
		s.flushedNodeCounts[k] = v
	}

	nodesPath := filepath.Join(s.dir, "nodes.json")
	tmpPath := nodesPath + ".tmp"

	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create temp nodes file: %w", err)
	}

	encoder := json.NewEncoder(f)
	if err := encoder.Encode(s.flushedNodeCounts); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("encode nodes file: %w", err)
	}

	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("sync nodes file: %w", err)
	}

	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp nodes file: %w", err)
	}

	if err := os.Rename(tmpPath, nodesPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename nodes file: %w", err)
	}

	return nil
}

func (s *SMSTArtifactStore) GetRoot() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.smst.Count() == 0 {
		return nil
	}

	root, _ := s.smst.GetRoot()
	return root
}

func (s *SMSTArtifactStore) GetRootAt(snapshotCount uint32) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, ErrStoreClosed
	}

	if snapshotCount == 0 {
		return nil, nil
	}

	if snapshotCount > s.smst.Count() {
		return nil, fmt.Errorf("snapshot count %d exceeds current count %d", snapshotCount, s.smst.Count())
	}

	if root, ok := s.flushedRoots[snapshotCount]; ok {
		return root, nil
	}

	tree := s.rebuildTreeAt(snapshotCount)
	root, _ := tree.GetRoot()
	return root, nil
}

func (s *SMSTArtifactStore) GetFlushedRoot() (count uint32, root []byte) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.flushedLeafCount == 0 {
		return 0, nil
	}

	if root, ok := s.flushedRoots[s.flushedLeafCount]; ok {
		return s.flushedLeafCount, root
	}

	r, _ := s.smst.GetRoot()
	return s.flushedLeafCount, r
}

func (s *SMSTArtifactStore) Count() uint32 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.smst.Count()
}

func (s *SMSTArtifactStore) GetNodeDistribution() map[string]uint32 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[string]uint32, len(s.flushedNodeCounts))
	for k, v := range s.flushedNodeCounts {
		result[k] = v
	}
	return result
}

func (s *SMSTArtifactStore) GetNodeCounts() map[string]uint32 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[string]uint32, len(s.nodeCounts))
	for k, v := range s.nodeCounts {
		result[k] = v
	}
	return result
}

func (s *SMSTArtifactStore) GetArtifact(denseIndex uint32) (nonce int32, vector []byte, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return 0, nil, ErrStoreClosed
	}

	if denseIndex >= s.smst.Count() {
		return 0, nil, ErrLeafIndexOutOfRange
	}

	// Use SMST tree to find nonce at this dense index (tree traversal order)
	nonce, _, err = s.smst.GetLeafByDenseIndex(denseIndex)
	if err != nil {
		return 0, nil, err
	}

	// Look up artifact data by nonce
	return s.getArtifactByNonce(nonce)
}

// getArtifactByNonce finds artifact data using nonce-to-offset index (O(1) lookup).
func (s *SMSTArtifactStore) getArtifactByNonce(targetNonce int32) (int32, []byte, error) {
	// Check flushed artifacts first (via index)
	if offset, ok := s.nonceToOffset[targetNonce]; ok {
		nonce, vector, _, err := readArtifactAt(s.dataFile, int64(offset))
		if err != nil {
			return 0, nil, fmt.Errorf("read artifact: %w", err)
		}
		return nonce, vector, nil
	}

	// Search in buffer (not yet flushed)
	for _, art := range s.buffer {
		if art.nonce == targetNonce {
			return art.nonce, art.vector, nil
		}
	}

	return 0, nil, ErrLeafIndexOutOfRange
}

func (s *SMSTArtifactStore) GetProof(denseIndex uint32, snapshotCount uint32) ([][]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, ErrStoreClosed
	}

	if denseIndex >= snapshotCount {
		return nil, ErrLeafIndexOutOfRange
	}

	if snapshotCount > s.smst.Count() {
		return nil, fmt.Errorf("snapshot count %d exceeds current count %d", snapshotCount, s.smst.Count())
	}

	var tree *SMST
	if snapshotCount == s.smst.Count() {
		tree = s.smst
	} else if s.prebuiltSnapshot != nil && s.prebuiltCount == snapshotCount {
		tree = s.prebuiltSnapshot
	} else {
		tree = s.rebuildTreeAt(snapshotCount)
	}

	nonce, _, err := tree.GetLeafByDenseIndex(denseIndex)
	if err != nil {
		return nil, err
	}

	proofWithCounts := s.buildProofWithCounts(tree, nonce)
	return encodeProofForTransport(proofWithCounts), nil
}

func (s *SMSTArtifactStore) buildProofWithCounts(tree *SMST, nonce int32) []SMSTProofElement {
	path := tree.noncePath(nonce)
	elements := make([]SMSTProofElement, 0, tree.depth)

	var collectWithCounts func(node *smstNode, level int)
	collectWithCounts = func(node *smstNode, level int) {
		if level == tree.depth || node == nil {
			return
		}

		goRight := path[level]
		if goRight {
			elements = append(elements, SMSTProofElement{
				SiblingHash:  tree.nodeHash(node.left, level+1),
				SiblingCount: tree.nodeCount(node.left),
			})
			collectWithCounts(node.right, level+1)
		} else {
			elements = append(elements, SMSTProofElement{
				SiblingHash:  tree.nodeHash(node.right, level+1),
				SiblingCount: tree.nodeCount(node.right),
			})
			collectWithCounts(node.left, level+1)
		}
	}

	collectWithCounts(tree.root, 0)
	return elements
}

func encodeProofForTransport(proof []SMSTProofElement) [][]byte {
	result := make([][]byte, len(proof))
	for i, elem := range proof {
		encoded := make([]byte, 36)
		copy(encoded[:32], elem.SiblingHash)
		binary.LittleEndian.PutUint32(encoded[32:], elem.SiblingCount)
		result[i] = encoded
	}
	return result
}

func (s *SMSTArtifactStore) rebuildTreeAt(count uint32) *SMST {
	tree := NewSMST(s.smst.depth)

	// Read flushed artifacts from disk
	flushedToRead := count
	if flushedToRead > s.flushedLeafCount {
		flushedToRead = s.flushedLeafCount
	}

	var skipped uint32
	for i := uint32(0); i < flushedToRead; i++ {
		offset := s.offsets[i]
		nonce, vector, _, err := readArtifactAt(s.dataFile, int64(offset))
		if err != nil {
			skipped++
			continue
		}
		leafData := encodeLeaf(nonce, vector)
		leafHash := smstHashLeaf(leafData)
		if _, err := tree.Insert(nonce, leafHash); err != nil {
			log.Printf("[WARN] SMST rebuildTreeAt: insert failed for nonce %d: %v (possible data corruption)", nonce, err)
		}
	}
	if skipped > 0 {
		log.Printf("[WARN] SMST rebuildTreeAt: skipped %d/%d artifacts due to read errors", skipped, flushedToRead)
	}

	// Read remaining from buffer if needed
	if count > s.flushedLeafCount {
		remaining := count - s.flushedLeafCount
		for i := uint32(0); i < remaining && i < uint32(len(s.buffer)); i++ {
			art := s.buffer[i]
			leafData := encodeLeaf(art.nonce, art.vector)
			leafHash := smstHashLeaf(leafData)
			tree.Insert(art.nonce, leafHash)
		}
	}

	return tree
}

// PrebuildSnapshot builds the SMST at the specified count for fast proof queries.
// Should be called after weight distribution is determined.
func (s *SMSTArtifactStore) PrebuildSnapshot(count uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if count > s.smst.Count() {
		return fmt.Errorf("count %d exceeds current count %d", count, s.smst.Count())
	}

	s.prebuiltSnapshot = s.rebuildTreeAt(count)
	s.prebuiltCount = count

	return nil
}

func (s *SMSTArtifactStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}

	s.closed = true

	if err := s.flushLocked(); err != nil {
		return fmt.Errorf("flush on close: %w", err)
	}

	if err := s.dataFile.Close(); err != nil {
		return fmt.Errorf("close data file: %w", err)
	}

	return nil
}

func writeArtifact(w io.Writer, nonce int32, vector []byte) (int, error) {
	totalLen := 4 + len(vector)
	header := make([]byte, 8)
	binary.LittleEndian.PutUint32(header[0:4], uint32(totalLen))
	binary.LittleEndian.PutUint32(header[4:8], uint32(nonce))

	n1, err := w.Write(header)
	if err != nil {
		return n1, err
	}

	n2, err := w.Write(vector)
	if err != nil {
		return n1 + n2, err
	}

	return n1 + n2, nil
}

func readArtifact(r io.Reader) (int32, []byte, int, error) {
	var header [8]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, 0, err
	}

	totalLen := binary.LittleEndian.Uint32(header[0:4])
	nonce := int32(binary.LittleEndian.Uint32(header[4:8]))

	vectorLen := totalLen - 4
	vector := make([]byte, vectorLen)
	if _, err := io.ReadFull(r, vector); err != nil {
		return 0, nil, 0, err
	}

	return nonce, vector, 8 + int(vectorLen), nil
}

func readArtifactAt(r io.ReaderAt, offset int64) (int32, []byte, int, error) {
	var header [8]byte
	if _, err := r.ReadAt(header[:], offset); err != nil {
		return 0, nil, 0, err
	}

	totalLen := binary.LittleEndian.Uint32(header[0:4])
	nonce := int32(binary.LittleEndian.Uint32(header[4:8]))

	vectorLen := totalLen - 4
	vector := make([]byte, vectorLen)
	if _, err := r.ReadAt(vector, offset+8); err != nil {
		return 0, nil, 0, err
	}

	return nonce, vector, 8 + int(vectorLen), nil
}

func encodeLeaf(nonce int32, vector []byte) []byte {
	buf := make([]byte, 4+len(vector))
	binary.LittleEndian.PutUint32(buf[:4], uint32(nonce))
	copy(buf[4:], vector)
	return buf
}
