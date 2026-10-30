package dashboard

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The decision ladder's inputs (hivecommons/hive#15289). "Let hive decide"
// on #15219 adopted a conflicting clanker PR in supersede mode although
// its issue #15202 had already been fixed and merged by #15240 2.7h
// earlier. The ladder in pkg/delegatedecision needs three cheap facts the
// gather did not read: whether a merged PR already delivered the work,
// what the author drives (clanker / hive / person, for the wording), and
// whether their relay has picked the rework up.

// gatherDelegatedLadderInput fills Input.SupersededBy, AuthorTooling,
// ReworkPickedUp and RepoPaused for a contributor PR. A supersession
// lookup failure aborts the decision: the fallback rung is adoption, and
// "unknown" must not read as "not superseded".
func (s *Server) gatherDelegatedLadderInput(ctx context.Context, repoFull string, pr *gh.PullRequest, in *delegatedecision.Input) error {
	client := s.deps.GHClient
	in.RepoPaused = client.RepoIsPaused(repoFull)
	in.AuthorTooling = s.delegatedAuthorTooling(in.Labels, in.OwnerAuthor)
	if in.HiveAuthored {
		return nil
	}
	sup, ok, err := client.DetectMergedSupersession(ctx, repoFull, pr)
	if err != nil {
		return fmt.Errorf("reading whether %s#%d is already fixed: %w", repoFull, pr.GetNumber(), err)
	}
	if ok {
		in.SupersededBy = &delegatedecision.MergedCloser{PR: sup.Closer, Issue: sup.Issue, SHA: sup.MergeSHA}
	}
	if !in.PersonAuthored() {
		in.ReworkPickedUp = s.delegatedReworkPickedUp(repoFull, pr, in.Author, time.Now())
	}
	return nil
}

// delegatedAuthorTooling reads the author-kind labels (names from
// governor.labels.author) into the wording the decision uses.
func (s *Server) delegatedAuthorTooling(labels []string, owner bool) delegatedecision.AuthorTooling {
	human, clanker, hive := authorKindLabelPfx+authorKindHuman, authorKindLabelPfx+authorKindClanker, authorKindLabelPfx+authorKindHive
	if s != nil && s.deps != nil && s.deps.Config != nil {
		a := s.deps.Config.Governor.Labels.Author
		if strings.TrimSpace(a.Human) != "" {
			human = a.Human
		}
		if strings.TrimSpace(a.Clanker) != "" {
			clanker = a.Clanker
		}
		if strings.TrimSpace(a.Hive) != "" {
			hive = a.Hive
		}
	}
	return delegatedecision.AuthorToolingFromLabels(labels, hive, clanker, human, owner)
}

// delegatedReworkPickedUp reads the rework-pickup signal of
// hivecommons/hive#15280 for the PR's author: their relay holds or queued
// the fix task, someone acted on the PR since the request, or a new head
// is up. It rests on the stage record and the hub's fix queue; no GitHub
// call. "requested" (nobody has acted) is not a pickup.
func (s *Server) delegatedReworkPickedUp(repoFull string, pr *gh.PullRequest, author string, now time.Time) bool {
	rec, ok := s.stageStoreOrShared().Get(stagelabels.Key(repoFull, pr.GetNumber()))
	if !ok {
		return false
	}
	it := StageTrackerItem{Repo: repoFull, Kind: "pr", Number: pr.GetNumber(), Head: strings.TrimSpace(pr.GetHead().GetSHA())}
	if u := pr.GetUpdatedAt(); !u.IsZero() {
		t := u.Time
		it.UpdatedAt = &t
	}
	rw := stageTrackerReworkFor(it, author, stagelabels.ReturnedToContributor, rec, true, s.stageTrackerFixWorkLookup(), now)
	if rw == nil {
		return false
	}
	switch rw.State {
	case reworkStateAcknowledged, reworkStateInProgress, reworkStatePushed:
		return true
	}
	return false
}

// logDelegatedLadder writes the one structured line per decision that says
// which rung decided and why the rungs above it did not apply.
func (s *Server) logDelegatedLadder(repoFull string, number int, d delegatedecision.Decision) {
	if d.Ladder == nil || s == nil || s.logger == nil {
		return
	}
	s.logger.Info("delegated decision ladder",
		slog.String("repo", repoFull), slog.Int("number", number),
		slog.String("decision", d.Ladder.Decision), slog.String("reason", d.Ladder.Reason),
		slog.Any("ruled_out", d.Ladder.RuledOut), slog.String("verdict", string(d.Verdict)))
}

// delegatedLadderOutcome is the toast's outcome line for a ladder decision
// that the generic verdict text would understate: "Closed as superseded:
// #15202 already fixed by #15240 (25de3ba)". ok is false when the generic
// text stands.
func delegatedLadderOutcome(in delegatedecision.Input, d delegatedecision.Decision) (string, bool) {
	switch d.Rule {
	case delegatedecision.RuleAlreadyFixed:
		c := in.SupersededBy
		if c == nil || d.Verdict != delegatedecision.VerdictCloseSuperseded {
			return "", false
		}
		ref := fmt.Sprintf("#%d", c.PR)
		if c.SHA != "" {
			ref += " (" + delegatedShortSHA(c.SHA) + ")"
		}
		if c.Issue > 0 {
			return fmt.Sprintf("Closed as superseded: #%d already fixed by %s.", c.Issue, ref), true
		}
		return fmt.Sprintf("Closed as superseded by merged %s.", ref), true
	case delegatedecision.RuleContributorOffline:
		if d.Verdict != delegatedecision.VerdictAdoptSupersede {
			return "", false
		}
		// Appended to the adopt outcome as its "Why:", naming the author's
		// clanker, hive or the author — never the wrong one.
		return "Why: " + in.OfflineWording() + ".", true
	}
	return "", false
}

func delegatedShortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
