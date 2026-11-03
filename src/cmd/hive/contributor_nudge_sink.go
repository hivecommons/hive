package main

import (
	"context"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github/automerge"
)

// lateContributorNudgeSink resolves the contribute hub at call time. The
// self-authored sweep's automerge.Options are built in bootAgents, which runs
// before bootDashboard, so b.dashSrv is still nil there and the engine that
// reconciles stage labels every sweep had NO nudge sink: every stage-072
// nudge it posted was mode=comment, online=false, queue_depth=0 even for a
// trusted relay contributor whose relay was connected minutes later
// (hivecommons/hive #12428, #12471). The marker then deduped the nudge, so
// the sweep that did have the sink never re-queued the fix.
type lateContributorNudgeSink struct {
	hub func() *dashboard.ContributeWSHub
}

func (s lateContributorNudgeSink) Nudge(ctx context.Context, n automerge.ContributorNudge) automerge.ContributorNudgeOutcome {
	if h := s.hub(); h != nil {
		return h.Nudge(ctx, n)
	}
	return automerge.ContributorNudgeOutcome{}
}

// Presence implements automerge.ContributorPresenceSource by delegating to
// the hub. Without this the engine's type assertion failed (the hub has the
// method, the wrapper did not), so adoption never saw live presence and
// every clanker PR waited the nudge-time grace instead of the offline wait.
func (s lateContributorNudgeSink) Presence(ctx context.Context, login string) automerge.ContributorPresence {
	if h := s.hub(); h != nil {
		return h.Presence(ctx, login)
	}
	return automerge.ContributorPresence{}
}

var _ automerge.ContributorPresenceSource = lateContributorNudgeSink{}

func (s lateContributorNudgeSink) Resolve(ctx context.Context, owner, repo string, number int) {
	if h := s.hub(); h != nil {
		h.Resolve(ctx, owner, repo, number)
	}
}

// Abandoned implements automerge.ContributorAbandonSink so every adoption
// the sweep performs lands on the author's contributor profile
// (hivecommons/hive#15782); without the forwarder the engine's type
// assertion fails on the wrapper, as it once did for Presence.
func (s lateContributorNudgeSink) Abandoned(ctx context.Context, a automerge.ContributorAbandon) {
	if h := s.hub(); h != nil {
		h.Abandoned(ctx, a)
	}
}

var _ automerge.ContributorAbandonSink = lateContributorNudgeSink{}
