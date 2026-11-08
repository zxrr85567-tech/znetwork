package zstate

import (
	"bytes"
	"testing"

	"znetwork/internal/zcrypto"
	"znetwork/internal/zmerkle"
	"znetwork/internal/ztypes"
)

// pubkey returns a deterministic test public key.
func pubkey() ztypes.PublicKey {
	d := zcrypto.Digest("test-pub")
	var p ztypes.PublicKey
	copy(p[:], d[:])
	return p
}

func addr(b byte) ztypes.Address {
	var a ztypes.Address
	for i := range a {
		a[i] = b
	}
	return a
}

func TestRootIsDeterministicAndOrderIndependent(t *testing.T) {
	a := New()
	b := New()
	// Insert the same entries in opposite orders.
	keys := [][]byte{{0x01, 1}, {0x01, 2}, {0x02, 9}}
	for _, k := range keys {
		a.SetRaw(k, []byte("value"))
	}
	for i := len(keys) - 1; i >= 0; i-- {
		b.SetRaw(keys[i], []byte("value"))
	}
	if a.Root() != b.Root() {
		t.Fatal("state root depends on insertion order")
	}
	// Changing a value changes the root.
	c := a.Clone()
	c.SetRaw(keys[0], []byte("other"))
	if c.Root() == a.Root() {
		t.Fatal("state root ignores value changes")
	}
	// Deleting restores the original root.
	c.SetRaw(keys[0], []byte("value"))
	if c.Root() != a.Root() {
		t.Fatal("root did not return after restoring the value")
	}
	c.Delete(keys[0])
	if c.Root() == a.Root() {
		t.Fatal("state root ignores deletions")
	}
	// Clone isolation: writes to the clone do not affect the original.
	if a.Root() != b.Root() {
		t.Fatal("clone mutation leaked into the source state")
	}
}

func TestEmptyStateRootIsStable(t *testing.T) {
	a, b := New(), New()
	if a.Root() != b.Root() {
		t.Fatal("empty root is not stable")
	}
	if a.Root().IsZero() {
		t.Fatal("empty root is the zero hash")
	}
}

func TestInclusionProof(t *testing.T) {
	st := New()
	for i := byte(0); i < 20; i++ {
		acc := &ztypes.Account{Balance: uint64(i) * 1000, Nonce: uint64(i)}
		st.SetAccount(addr(i), acc)
	}
	root := st.Root()
	for i := byte(0); i < 20; i++ {
		key := AccountKey(addr(i))
		proof, gotRoot, ok := st.Prove(key)
		_ = gotRoot
		if !ok {
			t.Fatalf("account %d not found", i)
		}
		if gotRoot != root {
			t.Fatalf("proof anchors to a different root (%s != %s)", gotRoot.Short(), root.Short())
		}
		value, _ := st.Get(key)
		if err := VerifyInclusion(root, key, value, proof); err != nil {
			t.Fatalf("account %d: proof failed: %v", i, err)
		}
		// A proof for the wrong value must fail.
		if err := VerifyInclusion(root, key, []byte("wrong"), proof); err == nil {
			t.Fatalf("account %d: proof accepted a wrong value", i)
		}
		// A proof must not validate against another root.
		other := st.Clone()
		other.SetRaw([]byte{0x09}, []byte("x"))
		if err := VerifyInclusion(other.Root(), key, value, proof); err == nil {
			t.Fatalf("account %d: proof accepted against a different root", i)
		}
	}
}

func TestAbsenceProof(t *testing.T) {
	st := New()
	for i := byte(0); i < 8; i++ {
		st.SetAccount(addr(i*2), &ztypes.Account{Balance: 1})
	}
	missing := addr(101) // no such account
	if _, ok := st.Get(AccountKey(missing)); ok {
		t.Fatal("account unexpectedly present")
	}
	prev, prevProof, next, nextProof := st.AbsenceProof(AccountKey(missing))
	if len(prev) == 0 && len(next) == 0 {
		t.Fatal("absence proof has no neighbours")
	}
	// The neighbours must exist and the proofs must verify against the root.
	root := st.Root()
	if len(prev) > 0 {
		v, ok := st.Get(prev)
		if !ok {
			t.Fatal("prev key missing")
		}
		if err := VerifyInclusion(root, prev, v, prevProof); err != nil {
			t.Fatalf("prev proof: %v", err)
		}
		if bytes.Compare(prev, AccountKey(missing)) >= 0 {
			t.Fatal("prev is not below the missing key")
		}
	}
	if len(next) > 0 {
		v, ok := st.Get(next)
		if !ok {
			t.Fatal("next key missing")
		}
		if err := VerifyInclusion(root, next, v, nextProof); err != nil {
			t.Fatalf("next proof: %v", err)
		}
		if bytes.Compare(next, AccountKey(missing)) <= 0 {
			t.Fatal("next is not above the missing key")
		}
	}
}

