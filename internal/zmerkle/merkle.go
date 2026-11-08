// Package zmerkle implements the Merkle tree used for transaction roots,
// receipt roots, and the Z Network state tree.
//
// Structure: binary tree over an ordered leaf list, built bottom-up.
//
//	leaf hash   = Digest(domain+"-leaf",  key, value)
//	inner hash  = Digest(domain+"-node",  left, right)
//
// An odd node at any level is *promoted* unchanged to the next level instead of
// being hashed with itself. Self-duplication is the classic second-preimage
// weakness (CVE-2012-2459 style): two different leaf sets can hash to the same
// root. Promotion removes that class of ambiguity. Promotion is visible to
// verifiers, who simply skip a step when a node has no sibling.
//
// Proofs carry an explicit orientation flag per step, so a proof cannot be
// reinterpreted in mirror image.
package zmerkle

import (
	"errors"

	"znetwork/internal/zcrypto"
	"znetwork/internal/zencoding"
)

// Hash aliases the crypto hash type.
type Hash = zcrypto.Hash

// ErrBadProof is returned by VerifyProof for a structurally invalid proof.
var ErrBadProof = errors.New("zmerkle: invalid proof")

// Step is one level of an inclusion proof.
type Step struct {
	Sibling Hash `json:"sibling"`
	// SiblingIsRight is true when the sibling is the right child, i.e. the
	// running hash is the left input of the node hash.
	SiblingIsRight bool `json:"siblingIsRight"`
}

// Proof is an inclusion proof for a single leaf.
type Proof struct {
	LeafIndex uint64 `json:"leafIndex"`
	LeafCount uint64 `json:"leafCount"`
	Steps     []Step `json:"steps"`
}

// leafHash computes the canonical leaf hash.
func leafHash(domain string, key, value []byte) Hash {
	return zcrypto.Digest(domain+"-leaf", key, value)
}

// nodeHash computes the canonical internal-node hash.
func nodeHash(domain string, left, right Hash) Hash {
	return zcrypto.Digest(domain+"-node", left[:], right[:])
}

// Root computes the Merkle root of leaves given pre-hashed values.
// An empty list yields a well-defined domain-separated "empty" root.
func Root(domain string, leaves []Hash) Hash {
	if len(leaves) == 0 {
		return zcrypto.Digest(domain + "-empty")
	}
	level := make([]Hash, len(leaves))
	copy(level, leaves)
	for len(level) > 1 {
		next := make([]Hash, 0, (len(level)+1)/2)
		i := 0
		for ; i+1 < len(level); i += 2 {
			next = append(next, nodeHash(domain, level[i], level[i+1]))
		}
		if i < len(level) { // odd tail: promote
			next = append(next, level[i])
		}
		level = next
	}
	return level[0]
}

// RootOf computes the root of key/value pairs given in ascending key order.
// Callers must supply sorted data; ordered state trees rely on that invariant.
func RootOf(domain string, kvs []KV) Hash {
	leaves := make([]Hash, len(kvs))
	for i, kv := range kvs {
		leaves[i] = leafHash(domain, kv.Key, kv.Value)
	}
	return Root(domain, leaves)
}

// KV is a key/value pair.
type KV struct {
	Key   []byte
	Value []byte
}

// RootOfLeaves computes the root from already-hashed leaves. It is the function
// callers use when they need both the root and proofs over the same leaf set, so
// the two can never be computed with different leaf hashing rules.
func RootOfLeaves(domain string, leaves []Hash) Hash { return Root(domain, leaves) }

// BuildProof builds an inclusion proof for leaf index i of leaves.
func BuildProof(domain string, leaves []Hash, i int) (Proof, bool) {
	if i < 0 || i >= len(leaves) {
		return Proof{}, false
	}
	p := Proof{LeafIndex: uint64(i), LeafCount: uint64(len(leaves))}
	level := make([]Hash, len(leaves))
	copy(level, leaves)
	idx := i
	for len(level) > 1 {
		next := make([]Hash, 0, (len(level)+1)/2)
		j := 0
		for ; j+1 < len(level); j += 2 {
			next = append(next, nodeHash(domain, level[j], level[j+1]))
		}
		if j < len(level) {
			next = append(next, level[j])
		}
		// Record the sibling, if any.
		var sibIdx int
		var isRight bool
		if idx%2 == 0 {
			sibIdx, isRight = idx+1, true
		} else {
			sibIdx, isRight = idx-1, false
		}
		if sibIdx < len(level) {
			p.Steps = append(p.Steps, Step{Sibling: level[sibIdx], SiblingIsRight: isRight})
		}
		idx /= 2
		level = next
	}
	return p, true
}

// VerifyProof recomputes the root from leaf and proof and compares with root.
func VerifyProof(domain string, leaf Hash, p Proof, root Hash) error {
	cur := leaf
	for _, s := range p.Steps {
		if s.SiblingIsRight {
			cur = nodeHash(domain, cur, s.Sibling)
		} else {
			cur = nodeHash(domain, s.Sibling, cur)
		}
	}
	if cur != root {
		return ErrBadProof
	}
	return nil
}

// EncodeProof canonically encodes a proof (used by RPC responses).
func EncodeProof(p Proof) []byte {
	w := zencoding.NewWriter()
	w.U64(p.LeafIndex)
	w.U64(p.LeafCount)
	w.U32(uint32(len(p.Steps)))
	for _, s := range p.Steps {
		w.Raw(s.Sibling[:])
		w.Bool(s.SiblingIsRight)
	}
	return w.Done()
}

// DecodeProof parses an encoded proof with full bounds checking.
func DecodeProof(b []byte) (Proof, error) {
	var p Proof
	r := zencoding.NewReader(b)
	p.LeafIndex = r.U64()
	p.LeafCount = r.U64()
	n := r.Len32(33)
	if r.Err() != nil {
		return p, ErrBadProof
	}
	if n > 64 { // a 2^64-leaf tree needs at most 64 steps
		return p, ErrBadProof
	}
	p.Steps = make([]Step, n)
	for i := 0; i < n; i++ {
		h := r.Raw(32)
		if r.Err() != nil {
			return p, ErrBadProof
		}
		copy(p.Steps[i].Sibling[:], h)
		p.Steps[i].SiblingIsRight = r.Bool()
	}
	if err := r.Done(); err != nil {
		return p, ErrBadProof
	}
	return p, nil
}
