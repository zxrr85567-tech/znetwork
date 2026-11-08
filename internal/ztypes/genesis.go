package ztypes

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"znetwork/internal/zcrypto"
	"znetwork/internal/zencoding"
)

// GenesisAccount is one allocated balance in the genesis file.
type GenesisAccount struct {
	Address string `json:"address"`
	Balance uint64 `json:"balance_uz"`
}

// GenesisValidator is one genesis validator. Its self-bond is deducted from the
// validator account's allocated balance during InitChain, so the total supply is
// exactly the sum of allocations and no tokens are created out of thin air.
type GenesisValidator struct {
	PublicKey     string `json:"public_key"`
	Moniker       string `json:"moniker"`
	CommissionBps uint16 `json:"commission_bps"`
	SelfBond      uint64 `json:"self_bond_uz"`
}

// Genesis is the deterministic chain origin.
//
// The genesis hash commits to: protocol version, chain id, genesis time, the full
// parameter set, every allocation (sorted by address), and every validator
// (sorted by operator address). Given the same JSON input, the hash is identical
// on every machine — this is verified by TestGenesisHashReproducible and by the
// CLI (scripts/gen-genesis.sh) in CI.
type Genesis struct {
	Version     uint16             `json:"version"`
	ChainID     string             `json:"chain_id"`
	TimeMs      int64              `json:"time_ms"`
	Params      Params             `json:"-"`
	ParamsJSON  map[string]uint64  `json:"params"`
	Alloc       []GenesisAccount   `json:"alloc"`
	Validators  []GenesisValidator `json:"validators"`
	Description string             `json:"description,omitempty"`
}

// resolvedGenesis is the normalized form used for hashing: sorted, validated, and
// with addresses in raw bytes.
type resolvedGenesis struct {
	Version  uint16
	ChainID  string
	TimeMs   int64
	Params   *Params
	Accounts []struct {
		Addr    Address
		Balance uint64
	}
	Validators []struct {
		Operator      Address
		PubKey        PublicKey
		Moniker       string
		CommissionBps uint16
		SelfBond      uint64
	}
}

