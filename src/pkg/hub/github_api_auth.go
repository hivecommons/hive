package hub

import (
	"net/http"
	"os"
	"strings"
)

// hubGitHubTokenEnv names the optional hub-side GitHub token. It raises the
// api.github.com rate limit the hub's read-only polling shares (branch tips,
// commit compares, commit messages/dates, workflow runs, the dibs public-repo
// check) from 60/h per source IP to 5000/h.
//
// Unset means anonymous reads. That worked while the hub tracked two branches;
// with v4/v5/v6 plus standing branches polled every cycle the anonymous budget
// is exhausted within minutes, GitHub answers 403/429, and every dependent
// dashboard row (channel distances, commit messages) silently goes blank.
const hubGitHubTokenEnv = "HIVE_HUB_GITHUB_TOKEN"

// hubGitHubToken returns the configured token or "".
//
// Read per call rather than cached at init so a test (or an operator rotating
// the secret through a restart) never fights a stale value; os.Getenv is a
// map lookup and these requests are already network-bound.
func hubGitHubToken() string {
	return strings.TrimSpace(os.Getenv(hubGitHubTokenEnv))
}

// authGitHubRequest attaches the hub's GitHub token to an api.github.com
// request when one is configured. Every hub-originated GitHub API read goes
// through here so the rate-limit budget is one decision, not one per call
// site; a call site that forgets it is the bug this helper exists to remove.
func authGitHubRequest(req *http.Request) {
	if req == nil {
		return
	}
	if tok := hubGitHubToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}
