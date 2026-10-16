// Command ccli reports on Claude Code sessions across local accounts.
package main

import (
	"os"

	"github.com/lr00rl/ccli/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
