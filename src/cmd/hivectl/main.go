package main

import (
	"fmt"
	"os"

	"github.com/hivecommons/hive/pkg/hivectl/commands"
)

func main() {
	command := commands.NewRootCommand(os.Stdin, os.Stdout, os.Stderr)
	if err := command.Execute(); err != nil {
		// Redacted: cobra quotes unknown commands and arguments verbatim.
		fmt.Fprintln(os.Stderr, "Error:", commands.RedactTokens(err.Error()))
		os.Exit(commands.ExitCode(err))
	}
}
