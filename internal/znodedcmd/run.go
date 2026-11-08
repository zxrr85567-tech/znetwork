// Package znodedcmd implements the znd daemon command line.
package znodedcmd

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"znetwork/internal/zcrypto"
	"znetwork/internal/zindexer"
	"znetwork/internal/zmetrics"
	"znetwork/internal/znet"
	"znetwork/internal/znode"
	"znetwork/internal/zrpc"
	"znetwork/internal/ztypes"
)

// Main is the entry point. It returns a process exit code.
func Main(args []string) int {
	if len(args) == 0 {
		args = []string{"run"}
	}
	switch args[0] {
	case "run":
		return runNode(args[1:])
	case "init":
		return initNode(args[1:])
	case "version":
		fmt.Printf("znd %s (protocol %d)\n", Version, ztypes.ProtocolVersion)
		return 0
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "znd: unknown command %q\n\n", args[0])
		usage()
		return 2
	}
}

// Version is the node software version.
const Version = "1.0.0"

func usage() {
	fmt.Fprint(os.Stderr, `znd — Z Network node

Usage:
  znd run    --home DIR [options]     run a node
  znd init   --home DIR [options]     create a data directory and validator key
  znd version
  znd help

Run options:
  --home DIR            data directory (required)
  --genesis FILE        genesis JSON (required on first start)
  --validator-key FILE  encrypted validator keystore
  --password-file FILE  file containing the keystore password (or ZND_PASSWORD)
  --rpc ADDR            JSON-RPC listen address (default 127.0.0.1:26657)
  --p2p ADDR            P2P listen address (default 0.0.0.0:26656)
  --seeds LIST          comma-separated peer addresses
  --block-time MS       target block interval (default 1000)
  --log-level LEVEL     debug|info|warn|error (default info)
  --no-index            disable the transaction index
  --no-invariants       disable per-block supply invariant checking
`)
}

type runConfig struct {
	home         string
	genesis      string
	validatorKey string
	passwordFile string
	rpcAddr      string
	p2pAddr      string
	seeds        string
	blockTime    int64
	logLevel     string
	noIndex      bool
	noInvariants bool
	moniker      string
}

func parseRunFlags(name string, args []string) (*runConfig, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	cfg := &runConfig{}
	fs.StringVar(&cfg.home, "home", "", "data directory")
	fs.StringVar(&cfg.genesis, "genesis", "", "genesis JSON file")
	fs.StringVar(&cfg.validatorKey, "validator-key", "", "validator keystore path")
	fs.StringVar(&cfg.passwordFile, "password-file", "", "keystore password file")
	fs.StringVar(&cfg.rpcAddr, "rpc", "127.0.0.1:26657", "RPC listen address")
	fs.StringVar(&cfg.p2pAddr, "p2p", "0.0.0.0:26656", "P2P listen address")
	fs.StringVar(&cfg.seeds, "seeds", "", "comma-separated seed addresses")
	fs.Int64Var(&cfg.blockTime, "block-time", 1000, "block interval in ms")
	fs.StringVar(&cfg.logLevel, "log-level", "info", "log level")
	fs.BoolVar(&cfg.noIndex, "no-index", false, "disable the transaction index")
	fs.BoolVar(&cfg.noInvariants, "no-invariants", false, "disable invariant checks")
	fs.StringVar(&cfg.moniker, "moniker", "", "node name used in logs, handshakes and metrics (default: hostname)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if cfg.home == "" {
		return nil, errors.New("--home is required")
	}
	return cfg, nil
}

func logLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func newLogger(level string) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel(level)}))
}

