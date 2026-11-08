package zexec

import (
	"fmt"

	"znetwork/internal/zcrypto"
	"znetwork/internal/zstate"
	"znetwork/internal/ztypes"
	"znetwork/internal/zvm"
)

// precheckValue validates that the sender can pay `amount + fee` for a transaction
// whose gas usage is already known, so no partial state is ever written.
func precheckValue(acc *ztypes.Account, amount, gasUsed, gasPrice uint64) (fee uint64, err error) {
	fee, ovf := ztypes.MulU64(gasUsed, gasPrice)
	if ovf {
		return 0, fmt.Errorf("%w: fee overflow", ztypes.ErrTxGasLimit)
	}
	need, ovf := ztypes.AddU64(amount, fee)
	if ovf {
		return 0, fmt.Errorf("%w: cost overflow", ztypes.ErrInsufficientFunds)
	}
	if acc.Balance < need {
		return 0, invalidf("%w: balance %d < required %d (amount %d + fee %d)",
			ztypes.ErrInsufficientFunds, acc.Balance, need, amount, fee)
	}
	return fee, nil
}

// execTransfer moves the native token.
func (e *Engine) execTransfer(ctx *BlockContext, st *zstate.State, tx *ztypes.Tx, acc *ztypes.Account, rc *ztypes.Receipt, intrinsic uint64) error {
	p, err := ztypes.DecodeTransferPayload(tx.Payload)
	if err != nil {
		return err
	}
	gasUsed := intrinsic + GasTransferOp
	if gasUsed > tx.GasLimit {
		return invalidf("%w: transfer needs %d gas", ztypes.ErrGasExhausted, gasUsed)
	}
	if _, err := precheckValue(acc, p.Amount, gasUsed, tx.GasPrice); err != nil {
		return err
	}
	rc.GasUsed = gasUsed
	// Writes below cannot fail: all conditions were checked above.
	if err := debit(st, tx.Sender, p.Amount); err != nil {
		return err
	}
	if err := credit(st, p.To, p.Amount); err != nil {
		return err
	}
	rc.Events = append(rc.Events, ztypes.Event{
		Type: "transfer",
		Attrs: [][2]string{
			{"from", tx.Sender.Bech32()},
			{"to", p.To.Bech32()},
			{"amount_uz", fmt.Sprintf("%d", p.Amount)},
		},
	})
	return nil
}

// activeValidatorCount counts bonded validators.
func activeValidatorCount(st *zstate.State) (uint64, error) {
	vals, err := st.Validators()
	if err != nil {
		return 0, err
	}
	var n uint64
	for _, v := range vals {
		if v.Status == ztypes.ValidatorBonded {
			n++
		}
	}
	return n, nil
}

// execDelegate bonds stake to a validator.
func (e *Engine) execDelegate(ctx *BlockContext, st *zstate.State, tx *ztypes.Tx, acc *ztypes.Account, rc *ztypes.Receipt, intrinsic uint64) error {
	p, err := ztypes.DecodeDelegatePayload(tx.Payload)
	if err != nil {
		return err
	}
	val, err := st.GetValidator(p.Validator)
	if err != nil {
		return err
	}
	if val == nil {
		return invalidf("%w: validator %s is not registered", ztypes.ErrStakeInvalid, p.Validator.Bech32())
	}
	if val.Status == ztypes.ValidatorJailed {
		return invalidf("%w: validator is jailed", ztypes.ErrStakeInvalid)
	}
	gasUsed := intrinsic + GasDelegateOp
	if gasUsed > tx.GasLimit {
		return invalidf("%w: delegation needs %d gas", ztypes.ErrGasExhausted, gasUsed)
	}
	if _, err := precheckValue(acc, p.Amount, gasUsed, tx.GasPrice); err != nil {
		return err
	}
	// Active-set capacity: a validator may only enter the set if there is room.
	if val.Status != ztypes.ValidatorBonded {
		n, err := activeValidatorCount(st)
		if err != nil {
			return err
		}
		if n >= ctx.Params.MaxActiveValidators {
			return invalidf("%w: active validator set is full (%d)", ztypes.ErrStakeInvalid, n)
		}
	}
	rc.GasUsed = gasUsed

	if err := debit(st, tx.Sender, p.Amount); err != nil {
		return err
	}
	rec, err := st.GetDelegation(tx.Sender, p.Validator)
	if err != nil {
		return err
	}
	if rec == nil {
		rec = &ztypes.DelegationRecord{Delegator: tx.Sender, Validator: p.Validator}
	}
	nextAmount, ovf := ztypes.AddU64(rec.Amount, p.Amount)
	if ovf {
		return invalidf("%w: delegation amount overflow", ztypes.ErrStakeInvalid)
	}
	rec.Amount = nextAmount
	st.SetDelegation(rec)

	if tx.Sender == p.Validator {
		val.SelfBond = mustAdd(val.SelfBond, p.Amount)
	} else {
		val.Delegated = mustAdd(val.Delegated, p.Amount)
	}
	if val.Status != ztypes.ValidatorBonded && val.VotingPower() >= ctx.Params.MinValidatorStakeUZ {
		val.Status = ztypes.ValidatorBonded
	}
	st.SetValidator(p.Validator, val)

	rc.Events = append(rc.Events, ztypes.Event{
		Type: "delegate",
		Attrs: [][2]string{
			{"delegator", tx.Sender.Bech32()},
			{"validator", p.Validator.Bech32()},
			{"amount_uz", fmt.Sprintf("%d", p.Amount)},
			{"voting_power_uz", fmt.Sprintf("%d", val.VotingPower())},
			{"status", val.Status.String()},
		},
	})
	return nil
}

