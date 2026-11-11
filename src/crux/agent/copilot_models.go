package agent

import "github.com/hivecommons/hive/crux"

// CanonicalizeCopilotModel, CopilotLaunchModel, ClaudeLaunchModel and
// ModelForBackend are defined in crux so the dashboard canonicalizes model ids
// without importing the runtime; the launch and routing paths in this package
// call them through these names.
var (
	CanonicalizeCopilotModel = crux.CanonicalizeCopilotModel
	CopilotLaunchModel       = crux.CopilotLaunchModel
	ClaudeLaunchModel        = crux.ClaudeLaunchModel
	ModelForBackend          = crux.ModelForBackend
)