// readPassword returns the keystore password from a file or the environment. It
// never echoes the value and never writes it to disk.
func readPassword(file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("reading password file: %w", err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	if p, ok := os.LookupEnv("ZND_PASSWORD"); ok {
		return p, nil
	}
	return "", errors.New("validator key given but no password: use --password-file or ZND_PASSWORD")
}

func loadValidatorKey(cfg *runConfig) (*zcrypto.PrivateKey, error) {
	if cfg.validatorKey == "" {
		return nil, nil
	}
	ks, err := zcrypto.LoadKeystore(cfg.validatorKey)
	if err != nil {
		return nil, fmt.Errorf("loading validator keystore: %w", err)
	}
	pw, err := readPassword(cfg.passwordFile)
	if err != nil {
		return nil, err
	}
	key, err := ks.Decrypt(pw)
	if err != nil {
		return nil, fmt.Errorf("decrypting validator keystore: %w", err)
	}
	return key, nil
}

func loadGenesis(path string) (*ztypes.Genesis, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ztypes.ParseGenesisJSON(raw)
}

func initNode(args []string) int {
	cfg, err := parseRunFlags("init", args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "znd init:", err)
		return 2
	}
	if err := os.MkdirAll(cfg.home, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "znd init:", err)
		return 1
	}
	keyPath := filepath.Join(cfg.home, "validator.key.json")
	if _, err := os.Stat(keyPath); err == nil {
		fmt.Fprintf(os.Stderr, "znd init: %s already exists; refusing to overwrite a key\n", keyPath)
		return 1
	}
	key, err := zcrypto.GenerateKey()
	if err != nil {
		fmt.Fprintln(os.Stderr, "znd init: generating key:", err)
		return 1
	}
	pw := os.Getenv("ZND_PASSWORD")
	if pw == "" {
		if cfg.passwordFile == "" {
			fmt.Fprintln(os.Stderr, "znd init: set ZND_PASSWORD or --password-file to encrypt the new key")
			return 2
		}
		p, err := readPassword(cfg.passwordFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "znd init:", err)
			return 1
		}
		pw = p
	}
	ks, err := zcrypto.EncryptKey(key, pw, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "znd init: encrypting key:", err)
		return 1
	}
	if err := zcrypto.SaveKeystore(keyPath, ks); err != nil {
		fmt.Fprintln(os.Stderr, "znd init: saving key:", err)
		return 1
	}
	info := map[string]string{
		"address":    key.Address().Bech32(),
		"public_key": key.PublicKey().String(),
		"keystore":   keyPath,
		"home":       cfg.home,
		"next_step":  "create or copy genesis.json into the home directory, then run: znd run --home <dir> --genesis genesis.json --validator-key validator.key.json",
	}
	out, _ := json.MarshalIndent(info, "", "  ")
	fmt.Println(string(out))
	return 0
}

