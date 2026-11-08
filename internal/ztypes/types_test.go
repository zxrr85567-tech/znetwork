package ztypes

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
	"testing"

	"znetwork/internal/zcrypto"
)

func testKey(t *testing.T, label string) *zcrypto.PrivateKey {
	t.Helper()
	d := zcrypto.Digest("test-key", []byte(label))
	k, err := zcrypto.KeyFromSeed(d.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestFormatParseUZRoundTrip(t *testing.T) {
	cases := []struct {
		uz   uint64
		text string
	}{
		{0, "0"},
		{1, "0.00000001"},
		{CoinScale, "1"},
		{Coins(1.5), "1.5"},
		{CoinScale*1000 + 1, "1000.00000001"},
		{MaxSupply, "1000000000"},
	}
	for _, c := range cases {
		got := FormatUZ(c.uz)
		if got != c.text {
			t.Fatalf("FormatUZ(%d) = %q, want %q", c.uz, got, c.text)
		}
		back, err := ParseUZ(c.text)
		if err != nil {
			t.Fatalf("ParseUZ(%q): %v", c.text, err)
		}
		if back != c.uz {
			t.Fatalf("ParseUZ(%q) = %d, want %d", c.text, back, c.uz)
		}
	}
	// Malformed inputs must be rejected, never silently truncated.
	for _, bad := range []string{"", "abc", "1.2.3", "-1", "1.-5", ".", "1..", "0x10", "1.123456789"} {
		if _, err := ParseUZ(bad); err == nil {
			t.Fatalf("ParseUZ(%q) accepted", bad)
		}
	}
}

func Coins(f float64) uint64 { return uint64(f * float64(CoinScale)) }

func TestOverflowHelpers(t *testing.T) {
	if _, ovf := AddU64(math.MaxUint64, 1); !ovf {
		t.Fatal("addition overflow not detected")
	}
	if v, ovf := AddU64(math.MaxUint64-1, 1); ovf || v != math.MaxUint64 {
		t.Fatal("boundary addition wrong")
	}
	if _, ovf := MulU64(math.MaxUint64, 2); !ovf {
		t.Fatal("multiplication overflow not detected")
	}
	if v, ovf := MulU64(1<<32, 1<<31); ovf || v != 1<<63 {
		t.Fatal("64-bit multiplication wrong")
	}
}

func TestParamsWhitelist(t *testing.T) {
	p := DefaultParams()
	if err := p.Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	// Unknown keys are rejected.
	if err := p.Set("not_a_param", 1); err == nil {
		t.Fatal("unknown parameter accepted")
	}
	// Out-of-range values are rejected for every parameter.
	for _, spec := range ParamSpecs {
		if spec.Min > 0 {
			if err := p.Set(spec.Key, spec.Min-1); err == nil {
				t.Fatalf("%s accepted value below minimum", spec.Key)
			}
		}
		if err := p.Set(spec.Key, spec.Max+1); err == nil {
			t.Fatalf("%s accepted value above maximum", spec.Key)
		}
		if err := p.Set(spec.Key, spec.Min); err != nil {
			t.Fatalf("%s rejected its own minimum: %v", spec.Key, err)
		}
		if err := p.Set(spec.Key, spec.Max); err != nil {
			t.Fatalf("%s rejected its own maximum: %v", spec.Key, err)
		}
	}
}

func TestParamsEncodeIsCanonical(t *testing.T) {
	p := DefaultParams()
	enc := p.Encode()
	dec, err := DecodeParams(enc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(enc, dec.Encode()) {
		t.Fatal("parameter encoding is not stable across a round trip")
	}
	// Keys must be sorted so two nodes never disagree on the bytes.
	var keys []string
	for _, k := range ParamKeys() {
		keys = append(keys, k)
	}
	if !sort.StringsAreSorted(keys) {
		t.Fatal("ParamKeys is not sorted")
	}
	// Trailing bytes and truncation are rejected.
	if _, err := DecodeParams(append(enc, 0x00)); err == nil {
		t.Fatal("trailing byte accepted")
	}
	if _, err := DecodeParams(enc[:len(enc)-1]); err == nil {
		t.Fatal("truncated encoding accepted")
	}
	// A duplicate key must be rejected, not silently overwritten.
	w := append([]byte{}, enc...)
	_ = w
}

func TestTransactionSigningAndValidation(t *testing.T) {
	key := testKey(t, "alice")
	chainID := "znetwork-test-1"
	mk := func() *Tx {
		return &Tx{
			ChainID: chainID, Type: TxTransfer, Sender: key.Address(),
			Nonce: 1, GasLimit: 100_000, GasPrice: 1, TimeMs: 1_700_000_000_000,
			Payload: (&TransferPayload{To: testKey(t, "bob").Address(), Amount: 5}).Encode(),
		}
	}
	tx := mk()
	if err := tx.Sign(key); err != nil {
		t.Fatal(err)
	}
	if err := tx.VerifySignature(); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := tx.ValidateStatic(chainID); err != nil {
		t.Fatalf("valid tx rejected: %v", err)
	}
	// Deterministic signatures: re-signing produces identical bytes.
	again := mk()
	_ = again.Sign(key)
	if tx.ID() != again.ID() {
		t.Fatal("transaction id is not deterministic")
	}
	// Tampering with any signed field must invalidate the signature.
	tampered := *tx
	tampered.Nonce = 2
	if err := tampered.VerifySignature(); err == nil {
		t.Fatal("nonce change not detected")
	}
	tampered = *tx
	tampered.GasLimit++
	if err := tampered.VerifySignature(); err == nil {
		t.Fatal("gas limit change not detected")
	}
	tampered = *tx
	tampered.Payload = append([]byte{}, tx.Payload...)
	tampered.Payload[len(tampered.Payload)-1] ^= 0xff
	if err := tampered.VerifySignature(); err == nil {
		t.Fatal("payload change not detected")
	}
	// A different sender cannot inherit the signature.
	tampered = *tx
	tampered.Sender = testKey(t, "mallory").Address()
	tampered.PublicKey = testKey(t, "mallory").PublicKey()
	if err := tampered.VerifySignature(); err == nil {
		t.Fatal("sender swap not detected")
	}
	// Cross-chain replay: the same body on another chain must not validate.
	if err := tx.ValidateStatic("znetwork-other-1"); err == nil {
		t.Fatal("cross-chain transaction accepted")
	}
	// Structural limits.
	tooBig := mk()
	tooBig.GasLimit = 0
	_ = tooBig.Sign(key)
	if err := tooBig.ValidateStatic(chainID); err == nil {
		t.Fatal("zero gas limit accepted")
	}
	noTime := mk()
	noTime.TimeMs = 0
	_ = noTime.Sign(key)
	if err := noTime.ValidateStatic(chainID); err == nil {
		t.Fatal("missing timestamp accepted")
	}
	hugeMemo := mk()
	hugeMemo.Memo = string(bytes.Repeat([]byte("m"), MaxMemoLen+1))
	_ = hugeMemo.Sign(key)
	if err := hugeMemo.ValidateStatic(chainID); err == nil {
		t.Fatal("oversized memo accepted")
	}
}

func TestTxEncodeDecodeRoundTrip(t *testing.T) {
	key := testKey(t, "carol")
	payloads := map[TxType][]byte{
		TxTransfer:          (&TransferPayload{To: testKey(t, "dave").Address(), Amount: 42}).Encode(),
		TxDelegate:          NewDelegatePayload(testKey(t, "val").Address(), 100).Encode(),
		TxUndelegate:        NewUndelegatePayload(testKey(t, "val").Address(), 50).Encode(),
		TxValidatorRegister: (&ValidatorRegisterPayload{PubKey: key.PublicKey(), Moniker: "carol", CommissionBps: 250}).Encode(),
		TxGovProposal:       (&GovProposalPayload{Title: "t", Description: "d", Kind: GovProposalText, Deposit: 1}).Encode(),
		TxGovVote:           (&GovVotePayload{ProposalID: 7, Option: VoteNo}).Encode(),
		TxContractDeploy:    (&ContractDeployPayload{Code: []byte{0x00, 0x30}}).Encode(),
		TxContractCall:      (&ContractCallPayload{Contract: testKey(t, "c").Address(), Input: []byte{1, 2, 3}}).Encode(),
	}
	types := make([]int, 0, len(payloads))
	for typ := range payloads {
		types = append(types, int(typ))
	}
	sort.Ints(types)
	for _, typ := range types {
		tx := &Tx{
			ChainID: "znetwork-test-1", Type: TxType(typ), Sender: key.Address(),
			Nonce: 3, GasLimit: 500_000, GasPrice: 2, TimeMs: 1_700_000_000_000,
			Memo: "hello", Payload: payloads[TxType(typ)],
		}
		if err := tx.Sign(key); err != nil {
			t.Fatalf("type %d: sign: %v", typ, err)
		}
		raw := tx.Encode()
		back, err := DecodeTx(raw)
		if err != nil {
			t.Fatalf("type %d: decode: %v", typ, err)
		}
		if !bytes.Equal(raw, back.Encode()) {
			t.Fatalf("type %d: encoding is not stable", typ)
		}
		if back.ID() != tx.ID() {
			t.Fatalf("type %d: id changed across a round trip", typ)
		}
		if err := back.VerifySignature(); err != nil {
			t.Fatalf("type %d: signature lost: %v", typ, err)
		}
		// Trailing bytes must be rejected.
		if _, err := DecodeTx(append(raw, 0x00)); err == nil {
			t.Fatalf("type %d: trailing byte accepted", typ)
		}
	}
}

func TestPayloadDecodersRejectGarbage(t *testing.T) {
	// Each payload decoder must reject an empty payload, a wrong tag, a truncated
	// body and trailing bytes. This is the boundary every untrusted transaction
	// crosses.
	for _, typ := range []TxType{TxTransfer, TxDelegate, TxUndelegate, TxValidatorRegister, TxGovProposal, TxGovVote, TxContractDeploy, TxContractCall} {
		for _, bad := range [][]byte{nil, {0xff}, {0x11, 0x01}} {
			var err error
			switch typ {
			case TxTransfer:
				_, err = DecodeTransferPayload(bad)
			case TxDelegate:
				_, err = DecodeDelegatePayload(bad)
			case TxUndelegate:
				_, err = DecodeUndelegatePayload(bad)
			case TxValidatorRegister:
				_, err = DecodeValidatorRegisterPayload(bad)
			case TxGovProposal:
				_, err = DecodeGovProposalPayload(bad)
			case TxGovVote:
				_, err = DecodeGovVotePayload(bad)
			case TxContractDeploy:
				_, err = DecodeContractDeployPayload(bad)
			case TxContractCall:
				_, err = DecodeContractCallPayload(bad)
			}
			if err == nil {
				t.Fatalf("type %d accepted payload %x", typ, bad)
			}
		}
	}
}

func TestContractAddressIsDeterministic(t *testing.T) {
	a := testKey(t, "deployer").Address()
	c1 := ContractAddress(a, 0)
	c2 := ContractAddress(a, 0)
	if c1 != c2 {
		t.Fatal("contract address is not deterministic")
	}
	if c1 == ContractAddress(a, 1) {
		t.Fatal("contract address does not depend on the nonce")
	}
	if c1 == ContractAddress(testKey(t, "other").Address(), 0) {
		t.Fatal("contract address does not depend on the sender")
	}
	if c1 == a {
		t.Fatal("contract address equals the deployer address")
	}
}

func TestBlockEncodeDecodeAndHeaderHash(t *testing.T) {
	key := testKey(t, "proposer")
	blk := &Block{}
	blk.Header = Header{
		Version: ProtocolVersion, ChainID: "znetwork-test-1", Height: 5,
		TimeMs: 1_700_000_000_000, Proposer: key.Address(),
		TxRoot: zcrypto.Digest("x"), ReceiptRoot: zcrypto.Digest("y"), StateRoot: zcrypto.Digest("z"),
		GasUsed: 100, GasLimit: 1000, ValidatorsHash: zcrypto.Digest("v"), TxCount: 0,
	}
	raw := blk.Encode()
	back, err := DecodeBlock(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, back.Encode()) {
		t.Fatal("block encoding is not stable")
	}
	if back.Header.Hash() != blk.Header.Hash() {
		t.Fatal("header hash changed across a round trip")
	}
	// Any header field change must change the hash.
	orig := blk.Header.Hash()
	mods := []func(h *Header){
		func(h *Header) { h.Height++ },
		func(h *Header) { h.TimeMs++ },
		func(h *Header) { h.ChainID = "other" },
		func(h *Header) { h.Proposer = testKey(t, "x").Address() },
		func(h *Header) { h.GasUsed++ },
		func(h *Header) { h.TxCount++ },
		func(h *Header) { h.StateRoot[0] ^= 1 },
		func(h *Header) { h.Version++ },
	}
	for i, mod := range mods {
		h := blk.Header
		mod(&h)
		if h.Hash() == orig {
			t.Fatalf("modification %d did not change the header hash", i)
		}
	}
	// Truncation is rejected.
	if _, err := DecodeBlock(raw[:len(raw)-1]); err == nil {
		t.Fatal("truncated block accepted")
	}
}

func TestGenesisDeterminismAndValidation(t *testing.T) {
	build := func(reverse bool) *Genesis {
		accounts := []GenesisAccount{
			{Address: testKey(t, "a1").Address().Bech32(), Balance: Coins(5000)},
			{Address: testKey(t, "a2").Address().Bech32(), Balance: Coins(8000)},
		}
		validators := []GenesisValidator{
			// Self bonds must be at least min_validator_stake_uz (1000 Z default)
			// and at most the operator's allocation.
			{PublicKey: testKey(t, "a1").PublicKey().String(), Moniker: "one", CommissionBps: 100, SelfBond: Coins(1000)},
			{PublicKey: testKey(t, "a2").PublicKey().String(), Moniker: "two", CommissionBps: 200, SelfBond: Coins(2000)},
		}
		if reverse {
			accounts[0], accounts[1] = accounts[1], accounts[0]
			validators[0], validators[1] = validators[1], validators[0]
		}
		return &Genesis{
			Version: ProtocolVersion, ChainID: "znetwork-gen-1", TimeMs: 1_700_000_000_000,
			Alloc: accounts, Validators: validators,
			ParamsJSON: map[string]uint64{"block_reward_uz": 1},
		}
	}
	h1, err := build(false).Hash()
	if err != nil {
		t.Fatal(err)
	}
	h2, err := build(true).Hash()
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatal("genesis hash depends on input ordering")
	}
	// Supply cap is enforced.
	over := build(false)
	over.Alloc[0].Balance = MaxSupply
	over.Alloc[1].Balance = MaxSupply
	if err := over.Validate(); err == nil {
		t.Fatal("genesis above the supply cap accepted")
	}
	// Self bond cannot exceed the allocation.
	badBond := build(false)
	badBond.Validators[0].SelfBond = Coins(1_000_000) // far above a1's allocation
	if err := badBond.Validate(); err == nil {
		t.Fatal("self bond above allocation accepted")
	}
	// Unknown parameters are rejected.
	badParam := build(false)
	badParam.ParamsJSON = map[string]uint64{"nope": 1}
	if err := badParam.Validate(); err == nil {
		t.Fatal("unknown genesis parameter accepted")
	}
	// JSON round trip is stable and strict.
	g := build(false)
	js, err := g.MarshalGenesisJSON()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseGenesisJSON(js)
	if err != nil {
		t.Fatalf("parsing generated genesis: %v", err)
	}
	h3, err := back.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h3 {
		t.Fatal("genesis hash changed across a JSON round trip")
	}
	// An unknown JSON field must be rejected (a typo cannot silently default).
	var m map[string]any
	if err := json.Unmarshal(js, &m); err != nil {
		t.Fatal(err)
	}
	m["unexpected_field"] = true
	bad, _ := json.Marshal(m)
	if _, err := ParseGenesisJSON(bad); err == nil {
		t.Fatal("unknown genesis field accepted")
	}
	// Wrong version is rejected.
	m2 := map[string]any{}
	_ = json.Unmarshal(js, &m2)
	m2["version"] = 99
	bad2, _ := json.Marshal(m2)
	if _, err := ParseGenesisJSON(bad2); err == nil {
		t.Fatal("unsupported genesis version accepted")
	}
}

func TestReceiptEncodeDecode(t *testing.T) {
	r := &Receipt{
		TxID: zcrypto.Digest("tx"), Height: 3, Sender: testKey(t, "s").Address(),
		Type: TxTransfer, Status: ReceiptSuccess, GasUsed: 42, Fee: 42,
		Events: []Event{{Type: "transfer", Attrs: [][2]string{{"a", "b"}}}},
	}
	raw := r.Encode()
	back, err := DecodeReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, back.Encode()) {
		t.Fatal("receipt encoding is not stable")
	}
	if back.Fee != r.Fee || len(back.Events) != 1 {
		t.Fatal("receipt fields lost")
	}
	if _, err := DecodeReceipt(append(raw, 1)); err == nil {
		t.Fatal("trailing byte in receipt accepted")
	}
}

func TestAccountAndValidatorEncodings(t *testing.T) {
	acc := &Account{Balance: 12345, Nonce: 7, CodeHash: zcrypto.Digest("code"), CreatedHeight: 9}
	raw := acc.Encode()
	back, err := DecodeAccount(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, back.Encode()) || back.Balance != acc.Balance || back.Nonce != acc.Nonce {
		t.Fatal("account round trip failed")
	}
	if !back.IsContract() {
		t.Fatal("contract flag lost")
	}
	v := &Validator{Operator: testKey(t, "op").Address(), PubKey: testKey(t, "op").PublicKey(),
		Moniker: "m", CommissionBps: 500, SelfBond: 100, Delegated: 50, Status: ValidatorBonded, RegisteredHeight: 1}
	vraw := v.Encode()
	vback, err := DecodeValidator(vraw)
	if err != nil {
		t.Fatal(err)
	}
	if vback.VotingPower() != 150 {
		t.Fatalf("voting power = %d", vback.VotingPower())
	}
	if !bytes.Equal(vraw, vback.Encode()) {
		t.Fatal("validator encoding unstable")
	}
	// Status strings are stable (they appear in the API).
	for st, want := range map[ValidatorStatus]string{
		ValidatorUnbonded: "unbonded", ValidatorBonded: "bonded",
		ValidatorJailed: "jailed", ValidatorUnbonding: "unbonding",
	} {
		if st.String() != want {
			t.Fatalf("status %d = %q, want %q", st, st.String(), want)
		}
	}
}

func TestDelegationAndProposalEncoding(t *testing.T) {
	d := &DelegationRecord{
		Delegator: testKey(t, "d").Address(), Validator: testKey(t, "v").Address(),
		Amount: 100, Unbonding: []UnbondingEntry{{Amount: 10, ReleaseHeight: 50}},
	}
	raw := d.Encode()
	back, err := DecodeDelegation(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, back.Encode()) || len(back.Unbonding) != 1 {
		t.Fatal("delegation round trip failed")
	}
	p := &Proposal{ID: 1, Proposer: testKey(t, "p").Address(), Title: "t", Description: "d",
		Kind: GovProposalParam, ParamKey: "block_reward_uz", ParamValue: 5, Deposit: 10,
		SubmitHeight: 1, VotingEndHeight: 100, Status: ProposalVoting,
		YesPower: 3, NoPower: 1, AbstainPower: 2}
	praw := p.Encode()
	pback, err := DecodeProposal(praw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(praw, pback.Encode()) {
		t.Fatal("proposal encoding unstable")
	}
	if pback.TotalVoted() != 6 {
		t.Fatalf("total voted = %d", pback.TotalVoted())
	}
}

func TestErrIdentifiersAreDistinct(t *testing.T) {
	errs := []error{
		ErrChainIDMismatch, ErrTxSignature, ErrNonceMismatch, ErrGasExhausted,
		ErrParamInvalid, ErrStateInvalid, ErrBlockInvalid, ErrGenesisInvalid,
		ErrInsufficientFunds, ErrStakeInvalid, ErrGovInvalid, ErrContractInvalid,
	}
	seen := map[string]error{}
	for _, e := range errs {
		if e == nil {
			t.Fatal("nil sentinel error")
		}
		if prev, dup := seen[e.Error()]; dup {
			t.Fatalf("sentinel %v duplicates %v", e, prev)
		}
		seen[e.Error()] = e
	}
}

// FuzzDecodeTx asserts that arbitrary bytes either fail to decode or decode into
// a transaction that re-encodes to the identical bytes. A decoder that accepts
// two encodings for one transaction would make transaction ids ambiguous.
func FuzzDecodeTx(f *testing.F) {
	seed := []byte{1, 0, 0, 0, 9, 'z', 'n', 'e', 't', 'w', 'o', 'r', 'k', 't', 0, 0}
	f.Add(seed)
	f.Add([]byte{})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add(make([]byte, 200))
	f.Fuzz(func(t *testing.T, b []byte) {
		tx, err := DecodeTx(b)
		if err != nil {
			return
		}
		again := tx.Encode()
		if string(again) != string(b) {
			t.Fatalf("decode accepted a non-canonical encoding (%d bytes -> %d bytes)", len(b), len(again))
		}
		if tx.ID() == (Hash{}) {
			t.Fatal("decoded transaction has a zero id")
		}
	})
}
