// Package zstate implements Z Network's state machine storage.
//
// Layout: a single key/value map whose keys are prefixed by a one-byte class and
// then fixed-width identifiers, so lexicographic order equals protocol order:
//
//	0x01 | address(20)            -> Account
//	0x02 | address(20) | slot(8)  -> 32-byte contract storage value
//	0x03 | codehash(32)           -> contract bytecode
//	0x04 | address(20)            -> Validator
//	0x05 | delegator(20) | val(20)-> DelegationRecord
//	0x06 | proposalID(8)          -> Proposal
//	0x07 | proposalID(8) | voter(20)-> vote record (prevents double voting)
//	0x08 | name                    -> chain metadata (supply, params, counters)
//
// The state root is a Merkle tree over all (key, value) pairs in ascending key
// order. Two nodes that apply the same transactions therefore compute the same
// root, and any divergence is detected immediately at the header comparison.
//
// HONEST SCALING NOTE: the working set is held in memory and persisted as a full
// snapshot per height. That is correct and fast for development networks and for
// the state sizes exercised by this repository's tests (hundreds of thousands of
// keys are fine), but it is NOT a production-scale database. Replacing it with an
// incremental on-disk tree behind the same interface is tracked as an explicit
// Mainnet-readiness gap (see Z-NETWORK-KNOWN-LIMITATIONS.md, L-002).
package zstate

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"

	"znetwork/internal/zcrypto"
	"znetwork/internal/zmerkle"
	"znetwork/internal/ztypes"
)

// Key class prefixes.
const (
	classAccount    byte = 0x01
	classStorage    byte = 0x02
	classCode       byte = 0x03
	classValidator  byte = 0x04
	classDelegation byte = 0x05
	classProposal   byte = 0x06
	classVote       byte = 0x07
	classMeta       byte = 0x08
)

// stateDomain is the Merkle domain for the state tree.
const stateDomain = "ZNET/v1/state"

// Metadata keys.
const (
	MetaSupply      = "supply"
	MetaParams      = "params"
	MetaProposalSeq = "proposal_seq"
	MetaGenesisHash = "genesis_hash"
	MetaLastHeight  = "last_height"
	MetaLastBlock   = "last_block_hash"
	MetaChainID     = "chain_id"
)

// ErrCorrupt is returned when stored data fails integrity checks.
var ErrCorrupt = fmt.Errorf("zstate: corrupt or truncated data")

// State is the complete chain state at one height.
//
// A State is not safe for concurrent mutation; the node guards it with a mutex.
// Clone produces an independent copy for speculative execution.
type State struct {
	kv     map[string][]byte
	height uint64
	hash   ztypes.Hash // block hash that produced this state (zero at genesis)
	root   ztypes.Hash
	rootOK bool
}

// New returns an empty state at height 0.
func New() *State {
	return &State{kv: make(map[string][]byte, 64)}
}

// Height returns the height this state corresponds to.
func (s *State) Height() uint64 { return s.height }

// SetHeight records the height (used when sealing a block).
func (s *State) SetHeight(h uint64) { s.height = h }

// BlockHash returns the hash of the block that produced this state.
func (s *State) BlockHash() ztypes.Hash { return s.hash }

// SetBlockHash records the producing block hash.
func (s *State) SetBlockHash(h ztypes.Hash) { s.hash = h }

// Clone returns a deep copy. Used for speculative execution before commit.
func (s *State) Clone() *State {
	c := &State{
		kv:     make(map[string][]byte, len(s.kv)),
		height: s.height,
		hash:   s.hash,
		root:   s.root,
		rootOK: s.rootOK,
	}
	for k, v := range s.kv {
		cpy := make([]byte, len(v))
		copy(cpy, v)
		c.kv[k] = cpy
	}
	return c
}

// Size reports the number of keys.
func (s *State) Size() int { return len(s.kv) }

// Get returns the raw value for key.
func (s *State) Get(key []byte) ([]byte, bool) {
	v, ok := s.kv[string(key)]
	return v, ok
}

// SetRaw stores a raw value.
func (s *State) SetRaw(key, value []byte) {
	cpy := make([]byte, len(value))
	copy(cpy, value)
	s.kv[string(key)] = cpy
	s.rootOK = false
}

// Delete removes a key.
func (s *State) Delete(key []byte) {
	delete(s.kv, string(key))
	s.rootOK = false
}