func runNode(args []string) int {
	cfg, err := parseRunFlags("run", args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "znd run:", err)
		return 2
	}
	log := newLogger(cfg.logLevel)
	key, err := loadValidatorKey(cfg)
	if err != nil {
		log.Error("validator key", "err", err)
		return 1
	}

	genesisPath := cfg.genesis
	if genesisPath == "" {
		genesisPath = filepath.Join(cfg.home, "genesis.json")
	}
	var genesis *ztypes.Genesis
	if _, err := os.Stat(genesisPath); err == nil {
		genesis, err = loadGenesis(genesisPath)
		if err != nil {
			log.Error("genesis", "path", genesisPath, "err", err)
			return 1
		}
	} else if !os.IsNotExist(err) {
		log.Error("genesis", "err", err)
		return 1
	}

	chainID := ""
	if genesis != nil {
		chainID = genesis.ChainID
	} else {
		// Restart without genesis: read the chain id from the stored blocks.
		storedID, err := readStoredChainID(filepath.Join(cfg.home, "data"))
		if err != nil {
			log.Error("cannot start: no genesis file and no usable data directory", "err", err)
			return 1
		}
		chainID = storedID
	}

	moniker := cfg.moniker
	if moniker == "" {
		moniker = hostname()
	}
	if len(moniker) > ztypes.MaxMonikerLen {
		log.Error("moniker too long", "max", ztypes.MaxMonikerLen)
		return 2
	}

	dataDir := filepath.Join(cfg.home, "data")
	nodeCfg := znode.Config{
		ChainID:         chainID,
		DataDir:         dataDir,
		Genesis:         genesis,
		Validator:       key,
		Moniker:         moniker,
		ListenAddr:      cfg.p2pAddr,
		BlockTimeMs:     cfg.blockTime,
		MaxTxsPerBlock:  2000,
		CheckInvariants: !cfg.noInvariants,
	}
	n, err := znode.NewNode(nodeCfg, log)
	if err != nil {
		log.Error("starting node", "err", err)
		return 1
	}

	// P2P.
	pub := zcrypto.PublicKey{}
	if key != nil {
		pub = key.PublicKey()
	}
	peer := znet.NewServer(chainID, nodeCfg.Moniker, pub, func() uint64 { return n.Chain().Height() })
	addr, err := peer.Listen(cfg.p2pAddr)
	if err != nil {
		log.Error("p2p listen", "addr", cfg.p2pAddr, "err", err)
		return 1
	}
	log.Info("p2p listening", "addr", addr)
	n.AttachNetwork(peer)
	if cfg.seeds != "" {
		var seeds []string
		for _, s := range strings.Split(cfg.seeds, ",") {
			if s = strings.TrimSpace(s); s != "" {
				seeds = append(seeds, s)
			}
		}
		n.DialSeeds(seeds)
	}

	// Indexer.
	var idx *zindexer.Index
	if !cfg.noIndex {
		idx, err = zindexer.Open(filepath.Join(cfg.home, "index"), chainID)
		if err != nil {
			log.Error("opening index", "err", err)
			return 1
		}
		if err := idx.Sync(n.Chain()); err != nil {
			log.Error("index sync", "err", err)
			return 1
		}
		n.OnCommit = func(b *ztypes.Block, receipts []*ztypes.Receipt) {
			if err := idx.IndexBlock(b, receipts); err != nil {
				log.Error("indexing block", "height", b.Header.Height, "err", err)
			}
		}
		log.Info("index ready", "height", idx.Height(), "records", idx.Count())
	}

	// Metrics: process and chain gauges exposed over /metrics and z_metrics.
	reg := zmetrics.New()
	reg.SetLabel("chain_id", chainID)
	reg.SetLabel("node", n.Moniker())
	reg.Gauge("znet_height", "Current chain height.", func() float64 { return float64(n.Chain().Height()) })
	reg.Gauge("znet_mempool_size", "Transactions waiting in the mempool.", func() float64 { return float64(n.Pool().Size()) })
	reg.Gauge("znet_peer_count", "Connected peers.", func() float64 { return float64(n.PeerCount()) })
	reg.Gauge("znet_supply_uz", "Total supply in micro-Z.", func() float64 { return float64(n.Chain().State().Supply()) })
	reg.Gauge("znet_validator_count", "Active validators in the current set.", func() float64 {
		set, err := znode.ActiveValidatorSet(n.Chain().State())
		if err != nil {
			return 0
		}
		return float64(len(set.Validators))
	})
	prevOnCommit := n.OnCommit
	n.Metrics = reg
	n.OnCommit = func(b *ztypes.Block, r []*ztypes.Receipt) {
		reg.IncBlockApplied()
		if prevOnCommit != nil {
			prevOnCommit(b, r)
		}
	}

	// RPC.
	svc := zrpc.NewService(chainID, n.Chain(), n.Pool(), n.SubmitTx, n.PeerCount)
	svc.SetMetrics(reg)
	if token := os.Getenv("ZND_RPC_TOKEN"); token != "" {
		svc.SetAuthToken(token)
		log.Info("RPC write authentication enabled")
	}
	rpcServer, rpcAddr, err := svc.Serve(cfg.rpcAddr)
	if err != nil {
		log.Error("rpc listen", "addr", cfg.rpcAddr, "err", err)
		return 1
	}
	log.Info("rpc listening", "addr", rpcAddr.String())

	n.Start()
	log.Info("znd running", "version", Version, "chain_id", chainID, "home", cfg.home)

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case s := <-sig:
		log.Info("shutting down", "signal", s.String())
	case err := <-n.Errors():
		log.Error("fatal node error", "err", err)
	}
	n.Stop()
	peer.Close()
	_ = rpcServer.Close()
	if idx != nil {
		if err := idx.Close(); err != nil {
			log.Warn("closing index", "err", err)
		}
	}
	log.Info("stopped")
	return 0
}
