package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/prgate"
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

// prGateCheckNames are the workflow-recorded checks the shadow comparison reads.
var prGateCheckNames = map[string]bool{
	prgate.ContextVerify: true, prgate.ContextPRType: true, prgate.ContextPRIssue: true,
}

// prGateAPI adapts the GitHub client to prgate.API.
type prGateAPI struct{ c *github.Client }

func (a prGateAPI) ReadSubject(ctx context.Context, repo string, number int) (prgate.Subject, error) {
	s, err := a.c.ReadPRGateSubject(ctx, repo, number)
	if err != nil {
		return prgate.Subject{}, err
	}
	return prgate.Subject{HeadSHA: s.HeadSHA, Title: s.Title, State: s.State, Draft: s.Draft}, nil
}

func (a prGateAPI) PostStatus(ctx context.Context, repo string, number int, sha, statusContext, state, description string) error {
	err := a.c.PostPRGateStatus(ctx, repo, number, sha, statusContext, state, description)
	if errors.Is(err, github.ErrPRGateStatusForbidden) {
		return fmt.Errorf("%w: %v", prgate.ErrForbidden, err)
	}
	return err
}

// prGateModeOf reads auto_merge.pr_gate for one repository, live.
func (s *Server) prGateModeOf(repo string) prgate.Mode {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return prgate.ModeOff
	}
	mode, _ := prgate.ParseMode(s.deps.Config.AutoMerge.PRGate.ModeFor(repo))
	return mode
}

func (s *Server) prGateInstance() *prgate.Gate {
	s.prGateOnce.Do(func() {
		if s.deps == nil || s.deps.GHClient == nil {
			return
		}
		s.prGate = prgate.New(prGateAPI{c: s.deps.GHClient}, s.prGateModeOf, s.webhookLogger())
	})
	return s.prGate
}

// observePRGateEvent feeds one verified webhook delivery to the PR gate. With
// the mode off (the default) it returns before any allocation or API call.
// `pull_request` deliveries post the gate on the event's head; `check_run`
// deliveries of the workflow's own checks are compared with the hive's verdict
// for that head. The work runs off the request goroutine and is bounded; a
// dropped burst is picked up by the backstop sweep.
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
		if p.Action == "completed" && p.CheckRun != nil && prGateCheckNames[p.CheckRun.Name] {
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
		s.webhookLogger().Warn("pr-gate: event dropped, too many in flight; the backstop covers it", "repo", repo, "number", number)
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
