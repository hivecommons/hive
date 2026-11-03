package main

import (
	"os"
	"strings"
)

func sharedAgentPoolEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("HIVE_SHARED_AGENT_POOL_ENABLED"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
