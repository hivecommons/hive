package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

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
	return &PRGate{gate: prgate.New(prGateAPI{c: c}, func(repo string) prgate.Mode {
		m, _ := prgate.ParseMode(modeOf(repo))
		return m
	}, log)}
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