// Resolve validates the genesis document and normalizes it.
func (g *Genesis) Resolve() (*resolvedGenesis, error) {
	if g.Version != ProtocolVersion {
		return nil, fmt.Errorf("%w: version %d (want %d)", ErrGenesisInvalid, g.Version, ProtocolVersion)
	}
	if len(g.ChainID) == 0 || len(g.ChainID) > MaxChainIDLen || !validChainID(g.ChainID) {
		return nil, fmt.Errorf("%w: chain id %q must match ^[a-z0-9][a-z0-9-]*$ (1..64)", ErrGenesisInvalid, g.ChainID)
	}
	if g.TimeMs <= 0 {
		return nil, fmt.Errorf("%w: genesis time must be positive", ErrGenesisInvalid)
	}
	params := DefaultParams()
	for k, v := range g.ParamsJSON {
		if err := params.Set(k, v); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrGenesisInvalid, err)
		}
	}
	if err := params.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGenesisInvalid, err)
	}

	rg := &resolvedGenesis{Version: g.Version, ChainID: g.ChainID, TimeMs: g.TimeMs, Params: &params}

	seen := make(map[Address]bool, len(g.Alloc))
	var total uint64
	for i, a := range g.Alloc {
		addr, err := zcrypto.ParseAddressText(a.Address)
		if err != nil {
			return nil, fmt.Errorf("%w: alloc[%d]: %v", ErrGenesisInvalid, i, err)
		}
		if seen[addr] {
			return nil, fmt.Errorf("%w: duplicate allocation for %s", ErrGenesisInvalid, a.Address)
		}
		seen[addr] = true
		if a.Balance == 0 {
			return nil, fmt.Errorf("%w: alloc[%d] has zero balance", ErrGenesisInvalid, i)
		}
		var overflow bool
		total, overflow = addOverflow(total, a.Balance)
		if overflow || total > MaxSupply {
			return nil, fmt.Errorf("%w: total allocation exceeds max supply", ErrGenesisInvalid)
		}
		rg.Accounts = append(rg.Accounts, struct {
			Addr    Address
			Balance uint64
		}{addr, a.Balance})
	}
	if len(rg.Accounts) == 0 {
		return nil, fmt.Errorf("%w: no allocated accounts", ErrGenesisInvalid)
	}
	// Canonical order: ascending address bytes.
	sort.Slice(rg.Accounts, func(i, j int) bool {
		return bytesLess(rg.Accounts[i].Addr, rg.Accounts[j].Addr)
	})

	balanceOf := make(map[Address]uint64, len(rg.Accounts))
	for _, a := range rg.Accounts {
		balanceOf[a.Addr] = a.Balance
	}
	pubSeen := make(map[PublicKey]bool, len(g.Validators))
	opSeen := make(map[Address]bool, len(g.Validators))
	for i, v := range g.Validators {
		pub, err := zcrypto.ParsePublicKey(v.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("%w: validator[%d]: %v", ErrGenesisInvalid, i, err)
		}
		if pubSeen[pub] {
			return nil, fmt.Errorf("%w: duplicate validator key", ErrGenesisInvalid)
		}
		pubSeen[pub] = true
		op := pub.Address()
		if opSeen[op] {
			return nil, fmt.Errorf("%w: duplicate validator operator", ErrGenesisInvalid)
		}
		opSeen[op] = true
		if len(v.Moniker) == 0 || len(v.Moniker) > MaxMonikerLen {
			return nil, fmt.Errorf("%w: validator[%d] moniker length", ErrGenesisInvalid, i)
		}
		if v.CommissionBps > 10000 {
			return nil, fmt.Errorf("%w: validator[%d] commission > 100%%", ErrGenesisInvalid, i)
		}
		if v.SelfBond < params.MinValidatorStakeUZ {
			return nil, fmt.Errorf("%w: validator[%d] self bond %d below minimum %d", ErrGenesisInvalid, i, v.SelfBond, params.MinValidatorStakeUZ)
		}
		if balanceOf[op] < v.SelfBond {
			return nil, fmt.Errorf("%w: validator[%d] allocation cannot cover its self bond", ErrGenesisInvalid, i)
		}
		rg.Validators = append(rg.Validators, struct {
			Operator      Address
			PubKey        PublicKey
			Moniker       string
			CommissionBps uint16
			SelfBond      uint64
		}{op, pub, v.Moniker, v.CommissionBps, v.SelfBond})
	}
	if len(rg.Validators) == 0 {
		return nil, fmt.Errorf("%w: at least one validator is required", ErrGenesisInvalid)
	}
	if uint64(len(rg.Validators)) > params.MaxActiveValidators {
		return nil, fmt.Errorf("%w: %d validators exceeds max_active_validators=%d", ErrGenesisInvalid, len(rg.Validators), params.MaxActiveValidators)
	}
	sort.Slice(rg.Validators, func(i, j int) bool {
		return bytesLess(rg.Validators[i].Operator, rg.Validators[j].Operator)
	})
	return rg, nil
}

// Validate reports whether the genesis document is acceptable.
func (g *Genesis) Validate() error {
	_, err := g.Resolve()
	return err
}

// Encode returns the canonical genesis encoding used for hashing.
func (g *Genesis) Encode() ([]byte, error) {
	rg, err := g.Resolve()
	if err != nil {
		return nil, err
	}
	w := zencoding.NewWriterSize(1024)
	w.U8(0x01)
	w.U16(rg.Version)
	w.Str(rg.ChainID)
	w.I64(rg.TimeMs)
	pb := rg.Params.Encode()
	w.Bytes(pb)
	w.U32(uint32(len(rg.Accounts)))
	for _, a := range rg.Accounts {
		w.Raw(a.Addr[:])
		w.U64(a.Balance)
	}
	w.U32(uint32(len(rg.Validators)))
	for _, v := range rg.Validators {
		w.Raw(v.Operator[:])
		w.Raw(v.PubKey[:])
		w.Str(v.Moniker)
		w.U16(v.CommissionBps)
		w.U64(v.SelfBond)
	}
	return w.Done(), nil
}

