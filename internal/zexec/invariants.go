package zexec

import (
	"fmt"

	"znetwork/internal/zstate"
	"znetwork/internal/ztypes"
)

// Accounting is the auditable breakdown of where every token is.
type Accounting struct {
	// Supply is the recorded total supply (minted minus nothing: v1 has no burn).
	Supply uint64 `json:"supply_uz"`
	// Liquid is the sum of all account balances (including the treasury account
	// and contract accounts).
	Liquid uint64 `json:"liquid_uz"`
	// Bonded is the sum of all validator voting power (self bonds + delegations).
	Bonded uint64 `json:"bonded_uz"`
	// Unbonding is the sum of all pending unbonding entries.
	Unbonding uint64 `json:"unbonding_uz"`
}

// Diff returns (liquid+bonded+unbonding) - supply. Zero means the books balance.
func (a Accounting) Diff() int64 {
	sum := a.Liquid + a.Bonded + a.Unbonding
	return int64(sum) - int64(a.Supply)
}

// Account traverses the whole state and computes the accounting breakdown.
//
// This is the function the node uses at the end of every block when invariant
// checking is enabled, the tests use to assert conservation, and the RPC uses to
// answer z_getSupply. Because it is the single implementation, a divergence
// between the three cannot hide.
func Account(st *zstate.State) (Accounting, error) {
	var a Accounting
	a.Supply = st.Supply()

	for _, k := range st.KeysWithPrefix([]byte{0x01}) { // account class
		v, _ := st.Get(k)
		acc, err := ztypes.DecodeAccount(v)
		if err != nil {
			return a, fmt.Errorf("zexec: decoding account %x: %w", k, err)
		}
		next, ovf := ztypes.AddU64(a.Liquid, acc.Balance)
		if ovf {
			return a, fmt.Errorf("%w: liquid balance sum overflow", ztypes.ErrStateInvalid)
		}
		a.Liquid = next
	}

	vals, err := st.Validators()
	if err != nil {
		return a, err
	}
	for _, v := range vals {
		next, ovf := ztypes.AddU64(a.Bonded, v.VotingPower())
		if ovf {
			return a, fmt.Errorf("%w: bonded sum overflow", ztypes.ErrStateInvalid)
		}
		a.Bonded = next
	}

	dels, err := st.Delegations()
	if err != nil {
		return a, err
	}
	for _, d := range dels {
		for _, u := range d.Unbonding {
			next, ovf := ztypes.AddU64(a.Unbonding, u.Amount)
			if ovf {
				return a, fmt.Errorf("%w: unbonding sum overflow", ztypes.ErrStateInvalid)
			}
			a.Unbonding = next
		}
	}
	return a, nil
}

// VerifySupply asserts the conservation invariant and returns a descriptive error.
func VerifySupply(st *zstate.State) error {
	a, err := Account(st)
	if err != nil {
		return err
	}
	if a.Diff() != 0 {
		return fmt.Errorf("%w: supply %d != liquid %d + bonded %d + unbonding %d (diff %d)",
			ztypes.ErrStateInvalid, a.Supply, a.Liquid, a.Bonded, a.Unbonding, a.Diff())
	}
	// Delegation amounts must agree with the validator aggregates they feed.
	vals, err := st.Validators()
	if err != nil {
		return err
	}
	dels, err := st.Delegations()
	if err != nil {
		return err
	}
	aggSelf := map[ztypes.Address]uint64{}
	aggDel := map[ztypes.Address]uint64{}
	for _, d := range dels {
		if d.Delegator == d.Validator {
			aggSelf[d.Validator] = mustAdd(aggSelf[d.Validator], d.Amount)
		} else {
			aggDel[d.Validator] = mustAdd(aggDel[d.Validator], d.Amount)
		}
	}
	for _, v := range vals {
		if aggSelf[v.Operator] != v.SelfBond {
			return fmt.Errorf("%w: validator %s self bond %d != delegation sum %d",
				ztypes.ErrStateInvalid, v.Operator.Bech32(), v.SelfBond, aggSelf[v.Operator])
		}
		if aggDel[v.Operator] != v.Delegated {
			return fmt.Errorf("%w: validator %s delegated %d != delegation sum %d",
				ztypes.ErrStateInvalid, v.Operator.Bech32(), v.Delegated, aggDel[v.Operator])
		}
	}
	return nil
}
