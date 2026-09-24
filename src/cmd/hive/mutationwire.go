package main

import (
	"os"
	"strings"
)

const defaultMutationStateDir = "/data/convergence/mutation"

func mutationStateDir() string {
	if dir := strings.TrimSpace(os.Getenv("HIVE_MUTATION_STATE_DIR")); dir != "" {
		return dir
	}
	return defaultMutationStateDir
}
