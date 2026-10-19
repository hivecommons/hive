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
// stage labeler. Each submission re-reads the item first (enrichStageFacts)
// so the derivation and the write plan see the labels and state GitHub has
// now, not the ones the caller remembers.
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
		Writer: clientStageWriter{c: c},
		Issue:  IssueDeriver(),
		PR:     stageengine.PRDeriver(),
		Logger: c.logger,
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

// clientStageWriter is the stageengine.StageWriter behind the default sink
// until the single StageWriter of plan PR-C owns every write: it is
// stagelabels.Enforce over Client.StageLabeler, i.e. the same journaled
// read-then-write the retired claim-hook helper used. An out-of-cycle
// Decision carries no role or health opinion (its deriver saw a fact, not
// the lanes), so the role and `stalled` the item already wears are kept,
// exactly as the retired EnforceStage path kept them.
type clientStageWriter struct{ c *Client }

func (w clientStageWriter) Apply(ctx context.Context, ref stageengine.Ref, hint []string, want stagelabels.Desired) (stagelabels.Outcome, error) {
	if w.c == nil {
		return stagelabels.Outcome{}, ErrNoGitHubClient
	}
	if want.Role == stagelabels.RoleNone {
		want.Role = stagelabels.RoleOf(hint)
	}
	if !want.Stalled && issueHasLabel(hint, stagelabels.StalledLabel) {
		want.Stalled = true
	}
	return stagelabels.Enforce(ctx, w.c.StageLabeler(), ref.Kind, ref.Owner, ref.Repo, ref.Number, hint, want)
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
