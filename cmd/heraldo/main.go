// Command heraldo is the heraldo CLI. Stage 0 has only `heraldo version`.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/noahkiss/heraldo/internal/version"
)

const usage = `usage: heraldo <command>

commands:
  version   print the version
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, "heraldo", version.String())
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "heraldo: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
