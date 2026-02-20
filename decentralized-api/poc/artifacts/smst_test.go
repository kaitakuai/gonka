package artifacts

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestSMSTEmpty(t *testing.T) {
	tree := NewSMST(24)

	root, count := tree.GetRoot()
	if count != 0 {
		t.Errorf("expected count 0, got %d", count)
	}
	if root == nil {
		t.Error("expected non-nil empty root")
	}
}

func TestSMSTInsertAndCount(t *testing.T) {
	tree := NewSMST(24)

	for i := int32(0); i < 10; i++ {
		leafHash := smstHashLeaf(encodeLeaf(i, []byte{byte(i)}))
		_, err := tree.Insert(i, leafHash)
		if err != nil {
			t.Fatalf("Insert(%d) failed: %v", i, err)
		}
	}

	if tree.Count() != 10 {
		t.Errorf("expected count 10, got %d", tree.Count())
	}
}

func TestSMSTDuplicateRejection(t *testing.T) {
	tree := NewSMST(24)

	leafHash := smstHashLeaf(encodeLeaf(42, []byte{1, 2, 3}))
	if _, err := tree.Insert(42, leafHash); err != nil {
		t.Fatalf("First Insert failed: %v", err)
	}

	if _, err := tree.Insert(42, leafHash); err != ErrDuplicateNonce {
		t.Errorf("expected ErrDuplicateNonce, got %v", err)
	}

	if tree.Count() != 1 {
		t.Errorf("expected count 1, got %d", tree.Count())
	}
}

func TestSMSTDenseIndexNavigation(t *testing.T) {
	tree := NewSMST(24)

	nonces := []int32{100, 5, 1000, 50, 10}
	for _, nonce := range nonces {
		leafHash := smstHashLeaf(encodeLeaf(nonce, []byte{byte(nonce)}))
		tree.Insert(nonce, leafHash)
	}

	for i := uint32(0); i < tree.Count(); i++ {
		_, proof, err := tree.GetLeafByDenseIndex(i)
		if err != nil {
			t.Fatalf("GetLeafByDenseIndex(%d) failed: %v", i, err)
		}
		if len(proof) == 0 {
			t.Errorf("expected non-empty proof for index %d", i)
		}
	}
}

func TestSMSTRootConsistency(t *testing.T) {
	tree1 := NewSMST(24)
	tree2 := NewSMST(24)

	nonces := []int32{10, 20, 30}
	for _, n := range nonces {
		leafHash := smstHashLeaf(encodeLeaf(n, []byte{byte(n)}))
		tree1.Insert(n, leafHash)
	}

	for i := len(nonces) - 1; i >= 0; i-- {
		n := nonces[i]
		leafHash := smstHashLeaf(encodeLeaf(n, []byte{byte(n)}))
		tree2.Insert(n, leafHash)
	}

	root1, _ := tree1.GetRoot()
	root2, _ := tree2.GetRoot()

	if !bytes.Equal(root1, root2) {
		t.Error("roots should be equal regardless of insertion order")
	}
}

func TestSMSTDepthExpansion(t *testing.T) {
	tree := NewSMST(4)

	largeNonce := int32(1 << 20)
	leafHash := smstHashLeaf(encodeLeaf(largeNonce, []byte{1}))

	_, err := tree.Insert(largeNonce, leafHash)
	if err != nil {
		t.Fatalf("Insert with large nonce failed: %v", err)
	}

	if tree.Depth() < 20 {
		t.Errorf("expected depth >= 20, got %d", tree.Depth())
	}
}

func TestSMSTStoreBasics(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSMST(dir)
	if err != nil {
		t.Fatalf("OpenSMST failed: %v", err)
	}
	defer store.Close()

	if store.Count() != 0 {
		t.Errorf("expected count 0, got %d", store.Count())
	}

	for i := int32(0); i < 10; i++ {
		if err := store.Add(i, []byte{byte(i)}); err != nil {
			t.Fatalf("Add(%d) failed: %v", i, err)
		}
	}

	if store.Count() != 10 {
		t.Errorf("expected count 10, got %d", store.Count())
	}
}

func TestSMSTStoreDuplicateRejection(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenSMST(dir)
	defer store.Close()

	if err := store.Add(42, []byte{1}); err != nil {
		t.Fatalf("First Add failed: %v", err)
	}

	if err := store.Add(42, []byte{2}); err != ErrDuplicateNonce {
		t.Errorf("expected ErrDuplicateNonce, got %v", err)
	}
}

