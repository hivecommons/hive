package dashboard

import (
	"fmt"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/watchdog"
	"github.com/hivecommons/hive/pkg/config"
)

// agentDeliveryFailure uses the watchdog's latest published observation rather
// than running a tmux command on the health or heartbeat path.
func agentDeliveryFailure(proc *crux.Process, cfg *config.Config) string {
	if ready, ok := watchdog.FindCondition(proc.WatchdogConditions, watchdog.ConditionReady); ok && ready.Reason == string(watchdog.ClassNoSession) {
		return "tmux session missing"
	}
	threshold := cfg.KickFailureThresholdOrDefault()
	if proc.ConsecutiveKickFailures >= threshold {
		return fmt.Sprintf("%d consecutive kick delivery failures: %s", proc.ConsecutiveKickFailures, proc.LastKickError)
	}
	return ""
}