func TestTypedAccessors(t *testing.T) {
	st := New()
	a := addr(1)
	acc, err := st.GetAccount(a)
	if err != nil {
		t.Fatal(err)
	}
	if acc.Balance != 0 || acc.Nonce != 0 {
		t.Fatal("unknown account is not zero")
	}
	acc.Balance = 5
	acc.Nonce = 2
	st.SetAccount(a, acc)
	back, _ := st.GetAccount(a)
	if back.Balance != 5 || back.Nonce != 2 {
		t.Fatal("account did not persist")
	}

	v := &ztypes.Validator{Operator: a, PubKey: pubkey(), Moniker: "m", SelfBond: 10, Status: ztypes.ValidatorBonded}
	st.SetValidator(a, v)
	if got, _ := st.GetValidator(a); got == nil || got.Moniker != "m" {
		t.Fatal("validator did not persist")
	}
	vals, err := st.Validators()
	if err != nil || len(vals) != 1 {
		t.Fatalf("validators = %v, %v", vals, err)
	}

	d := &ztypes.DelegationRecord{Delegator: addr(2), Validator: a, Amount: 7}
	st.SetDelegation(d)
	if got, _ := st.GetDelegation(addr(2), a); got == nil || got.Amount != 7 {
		t.Fatal("delegation did not persist")
	}
	if got, _ := st.DelegationsFor(a); len(got) != 1 {
		t.Fatal("delegations for validator wrong")
	}
	st.DeleteDelegation(addr(2), a)
	if got, _ := st.GetDelegation(addr(2), a); got != nil {
		t.Fatal("delegation not deleted")
	}

	st.SetSupply(12345)
	if st.Supply() != 12345 {
		t.Fatal("supply did not persist")
	}
	st.SetSlot(a, 3, 99)
	if st.GetSlot(a, 3) != 99 {
		t.Fatal("storage slot did not persist")
	}
	st.SetSlot(a, 3, 0)
	if st.GetSlot(a, 3) != 0 {
		t.Fatal("zeroing a slot failed")
	}
	code := []byte{1, 2, 3}
	h := zcrypto.Digest("code")
	st.SetCode(h, code)
	if got, ok := st.GetCode(h); !ok || !bytes.Equal(got, code) {
		t.Fatal("code did not persist")
	}

	// Proposal ids are monotonically increasing.
	id1 := st.NextProposalID()
	id2 := st.NextProposalID()
	if id1+1 != id2 {
		t.Fatalf("proposal ids not sequential: %d then %d", id1, id2)
	}
	if st.ProposalCount() != 2 {
		t.Fatalf("proposal count = %d", st.ProposalCount())
	}
	// Votes are recorded once per (proposal, voter).
	if st.HasVoted(1, a) {
		t.Fatal("vote reported before being recorded")
	}
	st.RecordVote(1, a, ztypes.VoteYes)
	if !st.HasVoted(1, a) {
		t.Fatal("vote not recorded")
	}
}

func TestTotalBondedStake(t *testing.T) {
	st := New()
	params := ztypes.DefaultParams()
	st.SetParams(&params)
	for i := byte(1); i <= 3; i++ {
		st.SetValidator(addr(i), &ztypes.Validator{
			Operator: addr(i), PubKey: pubkey(), SelfBond: uint64(i) * 1000,
			Delegated: 500, Status: ztypes.ValidatorBonded,
		})
	}
	// An unbonded validator must not count.
	st.SetValidator(addr(9), &ztypes.Validator{
		Operator: addr(9), PubKey: pubkey(), SelfBond: 99999, Status: ztypes.ValidatorUnbonded,
	})
	total, err := st.TotalBondedStake()
	if err != nil {
		t.Fatal(err)
	}
	want := (1000 + 500) + (2000 + 500) + (3000 + 500)
	if total != uint64(want) {
		t.Fatalf("bonded stake = %d, want %d", total, want)
	}
}