// Hash returns the genesis hash: Digest("ZNET/v1/genesis", encode(genesis)).
func (g *Genesis) Hash() (Hash, error) {
	b, err := g.Encode()
	if err != nil {
		return Hash{}, err
	}
	return zcrypto.Digest(domainGenesis, b), nil
}

// TotalAllocated returns the sum of all allocations.
func (g *Genesis) TotalAllocated() (uint64, error) {
	rg, err := g.Resolve()
	if err != nil {
		return 0, err
	}
	var total uint64
	for _, a := range rg.Accounts {
		var ov bool
		total, ov = addOverflow(total, a.Balance)
		if ov {
			return 0, fmt.Errorf("%w: allocation overflow", ErrGenesisInvalid)
		}
	}
	return total, nil
}

// ParseGenesisJSON parses a genesis document, rejecting unknown fields so that a
// typo in a genesis file can never silently fall back to a default.
func ParseGenesisJSON(b []byte) (*Genesis, error) {
	var raw struct {
		Version     uint16             `json:"version"`
		ChainID     string             `json:"chain_id"`
		TimeMs      int64              `json:"time_ms"`
		Params      map[string]uint64  `json:"params"`
		Alloc       []GenesisAccount   `json:"alloc"`
		Validators  []GenesisValidator `json:"validators"`
		Description string             `json:"description"`
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGenesisInvalid, err)
	}
	g := &Genesis{
		Version:     raw.Version,
		ChainID:     raw.ChainID,
		TimeMs:      raw.TimeMs,
		ParamsJSON:  raw.Params,
		Alloc:       raw.Alloc,
		Validators:  raw.Validators,
		Description: raw.Description,
	}
	if err := g.Validate(); err != nil {
		return nil, err
	}
	return g, nil
}

// MarshalGenesisJSON renders the genesis document as indented JSON.
func (g *Genesis) MarshalGenesisJSON() ([]byte, error) {
	rg, err := g.Resolve()
	if err != nil {
		return nil, err
	}
	params := make(map[string]uint64, len(ParamSpecs))
	for _, s := range ParamSpecs {
		v, _ := rg.Params.Get(s.Key)
		params[s.Key] = v
	}
	out := struct {
		Version     uint16             `json:"version"`
		ChainID     string             `json:"chain_id"`
		TimeMs      int64              `json:"time_ms"`
		Params      map[string]uint64  `json:"params"`
		Alloc       []GenesisAccount   `json:"alloc"`
		Validators  []GenesisValidator `json:"validators"`
		Description string             `json:"description,omitempty"`
	}{g.Version, g.ChainID, g.TimeMs, params, nil, nil, g.Description}
	for _, a := range rg.Accounts {
		out.Alloc = append(out.Alloc, GenesisAccount{Address: a.Addr.Bech32(), Balance: a.Balance})
	}
	for _, v := range rg.Validators {
		out.Validators = append(out.Validators, GenesisValidator{
			PublicKey: v.PubKey.String(), Moniker: v.Moniker, CommissionBps: v.CommissionBps, SelfBond: v.SelfBond,
		})
	}
	return json.MarshalIndent(out, "", "  ")
}

func validChainID(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			continue
		}
		if c >= '0' && c <= '9' {
			continue
		}
		if c == '-' && i > 0 {
			continue
		}
		return false
	}
	return true
}

func bytesLess(a, b Address) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// addOverflow adds two uint64 with overflow detection.
func addOverflow(a, b uint64) (uint64, bool) {
	s := a + b
	return s, s < a
}

// AddU64 adds with overflow detection (exported for execution code).
func AddU64(a, b uint64) (uint64, bool) { return addOverflow(a, b) }

// MulU64 multiplies with overflow detection.
func MulU64(a, b uint64) (uint64, bool) {
	if a == 0 || b == 0 {
		return 0, false
	}
	p := a * b
	return p, p/b != a
}
