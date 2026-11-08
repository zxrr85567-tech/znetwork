package zexec

import (
	"math/rand"
	"testing"

	"znetwork/internal/zcrypto"
	"znetwork/internal/zdevnet"
	"znetwork/internal/zstate"
	"znetwork/internal/ztypes"
	"znetwork/internal/zvm"
)

type harness struct {
	t      *testing.T
	eng    *Engine
	st     *zstate.State
	params *ztypes.Params
	net    *zdevnet.Network
	height uint64
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	net, err := zdevnet.New(zdevnet.DefaultOptions())
	if err != nil {
		t.Fatalf("devnet: %v", err)
	}
	eng := NewEngine(net.ChainID)
	st, err := eng.InitChain(net.Genesis)
	if err != nil {
		t.Fatalf("InitChain: %v", err)
	}
	params, err := st.Params()
	if err != nil {
		t.Fatalf("params: %v", err)
	}
	return &harness{t: t, eng: eng, st: st, params: params, net: net, height: 1}
}

func (h *harness) ctx() *BlockContext {
	return &BlockContext{
		Height:   h.height,
		TimeMs:   1_700_000_000_000 + int64(h.height)*1000,
		Proposer: h.net.Nodes[0].Address,
		Params:   h.params,
	}
}

func (h *harness) exec(tx *ztypes.Tx) *ztypes.Receipt {
	h.t.Helper()
	return h.eng.ExecuteTx(h.ctx(), h.st, tx)
}

// signed builds a signed transaction with a sensible gas limit and price.
func (h *harness) signed(k *zcrypto.PrivateKey, typ ztypes.TxType, payload []byte) *ztypes.Tx {
	h.t.Helper()
	acc, err := h.st.GetAccount(k.Address())
	if err != nil {
		h.t.Fatal(err)
	}
	tx := &ztypes.Tx{
		ChainID:  h.net.ChainID,
		Type:     typ,
		Sender:   k.Address(),
		Nonce:    acc.Nonce,
		GasLimit: 500_000,
		GasPrice: 1,
		TimeMs:   1_700_000_000_000 + int64(h.height)*1000,
		Payload:  payload,
	}
	if err := tx.Sign(k); err != nil {
		h.t.Fatalf("sign: %v", err)
	}
	return tx
}

func (h *harness) mustOK(tx *ztypes.Tx) *ztypes.Receipt {
	h.t.Helper()
	rc := h.exec(tx)
	if rc.Status != ztypes.ReceiptSuccess {
		h.t.Fatalf("transaction failed: %s", rc.Error)
	}
	return rc
}

func (h *harness) mustFail(tx *ztypes.Tx) *ztypes.Receipt {
	h.t.Helper()
	rc := h.exec(tx)
	if rc.Status == ztypes.ReceiptSuccess {
		h.t.Fatal("transaction unexpectedly succeeded")
	}
	return rc
}

func (h *harness) balance(a ztypes.Address) uint64 {
	h.t.Helper()
	acc, err := h.st.GetAccount(a)
	if err != nil {
		h.t.Fatal(err)
	}
	return acc.Balance
}

func TestInitChainAccounting(t *testing.T) {
	h := newHarness(t)
	if err := VerifySupply(h.st); err != nil {
		t.Fatalf("supply invariant broken at genesis: %v", err)
	}
	a, err := Account(h.st)
	if err != nil {
		t.Fatal(err)
	}
	if a.Supply == 0 || a.Bonded == 0 {
		t.Fatalf("unexpected accounting: %+v", a)
	}
	// Genesis validators must be bonded.
	vals, _ := h.st.Validators()
	if len(vals) != 4 {
		t.Fatalf("validators = %d", len(vals))
	}
	for _, v := range vals {
		if v.Status != ztypes.ValidatorBonded {
			t.Fatalf("validator %s not bonded", v.Moniker)
		}
		if v.SelfBond == 0 {
			t.Fatalf("validator %s has no self bond", v.Moniker)
		}
	}
}

