// Command zcli is the Z Network command line toolkit.
package main

import (
	"os"

	"znetwork/internal/zclicmd"
)

func main() { os.Exit(zclicmd.Main(os.Args[1:])) }