func TestParamsRequiresInitialisation(t *testing.T) {
	st := New()
	if _, err := st.Params(); err == nil {
		t.Fatal("params available before initialisation")
	}
	p := ztypes.DefaultParams()
	st.SetParams(&p)
	got, err := st.Params()
	if err != nil {
		t.Fatal(err)
	}
	if got.BlockRewardUZ != p.BlockRewardUZ {
		t.Fatal("params did not persist")
	}
}

func TestEntriesAreSorted(t *testing.T) {
	st := New()
	for i := byte(9); i > 0; i-- {
		st.SetRaw([]byte{0x01, i}, []byte{i})
	}
	entries := st.Entries()
	for i := 1; i < len(entries); i++ {
		if bytes.Compare(entries[i-1].Key, entries[i].Key) >= 0 {
			t.Fatal("entries are not sorted")
		}
	}
	keys := st.SortedKeys()
	for i := 1; i < len(keys); i++ {
		if bytes.Compare(keys[i-1], keys[i]) >= 0 {
			t.Fatal("sorted keys are not sorted")
		}
	}
	// KeysWithPrefix returns only matching keys.
	got := st.KeysWithPrefix([]byte{0x01})
	if len(got) != 9 {
		t.Fatalf("prefix match = %d", len(got))
	}
}

func TestProofEncodingRejectsTampering(t *testing.T) {
	st := New()
	for i := byte(0); i < 5; i++ {
		st.SetAccount(addr(i), &ztypes.Account{Balance: uint64(i)})
	}
	key := AccountKey(addr(2))
	proof, _, ok := st.Prove(key)
	if !ok {
		t.Fatal("no proof")
	}
	enc := zmerkle.EncodeProof(proof)
	back, err := zmerkle.DecodeProof(enc)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := st.Get(key)
	if err := VerifyInclusion(st.Root(), key, value, back); err != nil {
		t.Fatalf("round-tripped proof failed: %v", err)
	}
	// Flipping any byte must either fail to decode or fail to verify. (Note that
	// flipping a byte inside a SIBLING hash always breaks verification; flipping a
	// byte of the leaf index metadata cannot, because the index is informational
	// and the step list is what is verified. Both behaviours are asserted here so
	// the property is documented rather than assumed.)
	// The encoded form is: leafIndex(8) | leafCount(8) | stepCount(4) | steps.
	// Corrupting anything from the step count onwards must break verification;
	// leafIndex/leafCount are informational metadata (verification uses the step
	// list), which is asserted explicitly below.
	header := 8 + 8 + 4
	for i := header; i < len(enc); i++ {
		tampered := append([]byte{}, enc...)
		tampered[i] ^= 0xff
		p, err := zmerkle.DecodeProof(tampered)
		if err != nil {
			continue // structurally rejected: good
		}
		if err := VerifyInclusion(st.Root(), key, value, p); err == nil {
			t.Fatalf("tampered proof at byte %d verified against the root", i)
		}
	}
	// Metadata is not part of verification: confirm that documented behaviour.
	// Flip a byte inside leafIndex (bytes 0..7) instead of leafCount, because
	// leafCount participates in structural sanity checks.
	meta := append([]byte{}, enc...)
	meta[3] ^= 0x01
	pMeta, err := zmerkle.DecodeProof(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyInclusion(st.Root(), key, value, pMeta); err != nil {
		t.Fatalf("leafCount metadata affected verification: %v", err)
	}
}

func TestCorruptAccountBytesAreReported(t *testing.T) {
	st := New()
	st.SetRaw(AccountKey(addr(1)), []byte{0xff, 0xff})
	if _, err := st.GetAccount(addr(1)); err == nil {
		t.Fatal("corrupt account decoded without error")
	}
}
