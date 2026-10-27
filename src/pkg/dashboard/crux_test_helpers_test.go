package dashboard

import (
	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/cruxtest"
)

// agentManager accesses the fixture controls behind a Dependencies.AgentMgr
// for tests that drive runtime setup seams (kick-dispatch recording, kick log
// directories, capability probes). Production code holds crux.Runtime and
// must not need this.
func agentManager(rt crux.Runtime) cruxtest.Runtime {
	return rt.(cruxtest.Runtime)
}
