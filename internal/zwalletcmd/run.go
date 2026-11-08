// Package zwalletcmd implements the zwallet command line: key management and
// transaction signing for Z Network.
//
// Security model:
//   - Private keys are only ever written encrypted (AES-256-GCM + PBKDF2, see the
//     keystore specification).
//   - Passwords come from a file or an environment variable, never from a command
//     line argument (process listings leak arguments).
//   - Keys are never transmitted: the wallet signs locally and sends only signed
//     bytes to the RPC endpoint.
//   - `zwallet generate` refuses to overwrite an existing keystore.
package zwalletcmd

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"znetwork/internal/zcrypto"
	"znetwork/internal/zsdk"
	"znetwork/internal/ztypes"
)

// Version is the wallet software version.
const Version = "1.0.0"

// Main is the entry point.
func Main(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "generate":
		return generate(rest)
	case "address":
		return address(rest)
	case "balance":
		return cmdBalance(rest)
	case "send":
		return send(rest)
	case "delegate":
		return delegate(rest)
	case "undelegate":
		return undelegate(rest)
	case "register-validator":
		return registerValidator(rest)
	case "vote":
		return vote(rest)
	case "propose":
		return propose(rest)
	case "deploy":
		return deploy(rest)
	case "call":
		return callContract(rest)
	case "sign":
		return signTx(rest)
	case "version", "--version":
		fmt.Printf("zwallet %s (protocol %d)\n", Version, ztypes.ProtocolVersion)
		return 0
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "zwallet: unknown command %q\n", cmd)
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `zwallet — Z Network wallet

Usage:
  zwallet generate   --key FILE [--password-file FILE | ZWALLET_PASSWORD]
  zwallet address    --key FILE [--password-file FILE]
  zwallet balance    --rpc URL --address z1...
  zwallet send       --rpc URL --key FILE --to z1... --amount 1.5 [--memo TEXT] [--wait]
  zwallet delegate   --rpc URL --key FILE --validator z1... --amount 1.5
  zwallet undelegate --rpc URL --key FILE --validator z1... --amount 1.5
  zwallet register-validator --rpc URL --key FILE --moniker NAME [--commission-bps 1000]
  zwallet propose    --rpc URL --key FILE --title T --description D [--param KEY --value N] [--deposit 100]
  zwallet vote       --rpc URL --key FILE --proposal ID --option yes|no|abstain
  zwallet deploy     --rpc URL --key FILE --asm-file FILE.asm [--wait]
  zwallet call       --rpc URL --key FILE --contract z1... [--input HEX] [--wait]
  zwallet sign       --key FILE --tx-hex HEX       (prints the signed transaction)

Amounts are decimal Z (1 Z = 100000000 uz). The wallet always signs locally;
private keys are never sent to any server.

Environment:
  ZWALLET_PASSWORD   keystore password (prefer this or --password-file)
  ZWALLET_RPC        default --rpc value
`)
}

// password resolves the keystore password.
func password(file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("reading password file: %w", err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	if p, ok := os.LookupEnv("ZWALLET_PASSWORD"); ok && p != "" {
		return p, nil
	}
	return "", errors.New("no password: use --password-file or ZWALLET_PASSWORD")
}

func loadKey(path, passwordFile string) (*zcrypto.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("--key is required")
	}
	ks, err := zcrypto.LoadKeystore(path)
	if err != nil {
		return nil, fmt.Errorf("loading keystore: %w", err)
	}
	pw, err := password(passwordFile)
	if err != nil {
		return nil, err
	}
	key, err := ks.Decrypt(pw)
	if err != nil {
		return nil, fmt.Errorf("decrypting keystore (wrong password?): %w", err)
	}
	return key, nil
}

