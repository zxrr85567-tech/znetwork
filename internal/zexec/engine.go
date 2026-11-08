// Package zexec implements Z Network's deterministic state-transition function
// (AGENT_05), including the native asset, staking, governance and contract calls.
//
// Design rules that the implementation is held to:
//
//  1. Validation before mutation for native transactions. Every check that can
//     fail runs before the first write, so a rejected transaction never leaves
//     partial state behind. Contract executions (which can write and then fail)
//     run against a journaled host whose writes are committed only on success.
//  2. No floating point anywhere. All monetary arithmetic is integer, with
//     explicit overflow checks.
//  3. Deterministic iteration. Validators, delegations and proposals are always
//     traversed in key order (address bytes / numeric ids), never map order.
//  4. Supply conservation. Every mint is explicit, and
//     `supply == sum(balances) + sum(bonded stake) + sum(unbonding) + escrowed deposits`
//     is asserted by tests after every block (see invariant_test.go).
package zexec

import (
	"errors"
	"fmt"

	"znetwork/internal/zstate"
	"znetwork/internal/ztypes"
)

// Native gas costs. These are consensus constants for protocol version 1.
const (
	// GasTransferOp is charged for the state work of a transfer.
	GasTransferOp uint64 = 5_000
	// GasDelegateOp is charged for updating delegation and validator records.
	GasDelegateOp uint64 = 20_000
	// GasUndelegateOp includes writing an unbonding entry.
	GasUndelegateOp uint64 = 25_000
	// GasValidatorRegisterOp is charged for establishing a validator record.
	GasValidatorRegisterOp uint64 = 30_000
	// GasProposalOp is charged for escrowing a governance deposit.
	GasProposalOp uint64 = 50_000
	// GasVoteOp is charged for recording a vote.
	GasVoteOp uint64 = 5_000
	// GasDeployPerByte is charged per byte of contract code (storage growth).
	GasDeployPerByte uint64 = 200
	// GovQuorumBps is the share of bonded stake that must vote for a proposal to
	// be valid. 3340 bps = 33.4%.
	GovQuorumBps uint64 = 3340
)

// invalidTx marks a transaction that is not valid for inclusion at all: it is
// rejected with no state change and no fee. This is deliberately distinct from an
// execution failure (a contract revert), which is charged and consumes a nonce.
type invalidTx struct{ err error }

func (e *invalidTx) Error() string { return e.err.Error() }
func (e *invalidTx) Unwrap() error { return e.err }

// invalidf builds an invalidTx error.
func invalidf(format string, a ...any) error {
	return &invalidTx{err: fmt.Errorf(format, a...)}
}

// IsInvalidTx reports whether err marks a statically invalid transaction.
func IsInvalidTx(err error) bool {
	var it *invalidTx
	return errors.As(err, &it)
}

// Engine is the state-transition function. It is stateless apart from the chain
// identity, so multiple engines (e.g. speculative and committed) can coexist.
type Engine struct {
	chainID string
}

// NewEngine returns an engine bound to a chain id.
func NewEngine(chainID string) *Engine { return &Engine{chainID: chainID} }

// ChainID returns the bound chain id.
func (e *Engine) ChainID() string { return e.chainID }

// BlockContext is the per-block execution context.
type BlockContext struct {
	Height   uint64
	TimeMs   int64
	Proposer ztypes.Address
	Params   *ztypes.Params
}

