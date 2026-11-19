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
		// The merge lanes read the gate for the held reason of a PR whose
		// required context the hive cannot post.
		s.prGate.RegisterAsDefault()
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

// prGateGrantRefreshTimeout bounds one installation grant read.
const prGateGrantRefreshTimeout = 30 * time.Second

// prGateScope returns the repositories auto_merge.pr_gate applies to.
func (s *Server) prGateScope() []string {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return nil
	}
	var scope []string
	for _, repo := range s.deps.Config.AutoMerge.PRGate.Repos {
		if repo = strings.TrimSpace(repo); repo != "" {
			scope = append(scope, repo)
		}
	}
	return scope
}

// prGateDependsOnRequired reports whether a required context of any scoped
// repository is one the gate posts. A repository whose required set is not
// declared in config counts as depending on it: the operator listed it for the
// gate because its merge check comes from the gate's workflow.
func (s *Server) prGateDependsOnRequired(scope []string) bool {
	am := s.deps.Config.AutoMerge
	for _, repo := range scope {
		set, known := am.RequiredCheckSetForRepo(repo)
		if !known {
			return true
		}
		for name := range set {
			if github.IsPRGateCheckName(name) {
				return true
			}
		}
	}
	return false
}

// PRGateHealth is the gate's state for /api/status, the spoke banner and the
// hub heartbeat. It is nil unless at least one repository is configured for
// the gate, so a hive that never sets auto_merge.pr_gate reports nothing and
// makes no request. A stale installation grant is re-read in the background.
func (s *Server) PRGateHealth() *github.PRGateHealth {
	scope := s.prGateScope()
	if len(scope) == 0 || s.deps == nil || s.deps.Config == nil {
		return nil
	}
	active := false
	for _, repo := range scope {
		if s.prGateModeOf(repo) != config.PRGateModeOff {
			active = true
			break
		}
	}
	if !active {
		return nil
	}
	g := s.prGateInstance()
	if g == nil {
		return nil
	}
	if g.GrantStale() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), prGateGrantRefreshTimeout)
			defer cancel()
			g.RefreshGrant(ctx)
		}()
	}
	return g.Health(scope, s.prGateDependsOnRequired(scope), time.Now())
}

// PRGateCause is the heartbeat view of PRGateHealth: why the gate cannot be
// posted ("" when it can, or is not configured) and whether a required
// context depends on it.
func (s *Server) PRGateCause() (cause string, dependsOn bool) {
	h := s.PRGateHealth()
	if h == nil {
		return "", false
	}
	return h.Cause, h.DependsOnGate
}
