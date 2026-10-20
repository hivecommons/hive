package github

import (
	"context"
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// stageFactsState is the Client's stage FactSink: the one path an
// out-of-cycle writer (claim hooks, in-progress start marks, PR claim
// tracking, parking, /hive approve) may take to change an item's stage
// label (ADR-0025). Writers submit the fact they learned; the engine
// derives the stage and writes it. No code in this package adds or removes
// a `stage/*` label on its own any more.
type stageFactsState struct {
	mu   sync.Mutex
	sink stageengine.FactSink
}

// SetStageFactSink installs the FactSink the client's out-of-cycle writers
// submit facts to. nil restores the default (see StageFacts). It is the
// constructor option for tests and for a hub that already owns a
// reconciler; the hub's default wiring needs nothing here.
func (c *Client) SetStageFactSink(sink stageengine.FactSink) {
	if c == nil {
		return
	}
	c.stageFacts.mu.Lock()
	defer c.stageFacts.mu.Unlock()
	c.stageFacts.sink = sink
}

// StageFacts returns the FactSink the client's out-of-cycle writers use.
// The default is the engine's FactSink over an enforce-mode Reconciler that
// shares stagelabels.Shared() with the enumeration cycle, derives with this
// package's issue deriver (the PR side delegates to whatever cmd/hive
// registered via stageengine.SetDerivers) and writes through the client's
// one StageWriter (stage_writer.go), so a fact racing the sweep on an item
// queues behind it. Each submission re-reads the item first
// (enrichStageFacts) so the derivation and the write plan see the labels
// and state GitHub has now, not the ones the caller remembers.
func (c *Client) StageFacts() stageengine.FactSink {
	if c == nil {
		return nil
	}
	c.stageFacts.mu.Lock()
	defer c.stageFacts.mu.Unlock()
	if c.stageFacts.sink == nil {
		c.stageFacts.sink = c.defaultStageFactSink()
	}
	return c.stageFacts.sink
}

func (c *Client) defaultStageFactSink() stageengine.FactSink {
	r := stageengine.New(stageengine.Options{
		Mode:   stageengine.ModeEnforce,
		Store:  stagelabels.Shared(),
		Writer: c.StageWriter(),
		Issue:  IssueDeriver(),
		PR:     stageengine.PRDeriver(),
		Logger: c.logger,
		// Shares the process counters with the sweeps' reconciler so
		// /metrics and /api/status count fact-driven writes too.
		Metrics: stageengine.DefaultCounters(),
	})
	return stageengine.NewFactSink(r, stageengine.FactSinkOptions{Enrich: c.enrichStageFacts})
}

// stageRef builds the engine's Ref for an item of this client.
func (c *Client) stageRef(repo string, number int, kind stagelabels.Item) stageengine.Ref {
	owner, name := c.splitRepo(repo)
	return stageengine.Ref{Owner: owner, Repo: name, Number: number, Kind: kind}
}

// IssueStageRef is the engine Ref for an issue of repo ("owner/name", or a
// bare name under the client's org).
func (c *Client) IssueStageRef(repo string, number int) stageengine.Ref {
	return c.stageRef(repo, number, stagelabels.IssueItem)
}

// PRStageRef is the engine Ref for a pull request of repo.
func (c *Client) PRStageRef(repo string, number int) stageengine.Ref {
	return c.stageRef(repo, number, stagelabels.PRItem)
}

// enrichStageFacts is the sink's Enrich hook: a fresh read of the item tops
// the minimal Facts up with what GitHub has now — its labels (the park
// family, likely-done, the current stage), whether it is still open, and
// whether the "issue" is really a pull request (a Ref built from an issue
// number can name either; the stage families differ). An un-park fact
// drops the decision-park labels from the labels it reports, since the
// caller removes them in the same breath and the derivation must not see
// the item as still parked.
func (c *Client) enrichStageFacts(ctx context.Context, f stageengine.Facts) (stageengine.Facts, error) {
	if c == nil || c.client == nil {
		return f, ErrNoGitHubClient
	}
	issue, _, err := c.client.Issues.Get(ctx, f.Ref.Owner, f.Ref.Repo, f.Ref.Number)
	if err != nil {
		return f, err
	}
	fresh := extractLabels(issue.Labels)
	if f.Park.ApprovedDirection {
		fresh = withoutLabels(fresh, unparkRemovableLabels)
	}
	f.Labels = unionLabels(fresh, f.Labels)
	f.Open = !strings.EqualFold(issue.GetState(), "closed")
	f.Merged = !issue.GetPullRequestLinks().GetMergedAt().IsZero()
	if issue.IsPullRequest() && f.Ref.Kind != stagelabels.PRItem {
		f.Ref.Kind = stagelabels.PRItem
		f.Issue = nil
		if f.PR == nil {
			f.PR = &stageengine.PRFacts{}
		}
	}
	f.Park.Holds = stagelabels.Holds(f.Labels)
	f.Park.NeedsHuman = f.Park.NeedsHuman || hasIssueNeedsHumanLabel(f.Labels)
	if f.Park.DecisionLabel == "" {
		if label := hardSuppressIssueLabel(f.Labels); label != "" && !strings.EqualFold(label, issueNeedsHumanLabel) {
			f.Park.DecisionLabel = label
		}
	}
	if f.Issue != nil {
		f.Issue.LikelyDone = issueHasLabel(f.Labels, LikelyDoneLabel)
		f.Issue.Stalled = issueHasLabel(f.Labels, stagelabels.StalledLabel)
	}
	if f.Author.Login == "" {
		f.Author.Login = issue.GetUser().GetLogin()
	}
	return f, nil
}

// unionLabels is fresh followed by the extra labels it lacks. Stage
// spellings among the extras are dropped: the fact names the stage the
// engine should derive (a park label such as stage/045-needs-decision), not
// a stage the item already wears — GitHub's labels are the only current
// stage.
func unionLabels(fresh, extra []string) []string {
	out := append([]string(nil), fresh...)
	for _, l := range extra {
		l = strings.TrimSpace(l)
		if l == "" || stagelabels.IsStageLabel(strings.ToLower(l)) {
			continue
		}
		if !issueHasLabel(out, l) {
			out = append(out, l)
		}
	}
	return out
}

// withoutLabels is labels minus any (case-insensitive) member of drop.
func withoutLabels(labels, drop []string) []string {
	out := labels[:0:0]
	for _, l := range labels {
		if !issueHasLabel(drop, l) {
			out = append(out, l)
		}
	}
	return out
}
