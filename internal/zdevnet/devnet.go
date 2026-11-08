// Package zdevnet builds deterministic development networks.
//
// SECURITY NOTICE: the keys produced by this package are derived from public
// domain-separated strings. They are TEST KEYS. They are worthless for anything
// except a local development network, and every faucet/alloc they own is test
// money. Production keys must be generated with zcrypto.GenerateKey on a trusted
// machine and stored in an encrypted keystore (see the wallet guide).
package zdevnet

import (
	"fmt"

	"znetwork/internal/zcrypto"
	"znetwork/internal/ztypes"
)

// DevKeysDomain separates development key derivation from every protocol use.
const DevKeysDomain = "ZNET/v1/devnet-key"

// DeterministicKey derives a test key from a label.
func DeterministicKey(label string) (*zcrypto.PrivateKey, error) {
	d := zcrypto.Digest(DevKeysDomain, []byte(label))
	return zcrypto.KeyFromSeed(d.Bytes())
}

// Node describes one validator/account pair in a generated network.
type Node struct {
	Name     string
	Key      *zcrypto.PrivateKey
	Address  ztypes.Address
	Operator ztypes.Address
}

// Network is a generated genesis plus its keys.
type Network struct {
	ChainID    string
	Genesis    *ztypes.Genesis
	Nodes      []Node
	Faucet     *zcrypto.PrivateKey
	FaucetAddr ztypes.Address
	Users      []Node
}

// Options configures a generated network.
type Options struct {
	ChainID       string
	TimeMs        int64
	Validators    int
	Users         int
	BalanceUZ     uint64 // liquid balance per node (before self bond)
	SelfBondUZ    uint64
	UserBalanceUZ uint64
	FaucetUZ      uint64
	Params        map[string]uint64
	Description   string
}

// DefaultOptions are the development-network defaults, sized for fast tests.
func DefaultOptions() Options {
	return Options{
		ChainID:       "znetwork-devnet-1",
		TimeMs:        1_700_000_000_000,
		Validators:    4,
		Users:         2,
		BalanceUZ:     10_000 * ztypes.CoinScale,
		SelfBondUZ:    1_000 * ztypes.CoinScale,
		UserBalanceUZ: 500 * ztypes.CoinScale,
		FaucetUZ:      1_000_000 * ztypes.CoinScale,
		Description:   "Z Network deterministic development network (TEST ONLY)",
	}
}

// New generates a network.
func New(opt Options) (*Network, error) {
	if opt.Validators < 1 || opt.Validators > 64 {
		return nil, fmt.Errorf("zdevnet: validators must be 1..64")
	}
	if opt.Users < 0 || opt.Users > 64 {
		return nil, fmt.Errorf("zdevnet: users must be 0..64")
	}
	net := &Network{ChainID: opt.ChainID}
	g := &ztypes.Genesis{
		Version:     ztypes.ProtocolVersion,
		ChainID:     opt.ChainID,
		TimeMs:      opt.TimeMs,
		ParamsJSON:  map[string]uint64{},
		Description: opt.Description,
	}
	for k, v := range opt.Params {
		g.ParamsJSON[k] = v
	}
	// The minimum stake must be consistent with the self bond we allocate.
	if _, ok := g.ParamsJSON["min_validator_stake_uz"]; !ok && opt.SelfBondUZ > 0 {
		g.ParamsJSON["min_validator_stake_uz"] = opt.SelfBondUZ
	}

	for i := 0; i < opt.Validators; i++ {
		name := fmt.Sprintf("validator-%d", i+1)
		k, err := DeterministicKey("validator/" + name)
		if err != nil {
			return nil, err
		}
		n := Node{Name: name, Key: k, Address: k.Address(), Operator: k.PublicKey().Address()}
		net.Nodes = append(net.Nodes, n)
		g.Alloc = append(g.Alloc, ztypes.GenesisAccount{Address: n.Address.Bech32(), Balance: opt.BalanceUZ})
		g.Validators = append(g.Validators, ztypes.GenesisValidator{
			PublicKey:     k.PublicKey().String(),
			Moniker:       name,
			CommissionBps: 1000, // 10%
			SelfBond:      opt.SelfBondUZ,
		})
	}
	for i := 0; i < opt.Users; i++ {
		name := fmt.Sprintf("user-%d", i+1)
		k, err := DeterministicKey("user/" + name)
		if err != nil {
			return nil, err
		}
		n := Node{Name: name, Key: k, Address: k.Address()}
		net.Users = append(net.Users, n)
		g.Alloc = append(g.Alloc, ztypes.GenesisAccount{Address: n.Address.Bech32(), Balance: opt.UserBalanceUZ})
	}
	fk, err := DeterministicKey("faucet")
	if err != nil {
		return nil, err
	}
	net.Faucet = fk
	net.FaucetAddr = fk.Address()
	g.Alloc = append(g.Alloc, ztypes.GenesisAccount{Address: net.FaucetAddr.Bech32(), Balance: opt.FaucetUZ})

	if err := g.Validate(); err != nil {
		return nil, err
	}
	net.Genesis = g
	return net, nil
}
