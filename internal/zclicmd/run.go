// Package zclicmd implements zcli: chain inspection, genesis tooling and local
// network management.
package zclicmd

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"znetwork/internal/zcrypto"
	"znetwork/internal/zdevnet"
	"znetwork/internal/zsdk"
	"znetwork/internal/ztypes"
)

// Version is the CLI version.
const Version = "1.0.0"

// Main is the entry point.
func Main(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "status":
		return status(rest)
	case "block":
		return block(rest)
	case "blocks":
		return blocks(rest)
	case "tx":
		return tx(rest)
	case "account":
		return account(rest)
	case "validators":
		return validators(rest)
	case "delegations":
		return delegations(rest)
	case "proposals":
		return proposals(rest)
	case "params":
		return params(rest)
	case "supply":
		return supply(rest)
	case "mempool":
		return mempool(rest)
	case "decode":
		return decode(rest)
	case "genesis":
		return genesisCmd(rest)
	case "devnet":
		return devnetCmd(rest)
	case "keygen":
		return keygen(rest)
	case "version", "--version":
		fmt.Printf("zcli %s (protocol %d)\n", Version, ztypes.ProtocolVersion)
		return 0
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "zcli: unknown command %q\n", cmd)
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `zcli — Z Network command line tools

Chain inspection (requires --rpc):
  zcli status     --rpc URL
  zcli block      --rpc URL --height N | --hash HEX
  zcli blocks     --rpc URL [--from N] [--limit N]
  zcli tx         --rpc URL --id HEX
  zcli account    --rpc URL --address z1...
  zcli validators --rpc URL
  zcli delegations --rpc URL --address z1...
  zcli proposals  --rpc URL
  zcli params     --rpc URL
  zcli supply     --rpc URL
  zcli mempool    --rpc URL

Offline tools:
  zcli decode     --tx-hex HEX            decode a transaction
  zcli genesis hash --file genesis.json   print the genesis hash
  zcli genesis check --file genesis.json  validate a genesis file
  zcli devnet     --out DIR [--validators N] [--users N] [--chain-id ID]
                                          write a deterministic devnet genesis + test keys
  zcli keygen     [--label NAME]          print a test key (TEST ONLY)

All read commands print JSON so they compose with jq.
`)
}

func rpcURL(v string) string {
	if v != "" {
		return v
	}
	return os.Getenv("ZWALLET_RPC")
}

func dial(url string) (*zsdk.Client, int) {
	u := rpcURL(url)
	if u == "" {
		fmt.Fprintln(os.Stderr, "zcli: --rpc is required (or set ZWALLET_RPC)")
		return nil, 2
	}
	c, err := zsdk.Dial(u)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zcli:", err)
		return nil, 1
	}
	return c, 0
}

func printJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, "zcli:", err)
		return 1
	}
	return 0
}

func status(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	rpc := fs.String("rpc", "", "RPC URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c, code := dial(*rpc)
	if c == nil {
		return code
	}
	info, err := c.ChainInfo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "zcli status:", err)
		return 1
	}
	supply, err := c.Supply()
	if err != nil {
		fmt.Fprintln(os.Stderr, "zcli status:", err)
		return 1
	}
	return printJSON(map[string]any{"chain": info, "supply": supply})
}

func block(args []string) int {
	fs := flag.NewFlagSet("block", flag.ContinueOnError)
	rpc := fs.String("rpc", "", "RPC URL")
	height := fs.Uint64("height", 0, "block height")
	hash := fs.String("hash", "", "block hash")
	useHeight := false
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "height" {
			useHeight = true
		}
	})
	c, code := dial(*rpc)
	if c == nil {
		return code
	}
	var (
		res map[string]any
		err error
	)
	if *hash != "" {
		res, err = c.BlockByHash(*hash)
	} else if useHeight {
		res, err = c.Block(*height)
	} else {
		h, herr := c.Height()
		if herr != nil {
			fmt.Fprintln(os.Stderr, "zcli block:", herr)
			return 1
		}
		res, err = c.Block(h)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "zcli block:", err)
		return 1
	}
	return printJSON(res)
}

func blocks(args []string) int {
	fs := flag.NewFlagSet("blocks", flag.ContinueOnError)
	rpc := fs.String("rpc", "", "RPC URL")
	from := fs.Uint64("from", 0, "first height")
	limit := fs.Uint64("limit", 10, "count")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c, code := dial(*rpc)
	if c == nil {
		return code
	}
	res, err := c.Blocks(*from, *limit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zcli blocks:", err)
		return 1
	}
	return printJSON(res)
}

func tx(args []string) int {
	fs := flag.NewFlagSet("tx", flag.ContinueOnError)
	rpc := fs.String("rpc", "", "RPC URL")
	id := fs.String("id", "", "transaction id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *id == "" {
		fmt.Fprintln(os.Stderr, "zcli tx: --id is required")
		return 2
	}
	c, code := dial(*rpc)
	if c == nil {
		return code
	}
	res, err := c.Transaction(*id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zcli tx:", err)
		return 1
	}
	return printJSON(res)
}