func generate(args []string) int {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	path := fs.String("key", "", "keystore output path")
	pwFile := fs.String("password-file", "", "file containing the password")
	force := fs.Bool("force", false, "overwrite an existing keystore (dangerous)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *path == "" {
		fmt.Fprintln(os.Stderr, "zwallet generate: --key is required")
		return 2
	}
	if _, err := os.Stat(*path); err == nil && !*force {
		fmt.Fprintf(os.Stderr, "zwallet generate: %s exists; refusing to overwrite\n", *path)
		return 1
	}
	pw, err := password(*pwFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet generate:", err)
		return 2
	}
	key, err := zcrypto.GenerateKey()
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet generate:", err)
		return 1
	}
	ks, err := zcrypto.EncryptKey(key, pw, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet generate:", err)
		return 1
	}
	if err := zcrypto.SaveKeystore(*path, ks); err != nil {
		fmt.Fprintln(os.Stderr, "zwallet generate:", err)
		return 1
	}
	out, _ := json.MarshalIndent(map[string]string{
		"address":    key.Address().Bech32(),
		"public_key": key.PublicKey().String(),
		"keystore":   *path,
		"warning":    "test/development key material unless you generated it on a trusted machine",
	}, "", "  ")
	fmt.Println(string(out))
	return 0
}

func address(args []string) int {
	fs := flag.NewFlagSet("address", flag.ContinueOnError)
	keyPath := fs.String("key", "", "keystore path")
	pwFile := fs.String("password-file", "", "password file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	key, err := loadKey(*keyPath, *pwFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet address:", err)
		return 1
	}
	fmt.Printf("%s\n%s\n", key.Address().Bech32(), key.PublicKey().String())
	return 0
}

func rpcURL(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv("ZWALLET_RPC")
}

func parseAmount(s string) (uint64, error) {
	if s == "" {
		return 0, errors.New("--amount is required")
	}
	v, err := ztypes.ParseUZ(s)
	if err != nil {
		return 0, err
	}
	return v, nil
}

func client(url string) (*zsdk.Client, error) {
	if url == "" {
		return nil, errors.New("--rpc is required (or set ZWALLET_RPC)")
	}
	return zsdk.Dial(url)
}

func cmdBalance(args []string) int {
	fs := flag.NewFlagSet("balance", flag.ContinueOnError)
	rpc := fs.String("rpc", "", "RPC URL")
	addr := fs.String("address", "", "address")
	keyPath := fs.String("key", "", "keystore (alternative to --address)")
	pwFile := fs.String("password-file", "", "password file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	addressStr := *addr
	if addressStr == "" && *keyPath != "" {
		k, err := loadKey(*keyPath, *pwFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "zwallet balance:", err)
			return 1
		}
		addressStr = k.Address().Bech32()
	}
	if addressStr == "" {
		fmt.Fprintln(os.Stderr, "zwallet balance: --address or --key is required")
		return 2
	}
	c, err := client(rpcURL(*rpc))
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet balance:", err)
		return 1
	}
	acc, err := c.Account(addressStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet balance:", err)
		return 1
	}
	fmt.Printf("address %s\nbalance %s Z (%d uz)\nnonce   %d\n",
		acc.Address, acc.BalanceZ, acc.BalanceUZ, acc.Nonce)
	return 0
}

// submit builds, signs and sends a transaction through the SDK.
func submit(c *zsdk.Client, tx *ztypes.Tx, wait bool) int {
	if tx == nil {
		fmt.Fprintln(os.Stderr, "zwallet: could not build transaction")
		return 1
	}
	id, err := c.SendTx(tx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet:", err)
		return 1
	}
	fmt.Printf("submitted %s\n", id)
	if wait {
		h, err := c.WaitForTx(id, waitTimeout)
		if err != nil {
			fmt.Fprintln(os.Stderr, "zwallet:", err)
			return 1
		}
		fmt.Printf("committed at height %d\n", h)
	}
	return 0
}

func send(args []string) int {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	rpc := fs.String("rpc", "", "RPC URL")
	keyPath := fs.String("key", "", "keystore")
	pwFile := fs.String("password-file", "", "password file")
	to := fs.String("to", "", "recipient address")
	amount := fs.String("amount", "", "amount in Z")
	memo := fs.String("memo", "", "optional memo")
	fee := fs.Uint64("gas-price", 1, "gas price in uz")
	wait := fs.Bool("wait", false, "wait for inclusion")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	key, err := loadKey(*keyPath, *pwFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet send:", err)
		return 1
	}
	toAddr, err := zcrypto.ParseAddress(*to)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet send: recipient:", err)
		return 2
	}
	uz, err := parseAmount(*amount)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet send:", err)
		return 2
	}
	c, err := client(rpcURL(*rpc))
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet send:", err)
		return 1
	}
	tx, err := zsdk.Transfer(c, key, toAddr, uz, zsdk.Options{Memo: *memo, GasPrice: *fee})
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet send:", err)
		return 1
	}
	return submit(c, tx, *wait)
}