// InitChain builds the genesis state from a validated genesis document.
//
// Money flow is exact: the total supply equals the sum of allocations. Validator
// self-bonds are moved out of the operator's liquid balance into bonded stake and
// recorded as a delegation from the operator to itself.
func (e *Engine) InitChain(g *ztypes.Genesis) (*zstate.State, error) {
	rg, err := g.Resolve()
	if err != nil {
		return nil, err
	}
	gh, err := g.Hash()
	if err != nil {
		return nil, err
	}
	st := zstate.New()
	st.SetParams(rg.Params)
	st.SetGenesisHash(gh)
	st.SetChainID(rg.ChainID)

	var total uint64
	for _, a := range rg.Accounts {
		acc := &ztypes.Account{Balance: a.Balance, Nonce: 0, CreatedHeight: 0}
		st.SetAccount(a.Addr, acc)
		var ov bool
		total, ov = ztypes.AddU64(total, a.Balance)
		if ov || total > ztypes.MaxSupply {
			return nil, fmt.Errorf("%w: supply overflow", ztypes.ErrGenesisInvalid)
		}
	}
	st.SetSupply(total)

	for _, v := range rg.Validators {
		val := &ztypes.Validator{
			Operator:         v.Operator,
			PubKey:           v.PubKey,
			Moniker:          v.Moniker,
			CommissionBps:    v.CommissionBps,
			SelfBond:         v.SelfBond,
			Status:           ztypes.ValidatorBonded,
			RegisteredHeight: 0,
		}
		acc, err := st.GetAccount(v.Operator)
		if err != nil {
			return nil, err
		}
		if acc.Balance < v.SelfBond {
			return nil, fmt.Errorf("%w: validator %s cannot cover self bond", ztypes.ErrGenesisInvalid, v.Operator.Bech32())
		}
		acc.Balance -= v.SelfBond
		st.SetAccount(v.Operator, acc)
		st.SetValidator(v.Operator, val)
		st.SetDelegation(&ztypes.DelegationRecord{
			Delegator: v.Operator,
			Validator: v.Operator,
			Amount:    v.SelfBond,
		})
	}
	// Bind the genesis block hash placeholder; the caller seals height 0 state.
	st.SetHeight(0)
	return st, nil
}

// BeginBlock applies height-triggered transitions that precede transactions:
// releasing matured unbonding entries and tallying finished proposals.
func (e *Engine) BeginBlock(ctx *BlockContext, st *zstate.State) error {
	if err := e.releaseUnbonding(ctx, st); err != nil {
		return err
	}
	if err := e.tallyProposals(ctx, st); err != nil {
		return err
	}
	return nil
}

func (e *Engine) releaseUnbonding(ctx *BlockContext, st *zstate.State) error {
	dels, err := st.Delegations()
	if err != nil {
		return err
	}
	for _, d := range dels {
		if len(d.Unbonding) == 0 {
			continue
		}
		keep := d.Unbonding[:0]
		var released uint64
		for _, u := range d.Unbonding {
			if u.ReleaseHeight <= ctx.Height {
				var ov bool
				released, ov = ztypes.AddU64(released, u.Amount)
				if ov {
					return fmt.Errorf("%w: release overflow", ztypes.ErrStateInvalid)
				}
				continue
			}
			keep = append(keep, u)
		}
		d.Unbonding = keep
		if released > 0 {
			acc, err := st.GetAccount(d.Delegator)
			if err != nil {
				return err
			}
			var ov bool
			acc.Balance, ov = ztypes.AddU64(acc.Balance, released)
			if ov {
				return fmt.Errorf("%w: balance overflow on unbonding release", ztypes.ErrStateInvalid)
			}
			st.SetAccount(d.Delegator, acc)
			st.SetDelegation(d)
		}
	}
	return nil
}

func (e *Engine) tallyProposals(ctx *BlockContext, st *zstate.State) error {
	props, err := st.Proposals()
	if err != nil {
		return err
	}
	bonded, err := st.TotalBondedStake()
	if err != nil {
		return err
	}
	for _, p := range props {
		if p.Status != ztypes.ProposalVoting || p.VotingEndHeight > ctx.Height {
			continue
		}
		quorum := mulDiv(bonded, GovQuorumBps, 10_000)
		passed := p.TotalVoted() >= quorum && p.YesPower > p.NoPower && p.YesPower > 0
		if !passed {
			p.Status = ztypes.ProposalRejected
			// The deposit stays with the proposer: it was never spent, and
			// burning deposits for ordinary rejection would make governance
			// needlessly punitive for a development network.
			st.SetProposal(p)
			continue
		}
		p.Status = ztypes.ProposalPassed
		if p.Kind == ztypes.GovProposalParam {
			params, err := st.Params()
			if err != nil {
				return err
			}
			if err := params.Set(p.ParamKey, p.ParamValue); err != nil {
				// A proposal that cannot be applied is rejected, never partially
				// applied.
				p.Status = ztypes.ProposalRejected
				st.SetProposal(p)
				continue
			}
			st.SetParams(params)
			p.Status = ztypes.ProposalExecuted
		}
		st.SetProposal(p)
	}
	return nil
}

