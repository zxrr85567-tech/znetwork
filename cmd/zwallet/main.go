// Command zwallet is the Z Network command line wallet.
package main

import (
	"os"

	"znetwork/internal/zwalletcmd"
)

func main() { os.Exit(zwalletcmd.Main(os.Args[1:])) }
