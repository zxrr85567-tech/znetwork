package znodedcmd

import (
	"fmt"
	"os"

	"znetwork/internal/zstore"
)

// hostname returns the machine name, used as the default moniker.
func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "znd"
	}
	// Keep the moniker within the protocol limit.
	if len(h) > 64 {
		h = h[:64]
	}
	return h
}

// readStoredChainID opens the data directory and returns the chain id of the stored
// chain, so a node can restart without being handed the genesis file.
func readStoredChainID(dataDir string) (string, error) {
	st, err := zstore.Open(dataDir)
	if err != nil {
		return "", err
	}
	blocks, _, err := st.ReadBlocksFile()
	if err != nil && len(blocks) == 0 {
		return "", err
	}
	if len(blocks) == 0 {
		return "", fmt.Errorf("no stored blocks in %s", dataDir)
	}
	return blocks[0].Header.ChainID, nil
}