// ExecuteTx validates and executes one transaction against st.
//
// It returns a receipt in every case: a failed transaction is still charged and
// still consumes its nonce, exactly like a revert in a real chain. Native
// transactions validate fully before mutating; contract calls execute against a
// journaled host.
func (e *Engine) ExecuteTx(ctx *BlockContext, st *zstate.State, tx *ztypes.Tx) *ztypes.Receipt {
	rc := &ztypes.Receipt{
		TxID:      tx.ID(),
		Height:    ctx.Height,
		Sender:    tx.Sender,
		Type:      tx.Type,
		GasWanted: tx.GasLimit,
	}
	reject := func(gasUsed uint64, err error) *ztypes.Receipt {
		rc.Status = ztypes.ReceiptFailed
		rc.GasUsed = gasUsed
		rc.Error = err.Error()
		if gasUsed > tx.GasLimit {
			rc.GasUsed = tx.GasLimit
		}
		fee, ferr := ztypes.MulU64(rc.GasUsed, tx.GasPrice)
		if ferr {
			rc.Fee = 0
		} else {
			rc.Fee = fee
		}
		return rc
	}

	// --- stage 1: stateless validation -------------------------------------
	if err := tx.ValidateStatic(e.chainID); err != nil {
		return reject(0, err)
	}
	if tx.GasPrice < ctx.Params.MinGasPriceUZ {
		return reject(0, fmt.Errorf("%w: gas price %d below minimum %d", ztypes.ErrTxGasLimit, tx.GasPrice, ctx.Params.MinGasPriceUZ))
	}
	intrinsic, err := e.intrinsicGas(ctx.Params, tx)
	if err != nil {
		return reject(0, err)
	}
	if intrinsic > tx.GasLimit {
		return reject(0, fmt.Errorf("%w: intrinsic gas %d exceeds limit %d", ztypes.ErrGasExhausted, intrinsic, tx.GasLimit))
	}
	if rc.Index >= ztypes.MaxTxPerBlock {
		return reject(0, fmt.Errorf("%w: too many transactions", ztypes.ErrBlockInvalid))
	}

	// --- stage 2: account checks ------------------------------------------
	acc, err := st.GetAccount(tx.Sender)
	if err != nil {
		return reject(0, err)
	}
	if tx.Nonce != acc.Nonce {
		return reject(0, fmt.Errorf("%w: got %d want %d", ztypes.ErrNonceMismatch, tx.Nonce, acc.Nonce))
	}
	maxFee, ovf := ztypes.MulU64(tx.GasLimit, tx.GasPrice)
	if ovf {
		return reject(0, fmt.Errorf("%w: fee overflow", ztypes.ErrTxGasLimit))
	}
	if acc.Balance < maxFee {
		return reject(0, fmt.Errorf("%w: balance %d cannot cover max fee %d", ztypes.ErrInsufficientFunds, acc.Balance, maxFee))
	}

	// --- stage 3: type-specific execution ---------------------------------
	execErr := e.execByType(ctx, st, tx, acc, rc, intrinsic)
	if execErr != nil && IsInvalidTx(execErr) {
		// Invalid: no state change at all. A block containing this transaction
		// is itself invalid, so it can never be committed.
		rc.Status = ztypes.ReceiptFailed
		rc.Error = execErr.Error()
		rc.GasUsed = 0
		rc.Fee = 0
		return rc
	}
	if execErr != nil {
		// Failure path: charge the gas that was actually consumed, keep the
		// nonce bump, and make sure no value transfer escaped.
		rc.Status = ztypes.ReceiptFailed
		rc.Error = execErr.Error()
		if rc.GasUsed < intrinsic {
			rc.GasUsed = intrinsic
		}
		if rc.GasUsed > tx.GasLimit {
			rc.GasUsed = tx.GasLimit
		}
	} else {
		rc.Status = ztypes.ReceiptSuccess
		if rc.GasUsed < intrinsic {
			rc.GasUsed = intrinsic
		}
		if rc.GasUsed > tx.GasLimit {
			rc.GasUsed = tx.GasLimit
		}
	}
	fee, fovf := ztypes.MulU64(rc.GasUsed, tx.GasPrice)
	if fovf {
		return reject(0, fmt.Errorf("%w: fee overflow", ztypes.ErrTxGasLimit))
	}
	rc.Fee = fee

	// Settle: nonce bump and fee payment happen for both outcomes, but only after
	// every check above passed.
	acc, err = st.GetAccount(tx.Sender)
	if err != nil {
		return reject(0, err)
	}
	acc.Nonce++
	if acc.Balance < fee {
		// Should be impossible: maxFee >= fee and maxFee was pre-checked.
		return reject(0, fmt.Errorf("%w: cannot settle fee", ztypes.ErrStateInvalid))
	}
	acc.Balance -= fee
	st.SetAccount(tx.Sender, acc)
	if fee > 0 {
		proposer := ctx.Proposer
		if proposer.IsZero() {
			proposer = tx.Sender
		}
		pacc, err := st.GetAccount(proposer)
		if err != nil {
			return reject(0, err)
		}
		var fov bool
		pacc.Balance, fov = ztypes.AddU64(pacc.Balance, fee)
		if fov {
			return reject(0, fmt.Errorf("%w: fee credit overflow", ztypes.ErrStateInvalid))
		}
		st.SetAccount(proposer, pacc)
	}
	return rc
}