// Root computes (and caches) the Merkle root over all entries.
func (s *State) Root() ztypes.Hash {
	if s.rootOK {
		return s.root
	}
	kvs := make([]zmerkle.KV, 0, len(s.kv))
	for k, v := range s.kv {
		kvs = append(kvs, zmerkle.KV{Key: []byte(k), Value: v})
	}
	sort.Slice(kvs, func(i, j int) bool { return bytes.Compare(kvs[i].Key, kvs[j].Key) < 0 })
	s.root = zmerkle.RootOf(stateDomain, kvs)
	s.rootOK = true
	return s.root
}

// SortedKeys returns all keys in ascending order.
func (s *State) SortedKeys() [][]byte {
	keys := make([][]byte, 0, len(s.kv))
	for k := range s.kv {
		keys = append(keys, []byte(k))
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i], keys[j]) < 0 })
	return keys
}

// KeysWithPrefix returns keys sharing prefix, in ascending order.
func (s *State) KeysWithPrefix(prefix []byte) [][]byte {
	var out [][]byte
	for k := range s.kv {
		if len(k) >= len(prefix) && k[:len(prefix)] == string(prefix) {
			out = append(out, []byte(k))
		}
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i], out[j]) < 0 })
	return out
}

// Entries returns a copy of all key/value pairs in ascending key order. Used for
// snapshotting and for computing roots in tests.
func (s *State) Entries() []zmerkle.KV {
	keys := s.SortedKeys()
	out := make([]zmerkle.KV, len(keys))
	for i, k := range keys {
		v := s.kv[string(k)]
		cpy := make([]byte, len(v))
		copy(cpy, v)
		out[i] = zmerkle.KV{Key: k, Value: cpy}
	}
	return out
}

// Prove returns an inclusion proof for key together with the state root the proof
// anchors to. The boolean is false when the key is absent (absence proofs require
// a neighbour pair; see AbsenceProof).
func (s *State) Prove(key []byte) (zmerkle.Proof, ztypes.Hash, bool) {
	entries := s.Entries()
	idx := -1
	for i, e := range entries {
		if bytes.Equal(e.Key, key) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return zmerkle.Proof{}, ztypes.Hash{}, false
	}
	leaves := make([]ztypes.Hash, len(entries))
	for i, e := range entries {
		leaves[i] = zcrypto.Digest(stateDomain+"-leaf", e.Key, e.Value)
	}
	p, ok := zmerkle.BuildProof(stateDomain, leaves, idx)
	if !ok {
		return zmerkle.Proof{}, ztypes.Hash{}, false
	}
	// Return the tree root that this proof anchors to (not the leaf hash): callers
	// verify the proof against a root they already trust.
	return p, zmerkle.RootOfLeaves(stateDomain, leaves), true
}

// AbsenceProof returns the keys immediately before and after key (either may be
// absent) plus their inclusion proofs. Together they prove the key is not in the
// tree, because the tree is ordered.
func (s *State) AbsenceProof(key []byte) (prev []byte, prevProof zmerkle.Proof, next []byte, nextProof zmerkle.Proof) {
	entries := s.Entries()
	leaves := make([]ztypes.Hash, len(entries))
	for i, e := range entries {
		leaves[i] = zcrypto.Digest(stateDomain+"-leaf", e.Key, e.Value)
	}
	for i, e := range entries {
		if bytes.Compare(e.Key, key) < 0 {
			prev = e.Key
			prevProof, _ = zmerkle.BuildProof(stateDomain, leaves, i)
			continue
		}
		next = e.Key
		nextProof, _ = zmerkle.BuildProof(stateDomain, leaves, i)
		break
	}
	return prev, prevProof, next, nextProof
}

// VerifyInclusion verifies a proof against a root.
func VerifyInclusion(root ztypes.Hash, key, value []byte, p zmerkle.Proof) error {
	leaf := zcrypto.Digest(stateDomain+"-leaf", key, value)
	return zmerkle.VerifyProof(stateDomain, leaf, p, root)
}

// ---- key constructors ------------------------------------------------------

// AccountKey returns the key for an account record.
func AccountKey(a ztypes.Address) []byte {
	k := make([]byte, 21)
	k[0] = classAccount
	copy(k[1:], a[:])
	return k
}

// StorageKey returns the key for one contract storage slot.
func StorageKey(contract ztypes.Address, slot uint64) []byte {
	k := make([]byte, 29)
	k[0] = classStorage
	copy(k[1:], contract[:])
	binary.BigEndian.PutUint64(k[21:], slot)
	return k
}

// CodeKey returns the key under which contract bytecode is stored.
func CodeKey(h ztypes.Hash) []byte {
	k := make([]byte, 33)
	k[0] = classCode
	copy(k[1:], h[:])
	return k
}

