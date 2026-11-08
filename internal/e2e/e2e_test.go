// Package e2e contains end-to-end system tests that build the real binaries and
// run them as separate operating-system processes.
//
// These tests are the closest thing to "a user runs this" that exists in the
// repository: no code is imported, the node is started with `znd run`, the
// transaction is signed by `zwallet`, and the result is read back with `zcli`
// and the RPC API. If these pass, the packaged binaries actually work together.
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// buildBinaries compiles znd, zwallet and zcli into a temp dir once per run.
var (
	buildOnce sync.Once
	binDir    string
	buildErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	// The binaries are ~27 MB; leaving them behind fills the tmpfs after a few
	// runs and turns later builds into "no space left on device" failures.
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
	os.Exit(code)
}

func binaries(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "znetwork-bin-")
		if err != nil {
			buildErr = err
			return
		}
		binDir = dir
		root, err := moduleRoot()
		if err != nil {
			buildErr = err
			return
		}
		for _, name := range []string{"znoded:znd", "zwallet:zwallet", "zcli:zcli"} {
			parts := strings.SplitN(name, ":", 2)
			out := filepath.Join(dir, parts[1])
			cmd := exec.Command("go", "build", "-trimpath", "-o", out, "./cmd/"+parts[0])
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
			if b, err := cmd.CombinedOutput(); err != nil {
				buildErr = fmt.Errorf("building %s: %v\n%s", parts[0], err, b)
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatalf("building binaries: %v", buildErr)
	}
	return binDir
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found")
		}
		dir = parent
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

type process struct {
	cmd  *exec.Cmd
	logs *strings.Builder
	mu   sync.Mutex
}

func (p *process) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.logs.String()
}

func start(t *testing.T, bin string, args ...string) *process {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	p := &process{cmd: cmd, logs: &strings.Builder{}}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", bin, err)
	}
	collect := func(r io.Reader) {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				p.mu.Lock()
				p.logs.Write(buf[:n])
				p.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}
	go collect(stdout)
	go collect(stderr)
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
	})
	return p
}