// intrinsicGas returns base gas plus per-byte gas for the encoded transaction.
func (e *Engine) intrinsicGas(p *ztypes.Params, tx *ztypes.Tx) (uint64, error) {
	perByte, ovf := ztypes.MulU64(uint64(len(tx.Encode())), p.GasPerByteUZ)
	if ovf {
		return 0, fmt.Errorf("%w: calldata gas overflow", ztypes.ErrTxGasLimit)
	}
	total, ovf := ztypes.AddU64(p.BaseTxGas, perByte)
	if ovf {
		return 0, fmt.Errorf("%w: intrinsic gas overflow", ztypes.ErrTxGasLimit)
	}
	return total, nil
}

func (e *Engine) execByType(ctx *BlockContext, st *zstate.State, tx *ztypes.Tx, acc *ztypes.Account, rc *ztypes.Receipt, intrinsic uint64) error {
	switch tx.Type {
	case ztypes.TxTransfer:
		return e.execTransfer(ctx, st, tx, acc, rc, intrinsic)
	case ztypes.TxDelegate:
		return e.execDelegate(ctx, st, tx, acc, rc, intrinsic)
	case ztypes.TxUndelegate:
		return e.execUndelegate(ctx, st, tx, acc, rc, intrinsic)
	case ztypes.TxValidatorRegister:
		return e.execValidatorRegister(ctx, st, tx, acc, rc, intrinsic)
	case ztypes.TxGovProposal:
		return e.execGovProposal(ctx, st, tx, acc, rc, intrinsic)
	case ztypes.TxGovVote:
		return e.execGovVote(ctx, st, tx, acc, rc, intrinsic)
	case ztypes.TxContractDeploy:
		return e.execContractDeploy(ctx, st, tx, acc, rc, intrinsic)
	case ztypes.TxContractCall:
		return e.execContractCall(ctx, st, tx, acc, rc, intrinsic)
	default:
		return fmt.Errorf("%w: %d", ztypes.ErrTxType, uint8(tx.Type))
	}
}

