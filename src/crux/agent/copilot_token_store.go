package agent

import (
	"sync"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent/copilottoken"
)

// copilotConfigHeader is the // preamble the Copilot CLI writes atop config.json.
const copilotConfigHeader = copilottoken.ConfigHeader

// configHasTokens returns true if either the Copilot config or Claude
// credentials file holds a credential a restart can still use. Used to decide
// whether an agent stuck on a login prompt can be auto-restarted. See
// copilottoken.ConfigHasTokens for why Claude uses HasUsableToken.
func configHasTokens() bool {
	return copilottoken.ConfigHasTokens(sharedClaudeCredentialPath, sharedCopilotConfigPath)
}

func copilotConfigHasTokens() bool {
	return copilottoken.CredentialFileHasTokens(sharedCopilotConfigPath)
}

func copilotCredentialFileHasTokens(path string) bool {
	return copilottoken.CredentialFileHasTokens(path)
}

func readCopilotConfig(path string) (map[string]interface{}, error) {
	return copilottoken.ReadConfig(path)
}

// sharedCopilotConfigMu serializes the hive's own writes to the shared
// Copilot config.json: the token writers below (dashboard login, token sync,
// expired-token clear) and the config bridge's merge of CLI-written fields
// (mergeCopilotConfigIntoShared, #18671). Without it a merge that read the
// shared file just before a dashboard login could rename its stale copy over
// the new login. The bridge only ever TryLocks it, so a token writer blocked
// on a GitHub login lookup never stalls an agent's pane poller.
var sharedCopilotConfigMu sync.Mutex

func clearExpiredTokens() error {
	sharedCopilotConfigMu.Lock()
	defer sharedCopilotConfigMu.Unlock()
	return copilottoken.ClearExpiredTokens(sharedCopilotConfigPath, sharedConfigDesiredMode)
}

func restoreCopilotTokens(path, token string) error {
	sharedCopilotConfigMu.Lock()
	defer sharedCopilotConfigMu.Unlock()
	return copilottoken.RestoreTokens(path, token, sharedConfigDesiredMode, githubTokenLogin)
}

func replaceCopilotTokens(path, token string) error {
	sharedCopilotConfigMu.Lock()
	defer sharedCopilotConfigMu.Unlock()
	return copilottoken.ReplaceTokens(path, token, sharedConfigDesiredMode, githubTokenLogin)
}

// GitHubTokenLogin resolves the GitHub login that owns token via GET /user, or
// "" on any failure. Exported for the dashboard's model-discovery notice,
// which names the account behind a rejected Copilot credential (#7302).
func GitHubTokenLogin(token string) string {
	return githubTokenLogin(token)
}

// githubTokenLogin resolves the GitHub login that owns token via GET /user, or
// "" on any failure. Short-timeout, one call — used only on the rare seed path
// where the config lacks a valid identity and on the model-discovery rejection
// path. Overridable in tests.
var githubTokenLogin = crux.GitHubTokenLogin

func copilotIdentityKey(v interface{}) string { return copilottoken.IdentityKey(v) }

func extractCopilotToken(path string) string { return copilottoken.ExtractToken(path) }

func writeDurableCopilotToken(durablePath, token string) error {
	return copilottoken.WriteDurableToken(durablePath, token)
}
