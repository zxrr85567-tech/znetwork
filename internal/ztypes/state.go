package ztypes

import (
	"fmt"

	"znetwork/internal/zencoding"
)

// Account is the per-address state record.
//
// CodeHash is non-zero only for contract accounts (see AGENT_06/07). Storage
// lives in the global state tree under a per-contract prefix, so the account
// record stays small and every storage slot is independently provable.
type Account struct {
	Balance       uint64
	Nonce         uint64
	CodeHash      Hash
	CreatedHeight uint64
}

// IsContract reports whether the account holds contract code.
func (a *Account) IsContract() bool { return !a.CodeHash.IsZero() }

// Encode returns the canonical account encoding.
func (a *Account) Encode() []byte {
	w := zencoding.NewWriterSize(64)
	w.U8(0x01)
	w.U64(a.Balance)
	w.U64(a.Nonce)
	w.Raw(a.CodeHash[:])
	w.U64(a.CreatedHeight)
	return w.Done()
}

// DecodeAccount parses a canonical account record.
func DecodeAccount(b []byte) (*Account, error) {
	r := zencoding.NewReader(b)
	if v := r.U8(); v != 0x01 {
		return nil, fmt.Errorf("%w: account version %d", ErrStateInvalid, v)
	}
	a := &Account{}
	a.Balance = r.U64()
	a.Nonce = r.U64()
	copy(a.CodeHash[:], r.Raw(32))
	a.CreatedHeight = r.U64()
	if err := r.Done(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStateInvalid, err)
	}
	return a, nil
}

// ValidatorStatus is the lifecycle state of a validator.
type ValidatorStatus uint8

// Validator lifecycle states. Only ValidatorBonded validators in the active set
// participate in consensus and earn rewards.
const (
	// ValidatorUnbonded has no stake and does not participate.
	ValidatorUnbonded ValidatorStatus = 0
	// ValidatorBonded participates in consensus.
	ValidatorBonded ValidatorStatus = 1
	// ValidatorJailed is excluded from the active set (punishment).
	ValidatorJailed ValidatorStatus = 2
	// ValidatorUnbonding has left the active set and is waiting for release.
	ValidatorUnbonding ValidatorStatus = 3
)

// String returns a stable name.
func (s ValidatorStatus) String() string {
	switch s {
	case ValidatorUnbonded:
		return "unbonded"
	case ValidatorBonded:
		return "bonded"
	case ValidatorJailed:
		return "jailed"
	case ValidatorUnbonding:
		return "unbonding"
	default:
		return "unknown"
	}
}

// Validator is the on-chain validator record.
type Validator struct {
	Operator         Address
	PubKey           PublicKey
	Moniker          string
	CommissionBps    uint16
	SelfBond         uint64
	Delegated        uint64
	Status           ValidatorStatus
	RegisteredHeight uint64
}

// VotingPower is the validator's total bonded stake in base units.
func (v *Validator) VotingPower() uint64 { return v.SelfBond + v.Delegated }

// Encode returns the canonical validator encoding.
func (v *Validator) Encode() []byte {
	w := zencoding.NewWriterSize(160)
	w.U8(0x01)
	w.Raw(v.Operator[:])
	w.Raw(v.PubKey[:])
	w.Str(v.Moniker)
	w.U16(v.CommissionBps)
	w.U64(v.SelfBond)
	w.U64(v.Delegated)
	w.U8(uint8(v.Status))
	w.U64(v.RegisteredHeight)
	return w.Done()
}

// DecodeValidator parses a canonical validator record.
func DecodeValidator(b []byte) (*Validator, error) {
	r := zencoding.NewReader(b)
	if x := r.U8(); x != 0x01 {
		return nil, fmt.Errorf("%w: validator version %d", ErrStateInvalid, x)
	}
	v := &Validator{}
	copy(v.Operator[:], r.Raw(20))
	copy(v.PubKey[:], r.Raw(32))
	v.Moniker = r.Str()
	v.CommissionBps = r.U16()
	v.SelfBond = r.U64()
	v.Delegated = r.U64()
	v.Status = ValidatorStatus(r.U8())
	v.RegisteredHeight = r.U64()
	if err := r.Done(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStateInvalid, err)
	}
	if v.CommissionBps > 10000 {
		return nil, fmt.Errorf("%w: commission out of range", ErrStateInvalid)
	}
	return v, nil
}

// UnbondingEntry is a pending release of stake.
type UnbondingEntry struct {
	Amount        uint64
	ReleaseHeight uint64
}

// DelegationRecord is the state record for one (delegator, validator) pair.
type DelegationRecord struct {
	Delegator Address
	Validator Address
	Amount    uint64
	Unbonding []UnbondingEntry
}

