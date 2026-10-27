package crux

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/logscrub"
)

// Copilot token sources, as reported by Runtime.CopilotTokenSource. They are
// operator-facing phrases naming which credential GitHub saw: they are
// spliced verbatim into the model-picker notice and the "rejected by
// upstream" log line, so each one has to read as an answer to "which
// credential did GitHub just reject?". crux/agent aliases them.
const (
	// CopilotTokenSourceEnv: the COPILOT_GITHUB_TOKEN the hive process was
	// started with (a provisioned/machine token, typically).
	CopilotTokenSourceEnv = "the COPILOT_GITHUB_TOKEN environment variable"
	// CopilotTokenSourceDashboardLogin: a device-flow login completed on the
	// dashboard during this process's lifetime.
	CopilotTokenSourceDashboardLogin = "the dashboard Copilot login"
	// CopilotTokenSourceDurableFile: a dashboard login persisted by an earlier
	// hive process and read back at startup.
	CopilotTokenSourceDurableFile = "a dashboard Copilot login persisted at " + CopilotUserTokenPath
	// CopilotTokenSourceCLIConfig: an in-agent `/login` the reconciler promoted
	// out of the shared Copilot CLI config.
	CopilotTokenSourceCLIConfig = "an in-agent /login promoted from the shared Copilot CLI config"
)

// CopilotUserTokenPath is where the dashboard's device-flow login persists
// the Copilot OAuth token; injected into agents as COPILOT_GITHUB_TOKEN.
// Dashboard: Copilot auth status and login.
const CopilotUserTokenPath = "/data/copilot-user-token"

// GitHubTokenLogin resolves the GitHub login that owns token via GET /user, or
// "" on any failure. Short-timeout, one call. Dashboard: the model-discovery
// notice naming the account behind a rejected Copilot credential; crux/agent:
// the Copilot CLI config seed path.
func GitHubTokenLogin(token string) string {
	req, err := http.NewRequest("GET", gitHubUserURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := gitHubUserClient.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var body struct {
		Login string `json:"login"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return ""
	}
	return strings.TrimSpace(body.Login)
}

// gitHubUserURL and gitHubUserClient are the endpoint and client
// GitHubTokenLogin uses; tests point them at a local server.
var (
	gitHubUserURL    = "https://api.github.com/user"
	gitHubUserClient = &http.Client{Timeout: 5 * time.Second}
)

// RedactPaneTokens masks GitHub tokens, sk- API keys and device-login lines in
// pane-derived text. Dashboard: activity summaries.
func RedactPaneTokens(s string) string {
	return logscrub.RedactPaneTokens(s)
}