// EndBlock distributes the block reward to bonded stake and returns the minted
// amount. Total supply grows by exactly the block reward.
func (e *Engine) EndBlock(ctx *BlockContext, st *zstate.State) (minted uint64, err error) {
	reward := ctx.Params.BlockRewardUZ
	if reward == 0 {
		return 0, nil
	}
	supply := st.Supply()
	newSupply, ovf := ztypes.AddU64(supply, reward)
	if ovf || newSupply > ztypes.MaxSupply {
		// Supply cap reached: mint only what remains.
		if supply >= ztypes.MaxSupply {
			return 0, nil
		}
		reward = ztypes.MaxSupply - supply
		newSupply = ztypes.MaxSupply
	}
	validators, err := st.Validators()
	if err != nil {
		return 0, err
	}
	var bonded []*ztypes.Validator
	var totalPower uint64
	for _, v := range validators {
		if v.Status != ztypes.ValidatorBonded || v.VotingPower() == 0 {
			continue
		}
		bonded = append(bonded, v)
		totalPower, ovf = ztypes.AddU64(totalPower, v.VotingPower())
		if ovf {
			return 0, fmt.Errorf("%w: voting power overflow", ztypes.ErrStateInvalid)
		}
	}
	if totalPower == 0 {
		// No bonded stake: the reward is not created at all rather than being
		// handed to an arbitrary account.
		st.SetSupply(supply)
		return 0, nil
	}

	var distributed uint64
	for _, v := range bonded {
		vReward := mulDiv(reward, v.VotingPower(), totalPower)
		if vReward == 0 {
			continue
		}
		commission := mulDiv(vReward, uint64(v.CommissionBps), 10_000)
		toDelegators := vReward - commission
		if commission > 0 {
			if err := credit(st, v.Operator, commission); err != nil {
				return 0, err
			}
			distributed = mustAdd(distributed, commission)
		}
		dels, err := st.DelegationsFor(v.Operator)
		if err != nil {
			return 0, err
		}
		var shareTotal uint64
		for _, d := range dels {
			if d.Amount == 0 {
				continue
			}
			share := mulDiv(toDelegators, d.Amount, v.VotingPower())
			if share == 0 {
				continue
			}
			if err := credit(st, d.Delegator, share); err != nil {
				return 0, err
			}
			distributed = mustAdd(distributed, share)
			shareTotal = mustAdd(shareTotal, share)
		}
		// Truncation remainder of the delegator split goes to the validator
		// operator: deterministic, and it never leaves tokens uncreated.
		if rem := toDelegators - shareTotal; rem > 0 {
			if err := credit(st, v.Operator, rem); err != nil {
				return 0, err
			}
			distributed = mustAdd(distributed, rem)
		}
	}
	// Anything lost to per-validator truncation goes to the proposer, so the
	// minted amount is always fully accounted for.
	if distributed < reward {
		rem := reward - distributed
		target := ctx.Proposer
		if target.IsZero() {
			target = bonded[0].Operator
		}
		if err := credit(st, target, rem); err != nil {
			return 0, err
		}
		distributed = mustAdd(distributed, rem)
	}
	if distributed != reward {
		return 0, fmt.Errorf("%w: reward accounting mismatch (%d != %d)", ztypes.ErrStateInvalid, distributed, reward)
	}
	st.SetSupply(newSupply)
	return reward, nil
}

// credit adds amount to an account's liquid balance with overflow detection.
func credit(st *zstate.State, addr ztypes.Address, amount uint64) error {
	acc, err := st.GetAccount(addr)
	if err != nil {
		return err
	}
	next, ovf := ztypes.AddU64(acc.Balance, amount)
	if ovf {
		return fmt.Errorf("%w: balance overflow for %s", ztypes.ErrStateInvalid, addr.Bech32())
	}
	acc.Balance = next
	st.SetAccount(addr, acc)
	return nil
}

func debit(st *zstate.State, addr ztypes.Address, amount uint64) error {
	acc, err := st.GetAccount(addr)
	if err != nil {
		return err
	}
	if acc.Balance < amount {
		return fmt.Errorf("%w: balance %d < %d", ztypes.ErrInsufficientFunds, acc.Balance, amount)
	}
	acc.Balance -= amount
	st.SetAccount(addr, acc)
	return nil
}

func mustAdd(a, b uint64) uint64 {
	s, ovf := ztypes.AddU64(a, b)
	if ovf {
		panic("zexec: unexpected overflow in reward accounting")
	}
	return s
}

// mulDiv computes floor(a*b/div) without overflowing for the value ranges used by
// this protocol (amounts up to 1e17, shares up to 1e4).
func mulDiv(a, b, div uint64) uint64 {
	if div == 0 {
		return 0
	}
	// Use 128-bit intermediate via math/bits.
	hi, lo := mul64(a, b)
	if hi == 0 {
		return lo / div
	}
	// Fall back to big-int-free long division: (hi:lo)/div
	return div128(hi, lo, div)
}

func mul64(a, b uint64) (hi, lo uint64) {
	const mask = 0xffffffff
	a0, a1 := a&mask, a>>32
	b0, b1 := b&mask, b>>32
	p00 := a0 * b0
	p01 := a0 * b1
	p10 := a1 * b0
	p11 := a1 * b1
	mid := p01 + (p00 >> 32)
	mid += p10
	lo = (mid << 32) | (p00 & mask)
	hi = p11 + (mid >> 32)
	return hi, lo
}

func div128(hi, lo, div uint64) uint64 {
	var q uint64
	for i := 63; i >= 0; i-- {
		q <<= 1
		hi = hi<<1 | (lo >> 63)
		lo <<= 1
		if hi >= div {
			hi -= div
			q |= 1
		}
	}
	return q
}