// Encode returns the canonical delegation encoding.
func (d *DelegationRecord) Encode() []byte {
	w := zencoding.NewWriterSize(96 + 16*len(d.Unbonding))
	w.U8(0x01)
	w.Raw(d.Delegator[:])
	w.Raw(d.Validator[:])
	w.U64(d.Amount)
	w.U32(uint32(len(d.Unbonding)))
	for _, u := range d.Unbonding {
		w.U64(u.Amount)
		w.U64(u.ReleaseHeight)
	}
	return w.Done()
}

// DecodeDelegation parses a canonical delegation record.
func DecodeDelegation(b []byte) (*DelegationRecord, error) {
	r := zencoding.NewReader(b)
	if x := r.U8(); x != 0x01 {
		return nil, fmt.Errorf("%w: delegation version %d", ErrStateInvalid, x)
	}
	d := &DelegationRecord{}
	copy(d.Delegator[:], r.Raw(20))
	copy(d.Validator[:], r.Raw(20))
	d.Amount = r.U64()
	n := r.Len32(16)
	if r.Err() != nil || n > 1024 {
		return nil, fmt.Errorf("%w: unbonding count", ErrStateInvalid)
	}
	d.Unbonding = make([]UnbondingEntry, n)
	for i := 0; i < n; i++ {
		d.Unbonding[i].Amount = r.U64()
		d.Unbonding[i].ReleaseHeight = r.U64()
	}
	if err := r.Done(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStateInvalid, err)
	}
	return d, nil
}

// ProposalStatus is the governance lifecycle state.
type ProposalStatus uint8

// Proposal lifecycle states.
const (
	// ProposalVoting is open for votes.
	ProposalVoting ProposalStatus = 1
	// ProposalPassed reached quorum and majority; not yet executed.
	ProposalPassed ProposalStatus = 2
	// ProposalRejected failed quorum or majority.
	ProposalRejected ProposalStatus = 3
	// ProposalExecuted had its effect applied.
	ProposalExecuted ProposalStatus = 4
)

// String returns a stable name.
func (s ProposalStatus) String() string {
	switch s {
	case ProposalVoting:
		return "voting"
	case ProposalPassed:
		return "passed"
	case ProposalRejected:
		return "rejected"
	case ProposalExecuted:
		return "executed"
	default:
		return "unknown"
	}
}

// Proposal is a governance proposal.
type Proposal struct {
	ID              uint64
	Proposer        Address
	Title           string
	Description     string
	Kind            GovProposalKind
	ParamKey        string
	ParamValue      uint64
	Deposit         uint64
	SubmitHeight    uint64
	VotingEndHeight uint64
	Status          ProposalStatus
	YesPower        uint64
	NoPower         uint64
	AbstainPower    uint64
}

// TotalVoted returns the total voting power cast.
func (p *Proposal) TotalVoted() uint64 { return p.YesPower + p.NoPower + p.AbstainPower }

// Encode returns the canonical proposal encoding.
func (p *Proposal) Encode() []byte {
	w := zencoding.NewWriterSize(256)
	w.U8(0x01)
	w.U64(p.ID)
	w.Raw(p.Proposer[:])
	w.Str(p.Title)
	w.Str(p.Description)
	w.U8(uint8(p.Kind))
	w.Str(p.ParamKey)
	w.U64(p.ParamValue)
	w.U64(p.Deposit)
	w.U64(p.SubmitHeight)
	w.U64(p.VotingEndHeight)
	w.U8(uint8(p.Status))
	w.U64(p.YesPower)
	w.U64(p.NoPower)
	w.U64(p.AbstainPower)
	return w.Done()
}

// DecodeProposal parses a canonical proposal record.
func DecodeProposal(b []byte) (*Proposal, error) {
	r := zencoding.NewReader(b)
	if x := r.U8(); x != 0x01 {
		return nil, fmt.Errorf("%w: proposal version %d", ErrStateInvalid, x)
	}
	p := &Proposal{}
	p.ID = r.U64()
	copy(p.Proposer[:], r.Raw(20))
	p.Title = r.Str()
	p.Description = r.Str()
	p.Kind = GovProposalKind(r.U8())
	p.ParamKey = r.Str()
	p.ParamValue = r.U64()
	p.Deposit = r.U64()
	p.SubmitHeight = r.U64()
	p.VotingEndHeight = r.U64()
	p.Status = ProposalStatus(r.U8())
	p.YesPower = r.U64()
	p.NoPower = r.U64()
	p.AbstainPower = r.U64()
	if err := r.Done(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStateInvalid, err)
	}
	return p, nil
}
