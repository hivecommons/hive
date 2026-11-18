package prgate

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Timing and wording constants of the grant check and the runtime failure
// record.
const (
	// GrantTTL is how long a read of the installation's granted permissions
	// is trusted before the gate reads it again.
	GrantTTL = 15 * time.Minute
	// ForbiddenCooldown is how long a repository that answered a status write
	// with 403 is left alone. The refusal is recorded once and not retried in
	// a loop; the next attempt also re-reads the grant.
	ForbiddenCooldown = 15 * time.Minute
	// GrantWrite and GrantAdmin are the permission levels that allow a write.
	GrantWrite = "write"
	GrantAdmin = "admin"

	// SkipGrantMissing and SkipForbiddenCooldown are skip reasons.
	SkipGrantMissing      = "grant-missing"
	SkipForbiddenCooldown = "forbidden-cooldown"

	// StepPostStatus is the `unauthorized:<step>` name of the held reason a
	// PR shows when the gate cannot be posted on it.
	StepPostStatus = "post-pr-gate-status"

	// MissingGrantHint says what is missing and where an owner accepts it.
	MissingGrantHint = "The Hive GitHub App installation has no Commit statuses read and write grant. " +
		"An organization owner accepts it in the organization settings: GitHub Apps, the App's pending permission request."
)

// Cutoff is the date from which GitHub blocks pull_request_target workflows by
// default in public repositories.
var Cutoff = time.Date(2026, time.December, 2, 0, 0, 0, 0, time.UTC)

const hoursPerDay = 24

// DaysUntilCutoff is the whole days left before Cutoff at now, never negative.
func DaysUntilCutoff(now time.Time) int {
	if !now.Before(Cutoff) {
		return 0
	}
	return int(Cutoff.Sub(now) / (hoursPerDay * time.Hour))
}

// GrantInfo is what a grant check learned about the installation.
type GrantInfo struct {
	// Applicable is false for credentials that carry no App installation
	// grants (a personal access token): the runtime failure record still
	// applies, the grant check does not.
	Applicable   bool
	Installation string
	// Level is the granted Commit statuses level: "write", "admin", "read" or "".
	Level string
}

// Missing reports whether an applicable installation cannot write statuses.
func (i GrantInfo) Missing() bool {
	return i.Applicable && i.Level != GrantWrite && i.Level != GrantAdmin
}

// GrantProvider reads the installation's current grant.
type GrantProvider func(ctx context.Context) (GrantInfo, error)

type grantState struct {
	info       GrantInfo
	checkedAt  time.Time
	checked    bool
	refreshing bool
	err        string
}

// RefreshGrant reads the installation grant when the last read is older than
// GrantTTL (or was invalidated by a 403). Concurrent callers share one read.
// A failed read keeps the last known answer: an unknown grant never forces
// the gate off by itself, and never promotes it either.
func (g *Gate) RefreshGrant(ctx context.Context) {
	if g == nil || g.Grant == nil {
		return
	}
	g.mu.Lock()
	if g.grant.refreshing || (g.grant.checked && g.Now().Sub(g.grant.checkedAt) < GrantTTL) {
		g.mu.Unlock()
		return
	}
	g.grant.refreshing = true
	g.mu.Unlock()

	info, err := g.Grant(ctx)

	g.mu.Lock()
	defer g.mu.Unlock()
	g.grant.refreshing = false
	g.grant.checkedAt = g.Now()
	g.grant.checked = true
	if err != nil {
		g.grant.err = err.Error()
		g.Log.Warn("pr-gate: could not read the installation grant, keeping the last known answer", "error", err)
		return
	}
	g.grant.err = ""
	was := g.grant.info.Missing()
	g.grant.info = info
	switch {
	case info.Missing():
		g.Log.Warn("pr-gate: the installation has no Commit statuses write grant, the gate is forced off",
			"installation", info.Installation, "granted", grantLabel(info.Level), "hint", MissingGrantHint)
	case was:
		g.Log.Info("pr-gate: the installation now has the Commit statuses write grant", "installation", info.Installation)
	}
}