// execUndelegate starts the unbonding of delegated stake.
func (e *Engine) execUndelegate(ctx *BlockContext, st *zstate.State, tx *ztypes.Tx, acc *ztypes.Account, rc *ztypes.Receipt, intrinsic uint64) error {
	p, err := ztypes.DecodeUndelegatePayload(tx.Payload)
	if err != nil {
		return err
	}
	val, err := st.GetValidator(p.Validator)
	if err != nil {
		return err
	}
	if val == nil {
		return invalidf("%w: validator %s is not registered", ztypes.ErrStakeInvalid, p.Validator.Bech32())
	}
	rec, err := st.GetDelegation(tx.Sender, p.Validator)
	if err != nil {
		return err
	}
	if rec == nil || rec.Amount < p.Amount {
		have := uint64(0)
		if rec != nil {
			have = rec.Amount
		}
		return invalidf("%w: delegation %d < requested %d", ztypes.ErrStakeInvalid, have, p.Amount)
	}
	gasUsed := intrinsic + GasUndelegateOp
	if gasUsed > tx.GasLimit {
		return invalidf("%w: undelegation needs %d gas", ztypes.ErrGasExhausted, gasUsed)
	}
	if _, err := precheckValue(acc, 0, gasUsed, tx.GasPrice); err != nil {
		return err
	}
	rc.GasUsed = gasUsed

	rec.Amount -= p.Amount
	release := ctx.Height + ctx.Params.UnbondingBlocks
	rec.Unbonding = append(rec.Unbonding, ztypes.UnbondingEntry{Amount: p.Amount, ReleaseHeight: release})
	if rec.Amount == 0 && len(rec.Unbonding) == 0 {
		st.DeleteDelegation(rec.Delegator, rec.Validator)
	} else {
		st.SetDelegation(rec)
	}
	if tx.Sender == p.Validator {
		val.SelfBond -= p.Amount
	} else {
		val.Delegated -= p.Amount
	}
	prevStatus := val.Status
	if val.Status == ztypes.ValidatorBonded && val.VotingPower() < ctx.Params.MinValidatorStakeUZ {
		val.Status = ztypes.ValidatorUnbonding
	}
	st.SetValidator(p.Validator, val)

	rc.Events = append(rc.Events, ztypes.Event{
		Type: "undelegate",
		Attrs: [][2]string{
			{"delegator", tx.Sender.Bech32()},
			{"validator", p.Validator.Bech32()},
			{"amount_uz", fmt.Sprintf("%d", p.Amount)},
			{"release_height", fmt.Sprintf("%d", release)},
			{"status_from", prevStatus.String()},
			{"status_to", val.Status.String()},
		},
	})
	return nil
}

