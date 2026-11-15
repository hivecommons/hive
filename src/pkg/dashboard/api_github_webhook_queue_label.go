package dashboard

import (
	"encoding/json"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/github"
)

// nudgeQueueLabelEvent asks the automerge lanes for an immediate pass over a
// pull request someone just labelled, when its repository and base use
// GitHub's native merge queue. A PR the hive queued stays queued until a
// lane sees the label that withdraws it (a hold, do-not-merge, blocked,
// needs-human, a human-merge label), and without the forgestate cache the
// lanes see a label only on their next tick, about two minutes on a
// twenty-repo hive and up to fifteen. GitHub's queue does not wait:
// projectbluefin/testsuite#1235 merged 71 seconds after a person added
// `hold` to it. The targeted pass runs the lane's own gate, so the label
// decides exactly as it would on a tick.
//
// Labels the hive's own App adds are skipped: the lanes wrote them, and a
// pass per stage-label write would only feed itself. Returns whether a pass
// was requested.
func (s *Server) nudgeQueueLabelEvent(event string, body []byte, res github.WebhookEventResult) bool {
	if event != "pull_request" || s == nil || s.deps == nil || s.deps.SweepNudge == nil || s.deps.Config == nil || !s.deps.Config.AutoMerge.NativeMergeQueue.Enabled {
		return false
	}
	var p struct {
		Action      string          `json:"action"`
		Sender      *gh.User        `json:"sender"`
		PullRequest *gh.PullRequest `json:"pull_request"`
		Repository  struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &p); err != nil || p.Action != "labeled" || p.PullRequest == nil || p.PullRequest.GetNumber() <= 0 {
		return false
	}
	if !strings.EqualFold(p.PullRequest.GetState(), "open") || p.PullRequest.GetMerged() {
		return false
	}
	repo := strings.TrimSpace(res.Repo)
	if repo == "" {
		repo = p.Repository.FullName
	}
	if !s.nativeQueueScopeAllowed(repo, p.PullRequest.GetBase().GetRef()) {
		return false
	}
	if app := s.webhookAppLogin(); app != "" && strings.EqualFold(strings.TrimSpace(p.Sender.GetLogin()), app) {
		return false
	}
	s.deps.SweepNudge(repo, p.PullRequest.GetNumber())
	return true
}

// webhookAppLogin is the hive's own App bot login, "" when unknown.
func (s *Server) webhookAppLogin() string {
	if s == nil || s.deps == nil || s.deps.GHClient == nil {
		return ""
	}
	return strings.TrimSpace(s.deps.GHClient.AppBotLogin())
}
