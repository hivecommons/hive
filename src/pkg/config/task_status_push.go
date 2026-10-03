package config

// TaskStatusPushEnvVar overrides hub.task_status_push. Valid boolean values
// follow the other feature gates; empty or invalid values use the config.
const TaskStatusPushEnvVar = "HIVE_HUB_TASK_STATUS_PUSH"

// TaskStatusPushEnabled resolves the operator switch for the separate hub
// task-status loop. The default remains on for existing hub-linked hives.
// This does not disable the core heartbeat or remove its contributor fields.
func (h HubConfig) TaskStatusPushEnabled() bool {
	if v, ok := parseBoolEnv(TaskStatusPushEnvVar); ok {
		return v
	}
	if h.TaskStatusPush != nil {
		return *h.TaskStatusPush
	}
	return true
}