// run executes a binary to completion and returns stdout.
func run(t *testing.T, bin string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func rpcCall(t *testing.T, url, method string, params any) (json.RawMessage, error) {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
			Data    string `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("bad response %s: %w", data, err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("%s: %s %s", method, out.Error.Message, out.Error.Data)
	}
	return out.Result, nil
}

func waitRPC(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := rpcCall(t, url, "z_getHeight", nil); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("RPC at %s did not come up", url)
}

func waitHeight(t *testing.T, url string, want uint64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := rpcCall(t, url, "z_getHeight", nil)
		if err == nil {
			var h struct {
				Height uint64 `json:"height"`
			}
			_ = json.Unmarshal(res, &h)
			if h.Height >= want {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	res, _ := rpcCall(t, url, "z_getHeight", nil)
	t.Fatalf("height did not reach %d (last: %s)", want, res)
}

// TestSingleNodeFullLifecycle drives the documented user flow end to end with real
// processes: generate a devnet, start a node, query it, sign and submit a transfer
// with the wallet, wait for inclusion, read it back with the CLI, verify the
// indexer file on disk, then restart the node and confirm state persists.
func TestSingleNodeFullLifecycle(t *testing.T) {
	bin := binaries(t)
	work := t.TempDir()
	genesisDir := filepath.Join(work, "devnet")
	password := "e2e-test-password"
	pwFile := filepath.Join(work, "password.txt")
	if err := os.WriteFile(pwFile, []byte(password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Generate a single-validator devnet (one validator keeps the quorum local).
	out, err := run(t, filepath.Join(bin, "zcli"), "devnet", "--out", genesisDir,
		"--validators", "1", "--users", "2", "--password-file", pwFile)
	if err != nil {
		t.Fatalf("zcli devnet: %v\n%s", err, out)
	}
	var manifest struct {
		ChainID     string `json:"chain_id"`
		GenesisHash string `json:"genesis_hash"`
		Faucet      struct {
			Address  string `json:"address"`
			Keystore string `json:"keystore"`
		} `json:"faucet"`
		Accounts []struct {
			Name    string `json:"name"`
			Address string `json:"address"`
			Key     string `json:"keystore"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(out), &manifest); err != nil {
		t.Fatalf("devnet manifest: %v\n%s", err, out)
	}
	if manifest.ChainID == "" || manifest.GenesisHash == "" {
		t.Fatalf("incomplete manifest: %s", out)
	}
	// 2. The genesis hash must be reproducible from the written file.
	hashOut, err := run(t, filepath.Join(bin, "zcli"), "genesis", "hash",
		"--file", filepath.Join(genesisDir, "genesis.json"))
	if err != nil {
		t.Fatalf("genesis hash: %v\n%s", err, hashOut)
	}
	var hashRes struct {
		GenesisHash string `json:"genesis_hash"`
	}
	_ = json.Unmarshal([]byte(hashOut), &hashRes)
	if hashRes.GenesisHash != manifest.GenesisHash {
		t.Fatalf("genesis hash not reproducible: %s vs %s", hashRes.GenesisHash, manifest.GenesisHash)
	}

	// 3. Start the node with the real daemon.
	home := filepath.Join(work, "node")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	rpcPort := freePort(t)
	p2pPort := freePort(t)
	rpcURL := fmt.Sprintf("http://127.0.0.1:%d", rpcPort)
	valKey := ""
	for _, a := range manifest.Accounts {
		if strings.HasPrefix(a.Name, "validator-") {
			valKey = a.Key
		}
	}
	if valKey == "" {
		t.Fatal("no validator keystore in the devnet manifest")
	}
	nodeLog := start(t, filepath.Join(bin, "znd"), "run",
		"--home", home,
		"--genesis", filepath.Join(genesisDir, "genesis.json"),
		"--validator-key", valKey,
		"--password-file", pwFile,
		"--rpc", fmt.Sprintf("127.0.0.1:%d", rpcPort),
		"--p2p", fmt.Sprintf("127.0.0.1:%d", p2pPort),
		"--block-time", "100",
		"--log-level", "info",
	)
	waitRPC(t, rpcURL, 15*time.Second)
	waitHeight(t, rpcURL, 3, 20*time.Second)

	// 4. Chain identity reported by the running node must match the genesis file.
	res, err := rpcCall(t, rpcURL, "z_chainInfo", nil)
	if err != nil {
		t.Fatal(err)
	}
	var info struct {
		ChainID     string `json:"chain_id"`
		GenesisHash string `json:"genesis_hash"`
		Height      uint64 `json:"height"`
	}
	_ = json.Unmarshal(res, &info)
	if info.ChainID != manifest.ChainID {
		t.Fatalf("running chain id %q != manifest %q", info.ChainID, manifest.ChainID)
	}
	if info.GenesisHash != manifest.GenesisHash {
		t.Fatalf("running genesis %s != manifest %s", info.GenesisHash, manifest.GenesisHash)
	}

	// 5. The validator must be bonded and the books must balance.
	res, err = rpcCall(t, rpcURL, "z_getValidators", nil)
	if err != nil {
		t.Fatal(err)
	}
	var vals struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(res, &vals)
	if vals.Count != 1 {
		t.Fatalf("validator count = %d", vals.Count)
	}
	res, err = rpcCall(t, rpcURL, "z_getSupply", nil)
	if err != nil {
		t.Fatal(err)
	}
	var supply struct {
		Balanced bool `json:"balanced"`
	}
	_ = json.Unmarshal(res, &supply)
	if !supply.Balanced {
		t.Fatalf("supply is not balanced: %s", res)
	}

	// 6. Send 12.5 Z from the faucet to a user using the wallet binary.
	user := manifest.Accounts[len(manifest.Accounts)-1]
	if user.Name == "faucet" {
		t.Fatal("unexpected account list")
	}
	os.Setenv("ZWALLET_PASSWORD", password)
	defer os.Unsetenv("ZWALLET_PASSWORD")
	balanceOut, err := run(t, filepath.Join(bin, "zcli"), "account", "--rpc", rpcURL,
		"--address", user.Address)
	if err != nil {
		t.Fatalf("zcli account: %v\n%s", err, balanceOut)
	}
	var before struct {
		BalanceUZ uint64 `json:"balance_uz"`
	}
	_ = json.Unmarshal([]byte(balanceOut), &before)

	sendOut, err := run(t, filepath.Join(bin, "zwallet"), "send", "--rpc", rpcURL,
		"--key", manifest.Faucet.Keystore, "--to", user.Address, "--amount", "12.5",
		"--memo", "e2e transfer", "--wait")
	if err != nil {
		t.Fatalf("zwallet send: %v\n%s", err, sendOut)
	}
	if !strings.Contains(sendOut, "committed at height") {
		t.Fatalf("send did not report a commit: %s", sendOut)
	}
	var txID string
	for _, line := range strings.Split(sendOut, "\n") {
		if strings.HasPrefix(line, "submitted ") {
			txID = strings.TrimSpace(strings.TrimPrefix(line, "submitted "))
		}
	}
	if txID == "" {
		t.Fatalf("no transaction id reported: %s", sendOut)
	}

	// 7. The recipient's balance must have increased by exactly 12.5 Z.
	balanceOut, err = run(t, filepath.Join(bin, "zcli"), "account", "--rpc", rpcURL,
		"--address", user.Address)
	if err != nil {
		t.Fatalf("zcli account after send: %v\n%s", err, balanceOut)
	}
	var after struct {
		BalanceUZ uint64 `json:"balance_uz"`
	}
	_ = json.Unmarshal([]byte(balanceOut), &after)
	if after.BalanceUZ != before.BalanceUZ+1_250_000_000 {
		t.Fatalf("balance %d -> %d, expected +12.5 Z", before.BalanceUZ, after.BalanceUZ)
	}

	// 8. The transaction must be queryable and committed.
	txOut, err := run(t, filepath.Join(bin, "zcli"), "tx", "--rpc", rpcURL, "--id", txID)
	if err != nil {
		t.Fatalf("zcli tx: %v\n%s", err, txOut)
	}
	var txRes struct {
		Status string `json:"status"`
		Height uint64 `json:"height"`
		Memo   string `json:"memo"`
	}
	_ = json.Unmarshal([]byte(txOut), &txRes)
	if txRes.Status != "committed" || txRes.Height == 0 {
		t.Fatalf("tx = %s", txOut)
	}
	if txRes.Memo != "e2e transfer" {
		t.Fatalf("memo = %q", txRes.Memo)
	}

	// 9. The on-disk index must contain the transaction (proves the indexer ran).
	var indexHit bool
	_ = filepath.Walk(filepath.Join(home, "index"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if strings.Contains(string(b), txID) {
			indexHit = true
		}
		return nil
	})
	if !indexHit {
		t.Fatal("transaction not found in the on-disk index")
	}

	// 10. Restart the node and confirm the height and balance persist.
	heightBefore := info.Height
	// Re-read the live height: blocks keep being produced.
	res, _ = rpcCall(t, rpcURL, "z_getHeight", nil)
	var h struct {
		Height uint64 `json:"height"`
	}
	_ = json.Unmarshal(res, &h)
	heightBefore = h.Height
	_ = nodeLog.cmd.Process.Signal(syscall.SIGTERM)
	_ = nodeLog.cmd.Wait()

	node2 := start(t, filepath.Join(bin, "znd"), "run",
		"--home", home,
		"--validator-key", valKey,
		"--password-file", pwFile,
		"--rpc", fmt.Sprintf("127.0.0.1:%d", rpcPort),
		"--p2p", fmt.Sprintf("127.0.0.1:%d", p2pPort),
		"--block-time", "100")
	_ = node2
	waitRPC(t, rpcURL, 15*time.Second)
	// A restart must resume from at least the height it stopped at (it may have
	// produced one more block before shutting down).
	waitHeight(t, rpcURL, heightBefore, 20*time.Second)
	balanceOut, err = run(t, filepath.Join(bin, "zcli"), "account", "--rpc", rpcURL,
		"--address", user.Address)
	if err != nil {
		t.Fatalf("zcli account after restart: %v\n%s", err, balanceOut)
	}
	var afterRestart struct {
		BalanceUZ uint64 `json:"balance_uz"`
	}
	_ = json.Unmarshal([]byte(balanceOut), &afterRestart)
	if afterRestart.BalanceUZ < after.BalanceUZ {
		t.Fatalf("balance regressed across restart: %d < %d", afterRestart.BalanceUZ, after.BalanceUZ)
	}
	txOut, err = run(t, filepath.Join(bin, "zcli"), "tx", "--rpc", rpcURL, "--id", txID)
	if err != nil {
		t.Fatalf("transaction lost across restart: %v\n%s", err, txOut)
	}
}