func TestGenesisHashIsDeterministic(t *testing.T) {
	a, err := zdevnet.New(zdevnet.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	b, err := zdevnet.New(zdevnet.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	ha, err := a.Genesis.Hash()
	if err != nil {
		t.Fatal(err)
	}
	hb, err := b.Genesis.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Fatalf("genesis hash differs: %s vs %s", ha, hb)
	}
	// Changing one allocated balance must change the hash.
	b.Genesis.Alloc[0].Balance++
	hc, err := b.Genesis.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if ha == hc {
		t.Fatal("genesis hash ignores allocation changes")
	}
}

func TestTransferMovesFundsAndChargesFee(t *testing.T) {
	h := newHarness(t)
	sender := h.net.Users[0]
	recipient := h.net.Users[1].Address
	amount := uint64(10 * ztypes.CoinScale)
	before := h.balance(sender.Address)
	rcptBefore := h.balance(recipient)
	proposerBefore := h.balance(h.net.Nodes[0].Address)

	tx := h.signed(sender.Key, ztypes.TxTransfer, (&ztypes.TransferPayload{To: recipient, Amount: amount}).Encode())
	rc := h.mustOK(tx)

	if got := h.balance(sender.Address); got != before-amount-rc.Fee {
		t.Fatalf("sender balance = %d, want %d", got, before-amount-rc.Fee)
	}
	if got := h.balance(recipient); got != rcptBefore+amount {
		t.Fatalf("recipient balance = %d, want %d", got, rcptBefore+amount)
	}
	if got := h.balance(h.net.Nodes[0].Address); got != proposerBefore+rc.Fee {
		t.Fatalf("proposer did not receive the fee: %d", got)
	}
	if rc.Fee != rc.GasUsed {
		t.Fatalf("fee %d != gasUsed*gasPrice %d", rc.Fee, rc.GasUsed)
	}
	if err := VerifySupply(h.st); err != nil {
		t.Fatalf("supply invariant: %v", err)
	}
	if len(rc.Events) != 1 || rc.Events[0].Type != "transfer" {
		t.Fatalf("events = %+v", rc.Events)
	}
}

func TestTransferRejectsInsufficientFundsWithoutStateChange(t *testing.T) {
	h := newHarness(t)
	sender := h.net.Users[0]
	before := h.balance(sender.Address)
	huge := uint64(1_000_000_000 * ztypes.CoinScale)
	tx := h.signed(sender.Key, ztypes.TxTransfer, (&ztypes.TransferPayload{To: h.net.Users[1].Address, Amount: huge}).Encode())
	rc := h.mustFail(tx)
	if h.balance(sender.Address) != before {
		t.Fatal("failed transfer changed the balance")
	}
	acc, _ := h.st.GetAccount(sender.Address)
	if acc.Nonce != 0 {
		t.Fatalf("rejected transaction consumed a nonce: %d", acc.Nonce)
	}
	if rc.Fee != 0 {
		t.Fatalf("rejected transaction charged a fee: %d", rc.Fee)
	}
}

func TestReplayIsRejected(t *testing.T) {
	h := newHarness(t)
	sender := h.net.Users[0]
	tx := h.signed(sender.Key, ztypes.TxTransfer, (&ztypes.TransferPayload{To: h.net.Users[1].Address, Amount: 1}).Encode())
	h.mustOK(tx)
	// Same bytes again: nonce already consumed.
	rc := h.mustFail(tx)
	if rc.Status == ztypes.ReceiptSuccess {
		t.Fatal("replay accepted")
	}
}

func TestWrongChainIDIsRejected(t *testing.T) {
	h := newHarness(t)
	sender := h.net.Users[0]
	tx := h.signed(sender.Key, ztypes.TxTransfer, (&ztypes.TransferPayload{To: h.net.Users[1].Address, Amount: 1}).Encode())
	tx.ChainID = "znetwork-other-1"
	// The signature now covers a different body, so the signature check must fail
	// first; either way the transaction must be rejected.
	rc := h.mustFail(tx)
	if rc.GasUsed != 0 {
		t.Fatalf("invalid signature consumed gas: %d", rc.GasUsed)
	}
}

func TestTamperedSignatureRejected(t *testing.T) {
	h := newHarness(t)
	sender := h.net.Users[0]
	tx := h.signed(sender.Key, ztypes.TxTransfer, (&ztypes.TransferPayload{To: h.net.Users[1].Address, Amount: 1}).Encode())
	tx.Signature[0] ^= 0xff
	rc := h.mustFail(tx)
	if rc.GasUsed != 0 {
		t.Fatal("invalid signature charged gas")
	}
}

func TestDelegateAndUndelegateLifecycle(t *testing.T) {
	h := newHarness(t)
	user := h.net.Users[0]
	validator := h.net.Nodes[1]
	amount := uint64(100 * ztypes.CoinScale)

	vBefore, _ := h.st.GetValidator(validator.Operator)
	tx := h.signed(user.Key, ztypes.TxDelegate, ztypes.NewDelegatePayload(validator.Operator, amount).Encode())
	h.mustOK(tx)
	v, _ := h.st.GetValidator(validator.Operator)
	if v.Delegated != vBefore.Delegated+amount {
		t.Fatalf("validator delegated = %d, want %d", v.Delegated, vBefore.Delegated+amount)
	}
	if power, _ := stakedPower(h.st, user.Address); power != amount {
		t.Fatalf("user voting power = %d", power)
	}
	if err := VerifySupply(h.st); err != nil {
		t.Fatalf("supply invariant after delegate: %v", err)
	}

	// Undelegate half; funds must be released only after the unbonding period.
	half := amount / 2
	tx = h.signed(user.Key, ztypes.TxUndelegate, ztypes.NewUndelegatePayload(validator.Operator, half).Encode())
	h.mustOK(tx)
	rec, _ := h.st.GetDelegation(user.Address, validator.Operator)
	if rec.Amount != amount-half {
		t.Fatalf("delegation amount = %d", rec.Amount)
	}
	if len(rec.Unbonding) != 1 || rec.Unbonding[0].Amount != half {
		t.Fatalf("unbonding = %+v", rec.Unbonding)
	}
	if err := VerifySupply(h.st); err != nil {
		t.Fatalf("supply invariant after undelegate: %v", err)
	}

	releaseHeight := rec.Unbonding[0].ReleaseHeight
	balanceBefore := h.balance(user.Address)
	// Advance to just before release: nothing happens.
	h.height = releaseHeight - 1
	if err := h.eng.BeginBlock(h.ctx(), h.st); err != nil {
		t.Fatal(err)
	}
	if h.balance(user.Address) != balanceBefore {
		t.Fatal("funds released early")
	}
	// Advance to the release height.
	h.height = releaseHeight
	if err := h.eng.BeginBlock(h.ctx(), h.st); err != nil {
		t.Fatal(err)
	}
	if !(h.balance(user.Address) > balanceBefore) {
		t.Fatalf("funds not released: %d", h.balance(user.Address))
	}
	rec, _ = h.st.GetDelegation(user.Address, validator.Operator)
	if len(rec.Unbonding) != 0 {
		t.Fatalf("unbonding entry not cleared: %+v", rec.Unbonding)
	}
	if err := VerifySupply(h.st); err != nil {
		t.Fatalf("supply invariant after release: %v", err)
	}
}

func TestUndelegateMoreThanDelegatedIsRejected(t *testing.T) {
	h := newHarness(t)
	user := h.net.Users[0]
	validator := h.net.Nodes[0]
	tx := h.signed(user.Key, ztypes.TxUndelegate, ztypes.NewUndelegatePayload(validator.Operator, 1).Encode())
	h.mustFail(tx)
}

func TestValidatorRegistrationAndBonding(t *testing.T) {
	h := newHarness(t)
	// A brand new account registers as a validator and then bonds the minimum.
	newKey, err := zcrypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	// Fund it from the faucet.
	fund := h.signed(h.net.Faucet, ztypes.TxTransfer, (&ztypes.TransferPayload{To: newKey.Address(), Amount: 2_000 * ztypes.CoinScale}).Encode())
	h.mustOK(fund)

	reg := h.signed(newKey, ztypes.TxValidatorRegister, (&ztypes.ValidatorRegisterPayload{
		PubKey: newKey.PublicKey(), Moniker: "newbie", CommissionBps: 500,
	}).Encode())
	h.mustOK(reg)
	v, _ := h.st.GetValidator(newKey.Address())
	if v == nil || v.Status != ztypes.ValidatorUnbonded {
		t.Fatalf("new validator = %+v", v)
	}

	bond := h.signed(newKey, ztypes.TxDelegate, ztypes.NewDelegatePayload(newKey.Address(), 1_500*ztypes.CoinScale).Encode())
	h.mustOK(bond)
	v, _ = h.st.GetValidator(newKey.Address())
	if v.Status != ztypes.ValidatorBonded {
		t.Fatalf("validator status = %s, want bonded", v.Status)
	}
	if err := VerifySupply(h.st); err != nil {
		t.Fatalf("supply invariant: %v", err)
	}
}

func TestGovernanceParamChangeFlow(t *testing.T) {
	h := newHarness(t)
	user := h.net.Nodes[0] // a bonded validator can vote with its self bond
	deposit := h.params.MinProposalDepositUZ
	tx := h.signed(user.Key, ztypes.TxGovProposal, (&ztypes.GovProposalPayload{
		Title:       "Lower block reward",
		Description: "Reduce the development-network block reward for testing",
		Kind:        ztypes.GovProposalParam,
		ParamKey:    "block_reward_uz",
		ParamValue:  1 * ztypes.CoinScale,
		Deposit:     deposit,
	}).Encode())
	h.mustOK(tx)
	prop, _ := h.st.GetProposal(1)
	if prop == nil || prop.Status != ztypes.ProposalVoting {
		t.Fatalf("proposal = %+v", prop)
	}
	if h.balance(ztypes.SystemTreasuryAddress()) != deposit {
		t.Fatalf("deposit not escrowed: %d", h.balance(ztypes.SystemTreasuryAddress()))
	}
	if err := VerifySupply(h.st); err != nil {
		t.Fatalf("supply invariant after proposal: %v", err)
	}

	// Vote yes with every validator (each has a self bond).
	for _, n := range h.net.Nodes {
		vote := h.signed(n.Key, ztypes.TxGovVote, (&ztypes.GovVotePayload{ProposalID: 1, Option: ztypes.VoteYes}).Encode())
		h.mustOK(vote)
	}
	// Double voting must be rejected.
	dup := h.signed(h.net.Nodes[0].Key, ztypes.TxGovVote, (&ztypes.GovVotePayload{ProposalID: 1, Option: ztypes.VoteNo}).Encode())
	h.mustFail(dup)

	// Move past the voting period: the tally must apply the parameter change.
	h.height = prop.VotingEndHeight
	if err := h.eng.BeginBlock(h.ctx(), h.st); err != nil {
		t.Fatal(err)
	}
	prop, _ = h.st.GetProposal(1)
	if prop.Status != ztypes.ProposalExecuted {
		t.Fatalf("proposal status = %s", prop.Status)
	}
	params, _ := h.st.Params()
	if params.BlockRewardUZ != 1*ztypes.CoinScale {
		t.Fatalf("parameter not applied: %d", params.BlockRewardUZ)
	}
	if err := VerifySupply(h.st); err != nil {
		t.Fatalf("supply invariant after governance: %v", err)
	}
}

func TestGovernanceRejectsVotelessProposal(t *testing.T) {
	h := newHarness(t)
	user := h.net.Nodes[1]
	tx := h.signed(user.Key, ztypes.TxGovProposal, (&ztypes.GovProposalPayload{
		Title: "Nothing", Description: "", Kind: ztypes.GovProposalText, Deposit: h.params.MinProposalDepositUZ,
	}).Encode())
	h.mustOK(tx)
	prop, _ := h.st.GetProposal(1)
	h.height = prop.VotingEndHeight
	if err := h.eng.BeginBlock(h.ctx(), h.st); err != nil {
		t.Fatal(err)
	}
	prop, _ = h.st.GetProposal(1)
	if prop.Status != ztypes.ProposalRejected {
		t.Fatalf("status = %s, want rejected (no votes)", prop.Status)
	}
}

// counterASM increments storage slot 0 and writes the caller into slot 1.
const counterASM = `
	PUSH 0
	SLOAD
	PUSH 1
	ADD
	DUP
	PUSH 0
	SSTORE
	STOP
`

func TestContractDeployAndCall(t *testing.T) {
	h := newHarness(t)
	user := h.net.Users[0]
	asm, err := zvm.Assemble(counterASM)
	if err != nil {
		t.Fatal(err)
	}
	deploy := h.signed(user.Key, ztypes.TxContractDeploy, (&ztypes.ContractDeployPayload{Code: asm.Code}).Encode())
	rc := h.mustOK(deploy)
	if rc.ContractAddress.IsZero() {
		t.Fatal("no contract address in receipt")
	}
	expected := ztypes.ContractAddress(user.Address, deploy.Nonce)
	if rc.ContractAddress != expected {
		t.Fatalf("contract address = %s, want %s", rc.ContractAddress.Bech32(), expected.Bech32())
	}
	acc, _ := h.st.GetAccount(expected)
	if !acc.IsContract() {
		t.Fatal("account does not hold code")
	}
	if _, ok := h.st.GetCode(acc.CodeHash); !ok {
		t.Fatal("bytecode not stored")
	}

	// The constructor ran the same code once, so the counter starts at 1.
	base := h.st.GetSlot(expected, 0)
	if base != 1 {
		t.Fatalf("counter after constructor = %d, want 1", base)
	}
	for i := uint64(1); i <= 3; i++ {
		call := h.signed(user.Key, ztypes.TxContractCall, (&ztypes.ContractCallPayload{Contract: expected}).Encode())
		rc = h.mustOK(call)
		if got := h.st.GetSlot(expected, 0); got != base+i {
			t.Fatalf("counter = %d after %d calls, want %d", got, i, base+i)
		}
	}
	// The call output contains the stack, i.e. the counter value.
	if len(rc.Output) < 8 {
		t.Fatalf("call output too short: %x", rc.Output)
	}
	if err := VerifySupply(h.st); err != nil {
		t.Fatalf("supply invariant: %v", err)
	}
}

func TestContractRevertLeavesNoStorage(t *testing.T) {
	h := newHarness(t)
	user := h.net.Users[0]
	// Write then revert: slot 9 must remain unset.
	asm, err := zvm.Assemble("PUSH 5\nPUSH 9\nSSTORE\nPUSH 1\nREVERT")
	if err != nil {
		t.Fatal(err)
	}
	deploy := h.signed(user.Key, ztypes.TxContractDeploy, (&ztypes.ContractDeployPayload{Code: asm.Code}).Encode())
	rc := h.exec(deploy) // constructor reverts by design
	if rc.Status == ztypes.ReceiptSuccess {
		t.Fatal("reverting constructor reported success")
	}
	if got := h.st.GetSlot(ztypes.ContractAddress(user.Address, deploy.Nonce), 9); got != 0 {
		t.Fatalf("storage write survived a revert: %d", got)
	}
}

func TestContractGasLimitIsEnforced(t *testing.T) {
	h := newHarness(t)
	user := h.net.Users[0]
	// Infinite loop: cheap jumps that never terminate.
	asm, err := zvm.Assemble("loop:\nJUMP loop")
	if err != nil {
		t.Fatal(err)
	}
	acc, _ := h.st.GetAccount(user.Address)
	tx := &ztypes.Tx{
		ChainID: h.net.ChainID, Type: ztypes.TxContractDeploy, Sender: user.Address,
		Nonce: acc.Nonce, GasLimit: 200_000, GasPrice: 1, TimeMs: 1,
		Payload: (&ztypes.ContractDeployPayload{Code: asm.Code}).Encode(),
	}
	if err := tx.Sign(user.Key); err != nil {
		t.Fatal(err)
	}
	rc := h.mustFail(tx)
	if rc.GasUsed == 0 {
		t.Fatal("out-of-gas transaction consumed no gas")
	}
	if err := VerifySupply(h.st); err != nil {
		t.Fatalf("supply invariant: %v", err)
	}
}

func TestEndBlockMintsExactlyTheReward(t *testing.T) {
	h := newHarness(t)
	before := h.st.Supply()
	minted, err := h.eng.EndBlock(h.ctx(), h.st)
	if err != nil {
		t.Fatal(err)
	}
	if minted != h.params.BlockRewardUZ {
		t.Fatalf("minted = %d, want %d", minted, h.params.BlockRewardUZ)
	}
	if h.st.Supply() != before+minted {
		t.Fatal("supply not increased by exactly the minted amount")
	}
	if err := VerifySupply(h.st); err != nil {
		t.Fatalf("supply invariant after mint: %v", err)
	}
}

func TestEndBlockWithNoBondedStakeMintsNothing(t *testing.T) {
	h := newHarness(t)
	// Jail every validator by unbonding their entire self bond.
	for _, n := range h.net.Nodes {
		rec, _ := h.st.GetDelegation(n.Operator, n.Operator)
		if rec == nil {
			t.Fatal("genesis self delegation missing")
		}
		tx := h.signed(n.Key, ztypes.TxUndelegate, ztypes.NewUndelegatePayload(n.Operator, rec.Amount).Encode())
		h.mustOK(tx)
	}
	before := h.st.Supply()
	minted, err := h.eng.EndBlock(h.ctx(), h.st)
	if err != nil {
		t.Fatal(err)
	}
	if minted != 0 || h.st.Supply() != before {
		t.Fatalf("minted %d with no bonded stake", minted)
	}
}

func TestRewardDistributionIsProportionalAndExact(t *testing.T) {
	h := newHarness(t)
	// Give validator 0 a large extra delegation so shares are unequal.
	user := h.net.Users[0]
	tx := h.signed(user.Key, ztypes.TxDelegate, ztypes.NewDelegatePayload(h.net.Nodes[0].Operator, 300*ztypes.CoinScale).Encode())
	h.mustOK(tx)

	supplyBefore := h.st.Supply()
	balBefore := map[ztypes.Address]uint64{}
	for _, n := range h.net.Nodes {
		balBefore[n.Address] = h.balance(n.Address)
	}
	balBefore[user.Address] = h.balance(user.Address)

	minted, err := h.eng.EndBlock(h.ctx(), h.st)
	if err != nil {
		t.Fatal(err)
	}
	var gained uint64
	for addr, before := range balBefore {
		if now := h.balance(addr); now > before {
			gained += now - before
		}
	}
	if gained != minted {
		t.Fatalf("distributed %d, minted %d", gained, minted)
	}
	if h.st.Supply() != supplyBefore+minted {
		t.Fatal("supply mismatch")
	}
	// The delegator (no commission) must receive at least as much as it paid in
	// interest: sanctioning a proportional share of 2 Z on 300 Z of 4×(1000+300).
	if h.balance(user.Address) <= balBefore[user.Address] {
		t.Fatal("delegator received no reward")
	}
}

func TestMalformedPayloadRejected(t *testing.T) {
	h := newHarness(t)
	user := h.net.Users[0]
	cases := [][]byte{
		{},     // empty
		{0xff}, // wrong tag
		append([]byte{0x11}, make([]byte, 10)...), // truncated transfer
	}
	for i, payload := range cases {
		tx := h.signed(user.Key, ztypes.TxTransfer, payload)
		if rc := h.exec(tx); rc.Status == ztypes.ReceiptSuccess {
			t.Fatalf("case %d: malformed payload accepted", i)
		}
	}
}

func TestGasPriceBelowMinimumRejected(t *testing.T) {
	h := newHarness(t)
	params, _ := h.st.Params()
	if params.MinGasPriceUZ == 0 {
		t.Skip("minimum gas price is zero in this configuration")
	}
	user := h.net.Users[0]
	tx := h.signed(user.Key, ztypes.TxTransfer, (&ztypes.TransferPayload{To: h.net.Users[1].Address, Amount: 1}).Encode())
	tx.GasPrice = params.MinGasPriceUZ - 1
	if err := tx.Sign(user.Key); err != nil {
		t.Fatal(err)
	}
	h.mustFail(tx)
}

// TestRandomTransactionsNeverBreakInvariants is a stateful fuzz-style test: it
// signs and executes pseudo-random transactions (valid and corrupt) and asserts
// after every step that the books still balance and no panic occurs.
func TestRandomTransactionsNeverBreakInvariants(t *testing.T) {
	h := newHarness(t)
	rng := rand.New(rand.NewSource(42))
	keys := []*zcrypto.PrivateKey{h.net.Users[0].Key, h.net.Users[1].Key, h.net.Nodes[0].Key, h.net.Nodes[1].Key}
	types := []ztypes.TxType{ztypes.TxTransfer, ztypes.TxDelegate, ztypes.TxUndelegate, ztypes.TxGovVote, ztypes.TxContractCall}

	for i := 0; i < 400; i++ {
		k := keys[rng.Intn(len(keys))]
		typ := types[rng.Intn(len(types))]
		var payload []byte
		switch typ {
		case ztypes.TxTransfer:
			to := h.net.Users[rng.Intn(len(h.net.Users))].Address
			payload = (&ztypes.TransferPayload{To: to, Amount: uint64(rng.Intn(50)+1) * ztypes.CoinScale}).Encode()
		case ztypes.TxDelegate, ztypes.TxUndelegate:
			v := h.net.Nodes[rng.Intn(len(h.net.Nodes))].Operator
			payload = ztypes.NewDelegatePayload(v, uint64(rng.Intn(20)+1)*ztypes.CoinScale).Encode()
			if typ == ztypes.TxUndelegate {
				payload = ztypes.NewUndelegatePayload(v, uint64(rng.Intn(20)+1)*ztypes.CoinScale).Encode()
			}
		case ztypes.TxGovVote:
			payload = (&ztypes.GovVotePayload{ProposalID: uint64(rng.Intn(3) + 1), Option: ztypes.VoteOption(rng.Intn(3) + 1)}).Encode()
		case ztypes.TxContractCall:
			payload = (&ztypes.ContractCallPayload{Contract: ztypes.Address{byte(rng.Intn(256))}}).Encode()
		}
		tx := h.signed(k, typ, payload)
		// Occasionally corrupt the transaction.
		if rng.Intn(10) == 0 {
			tx.Payload = append(tx.Payload, 0xde, 0xad)
		}
		_ = h.exec(tx)
		if err := VerifySupply(h.st); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if i%50 == 0 {
			h.height++
			if _, err := h.eng.EndBlock(h.ctx(), h.st); err != nil {
				t.Fatalf("iteration %d EndBlock: %v", i, err)
			}
			if err := VerifySupply(h.st); err != nil {
				t.Fatalf("iteration %d after mint: %v", i, err)
			}
		}
	}
}
