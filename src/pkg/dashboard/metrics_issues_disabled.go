package dashboard

import "context"

// IssuesDisabledRepo is a watched repo whose Issues feature is turned off
// (has_issues=false). Agents cannot file issues there and the advisory issue
// has nowhere to go, so the dashboard warns proactively instead of waiting for
// a failed issue create (#9972).
type IssuesDisabledRepo struct {
	Repo string `json:"repo"`
	// Fork is the usual cause: GitHub disables Issues on forks by default.
	Fork bool `json:"fork"`
	// Message is the same cause-and-remedy text IssuesDisabledError carries.
	Message string `json:"message"`
}

// collectIssuesDisabled probes has_issues for every repo the GitHub client
// watches (project.repos). It runs on the metrics cadence: once at startup and
// every metricsCollectInterval after. A failed probe fails OPEN for that repo:
// the previous verdict is kept, so a transient API error neither invents a
// warning nor silently clears a real one.
func (mc *MetricsCollector) collectIssuesDisabled(ctx context.Context) {
	gh := mc.client()
	if gh == nil {
		return
	}

	prev := make(map[string]IssuesDisabledRepo)
	for _, r := range mc.GetIssuesDisabledRepos() {
		prev[r.Repo] = r
	}

	var found []IssuesDisabledRepo
	seen := make(map[string]bool)
	for _, entry := range gh.Repositories() {
		owner, name := gh.SplitRepo(entry)
		if owner == "" || name == "" {
			continue
		}
		full := owner + "/" + name
		if seen[full] {
			continue
		}
		seen[full] = true

		disabled, err := gh.ProbeIssuesDisabled(ctx, owner, name)
		if err != nil {
			mc.logger.Warn("could not check watched repo has_issues", "repo", full, "error", err)
			if p, ok := prev[full]; ok {
				found = append(found, p)
			}
			continue
		}
		if disabled != nil {
			found = append(found, IssuesDisabledRepo{Repo: disabled.Repo, Fork: disabled.Fork, Message: disabled.Error()})
		}
	}

	mc.issuesDisabledMu.Lock()
	mc.issuesDisabled = found
	mc.issuesDisabledMu.Unlock()
}

// GetIssuesDisabledRepos returns the watched repos last seen with Issues
// disabled, or nil when none (or no probe has run yet).
func (mc *MetricsCollector) GetIssuesDisabledRepos() []IssuesDisabledRepo {
	if mc == nil {
		return nil
	}
	mc.issuesDisabledMu.RLock()
	defer mc.issuesDisabledMu.RUnlock()
	if len(mc.issuesDisabled) == 0 {
		return nil
	}
	out := make([]IssuesDisabledRepo, len(mc.issuesDisabled))
	copy(out, mc.issuesDisabled)
	return out
}