// execValidatorRegister creates or updates a validator record.
//
// In protocol version 1 the account key and the consensus key of a validator are
// the same Ed25519 key: the transaction payload's public key must hash to the
// sender address (enforced in ztypes). Key separation between an operator account
// and a consensus key is a documented future upgrade (LIMITATIONS L-004).
func (e *Engine) execValidatorRegister(ctx *BlockContext, st *zstate.State, tx *ztypes.Tx, acc *ztypes.Account, rc *ztypes.Receipt, intrinsic uint64) error {
	p, err := ztypes.DecodeValidatorRegisterPayload(tx.Payload)
	if err != nil {
		return err
	}
	op := p.PubKey.Address()
	if op != tx.Sender {
		return invalidf("%w: validator key not owned by sender", ztypes.ErrStakeInvalid)
	}
	gasUsed := intrinsic + GasValidatorRegisterOp
	if gasUsed > tx.GasLimit {
		return invalidf("%w: registration needs %d gas", ztypes.ErrGasExhausted, gasUsed)
	}
	if _, err := precheckValue(acc, 0, gasUsed, tx.GasPrice); err != nil {
		return err
	}
	existing, err := st.GetValidator(op)
	if err != nil {
		return err
	}
	if existing != nil {
		// Updates may not be used to silently re-key or reset a validator.
		if existing.PubKey != p.PubKey {
			return invalidf("%w: validator key mismatch", ztypes.ErrStakeInvalid)
		}
		existing.Moniker = p.Moniker
		existing.CommissionBps = p.CommissionBps
		st.SetValidator(op, existing)
		rc.GasUsed = gasUsed
		rc.Events = append(rc.Events, ztypes.Event{Type: "validator_updated", Attrs: [][2]string{
			{"operator", op.Bech32()}, {"moniker", p.Moniker}, {"commission_bps", fmt.Sprintf("%d", p.CommissionBps)},
		}})
		return nil
	}
	// New validators enter as unbonded; they become bonded once their self-bond
	// reaches the minimum stake (via a delegate transaction).
	v := &ztypes.Validator{
		Operator:         op,
		PubKey:           p.PubKey,
		Moniker:          p.Moniker,
		CommissionBps:    p.CommissionBps,
		Status:           ztypes.ValidatorUnbonded,
		RegisteredHeight: ctx.Height,
	}
	st.SetValidator(op, v)
	rc.GasUsed = gasUsed
	rc.Events = append(rc.Events, ztypes.Event{Type: "validator_registered", Attrs: [][2]string{
		{"operator", op.Bech32()}, {"moniker", p.Moniker}, {"commission_bps", fmt.Sprintf("%d", p.CommissionBps)},
	}})
	return nil
}

// execGovProposal escrows a deposit and opens a proposal.
func (e *Engine) execGovProposal(ctx *BlockContext, st *zstate.State, tx *ztypes.Tx, acc *ztypes.Account, rc *ztypes.Receipt, intrinsic uint64) error {
	p, err := ztypes.DecodeGovProposalPayload(tx.Payload)
	if err != nil {
		return err
	}
	if p.Deposit < ctx.Params.MinProposalDepositUZ {
		return invalidf("%w: deposit %d below minimum %d", ztypes.ErrGovInvalid, p.Deposit, ctx.Params.MinProposalDepositUZ)
	}
	gasUsed := intrinsic + GasProposalOp
	if gasUsed > tx.GasLimit {
		return invalidf("%w: proposal needs %d gas", ztypes.ErrGasExhausted, gasUsed)
	}
	if _, err := precheckValue(acc, p.Deposit, gasUsed, tx.GasPrice); err != nil {
		return err
	}
	rc.GasUsed = gasUsed
	if err := debit(st, tx.Sender, p.Deposit); err != nil {
		return err
	}
	if err := credit(st, ztypes.SystemTreasuryAddress(), p.Deposit); err != nil {
		return err
	}
	id := st.NextProposalID()
	prop := &ztypes.Proposal{
		ID:              id,
		Proposer:        tx.Sender,
		Title:           p.Title,
		Description:     p.Description,
		Kind:            p.Kind,
		ParamKey:        p.ParamKey,
		ParamValue:      p.ParamValue,
		Deposit:         p.Deposit,
		SubmitHeight:    ctx.Height,
		VotingEndHeight: ctx.Height + ctx.Params.VotingPeriodBlocks,
		Status:          ztypes.ProposalVoting,
	}
	st.SetProposal(prop)
	rc.Events = append(rc.Events, ztypes.Event{Type: "proposal_submitted", Attrs: [][2]string{
		{"proposal_id", fmt.Sprintf("%d", id)},
		{"proposer", tx.Sender.Bech32()},
		{"kind", fmt.Sprintf("%d", p.Kind)},
		{"param_key", p.ParamKey},
		{"deposit_uz", fmt.Sprintf("%d", p.Deposit)},
	}})
	return nil
}