func grantLabel(level string) string {
	if strings.TrimSpace(level) == "" {
		return "none"
	}
	return level
}

// invalidateGrantLocked makes the next RefreshGrant read again. g.mu is held.
func (g *Gate) invalidateGrantLocked() { g.grant.checked = false }

// GrantStale reports whether a grant read is due: a provider is set and the
// last read is missing, invalidated or older than GrantTTL, and none is in flight.
func (g *Gate) GrantStale() bool {
	if g == nil || g.Grant == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.grant.refreshing && (!g.grant.checked || g.Now().Sub(g.grant.checkedAt) >= GrantTTL)
}

// GrantMissing reports whether the last grant check found the write grant
// missing.
func (g *Gate) GrantMissing() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.grant.info.Missing()
}

// Health is the gate's visible state for one hive: what the banner, the hub
// verdict and the held reason read.
type Health struct {
	Configured   Mode
	Effective    Mode
	GrantMissing bool
	Installation string
	Granted      string
	// ForbiddenRepos are the repositories whose last status write was refused.
	ForbiddenRepos []string
	// CannotPost is true when the grant is missing or a repository is refused.
	CannotPost bool
}

// ConfiguredMode is the configured mode of repo with enforce NOT clamped and
// the grant check NOT applied: what the operator asked for.
func (g *Gate) ConfiguredMode(repo string) Mode {
	if g == nil || g.ModeOf == nil {
		return ModeOff
	}
	return g.ModeOf(repo)
}

// RepoForbidden reports whether repo's last status write was refused within
// ForbiddenCooldown.
func (g *Gate) RepoForbidden(repo string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.repoForbiddenLocked(repo)
}

func (g *Gate) repoForbiddenLocked(repo string) bool {
	s := g.stats[strings.ToLower(repo)]
	return s != nil && g.Now().Before(s.forbiddenUntil)
}

// HealthFor summarises the gate over repos (the configured scope).
func (g *Gate) HealthFor(repos []string) Health {
	h := Health{Configured: ModeOff, Effective: ModeOff}
	if g == nil {
		return h
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	h.GrantMissing = g.grant.info.Missing()
	h.Installation = g.grant.info.Installation
	h.Granted = grantLabel(g.grant.info.Level)
	for _, repo := range repos {
		if g.ModeOf != nil {
			if m := g.ModeOf(repo); m != ModeOff {
				if h.Configured == ModeOff || m == ModeEnforce {
					h.Configured = m
				}
			}
		}
		if g.repoForbiddenLocked(repo) {
			h.ForbiddenRepos = append(h.ForbiddenRepos, repo)
		}
	}
	h.Effective = Effective(h.Configured)
	if h.GrantMissing {
		h.Effective = ModeOff
	}
	h.CannotPost = h.Configured != ModeOff && (h.GrantMissing || len(h.ForbiddenRepos) > 0)
	return h
}

// HoldReason returns the `unauthorized:<step>` reason a PR must show instead
// of a bare required-check-missing when its required gate context cannot be
// posted by the hive: the repository is in enforce mode (as configured, so a
// grant check that forced the gate off does not hide it) and either the grant
// is missing or the last write was refused. Empty means nothing to add.
func (g *Gate) HoldReason(repo, statusContext string) string {
	if g == nil || !isGateRequiredContext(statusContext) {
		return ""
	}
	if g.ConfiguredMode(repo) != ModeEnforce {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.grant.info.Missing() || g.repoForbiddenLocked(repo) {
		return "unauthorized:" + StepPostStatus
	}
	return ""
}

func isGateRequiredContext(name string) bool {
	return name == ContextVerify || name == ContextPRType || name == ContextPRIssue
}

// Describe renders the Health in one line for logs.
func (h Health) Describe() string {
	return fmt.Sprintf("configured=%s effective=%s grant_missing=%t forbidden_repos=%d", h.Configured, h.Effective, h.GrantMissing, len(h.ForbiddenRepos))
}