func account(args []string) int {
	fs := flag.NewFlagSet("account", flag.ContinueOnError)
	rpc := fs.String("rpc", "", "RPC URL")
	addr := fs.String("address", "", "address")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *addr == "" {
		fmt.Fprintln(os.Stderr, "zcli account: --address is required")
		return 2
	}
	c, code := dial(*rpc)
	if c == nil {
		return code
	}
	acc, err := c.Account(*addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zcli account:", err)
		return 1
	}
	return printJSON(acc)
}

func validators(args []string) int {
	fs := flag.NewFlagSet("validators", flag.ContinueOnError)
	rpc := fs.String("rpc", "", "RPC URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c, code := dial(*rpc)
	if c == nil {
		return code
	}
	res, err := c.Validators()
	if err != nil {
		fmt.Fprintln(os.Stderr, "zcli validators:", err)
		return 1
	}
	return printJSON(res)
}

func proposals(args []string) int {
	fs := flag.NewFlagSet("proposals", flag.ContinueOnError)
	rpc := fs.String("rpc", "", "RPC URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c, code := dial(*rpc)
	if c == nil {
		return code
	}
	res, err := c.Proposals()
	if err != nil {
		fmt.Fprintln(os.Stderr, "zcli proposals:", err)
		return 1
	}
	return printJSON(res)
}

// delegations prints every delegation held by an address, including unbonding
// entries with their release height.
func delegations(args []string) int {
	fs := flag.NewFlagSet("delegations", flag.ContinueOnError)
	rpc := fs.String("rpc", "", "RPC URL")
	address := fs.String("address", "", "delegator address")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *address == "" {
		fmt.Fprintln(os.Stderr, "zcli delegations: --address is required")
		return 2
	}
	c, code := dial(*rpc)
	if c == nil {
		return code
	}
	res, err := c.Delegations(*address)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zcli delegations:", err)
		return 1
	}
	return printJSON(res)
}

func params(args []string) int {
	fs := flag.NewFlagSet("params", flag.ContinueOnError)
	rpc := fs.String("rpc", "", "RPC URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c, code := dial(*rpc)
	if c == nil {
		return code
	}
	res, err := c.Params()
	if err != nil {
		fmt.Fprintln(os.Stderr, "zcli params:", err)
		return 1
	}
	return printJSON(res)
}

func supply(args []string) int {
	fs := flag.NewFlagSet("supply", flag.ContinueOnError)
	rpc := fs.String("rpc", "", "RPC URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c, code := dial(*rpc)
	if c == nil {
		return code
	}
	res, err := c.Supply()
	if err != nil {
		fmt.Fprintln(os.Stderr, "zcli supply:", err)
		return 1
	}
	return printJSON(res)
}

func mempool(args []string) int {
	fs := flag.NewFlagSet("mempool", flag.ContinueOnError)
	rpc := fs.String("rpc", "", "RPC URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c, code := dial(*rpc)
	if c == nil {
		return code
	}
	res, err := c.Mempool()
	if err != nil {
		fmt.Fprintln(os.Stderr, "zcli mempool:", err)
		return 1
	}
	return printJSON(res)
}

func decode(args []string) int {
	fs := flag.NewFlagSet("decode", flag.ContinueOnError)
	txHex := fs.String("tx-hex", "", "transaction hex")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	tx, err := zsdk.DecodeTx(*txHex)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zcli decode:", err)
		return 2
	}
	fee, _ := tx.Fee()
	return printJSON(map[string]any{
		"id":           tx.ID().String(),
		"chain_id":     tx.ChainID,
		"type":         uint8(tx.Type),
		"sender":       tx.Sender.Bech32(),
		"nonce":        tx.Nonce,
		"gas_limit":    tx.GasLimit,
		"gas_price":    tx.GasPrice,
		"max_fee_uz":   fee,
		"time_ms":      tx.TimeMs,
		"memo":         tx.Memo,
		"payload_hex":  hex.EncodeToString(tx.Payload),
		"signature_ok": func() bool { return tx.VerifySignature() == nil }(),
	})
}

func genesisCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "zcli genesis: expected 'hash' or 'check'")
		return 2
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("genesis "+sub, flag.ContinueOnError)
	file := fs.String("file", "", "genesis JSON file")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if *file == "" {
		fmt.Fprintln(os.Stderr, "zcli genesis: --file is required")
		return 2
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zcli genesis:", err)
		return 1
	}
	g, err := ztypes.ParseGenesisJSON(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zcli genesis:", err)
		return 1
	}
	switch sub {
	case "hash":
		h, err := g.Hash()
		if err != nil {
			fmt.Fprintln(os.Stderr, "zcli genesis:", err)
			return 1
		}
		total, err := g.TotalAllocated()
		if err != nil {
			fmt.Fprintln(os.Stderr, "zcli genesis:", err)
			return 1
		}
		return printJSON(map[string]any{
			"chain_id":       g.ChainID,
			"genesis_hash":   h.String(),
			"total_alloc_uz": total,
			"total_alloc_z":  ztypes.FormatUZ(total),
		})
	case "check":
		return printJSON(map[string]any{"valid": true, "chain_id": g.ChainID,
			"accounts": len(g.Alloc), "validators": len(g.Validators)})
	default:
		fmt.Fprintf(os.Stderr, "zcli genesis: unknown subcommand %q\n", sub)
		return 2
	}
}

// devnetCmd writes a deterministic development network: genesis.json plus the
// encrypted keystore of every node, so a local multi-node network can be brought
// up with the documented scripts.
func devnetCmd(args []string) int {
	fs := flag.NewFlagSet("devnet", flag.ContinueOnError)
	out := fs.String("out", "", "output directory")
	validators := fs.Int("validators", 4, "number of validators")
	users := fs.Int("users", 2, "number of funded users")
	chainID := fs.String("chain-id", "znetwork-devnet-1", "chain id")
	passwordFile := fs.String("password-file", "", "password file for the generated keystores")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "zcli devnet: --out is required")
		return 2
	}
	pw := os.Getenv("ZWALLET_PASSWORD")
	if pw == "" && *passwordFile != "" {
		b, err := os.ReadFile(*passwordFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "zcli devnet:", err)
			return 1
		}
		pw = strings.TrimRight(string(b), "\r\n")
	}
	if pw == "" {
		fmt.Fprintln(os.Stderr, "zcli devnet: set ZWALLET_PASSWORD (or --password-file) so the test keys are encrypted")
		return 2
	}
	net, err := zdevnet.New(zdevnet.Options{
		ChainID: *chainID, TimeMs: 1_700_000_000_000,
		Validators: *validators, Users: *users,
		BalanceUZ: 10_000 * ztypes.CoinScale, SelfBondUZ: 1_000 * ztypes.CoinScale,
		UserBalanceUZ: 500 * ztypes.CoinScale, FaucetUZ: 1_000_000 * ztypes.CoinScale,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "zcli devnet:", err)
		return 1
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "zcli devnet:", err)
		return 1
	}
	gj, err := net.Genesis.MarshalGenesisJSON()
	if err != nil {
		fmt.Fprintln(os.Stderr, "zcli devnet:", err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(*out, "genesis.json"), gj, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "zcli devnet:", err)
		return 1
	}
	keysDir := filepath.Join(*out, "keys")
	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "zcli devnet:", err)
		return 1
	}
	type nodeInfo struct {
		Name    string `json:"name"`
		Address string `json:"address"`
		Key     string `json:"keystore"`
	}
	var info []nodeInfo
	for _, nd := range net.Nodes {
		ks, err := zcrypto.EncryptKey(nd.Key, pw, 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, "zcli devnet:", err)
			return 1
		}
		path := filepath.Join(keysDir, nd.Name+".key.json")
		if err := zcrypto.SaveKeystore(path, ks); err != nil {
			fmt.Fprintln(os.Stderr, "zcli devnet:", err)
			return 1
		}
		info = append(info, nodeInfo{Name: nd.Name, Address: nd.Address.Bech32(), Key: path})
	}
	faucetKS, err := zcrypto.EncryptKey(net.Faucet, pw, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zcli devnet:", err)
		return 1
	}
	faucetPath := filepath.Join(keysDir, "faucet.key.json")
	if err := zcrypto.SaveKeystore(faucetPath, faucetKS); err != nil {
		fmt.Fprintln(os.Stderr, "zcli devnet:", err)
		return 1
	}
	for _, u := range net.Users {
		ks, err := zcrypto.EncryptKey(u.Key, pw, 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, "zcli devnet:", err)
			return 1
		}
		path := filepath.Join(keysDir, u.Name+".key.json")
		if err := zcrypto.SaveKeystore(path, ks); err != nil {
			fmt.Fprintln(os.Stderr, "zcli devnet:", err)
			return 1
		}
		info = append(info, nodeInfo{Name: u.Name, Address: u.Address.Bech32(), Key: path})
	}
	gh, err := net.Genesis.Hash()
	if err != nil {
		fmt.Fprintln(os.Stderr, "zcli devnet:", err)
		return 1
	}
	manifest := map[string]any{
		"chain_id":     net.ChainID,
		"genesis_hash": gh.String(),
		"genesis_file": filepath.Join(*out, "genesis.json"),
		"faucet":       map[string]string{"address": net.FaucetAddr.Bech32(), "keystore": faucetPath},
		"accounts":     info,
		"warning":      "TEST NETWORK ONLY: keys are derived deterministically and are worthless",
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	}
	mj, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(*out, "devnet.json"), mj, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "zcli devnet:", err)
		return 1
	}
	return printJSON(manifest)
}

func keygen(args []string) int {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	label := fs.String("label", "", "deterministic label (omit for a random key)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var (
		key *zcrypto.PrivateKey
		err error
	)
	if *label != "" {
		key, err = zdevnet.DeterministicKey(*label)
	} else {
		key, err = zcrypto.GenerateKey()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "zcli keygen:", err)
		return 1
	}
	return printJSON(map[string]any{
		"address":    key.Address().Bech32(),
		"public_key": key.PublicKey().String(),
		"seed_hex":   hex.EncodeToString(key.Seed()),
		"warning":    "never use this output as a production key; store keys encrypted via zwallet generate",
	})
}

var _ = errors.Is