// ValidatorKey returns the key for a validator record.
func ValidatorKey(op ztypes.Address) []byte {
	k := make([]byte, 21)
	k[0] = classValidator
	copy(k[1:], op[:])
	return k
}

// DelegationKey returns the key for a (delegator, validator) pair.
func DelegationKey(del, val ztypes.Address) []byte {
	k := make([]byte, 41)
	k[0] = classDelegation
	copy(k[1:], del[:])
	copy(k[21:], val[:])
	return k
}

// ProposalKey returns the key for a proposal.
func ProposalKey(id uint64) []byte {
	k := make([]byte, 9)
	k[0] = classProposal
	binary.BigEndian.PutUint64(k[1:], id)
	return k
}

// VoteKey returns the key recording one account's vote on one proposal.
func VoteKey(id uint64, voter ztypes.Address) []byte {
	k := make([]byte, 29)
	k[0] = classVote
	binary.BigEndian.PutUint64(k[1:], id)
	copy(k[9:], voter[:])
	return k
}

// MetaKeyName returns the key for a metadata entry.
func MetaKeyName(name string) []byte {
	k := make([]byte, 1+len(name))
	k[0] = classMeta
	copy(k[1:], name)
	return k
}

// ---- typed accessors -------------------------------------------------------

// GetAccount returns the account record, or a zero account when absent.
func (s *State) GetAccount(a ztypes.Address) (*ztypes.Account, error) {
	v, ok := s.Get(AccountKey(a))
	if !ok {
		return &ztypes.Account{}, nil
	}
	return ztypes.DecodeAccount(v)
}

// SetAccount writes an account record.
func (s *State) SetAccount(a ztypes.Address, acc *ztypes.Account) {
	s.SetRaw(AccountKey(a), acc.Encode())
}

// GetValidator returns a validator record, or nil when absent.
func (s *State) GetValidator(op ztypes.Address) (*ztypes.Validator, error) {
	v, ok := s.Get(ValidatorKey(op))
	if !ok {
		return nil, nil
	}
	return ztypes.DecodeValidator(v)
}

// SetValidator writes a validator record.
func (s *State) SetValidator(op ztypes.Address, v *ztypes.Validator) {
	s.SetRaw(ValidatorKey(op), v.Encode())
}

