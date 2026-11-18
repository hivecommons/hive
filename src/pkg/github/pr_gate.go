package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/prgate"
)

// PRGate is the PR-contents gate (pkg/prgate) bound to this package's GitHub
// client. The dashboard talks to the gate only through it, so pkg/dashboard
// does not grow a dependency of its own.
type PRGate struct{ gate *prgate.Gate }

// prGateAPI adapts the Client to prgate.API.
type prGateAPI struct{ c *Client }

func (a prGateAPI) ReadSubject(ctx context.Context, repo string, number int) (prgate.Subject, error) {
	s, err := a.c.ReadPRGateSubject(ctx, repo, number)
	if err != nil {
		return prgate.Subject{}, err
	}
	return prgate.Subject{HeadSHA: s.HeadSHA, Title: s.Title, State: s.State, Draft: s.Draft}, nil
}

func (a prGateAPI) PostStatus(ctx context.Context, repo string, number int, sha, statusContext, state, description string) error {
	err := a.c.PostPRGateStatus(ctx, repo, number, sha, statusContext, state, description)
	if errors.Is(err, ErrPRGateStatusForbidden) {
		return fmt.Errorf("%w: %v", prgate.ErrForbidden, err)
	}
	return err
}

// NewPRGate builds the gate over c. modeOf returns the configured mode string
// of a repository ("off", "shadow" or "enforce"; anything else reads as off).
func NewPRGate(c *Client, modeOf func(repo string) string, log *slog.Logger) *PRGate {
	g := prgate.New(prGateAPI{c: c}, func(repo string) prgate.Mode {
		m, _ := prgate.ParseMode(modeOf(repo))
		return m
	}, log)
	g.Grant = func(ctx context.Context) (prgate.GrantInfo, error) {
		grant, err := c.ReadPRGateGrant(ctx)
		return prgate.GrantInfo{Applicable: grant.Applicable, Installation: grant.Installation, Level: grant.Level}, err
	}
	return &PRGate{gate: g}
}

// Causes a gate cannot be posted, as the spoke reports them to the hub.
const (
	PRGateCauseGrantMissing = "grant-missing"
	PRGateCauseWriteRefused = "write-refused"
)

// PRGateHealth is the gate's visible state on /api/status and the heartbeat.
// It is only present while at least one repository is configured for the gate.
type PRGateHealth struct {
	Configured string `json:"configured"`
	Effective  string `json:"effective"`
	// Cause is "" while the gate can post, else PRGateCauseGrantMissing or
	// PRGateCauseWriteRefused.
	Cause          string   `json:"cause,omitempty"`
	Installation   string   `json:"installation,omitempty"`
	Granted        string   `json:"granted,omitempty"`
	ForbiddenRepos []string `json:"forbiddenRepos,omitempty"`
	// DependsOnGate is true when a required context of a scoped repository is
	// one the gate posts, and the gate is (or from the cutoff will be) what
	// supplies it: a failure to post then stalls merges.
	DependsOnGate   bool   `json:"dependsOnGate"`
	DaysUntilCutoff int    `json:"daysUntilCutoff"`
	Cutoff          string `json:"cutoff"`
	// Message says what is missing and where an owner accepts it; set only
	// while Cause is.
	Message string `json:"message,omitempty"`
}

// Health summarises the gate over the configured scope. dependsOnRequired is
// whether any scoped repository's required checks include a gate context (or
// are unknown); now supplies the cutoff countdown.
func (p *PRGate) Health(repos []string, dependsOnRequired bool, now time.Time) *PRGateHealth {
	if p == nil {
		return nil
	}
	h := p.gate.HealthFor(repos)
	if h.Configured == prgate.ModeOff {
		return nil
	}
	out := &PRGateHealth{
		Configured: string(h.Configured), Effective: string(h.Effective),
		Installation: h.Installation, Granted: h.Granted, ForbiddenRepos: h.ForbiddenRepos,
		DaysUntilCutoff: prgate.DaysUntilCutoff(now), Cutoff: prgate.Cutoff.Format(time.DateOnly),
	}
	switch {
	case h.GrantMissing:
		out.Cause = PRGateCauseGrantMissing
		out.Message = prgate.MissingGrantHint
	case len(h.ForbiddenRepos) > 0:
		out.Cause = PRGateCauseWriteRefused
		out.Message = "GitHub refused the gate's commit status write (403) on: " + strings.Join(h.ForbiddenRepos, ", ") + ". " + prgate.MissingGrantHint
	}
	// Shadow posts a name no branch rule reads: it only stalls merges once the
	// workflow can no longer supply the required context (the cutoff), or when
	// the operator asked for enforce.
	out.DependsOnGate = dependsOnRequired && (h.Configured == prgate.ModeEnforce || !now.Before(prgate.Cutoff))
	return out
}

// GrantStale reports whether the installation grant is due for a re-read.
func (p *PRGate) GrantStale() bool { return p != nil && p.gate.GrantStale() }

// RefreshGrant re-reads the installation grant when it is stale; a fresh
// answer costs no request.
func (p *PRGate) RefreshGrant(ctx context.Context) {
	if p != nil {
		p.gate.RefreshGrant(ctx)
	}
}

// RegisterAsDefault makes this the gate the merge lanes consult for the held
// reason. Nil clears it.
func (p *PRGate) RegisterAsDefault() {
	if p == nil {
		prgate.SetDefault(nil)
		return
	}
	prgate.SetDefault(p.gate)
}

// IsPRGateCheckName reports whether name is one of the workflow-recorded
// checks the shadow comparison reads.
func IsPRGateCheckName(name string) bool {
	return name == prgate.ContextVerify || name == prgate.ContextPRType || name == prgate.ContextPRIssue
}

// Active reports whether repo is configured for the gate.
func (p *PRGate) Active(repo string) bool { return p != nil && p.gate.Active(repo) }

// TryAcquire reserves one of the bounded concurrent event slots.
func (p *PRGate) TryAcquire() bool { return p != nil && p.gate.TryAcquire() }

// Release frees a slot taken by TryAcquire.
func (p *PRGate) Release() { p.gate.Release() }

// HandleEvent processes one pull_request delivery for repo (owner/repo).
func (p *PRGate) HandleEvent(ctx context.Context, repo string, number int, eventHead string) prgate.Outcome {
	return p.gate.HandleEvent(ctx, repo, number, eventHead)
}

// CompareWorkflow records the workflow's conclusion for one gate check on a
// head against the hive's verdict; it reports whether a comparison was made.
func (p *PRGate) CompareWorkflow(repo, head, name, conclusion string) bool {
	_, ok := p.gate.CompareWorkflow(repo, head, name, conclusion)
	return ok
}

// Snapshot returns the per-repository stats.
func (p *PRGate) Snapshot() map[string]prgate.RepoStats { return p.gate.Snapshot() }