func TestSMSTStoreRecovery(t *testing.T) {
	dir := t.TempDir()

	store1, _ := OpenSMST(dir)
	for i := int32(0); i < 5; i++ {
		store1.Add(i*10, []byte{byte(i)})
	}
	store1.Flush()
	root1 := store1.GetRoot()
	count1 := store1.Count()
	store1.Close()

	store2, err := OpenSMST(dir)
	if err != nil {
		t.Fatalf("Reopen failed: %v", err)
	}
	defer store2.Close()

	if store2.Count() != count1 {
		t.Errorf("recovered count: expected %d, got %d", count1, store2.Count())
	}

	root2 := store2.GetRoot()
	if !bytes.Equal(root1, root2) {
		t.Error("recovered root mismatch")
	}
}

func TestSMSTStoreProof(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenSMST(dir)
	defer store.Close()

	for i := int32(0); i < 8; i++ {
		store.Add(i, []byte{byte(i)})
	}

	root := store.GetRoot()
	for i := uint32(0); i < 8; i++ {
		proof, err := store.GetProof(i, 8)
		if err != nil {
			t.Fatalf("GetProof(%d) failed: %v", i, err)
		}
		if len(proof) == 0 {
			t.Errorf("expected non-empty proof for index %d", i)
		}

		nonce, vector, _ := store.GetArtifact(i)
		leafData := encodeLeaf(nonce, vector)

		proofElements := decodeProofFromTransport(proof)
		if !VerifySMSTProofWithCounts(root, 8, nonce, leafData, proofElements) {
			t.Errorf("proof verification failed for index %d", i)
		}
	}
}

func decodeProofFromTransport(proof [][]byte) []SMSTProofElement {
	elements := make([]SMSTProofElement, len(proof))
	for i, data := range proof {
		elements[i].SiblingHash = data[:32]
		elements[i].SiblingCount = binary.LittleEndian.Uint32(data[32:])
	}
	return elements
}

func TestSMSTStoreGetArtifact(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenSMST(dir)
	defer store.Close()

	artifacts := []struct {
		nonce  int32
		vector []byte
	}{
		{100, []byte{1, 2, 3}},
		{50, []byte{4, 5, 6}},
		{200, []byte{7, 8, 9}},
	}

	for _, a := range artifacts {
		store.Add(a.nonce, a.vector)
	}

	for i, a := range artifacts {
		nonce, vector, err := store.GetArtifact(uint32(i))
		if err != nil {
			t.Fatalf("GetArtifact(%d) failed: %v", i, err)
		}
		if nonce != a.nonce {
			t.Errorf("artifact %d: expected nonce %d, got %d", i, a.nonce, nonce)
		}
		if !bytes.Equal(vector, a.vector) {
			t.Errorf("artifact %d: vector mismatch", i)
		}
	}
}

func TestSMSTVerifyProof(t *testing.T) {
	tree := NewSMST(24)

	nonces := []int32{10, 20, 30, 40, 50}
	for _, n := range nonces {
		leafData := encodeLeaf(n, []byte{byte(n)})
		leafHash := smstHashLeaf(leafData)
		tree.Insert(n, leafHash)
	}

	_, count := tree.GetRoot()

	for i := uint32(0); i < count; i++ {
		nonce, proof, err := tree.GetLeafByDenseIndex(i)
		if err != nil {
			t.Fatalf("GetLeafByDenseIndex(%d) failed: %v", i, err)
		}

		proofByNonce, _ := tree.GetProofByNonce(nonce)
		if len(proofByNonce) != len(proof) {
			t.Errorf("proof length mismatch: dense=%d, nonce=%d", len(proof), len(proofByNonce))
		}
	}
}

func TestSMSTProofEncoding(t *testing.T) {
	proof := []SMSTProofElement{
		{SiblingHash: make([]byte, 32), SiblingCount: 100},
		{SiblingHash: make([]byte, 32), SiblingCount: 200},
	}

	for i := range proof[0].SiblingHash {
		proof[0].SiblingHash[i] = byte(i)
	}
	for i := range proof[1].SiblingHash {
		proof[1].SiblingHash[i] = byte(32 - i)
	}

	encoded := EncodeSMSTProof(proof)
	decoded, err := DecodeSMSTProof(encoded)
	if err != nil {
		t.Fatalf("DecodeSMSTProof failed: %v", err)
	}

	if len(decoded) != len(proof) {
		t.Fatalf("decoded length mismatch: expected %d, got %d", len(proof), len(decoded))
	}

	for i := range proof {
		if !bytes.Equal(decoded[i].SiblingHash, proof[i].SiblingHash) {
			t.Errorf("element %d: hash mismatch", i)
		}
		if decoded[i].SiblingCount != proof[i].SiblingCount {
			t.Errorf("element %d: count mismatch: expected %d, got %d", i, proof[i].SiblingCount, decoded[i].SiblingCount)
		}
	}
}