// TestTwoNodeNetwork covers the multi-process case: two validator nodes started as
// separate processes over TCP, connected by seeds, both committing the same chain.
func TestTwoNodeNetwork(t *testing.T) {
	bin := binaries(t)
	work := t.TempDir()
	password := "e2e-multi-password"
	pwFile := filepath.Join(work, "password.txt")
	if err := os.WriteFile(pwFile, []byte(password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	genesisDir := filepath.Join(work, "devnet")
	out, err := run(t, filepath.Join(bin, "zcli"), "devnet", "--out", genesisDir,
		"--validators", "2", "--users", "1", "--password-file", pwFile)
	if err != nil {
		t.Fatalf("zcli devnet: %v\n%s", err, out)
	}
	var manifest struct {
		Accounts []struct {
			Name string `json:"name"`
			Key  string `json:"keystore"`
		} `json:"accounts"`
	}
	_ = json.Unmarshal([]byte(out), &manifest)
	var keys []string
	for _, a := range manifest.Accounts {
		if strings.HasPrefix(a.Name, "validator-") {
			keys = append(keys, a.Key)
		}
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 validator keys, got %d", len(keys))
	}
	genesisPath := filepath.Join(genesisDir, "genesis.json")

	p2p1, p2p2 := freePort(t), freePort(t)
	rpc1, rpc2 := freePort(t), freePort(t)
	home1, home2 := filepath.Join(work, "n1"), filepath.Join(work, "n2")
	for _, h := range []string{home1, home2} {
		if err := os.MkdirAll(h, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	url1 := fmt.Sprintf("http://127.0.0.1:%d", rpc1)
	url2 := fmt.Sprintf("http://127.0.0.1:%d", rpc2)

	start(t, filepath.Join(bin, "znd"), "run", "--home", home1, "--genesis", genesisPath,
		"--validator-key", keys[0], "--password-file", pwFile,
		"--rpc", fmt.Sprintf("127.0.0.1:%d", rpc1), "--p2p", fmt.Sprintf("127.0.0.1:%d", p2p1),
		"--seeds", fmt.Sprintf("127.0.0.1:%d", p2p2), "--block-time", "100")
	start(t, filepath.Join(bin, "znd"), "run", "--home", home2, "--genesis", genesisPath,
		"--validator-key", keys[1], "--password-file", pwFile,
		"--rpc", fmt.Sprintf("127.0.0.1:%d", rpc2), "--p2p", fmt.Sprintf("127.0.0.1:%d", p2p2),
		"--seeds", fmt.Sprintf("127.0.0.1:%d", p2p1), "--block-time", "100")

	waitRPC(t, url1, 20*time.Second)
	waitRPC(t, url2, 20*time.Second)
	waitHeight(t, url1, 3, 40*time.Second)
	waitHeight(t, url2, 3, 40*time.Second)

	// Both nodes must report the same head hash.
	head := func(url string) string {
		res, err := rpcCall(t, url, "z_getBlocks", map[string]any{"from": 0, "limit": 1000})
		if err != nil {
			t.Fatalf("blocks: %v", err)
		}
		var list []struct {
			Hash string `json:"hash"`
		}
		_ = json.Unmarshal(res, &list)
		if len(list) == 0 {
			return ""
		}
		return list[len(list)-1].Hash
	}
	// Poll until both nodes agree at the same height, since production continues.
	deadline := time.Now().Add(30 * time.Second)
	agreed := false
	for time.Now().Before(deadline) {
		h1, _ := rpcCall(t, url1, "z_getHeight", nil)
		h2, _ := rpcCall(t, url2, "z_getHeight", nil)
		var a, b struct {
			Height uint64 `json:"height"`
		}
		_ = json.Unmarshal(h1, &a)
		_ = json.Unmarshal(h2, &b)
		if a.Height >= 5 && a.Height == b.Height {
			hashes1, err1 := rpcCall(t, url1, "z_getBlock", map[string]any{"height": a.Height})
			hashes2, err2 := rpcCall(t, url2, "z_getBlock", map[string]any{"height": a.Height})
			if err1 == nil && err2 == nil {
				var blk1, blk2 struct {
					Hash string `json:"hash"`
				}
				_ = json.Unmarshal(hashes1, &blk1)
				_ = json.Unmarshal(hashes2, &blk2)
				if blk1.Hash != "" && blk1.Hash == blk2.Hash {
					agreed = true
					break
				}
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !agreed {
		t.Fatalf("nodes never agreed on a block (head1=%s head2=%s)", head(url1), head(url2))
	}
	// Both must be live (peer counts reported).
	res, err := rpcCall(t, url1, "z_nodeInfo", nil)
	if err != nil {
		t.Fatal(err)
	}
	var nodeInfo struct {
		PeerCount int `json:"peer_count"`
	}
	_ = json.Unmarshal(res, &nodeInfo)
	if nodeInfo.PeerCount < 1 {
		t.Fatalf("node 1 has no peers: %s", res)
	}
}
