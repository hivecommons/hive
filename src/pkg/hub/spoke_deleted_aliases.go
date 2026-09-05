package hub

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

type InferenceBudgetProvider = spoke.InferenceBudgetProvider

func CollectClusterHealth(logger *slog.Logger) *HeartbeatClusterHealthReport {
	report := spoke.CollectClusterHealth(logger)
	if report == nil {
		return nil
	}
	var out HeartbeatClusterHealthReport
	data, err := json.Marshal(report)
	if err != nil {
		return nil
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return &out
}
func OpenFDCount() int    { return spoke.OpenFDCount() }
func FDSoftLimit() uint64 { return spoke.FDSoftLimit() }

func AgentActivityFor(mgr *agent.Manager, cfg *config.Config, govState governor.State, currentMode, name string, proc *agent.AgentProcess, onDemandFromPack map[string]bool) AgentActivity {
	act := spoke.AgentActivityFor(mgr, cfg, govState, currentMode, name, proc, onDemandFromPack)
	var out AgentActivity
	data, err := json.Marshal(act)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(data, &out)
	return out
}
func HeartbeatKickInterval(govState governor.State, name string, proc *agent.AgentProcess, onDemandFromPack map[string]bool) time.Duration {
	return spoke.HeartbeatKickInterval(govState, name, proc, onDemandFromPack)
}
func spokeAgentSummaries(agents []AgentSummary) []spoke.AgentSummary {
	var out []spoke.AgentSummary
	data, err := json.Marshal(agents)
	if err != nil {
		return nil
	}
	_ = json.Unmarshal(data, &out)
	return out
}
func QuotaExhaustedAgentCount(agents []AgentSummary) int {
	return spoke.QuotaExhaustedAgentCount(spokeAgentSummaries(agents))
}
func QuotaExhaustedProcessCount(statuses map[string]*agent.AgentProcess) int {
	return spoke.QuotaExhaustedProcessCount(statuses)
}
func QuotaExhaustedAgentReason(count int) string { return spoke.QuotaExhaustedAgentReason(count) }
func ProviderLimitHeartbeatFields(agents []AgentSummary, budget InferenceBudgetProvider) (string, int, bool, []string) {
	return spoke.ProviderLimitHeartbeatFields(spokeAgentSummaries(agents), budget)
}
func HashDashboardToken(token string) string       { return spoke.HashDashboardToken(token) }
func RolloutRestartSelf(logger *slog.Logger) error { return spoke.RolloutRestartSelf(logger) }
func SwitchImageSelf(logger *slog.Logger, image string) error {
	return spoke.SwitchImageSelf(logger, image)
}
func UpgradeSelfToSHA(logger *slog.Logger, targetSHA string) (bool, error) {
	return spoke.UpgradeSelfToSHA(logger, targetSHA)
}
func SelfImageReleaseChannel() string { return spoke.SelfImageReleaseChannel() }
func SelfDeploymentImage() string     { return spoke.SelfDeploymentImage() }
func MintSSOToken(seedHex, username, role, hiveID string, now time.Time) string {
	return spoke.MintSSOToken(seedHex, username, role, hiveID, now)
}
func VerifySSOToken(pubHex, token, expectedHiveID string, now time.Time) (string, string, error) {
	return spoke.VerifySSOToken(pubHex, token, expectedHiveID, now)
}
func TerminalSigningKey() string { return spoke.TerminalSigningKey() }
func MintTerminalAssertion(key, username, role, hiveID string, now time.Time) string {
	return spoke.MintTerminalAssertion(key, username, role, hiveID, now)
}
func VerifyTerminalAssertion(key, token, expectedHiveID string, now time.Time) (string, string, error) {
	return spoke.VerifyTerminalAssertion(key, token, expectedHiveID, now)
}
