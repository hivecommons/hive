package main

import (
	"fmt"
	"os"

	"github.com/kubestellar/hive/v2/pkg/hivectl/commands"
)

func main() {
	command := commands.NewRootCommand(os.Stdin, os.Stdout, os.Stderr)
	if err := command.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(commands.ExitCode(err))
	}
}
