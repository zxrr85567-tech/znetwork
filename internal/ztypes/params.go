package ztypes

import (
	"fmt"
	"sort"

	"znetwork/internal/zencoding"
)

// EconomicsNote is returned by the RPC alongside the live parameter set so no
// client can mistake these development-network values for approved economics.
const EconomicsNote = "development-network parameter proposals; not approved economic policy"

// ParamSpec describes one governable parameter: its bounds and its default.
// Governance can only change parameters that appear here (whitelist), and only
// within the declared bounds.
type ParamSpec struct {
	Key         string
	Description string
	Min         uint64
	Max         uint64
	Default     uint64
}

// ParamSpecs is the authoritative whitelist of governable parameters.
var ParamSpecs = []ParamSpec{
	{"block_reward_uz", "Native tokens minted per block and distributed to bonded stake", 0, 100_000 * CoinScale, 2 * CoinScale},
	{"min_validator_stake_uz", "Minimum self-bond required for a validator to enter the active set", 1 * CoinScale, 10_000_000 * CoinScale, 1000 * CoinScale},
	{"unbonding_blocks", "Blocks between an undelegation and release of funds", 1, 10_000_000, 100},
	{"voting_period_blocks", "Length of a governance voting period", 10, 10_000_000, 300},
	{"min_proposal_deposit_uz", "Deposit required to open a governance proposal", 0, 100_000_000 * CoinScale, 100 * CoinScale},
	{"max_active_validators", "Maximum size of the active validator set", 1, MaxValidators, 21},
	{"block_gas_limit", "Total gas available per block", 100_000, 10_000_000_000, 10_000_000},
	{"base_tx_gas", "Intrinsic gas charged for any transaction", 1000, 10_000_000, 21_000},
	{"gas_per_byte_uz", "Extra gas charged per byte of transaction payload", 0, 100_000, 16},
	{"min_gas_price_uz", "Minimum gas price accepted into the mempool", 0, 1_000_000 * CoinScale, 1},
}

// ParamByKey looks up a parameter specification.
func ParamByKey(key string) (ParamSpec, bool) {
	for _, s := range ParamSpecs {
		if s.Key == key {
			return s, true
		}
	}
	return ParamSpec{}, false
}

// Params is the chain's parameter set. Every field is a governable parameter.
//
// NOTE ON STATUS: the default values are *proposals* for a development network.
// They are not approved economic policy. See
// specifications/economics/TOKENOMICS_SPECIFICATION.md (§ Status of parameters).
type Params struct {
	BlockRewardUZ        uint64
	MinValidatorStakeUZ  uint64
	UnbondingBlocks      uint64
	VotingPeriodBlocks   uint64
	MinProposalDepositUZ uint64
	MaxActiveValidators  uint64
	BlockGasLimit        uint64
	BaseTxGas            uint64
	GasPerByteUZ         uint64
	MinGasPriceUZ        uint64
}

// DefaultParams returns the development-network defaults.
func DefaultParams() Params {
	p := Params{}
	for _, s := range ParamSpecs {
		_ = p.Set(s.Key, s.Default)
	}
	return p
}

// Get returns the value of a parameter by key.
func (p *Params) Get(key string) (uint64, bool) {
	switch key {
	case "block_reward_uz":
		return p.BlockRewardUZ, true
	case "min_validator_stake_uz":
		return p.MinValidatorStakeUZ, true
	case "unbonding_blocks":
		return p.UnbondingBlocks, true
	case "voting_period_blocks":
		return p.VotingPeriodBlocks, true
	case "min_proposal_deposit_uz":
		return p.MinProposalDepositUZ, true
	case "max_active_validators":
		return p.MaxActiveValidators, true
	case "block_gas_limit":
		return p.BlockGasLimit, true
	case "base_tx_gas":
		return p.BaseTxGas, true
	case "gas_per_byte_uz":
		return p.GasPerByteUZ, true
	case "min_gas_price_uz":
		return p.MinGasPriceUZ, true
	}
	return 0, false
}

// Set validates and assigns a parameter.
func (p *Params) Set(key string, v uint64) error {
	spec, ok := ParamByKey(key)
	if !ok {
		return fmt.Errorf("%w: unknown parameter %q", ErrParamInvalid, key)
	}
	if v < spec.Min || v > spec.Max {
		return fmt.Errorf("%w: %s=%d outside [%d,%d]", ErrParamInvalid, key, v, spec.Min, spec.Max)
	}
	switch key {
	case "block_reward_uz":
		p.BlockRewardUZ = v
	case "min_validator_stake_uz":
		p.MinValidatorStakeUZ = v
	case "unbonding_blocks":
		p.UnbondingBlocks = v
	case "voting_period_blocks":
		p.VotingPeriodBlocks = v
	case "min_proposal_deposit_uz":
		p.MinProposalDepositUZ = v
	case "max_active_validators":
		p.MaxActiveValidators = v
	case "block_gas_limit":
		p.BlockGasLimit = v
	case "base_tx_gas":
		p.BaseTxGas = v
	case "gas_per_byte_uz":
		p.GasPerByteUZ = v
	case "min_gas_price_uz":
		p.MinGasPriceUZ = v
	}
	return nil
}

// Validate checks every field against its bounds and cross-field constraints.
func (p *Params) Validate() error {
	for _, s := range ParamSpecs {
		v, ok := p.Get(s.Key)
		if !ok {
			return fmt.Errorf("%w: missing %s", ErrParamInvalid, s.Key)
		}
		if v < s.Min || v > s.Max {
			return fmt.Errorf("%w: %s=%d outside [%d,%d]", ErrParamInvalid, s.Key, v, s.Min, s.Max)
		}
	}
	if p.BlockRewardUZ > 0 && p.BlockRewardUZ > MaxSupply {
		return fmt.Errorf("%w: block reward above max supply", ErrParamInvalid)
	}
	if p.BlockGasLimit < p.BaseTxGas {
		return fmt.Errorf("%w: block gas limit below base tx gas", ErrParamInvalid)
	}
	return nil
}

// Keys returns the parameter keys in sorted order (stable output for hashing).
func ParamKeys() []string {
	keys := make([]string, 0, len(ParamSpecs))
	for _, s := range ParamSpecs {
		keys = append(keys, s.Key)
	}
	sort.Strings(keys)
	return keys
}

// Encode serializes params in sorted-key order so the encoding is canonical.
func (p *Params) Encode() []byte {
	w := zencoding.NewWriter()
	keys := ParamKeys()
	w.U8(0x01)
	w.U32(uint32(len(keys)))
	for _, k := range keys {
		v, _ := p.Get(k)
		w.Str(k)
		w.U64(v)
	}
	return w.Done()
}

// DecodeParams parses a canonical params encoding.
func DecodeParams(b []byte) (*Params, error) {
	r := zencoding.NewReader(b)
	if r.U8() != 0x01 {
		return nil, fmt.Errorf("%w: params version", ErrParamInvalid)
	}
	n := r.Len32(12)
	if r.Err() != nil || n > len(ParamSpecs) {
		return nil, fmt.Errorf("%w: params count", ErrParamInvalid)
	}
	p := &Params{}
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		k := r.Str()
		v := r.U64()
		if r.Err() != nil {
			return nil, fmt.Errorf("%w: %v", ErrParamInvalid, r.Err())
		}
		if seen[k] {
			return nil, fmt.Errorf("%w: duplicate parameter %q", ErrParamInvalid, k)
		}
		seen[k] = true
		if err := p.Set(k, v); err != nil {
			return nil, err
		}
	}
	if err := r.Done(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParamInvalid, err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}
