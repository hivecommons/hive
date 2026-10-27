//go:build extwork_claude_managed_agents

package main

import (
	"github.com/hivecommons/hive/pkg/extwork"
	"github.com/hivecommons/hive/pkg/extwork/managedagents"
)

// Claude Managed Agents is linked only when the private hosted spoke explicitly
// opts into the build tag. Configuration alone cannot add a vendor client or a
// credential-bearing backend to an ordinary hive binary.
func init() {
	extwork.DefaultRegistry.Register(managedagents.Engine, managedagents.Factory)
}