// stakedPower returns the voting power held by an account: the sum of its bonded
// delegations, including a self-bond.
func stakedPower(st *zstate.State, addr ztypes.Address) (uint64, error) {
	dels, err := st.Delegations()
	if err != nil {
		return 0, err
	}
	var power uint64
	for _, d := range dels {
		if d.Delegator != addr || d.Amount == 0 {
			continue
		}
		v, err := st.GetValidator(d.Validator)
		if err != nil {
			return 0, err
		}
		if v == nil || v.Status != ztypes.ValidatorBonded {
			continue
		}
		var ovf bool
		power, ovf = ztypes.AddU64(power, d.Amount)
		if ovf {
			return 0, fmt.Errorf("%w: voting power overflow", ztypes.ErrStateInvalid)
		}
	}
	return power, nil
}

// execGovVote records a vote weighted by bonded stake.
func (e *Engine) execGovVote(ctx *BlockContext, st *zstate.State, tx *ztypes.Tx, acc *ztypes.Account, rc *ztypes.Receipt, intrinsic uint64) error {
	p, err := ztypes.DecodeGovVotePayload(tx.Payload)
	if err != nil {
		return err
	}
	prop, err := st.GetProposal(p.ProposalID)
	if err != nil {
		return err
	}
	if prop == nil {
		return invalidf("%w: proposal %d not found", ztypes.ErrGovInvalid, p.ProposalID)
	}
	if prop.Status != ztypes.ProposalVoting {
		return invalidf("%w: proposal %d is %s", ztypes.ErrGovInvalid, p.ProposalID, prop.Status.String())
	}
	if ctx.Height >= prop.VotingEndHeight {
		return invalidf("%w: voting for proposal %d has ended", ztypes.ErrGovInvalid, p.ProposalID)
	}
	if st.HasVoted(p.ProposalID, tx.Sender) {
		return invalidf("%w: already voted on proposal %d", ztypes.ErrGovInvalid, p.ProposalID)
	}
	power, err := stakedPower(st, tx.Sender)
	if err != nil {
		return err
	}
	if power == 0 {
		return invalidf("%w: no bonded stake to vote with", ztypes.ErrGovInvalid)
	}
	gasUsed := intrinsic + GasVoteOp
	if gasUsed > tx.GasLimit {
		return invalidf("%w: vote needs %d gas", ztypes.ErrGasExhausted, gasUsed)
	}
	if _, err := precheckValue(acc, 0, gasUsed, tx.GasPrice); err != nil {
		return err
	}
	rc.GasUsed = gasUsed
	switch p.Option {
	case ztypes.VoteYes:
		prop.YesPower = mustAdd(prop.YesPower, power)
	case ztypes.VoteNo:
		prop.NoPower = mustAdd(prop.NoPower, power)
	case ztypes.VoteAbstain:
		prop.AbstainPower = mustAdd(prop.AbstainPower, power)
	default:
		return invalidf("%w: unknown option", ztypes.ErrGovInvalid)
	}
	st.SetProposal(prop)
	st.RecordVote(p.ProposalID, tx.Sender, p.Option)
	rc.Events = append(rc.Events, ztypes.Event{Type: "vote", Attrs: [][2]string{
		{"proposal_id", fmt.Sprintf("%d", p.ProposalID)},
		{"voter", tx.Sender.Bech32()},
		{"option", p.Option.String()},
		{"power_uz", fmt.Sprintf("%d", power)},
	}})
	return nil
}

// vmHost journals storage writes so an aborted call leaves no trace.
type vmHost struct {
	st       *zstate.State
	contract ztypes.Address
	writes   map[uint64]uint64
}

func (h *vmHost) Slot(slot uint64) uint64 {
	if v, ok := h.writes[slot]; ok {
		return v
	}
	return h.st.GetSlot(h.contract, slot)
}

func (h *vmHost) SetSlot(slot, value uint64) { h.writes[slot] = value }

func (h *vmHost) commit() {
	// Each slot is independent, so commit order cannot affect the final state.
	for slot, value := range h.writes {
		h.st.SetSlot(h.contract, slot, value)
	}
}