func delegate(args []string) int {
	return stakeOp("delegate", args, false)
}

func undelegate(args []string) int {
	return stakeOp("undelegate", args, true)
}

func stakeOp(name string, args []string, undelegate bool) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	rpc := fs.String("rpc", "", "RPC URL")
	keyPath := fs.String("key", "", "keystore")
	pwFile := fs.String("password-file", "", "password file")
	validator := fs.String("validator", "", "validator operator address")
	amount := fs.String("amount", "", "amount in Z")
	wait := fs.Bool("wait", false, "wait for inclusion")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	key, err := loadKey(*keyPath, *pwFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zwallet %s: %v\n", name, err)
		return 1
	}
	vAddr, err := zcrypto.ParseAddress(*validator)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zwallet %s: validator: %v\n", name, err)
		return 2
	}
	uz, err := parseAmount(*amount)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zwallet %s: %v\n", name, err)
		return 2
	}
	c, err := client(rpcURL(*rpc))
	if err != nil {
		fmt.Fprintf(os.Stderr, "zwallet %s: %v\n", name, err)
		return 1
	}
	var tx *ztypes.Tx
	if undelegate {
		tx, err = zsdk.Undelegate(c, key, vAddr, uz, zsdk.Options{})
	} else {
		tx, err = zsdk.Delegate(c, key, vAddr, uz, zsdk.Options{})
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "zwallet %s: %v\n", name, err)
		return 1
	}
	return submit(c, tx, *wait)
}

func registerValidator(args []string) int {
	fs := flag.NewFlagSet("register-validator", flag.ContinueOnError)
	rpc := fs.String("rpc", "", "RPC URL")
	keyPath := fs.String("key", "", "keystore")
	pwFile := fs.String("password-file", "", "password file")
	moniker := fs.String("moniker", "", "validator display name")
	commission := fs.Uint("commission-bps", 1000, "commission in basis points")
	wait := fs.Bool("wait", false, "wait for inclusion")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *commission > 10000 {
		fmt.Fprintln(os.Stderr, "zwallet register-validator: commission must be <= 10000 bps")
		return 2
	}
	key, err := loadKey(*keyPath, *pwFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet register-validator:", err)
		return 1
	}
	c, err := client(rpcURL(*rpc))
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet register-validator:", err)
		return 1
	}
	tx, err := zsdk.RegisterValidator(c, key, *moniker, uint16(*commission), zsdk.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet register-validator:", err)
		return 1
	}
	return submit(c, tx, *wait)
}

func vote(args []string) int {
	fs := flag.NewFlagSet("vote", flag.ContinueOnError)
	rpc := fs.String("rpc", "", "RPC URL")
	keyPath := fs.String("key", "", "keystore")
	pwFile := fs.String("password-file", "", "password file")
	proposal := fs.Uint64("proposal", 0, "proposal id")
	option := fs.String("option", "", "yes|no|abstain")
	wait := fs.Bool("wait", false, "wait for inclusion")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var opt ztypes.VoteOption
	switch strings.ToLower(*option) {
	case "yes":
		opt = ztypes.VoteYes
	case "no":
		opt = ztypes.VoteNo
	case "abstain":
		opt = ztypes.VoteAbstain
	default:
		fmt.Fprintln(os.Stderr, "zwallet vote: --option must be yes|no|abstain")
		return 2
	}
	key, err := loadKey(*keyPath, *pwFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet vote:", err)
		return 1
	}
	c, err := client(rpcURL(*rpc))
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet vote:", err)
		return 1
	}
	tx, err := zsdk.Vote(c, key, *proposal, opt, zsdk.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet vote:", err)
		return 1
	}
	return submit(c, tx, *wait)
}

