package artifacts

import "encoding/binary"

// VerifySMSTProof verifies a sparse merkle sum tree proof.
// The proof contains sibling hashes from leaf to root.
// Depth is inferred from proof length.
func VerifySMSTProof(rootHash []byte, count uint32, denseIndex uint32, nonce int32, leafData []byte, proof [][]byte) bool {
	if denseIndex >= count {
		return false
	}
	if len(proof) == 0 {
		return false
	}

	depth := len(proof)
	leafHash := smstHashLeaf(leafData)
	path := smstNoncePath(nonce, depth)

	currentHash := leafHash
	currentCount := uint32(1)

	for i := depth - 1; i >= 0; i-- {
		siblingHash := proof[i]
		goRight := path[i]

		var leftHash, rightHash []byte
		var leftCount, rightCount uint32

		if goRight {
			leftHash = siblingHash
			rightHash = currentHash
			leftCount = inferSiblingCount(denseIndex, currentCount, true)
			rightCount = currentCount
		} else {
			leftHash = currentHash
			rightHash = siblingHash
			leftCount = currentCount
			rightCount = inferSiblingCount(denseIndex, currentCount, false)
		}

		currentCount = leftCount + rightCount
		currentHash = smstHashNode(leftHash, rightHash, currentCount)
	}

	if currentCount != count {
		return false
	}

	return bytesEqual(currentHash, rootHash)
}

func smstNoncePath(nonce int32, depth int) []bool {
	path := make([]bool, depth)
	n := uint32(nonce)
	for i := 0; i < depth; i++ {
		bit := (n >> (depth - 1 - i)) & 1
		path[i] = bit == 1
	}
	return path
}

func inferSiblingCount(denseIndex, currentCount uint32, isLeftSibling bool) uint32 {
	if isLeftSibling {
		return denseIndex
	}
	return 0
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// VerifySMSTProofWithCounts verifies an SMST proof with explicit sibling counts.
// Proof elements are ordered from root (index 0) to leaf (index depth-1).
// Verification reconstructs the root by starting at the leaf and going up.
func VerifySMSTProofWithCounts(rootHash []byte, count uint32, nonce int32, leafData []byte, proof []SMSTProofElement) bool {
	return VerifySMSTProofWithCountsDebug(rootHash, count, nonce, leafData, proof, false)
}

// VerifySMSTProofWithCountsDebug is like VerifySMSTProofWithCounts but with optional debug output.
func VerifySMSTProofWithCountsDebug(rootHash []byte, count uint32, nonce int32, leafData []byte, proof []SMSTProofElement, debug bool) bool {
	if len(proof) == 0 {
		return false
	}

	depth := len(proof)
	leafHash := smstHashLeaf(leafData)
	path := smstNoncePath(nonce, depth)

	currentHash := leafHash
	currentCount := uint32(1)

	for i := depth - 1; i >= 0; i-- {
		elem := proof[i]
		goRight := path[i]

		var leftHash, rightHash []byte
		var leftCount, rightCount uint32

		if goRight {
			leftHash = elem.SiblingHash
			rightHash = currentHash
			leftCount = elem.SiblingCount
			rightCount = currentCount
		} else {
			leftHash = currentHash
			rightHash = elem.SiblingHash
			leftCount = currentCount
			rightCount = elem.SiblingCount
		}

		currentCount = leftCount + rightCount
		currentHash = smstHashNode(leftHash, rightHash, currentCount)
	}

	if currentCount != count {
		return false
	}

	return bytesEqual(currentHash, rootHash)
}

// SMSTProofElement contains a sibling hash and its subtree count.
type SMSTProofElement struct {
	SiblingHash  []byte
	SiblingCount uint32
}

// EncodeSMSTProof encodes a proof with counts to bytes.
// Format: [depth(1)][hash(32)][count(4)]...
func EncodeSMSTProof(proof []SMSTProofElement) []byte {
	if len(proof) == 0 {
		return nil
	}

	buf := make([]byte, 1+len(proof)*36)
	buf[0] = byte(len(proof))

	offset := 1
	for _, elem := range proof {
		copy(buf[offset:offset+32], elem.SiblingHash)
		binary.LittleEndian.PutUint32(buf[offset+32:offset+36], elem.SiblingCount)
		offset += 36
	}

	return buf
}

// DecodeSMSTProof decodes a proof with counts from bytes.
func DecodeSMSTProof(data []byte) ([]SMSTProofElement, error) {
	if len(data) < 1 {
		return nil, ErrLeafIndexOutOfRange
	}

	depth := int(data[0])
	if len(data) != 1+depth*36 {
		return nil, ErrLeafIndexOutOfRange
	}

	proof := make([]SMSTProofElement, depth)
	offset := 1
	for i := 0; i < depth; i++ {
		proof[i].SiblingHash = make([]byte, 32)
		copy(proof[i].SiblingHash, data[offset:offset+32])
		proof[i].SiblingCount = binary.LittleEndian.Uint32(data[offset+32 : offset+36])
		offset += 36
	}

	return proof, nil
}
