package dashboard

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// prGateEventTimeout bounds one gate pass (a PR re-read plus at most three
// status writes) so a slow GitHub never pins an event slot.
const prGateEventTimeout = 30 * time.Second

// prGatePRActions are the pull_request actions that can change what the gate
// would post: a new or reopened PR, a new head, an edited title, a PR leaving
// draft, and label changes (the workflow listens to the same set).
var prGatePRActions = map[string]bool{
	"opened": true, "edited": true, "synchronize": true, "reopened": true,
	"ready_for_review": true, "labeled": true, "unlabeled": true,
}

// prGateModeOf reads auto_merge.pr_gate for one repository, live.
func (s *Server) prGateModeOf(repo string) string {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return config.PRGateModeOff
	}
	return s.deps.Config.AutoMerge.PRGate.ModeFor(repo)
}

func (s *Server) prGateInstance() *github.PRGate {
	s.prGateOnce.Do(func() {
		if s.deps == nil || s.deps.GHClient == nil {
			return
		}
		s.prGate = github.NewPRGate(s.deps.GHClient, s.prGateModeOf, s.webhookLogger())
	})
	return s.prGate
}

// observePRGateEvent feeds one verified webhook delivery to the PR gate. With
// the mode off (the default) it returns before any allocation or API call.
// `pull_request` deliveries post the gate on the event's head; `check_run`
// deliveries of the workflow's own checks are compared with the hive's verdict
// for that head. The work runs off the request goroutine and is bounded; a
// dropped burst is logged.
func (s *Server) observePRGateEvent(event string, body []byte) string {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return ""
	}
	cfg := s.deps.Config.AutoMerge.PRGate
	if cfg.Mode == "" || strings.EqualFold(strings.TrimSpace(cfg.Mode), config.PRGateModeOff) {
		return ""
	}
	if event != "pull_request" && event != "check_run" {
		return ""
	}
	var p struct {
		Action     string `json:"action"`
		Number     int    `json:"number"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		PullRequest *struct {
			Number int `json:"number"`
			Head   struct {
				SHA string `json:"sha"`
			} `json:"head"`
		} `json:"pull_request"`
		CheckRun *struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
			HeadSHA    string `json:"head_sha"`
		} `json:"check_run"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return ""
	}
	repo := p.Repository.FullName
	g := s.prGateInstance()
	if g == nil || !g.Active(repo) {
		return ""
	}
	if event == "check_run" {
		if p.Action == "completed" && p.CheckRun != nil && github.IsPRGateCheckName(p.CheckRun.Name) {
			g.CompareWorkflow(repo, p.CheckRun.HeadSHA, p.CheckRun.Name, p.CheckRun.Conclusion)
			return "compared"
		}
		return ""
	}
	if !prGatePRActions[p.Action] || p.PullRequest == nil {
		return ""
	}
	number := p.PullRequest.Number
	if number <= 0 {
		number = p.Number
	}
	head := p.PullRequest.Head.SHA
	if number <= 0 || !g.TryAcquire() {
		s.webhookLogger().Warn("pr-gate: event dropped, too many in flight; a later event or sweep covers it", "repo", repo, "number", number)
		return "dropped"
	}
	go func() {
		defer g.Release()
		ctx, cancel := context.WithTimeout(context.Background(), prGateEventTimeout)
		defer cancel()
		g.HandleEvent(ctx, repo, number, head)
	}()
	return "scheduled"
}