func propose(args []string) int {
	fs := flag.NewFlagSet("propose", flag.ContinueOnError)
	rpc := fs.String("rpc", "", "RPC URL")
	keyPath := fs.String("key", "", "keystore")
	pwFile := fs.String("password-file", "", "password file")
	title := fs.String("title", "", "proposal title")
	desc := fs.String("description", "", "proposal description")
	paramKey := fs.String("param", "", "parameter to change (empty for a text proposal)")
	value := fs.Uint64("value", 0, "new parameter value")
	deposit := fs.String("deposit", "100", "deposit in Z")
	wait := fs.Bool("wait", false, "wait for inclusion")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	key, err := loadKey(*keyPath, *pwFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet propose:", err)
		return 1
	}
	dep, err := parseAmount(*deposit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet propose:", err)
		return 2
	}
	kind := ztypes.GovProposalText
	if *paramKey != "" {
		kind = ztypes.GovProposalParam
	}
	c, err := client(rpcURL(*rpc))
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet propose:", err)
		return 1
	}
	tx, err := zsdk.SubmitProposal(c, key, *title, *desc, kind, *paramKey, *value, dep, zsdk.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet propose:", err)
		return 1
	}
	return submit(c, tx, *wait)
}

func deploy(args []string) int {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	rpc := fs.String("rpc", "", "RPC URL")
	keyPath := fs.String("key", "", "keystore")
	pwFile := fs.String("password-file", "", "password file")
	asmFile := fs.String("asm-file", "", "contract assembly file")
	codeFlag := fs.String("code", "", "contract assembly inline (alternative to --asm-file)")
	wait := fs.Bool("wait", false, "wait for inclusion")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var src []byte
	switch {
	case *asmFile != "" && *codeFlag != "":
		fmt.Fprintln(os.Stderr, "zwallet deploy: use either --asm-file or --code, not both")
		return 2
	case *asmFile != "":
		b, err := os.ReadFile(*asmFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "zwallet deploy:", err)
			return 1
		}
		src = b
	case *codeFlag != "":
		src = []byte(*codeFlag)
	default:
		fmt.Fprintln(os.Stderr, "zwallet deploy: --asm-file or --code is required")
		return 2
	}
	key, err := loadKey(*keyPath, *pwFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet deploy:", err)
		return 1
	}
	c, err := client(rpcURL(*rpc))
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet deploy:", err)
		return 1
	}
	tx, err := zsdk.DeployContract(c, key, string(src), zsdk.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet deploy:", err)
		return 1
	}
	return submit(c, tx, *wait)
}

func callContract(args []string) int {
	fs := flag.NewFlagSet("call", flag.ContinueOnError)
	rpc := fs.String("rpc", "", "RPC URL")
	keyPath := fs.String("key", "", "keystore")
	pwFile := fs.String("password-file", "", "password file")
	contract := fs.String("contract", "", "contract address")
	input := fs.String("input", "", "calldata (hex)")
	wait := fs.Bool("wait", false, "wait for inclusion")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	addr, err := zcrypto.ParseAddress(*contract)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet call: contract:", err)
		return 2
	}
	var calldata []byte
	if *input != "" {
		calldata, err = hex.DecodeString(strings.TrimPrefix(*input, "0x"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "zwallet call: --input:", err)
			return 2
		}
	}
	key, err := loadKey(*keyPath, *pwFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet call:", err)
		return 1
	}
	c, err := client(rpcURL(*rpc))
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet call:", err)
		return 1
	}
	tx, err := zsdk.CallContract(c, key, addr, calldata, zsdk.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet call:", err)
		return 1
	}
	return submit(c, tx, *wait)
}

// signTx signs a transaction built by another tool: it decodes the unsigned hex,
// re-signs with the local key and prints the result. This is how offline signing
// workflows are supported.
func signTx(args []string) int {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	keyPath := fs.String("key", "", "keystore")
	pwFile := fs.String("password-file", "", "password file")
	txHex := fs.String("tx-hex", "", "unsigned transaction hex")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(*txHex, "0x"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet sign:", err)
		return 2
	}
	tx, err := ztypes.DecodeTx(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet sign:", err)
		return 2
	}
	key, err := loadKey(*keyPath, *pwFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zwallet sign:", err)
		return 1
	}
	if tx.Sender != key.Address() {
		fmt.Fprintln(os.Stderr, "zwallet sign: transaction sender does not match this key")
		return 1
	}
	if err := tx.Sign(key); err != nil {
		fmt.Fprintln(os.Stderr, "zwallet sign:", err)
		return 1
	}
	fmt.Printf("%s\n%s\n", tx.ID().String(), hex.EncodeToString(tx.Encode()))
	return 0
}

// waitTimeout is how long --wait polls for inclusion before giving up.
var waitTimeout = 30 * time.Second