// execContractDeploy stores bytecode and runs its constructor.
func (e *Engine) execContractDeploy(ctx *BlockContext, st *zstate.State, tx *ztypes.Tx, acc *ztypes.Account, rc *ztypes.Receipt, intrinsic uint64) error {
	p, err := ztypes.DecodeContractDeployPayload(tx.Payload)
	if err != nil {
		return err
	}
	if len(p.Code) > zvm.MaxProgramSize {
		return invalidf("%w: bytecode exceeds %d bytes", ztypes.ErrContractInvalid, zvm.MaxProgramSize)
	}
	codeGas, ovf := ztypes.MulU64(uint64(len(p.Code)), GasDeployPerByte)
	if ovf {
		return invalidf("%w: deployment gas overflow", ztypes.ErrTxGasLimit)
	}
	base, ovf := ztypes.AddU64(intrinsic, codeGas)
	if ovf || base > tx.GasLimit {
		return invalidf("%w: deployment needs %d gas", ztypes.ErrGasExhausted, base)
	}
	if _, err := precheckValue(acc, 0, base, tx.GasPrice); err != nil {
		return err
	}
	addr := ztypes.ContractAddress(tx.Sender, tx.Nonce)
	existing, err := st.GetAccount(addr)
	if err != nil {
		return err
	}
	if existing.IsContract() {
		return invalidf("%w: contract address %s already in use", ztypes.ErrContractInvalid, addr.Bech32())
	}
	codeHash := zcrypto.Digest("ZNET/v1/contract-code", p.Code)
	host := &vmHost{st: st, contract: addr, writes: map[uint64]uint64{}}
	res := zvm.Run(p.Code, zvm.Config{
		GasLimit: tx.GasLimit - base,
		Caller:   tx.Sender,
		Self:     addr,
		Value:    0,
		Calldata: p.InitData,
	}, host)
	rc.GasUsed = base + res.GasUsed
	if res.Reverted {
		return fmt.Errorf("%w: constructor failed: %v", ztypes.ErrContractInvalid, res.Err)
	}
	host.commit()
	st.SetCode(codeHash, p.Code)
	cacc, err := st.GetAccount(addr)
	if err != nil {
		return err
	}
	cacc.CodeHash = codeHash
	cacc.CreatedHeight = ctx.Height
	st.SetAccount(addr, cacc)
	rc.ContractAddress = addr
	rc.Output = res.Output
	for _, ev := range res.Events {
		rc.Events = append(rc.Events, ztypes.Event{Type: "contract_log", Attrs: [][2]string{
			{"contract", addr.Bech32()}, {"value", fmt.Sprintf("%d", ev)},
		}})
	}
	rc.Events = append(rc.Events, ztypes.Event{Type: "contract_deployed", Attrs: [][2]string{
		{"contract", addr.Bech32()},
		{"code_hash", codeHash.String()},
		{"code_size", fmt.Sprintf("%d", len(p.Code))},
		{"deployer", tx.Sender.Bech32()},
	}})
	return nil
}

// execContractCall invokes a deployed contract.
//
// Contract calls do not carry value in protocol version 1 (see LIMITATIONS L-005).
func (e *Engine) execContractCall(ctx *BlockContext, st *zstate.State, tx *ztypes.Tx, acc *ztypes.Account, rc *ztypes.Receipt, intrinsic uint64) error {
	p, err := ztypes.DecodeContractCallPayload(tx.Payload)
	if err != nil {
		return err
	}
	cacc, err := st.GetAccount(p.Contract)
	if err != nil {
		return err
	}
	if cacc.CodeHash.IsZero() {
		return invalidf("%w: %s is not a contract", ztypes.ErrContractInvalid, p.Contract.Bech32())
	}
	code, ok := st.GetCode(cacc.CodeHash)
	if !ok {
		return invalidf("%w: contract code missing for hash %s", ztypes.ErrStateInvalid, cacc.CodeHash.Short())
	}
	if intrinsic > tx.GasLimit {
		return invalidf("%w: call needs %d gas", ztypes.ErrGasExhausted, intrinsic)
	}
	if _, err := precheckValue(acc, 0, intrinsic, tx.GasPrice); err != nil {
		return err
	}
	host := &vmHost{st: st, contract: p.Contract, writes: map[uint64]uint64{}}
	res := zvm.Run(code, zvm.Config{
		GasLimit: tx.GasLimit - intrinsic,
		Caller:   tx.Sender,
		Self:     p.Contract,
		Value:    0,
		Calldata: p.Input,
	}, host)
	rc.GasUsed = intrinsic + res.GasUsed
	rc.ContractAddress = p.Contract
	rc.Output = res.Output
	if res.Reverted {
		return fmt.Errorf("%w: %v", ztypes.ErrContractInvalid, res.Err)
	}
	host.commit()
	for _, ev := range res.Events {
		rc.Events = append(rc.Events, ztypes.Event{Type: "contract_log", Attrs: [][2]string{
			{"contract", p.Contract.Bech32()}, {"value", fmt.Sprintf("%d", ev)},
		}})
	}
	return nil
}
