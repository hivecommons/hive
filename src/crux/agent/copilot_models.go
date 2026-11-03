package agent

import "github.com/hivecommons/hive/crux"

// CanonicalizeCopilotModel and CopilotLaunchModel are defined in crux so the
// dashboard canonicalizes Copilot model ids without importing the runtime;
// the launch and routing paths in this package call them through these names.
var (
	CanonicalizeCopilotModel = crux.CanonicalizeCopilotModel
	CopilotLaunchModel       = crux.CopilotLaunchModel
)