// Validators returns all validator records ordered by operator address.
func (s *State) Validators() ([]*ztypes.Validator, error) {
	keys := s.KeysWithPrefix([]byte{classValidator})
	out := make([]*ztypes.Validator, 0, len(keys))
	for _, k := range keys {
		v, _ := s.Get(k)
		rec, err := ztypes.DecodeValidator(v)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// GetDelegation returns a delegation record, or nil when absent.
func (s *State) GetDelegation(del, val ztypes.Address) (*ztypes.DelegationRecord, error) {
	v, ok := s.Get(DelegationKey(del, val))
	if !ok {
		return nil, nil
	}
	return ztypes.DecodeDelegation(v)
}

// SetDelegation writes a delegation record.
func (s *State) SetDelegation(d *ztypes.DelegationRecord) {
	s.SetRaw(DelegationKey(d.Delegator, d.Validator), d.Encode())
}

// DeleteDelegation removes an empty delegation record.
func (s *State) DeleteDelegation(del, val ztypes.Address) {
	s.Delete(DelegationKey(del, val))
}

// Delegations returns every delegation record ordered by (delegator, validator).
func (s *State) Delegations() ([]*ztypes.DelegationRecord, error) {
	keys := s.KeysWithPrefix([]byte{classDelegation})
	out := make([]*ztypes.DelegationRecord, 0, len(keys))
	for _, k := range keys {
		v, _ := s.Get(k)
		rec, err := ztypes.DecodeDelegation(v)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// DelegationsFor returns delegations to one validator, ordered by delegator.
func (s *State) DelegationsFor(val ztypes.Address) ([]*ztypes.DelegationRecord, error) {
	all, err := s.Delegations()
	if err != nil {
		return nil, err
	}
	out := make([]*ztypes.DelegationRecord, 0, len(all))
	for _, d := range all {
		if d.Validator == val {
			out = append(out, d)
		}
	}
	return out, nil
}

// GetProposal returns a proposal by id, or nil when absent.
func (s *State) GetProposal(id uint64) (*ztypes.Proposal, error) {
	v, ok := s.Get(ProposalKey(id))
	if !ok {
		return nil, nil
	}
	return ztypes.DecodeProposal(v)
}

// SetProposal writes a proposal record.
func (s *State) SetProposal(p *ztypes.Proposal) {
	s.SetRaw(ProposalKey(p.ID), p.Encode())
}

// Proposals returns all proposals ordered by id.
func (s *State) Proposals() ([]*ztypes.Proposal, error) {
	keys := s.KeysWithPrefix([]byte{classProposal})
	out := make([]*ztypes.Proposal, 0, len(keys))
	for _, k := range keys {
		v, _ := s.Get(k)
		p, err := ztypes.DecodeProposal(v)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// GetCode returns contract bytecode by hash.
func (s *State) GetCode(h ztypes.Hash) ([]byte, bool) { return s.Get(CodeKey(h)) }

// SetCode stores contract bytecode.
func (s *State) SetCode(h ztypes.Hash, code []byte) { s.SetRaw(CodeKey(h), code) }

// GetSlot returns a contract storage slot value (zero when unset).
func (s *State) GetSlot(contract ztypes.Address, slot uint64) uint64 {
	v, ok := s.Get(StorageKey(contract, slot))
	if !ok || len(v) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(v)
}

// SetSlot writes a contract storage slot.
func (s *State) SetSlot(contract ztypes.Address, slot, value uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], value)
	s.SetRaw(StorageKey(contract, slot), b[:])
}

// Supply returns the recorded total supply.
func (s *State) Supply() uint64 {
	v, ok := s.Get(MetaKeyName(MetaSupply))
	if !ok || len(v) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(v)
}

// SetSupply records the total supply.
func (s *State) SetSupply(v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	s.SetRaw(MetaKeyName(MetaSupply), b[:])
}

// Params returns the chain parameters.
func (s *State) Params() (*ztypes.Params, error) {
	v, ok := s.Get(MetaKeyName(MetaParams))
	if !ok {
		return nil, fmt.Errorf("%w: params missing from state", ErrCorrupt)
	}
	return ztypes.DecodeParams(v)
}

// SetParams writes the chain parameters.
func (s *State) SetParams(p *ztypes.Params) {
	if err := p.Validate(); err != nil {
		panic("zstate: refusing to store invalid params: " + err.Error())
	}
	s.SetRaw(MetaKeyName(MetaParams), p.Encode())
}

// NextProposalID returns the next proposal id (1-based) and increments it.
func (s *State) NextProposalID() uint64 {
	v, ok := s.Get(MetaKeyName(MetaProposalSeq))
	var id uint64
	if ok && len(v) == 8 {
		id = binary.BigEndian.Uint64(v)
	}
	id++
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], id)
	s.SetRaw(MetaKeyName(MetaProposalSeq), b[:])
	return id
}

// ProposalCount returns the number of proposals ever created.
func (s *State) ProposalCount() uint64 {
	v, ok := s.Get(MetaKeyName(MetaProposalSeq))
	if !ok || len(v) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(v)
}

// GenesisHash returns the recorded genesis hash.
func (s *State) GenesisHash() ztypes.Hash {
	v, _ := s.Get(MetaKeyName(MetaGenesisHash))
	var h ztypes.Hash
	copy(h[:], v)
	return h
}

// SetGenesisHash records the genesis hash.
func (s *State) SetGenesisHash(h ztypes.Hash) { s.SetRaw(MetaKeyName(MetaGenesisHash), h[:]) }

// ChainID returns the chain id committed in state.
func (s *State) ChainID() string {
	v, ok := s.Get(MetaKeyName(MetaChainID))
	if !ok {
		return ""
	}
	return string(v)
}

// SetChainID records the chain id in state.
func (s *State) SetChainID(id string) { s.SetRaw(MetaKeyName(MetaChainID), []byte(id)) }

// HasVoted reports whether an account has voted on a proposal.
func (s *State) HasVoted(id uint64, voter ztypes.Address) bool {
	_, ok := s.Get(VoteKey(id, voter))
	return ok
}

// RecordVote stores a vote receipt key (value is the option byte).
func (s *State) RecordVote(id uint64, voter ztypes.Address, option ztypes.VoteOption) {
	s.SetRaw(VoteKey(id, voter), []byte{byte(option)})
}

// TotalBondedStake sums voting power over bonded validators.
func (s *State) TotalBondedStake() (uint64, error) {
	vals, err := s.Validators()
	if err != nil {
		return 0, err
	}
	var total uint64
	for _, v := range vals {
		if v.Status != ztypes.ValidatorBonded {
			continue
		}
		var ov bool
		total, ov = ztypes.AddU64(total, v.VotingPower())
		if ov {
			return 0, fmt.Errorf("%w: bonded stake overflow", ztypes.ErrStateInvalid)
		}
	}
	return total, nil
}
