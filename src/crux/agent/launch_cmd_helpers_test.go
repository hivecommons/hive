package agent

import "github.com/hivecommons/crux/config"

func backendLaunchCmd(binary, model, backend string, isInference bool, effort string) string {
	return backendLaunchCmdWithBobSession(binary, model, backend, isInference, effort, "")
}

func toolRulesToLaunchCmd(binary, model, backend string, tools *config.ToolsConfig, isInference bool, effort string) string {
	return toolRulesToLaunchCmdWithBobSession(binary, model, backend, tools, isInference, effort, "")
}
