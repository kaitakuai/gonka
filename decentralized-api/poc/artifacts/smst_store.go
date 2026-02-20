package artifacts

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// SMSTArtifactStore provides artifact storage with SMST commitments.
// Nonce determines tree position, making duplicates impossible by design.
type SMSTArtifactStore struct {
	mu     sync.RWMutex
	dir    string
	closed bool

	dataFile  *os.File
	nodesFile *os.File

	buffer    []bufferedArtifact
	artifacts []storedArtifact
	smst      *SMST

	flushedLeafCount  uint32
	flushedDataOffset uint64
	flushedRoots      map[uint32][]byte

	nodeCounts        map[string]uint32
	flushedNodeCounts map[string]uint32

	prebuiltSnapshot *SMST
	prebuiltCount    uint32
}

type storedArtifact struct {
	nonce  int32
	vector []byte
	offset uint64
}

var _ ArtifactStore = (*SMSTArtifactStore)(nil)

// OpenSMST opens or creates an SMST artifact store in the given directory.
func OpenSMST(dir string) (*SMSTArtifactStore, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create dir: %w", err)
	}

	dataPath := filepath.Join(dir, "artifacts.data")
	nodesPath := filepath.Join(dir, "nodes.json")

	dataFile, err := os.OpenFile(dataPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("open data file: %w", err)
	}

	nodesFile, err := os.OpenFile(nodesPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		dataFile.Close()
		return nil, fmt.Errorf("open nodes file: %w", err)
	}

	s := &SMSTArtifactStore{
		dir:               dir,
		dataFile:          dataFile,
		nodesFile:         nodesFile,
		buffer:            make([]bufferedArtifact, 0, 1024),
		artifacts:         make([]storedArtifact, 0, 1024),
		smst:              NewSMST(smstDefaultDepth),
		flushedRoots:      make(map[uint32][]byte),
		nodeCounts:        make(map[string]uint32),
		flushedNodeCounts: make(map[string]uint32),
	}

	if err := s.recover(); err != nil {
		s.dataFile.Close()
		s.nodesFile.Close()
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

		s.artifacts = append(s.artifacts, storedArtifact{
			nonce:  nonce,
			vector: vector,
			offset: offset,
		})

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
	info, err := s.nodesFile.Stat()
	if err != nil {
		return fmt.Errorf("stat nodes file: %w", err)
	}

	if info.Size() == 0 {
		return nil
	}

	if _, err := s.nodesFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek nodes file: %w", err)
	}

	decoder := json.NewDecoder(s.nodesFile)
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
		s.artifacts = append(s.artifacts, storedArtifact{
			nonce:  art.nonce,
			vector: art.vector,
			offset: offset,
		})

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

	if err := s.nodesFile.Truncate(0); err != nil {
		return fmt.Errorf("truncate nodes file: %w", err)
	}
	if _, err := s.nodesFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek nodes file: %w", err)
	}

	encoder := json.NewEncoder(s.nodesFile)
	if err := encoder.Encode(s.flushedNodeCounts); err != nil {
		return fmt.Errorf("encode nodes file: %w", err)
	}

	if err := s.nodesFile.Sync(); err != nil {
		return fmt.Errorf("sync nodes file: %w", err)
	}

	return nil
}

func (s *SMSTArtifactStore) GetRoot() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()

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

	totalCount := uint32(len(s.artifacts)) + uint32(len(s.buffer))
	if denseIndex >= totalCount {
		return 0, nil, ErrLeafIndexOutOfRange
	}

	if denseIndex < uint32(len(s.artifacts)) {
		art := s.artifacts[denseIndex]
		return art.nonce, art.vector, nil
	}

	bufIdx := denseIndex - uint32(len(s.artifacts))
	art := s.buffer[bufIdx]
	return art.nonce, art.vector, nil
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

	for i := uint32(0); i < count && i < uint32(len(s.artifacts)); i++ {
		art := s.artifacts[i]
		leafData := encodeLeaf(art.nonce, art.vector)
		leafHash := smstHashLeaf(leafData)
		tree.Insert(art.nonce, leafHash)
	}

	remaining := count - uint32(len(s.artifacts))
	for i := uint32(0); i < remaining && i < uint32(len(s.buffer)); i++ {
		art := s.buffer[i]
		leafData := encodeLeaf(art.nonce, art.vector)
		leafHash := smstHashLeaf(leafData)
		tree.Insert(art.nonce, leafHash)
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

	if err := s.nodesFile.Close(); err != nil {
		return fmt.Errorf("close nodes file: %w", err)
	}

	return nil
}
