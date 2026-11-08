// Command znd runs a Z Network node.
//
//	znd init --home /var/lib/znd
//	znd run  --home /var/lib/znd --genesis genesis.json --validator-key validator.key.json
package main

import (
	"os"

	"znetwork/internal/znodedcmd"
)

func main() { os.Exit(znodedcmd.Main(os.Args[1:])) }
