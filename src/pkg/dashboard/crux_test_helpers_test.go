package dashboard

import (
	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/crux"
)

// agentManager unwraps the concrete manager behind a Dependencies.AgentMgr
// for tests that drive manager-only seams (kick-dispatch recording, kick log
// directories, capability probes). Production code holds crux.Runtime and
// must not need this.
func agentManager(rt crux.Runtime) *agent.Manager {
	return rt.(*agent.Manager)
}
