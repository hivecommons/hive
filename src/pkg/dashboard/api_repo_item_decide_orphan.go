package dashboard

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// "Let hive decide" on an orphaned adoption (hivecommons/hive#15052).
//
// The delegate's dedup answers a repeat click with "hive already recorded
// this decision (rule contributor-offline)" when the rule's comment is on
// the PR. On hivecommons/hive#14947 that decision had been carried out —
// the hive's copy #14977 opened — and then undone by the duplicate-fix
// sweep closing #14977 unmerged; the recorded decision no longer stood, and
// the answer was wrong. The delegate now reads the PR's adoption markers
// first: a supersede marker whose copy is closed without merging is an
// orphan, and the decision is to re-adopt, under its own rule so the dedup
// keys on the fresh copy rather than the dead one.

// Rules the orphan path decides under.
const (
	delegatedRuleAdoptionOrphaned = "adoption-orphaned"
	delegatedRuleAdoptionLooped   = stagelabels.LaneReasonAdoptionChildClosedTwice
)

// orphanedAdoption is what delegatedOrphanedAdoption found.
type orphanedAdoption struct {
	Dead     int       // the closed, unmerged copy
	MarkerAt time.Time // when the adoption that opened it was recorded
	Looped   bool      // copies were closed twice within the hour
}

// delegatedOrphanedAdoption reports whether the PR wears a live supersede
// marker naming a copy that is closed without merging. Read failures read
// as "not orphaned": the ordinary delegate path then runs.
func (s *Server) delegatedOrphanedAdoption(ctx context.Context, repoFull string, number int, in delegatedecision.Input, now time.Time) (orphanedAdoption, bool) {
	if in.Kind != delegatedecision.KindPR || in.HiveAuthored || s.deps == nil || s.deps.GHClient == nil || s.deps.GHClient.GoGitHub() == nil {
		return orphanedAdoption{}, false
	}
	owner, repoName, _ := strings.Cut(repoFull, "/")
	api := s.deps.GHClient.GoGitHub()
	comments, _, err := api.Issues.ListComments(ctx, owner, repoName, number, &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: delegatedDecisionGatherLimit}})
	if err != nil {
		return orphanedAdoption{}, false
	}
	scan := automerge.ScanAdoptionMarkers(comments)
	if !scan.Live {
		return orphanedAdoption{}, false
	}
	sup, _, err := api.PullRequests.Get(ctx, owner, repoName, scan.Marker.SupersededBy)
	if err != nil || sup.GetMerged() || !strings.EqualFold(sup.GetState(), "closed") {
		return orphanedAdoption{}, false
	}
	out := orphanedAdoption{Dead: scan.Marker.SupersededBy, MarkerAt: scan.Marker.At, Looped: automerge.AdoptionLoopFromMarkers(scan, now)}
	if store := s.stageStoreOrShared(); store != nil {
		if rec, ok := store.Get(stagelabels.Key(repoFull, number)); ok && rec.AdoptionChildClosedTwice() {
			out.Looped = true
		}
	}
	return out, true
}

// decision is the delegate's answer for the orphan: re-adopt, or — when
// the copies keep being closed — a recommendation a person must act on.
func (o orphanedAdoption) decision(ref string) delegatedecision.Decision {
	if o.Looped {
		text := fmt.Sprintf("the hive's copies of %s were closed without merging twice within an hour (last #%d); something keeps closing them, so the hive is not opening a third — reopen a copy, close this PR, or say what should happen", ref, o.Dead)
		return delegatedecision.Decision{
			Verdict: delegatedecision.VerdictRecommendOnly, Rule: delegatedRuleAdoptionLooped, Text: text, KeepsPark: true,
			Rationale: "a copy closed twice within the hour is a loop the hive must not feed (hivecommons/hive#15052)",
			Actions:   []delegatedecision.Action{{Kind: delegatedecision.ActionRecommendOnly, Text: text}},
		}
	}
	text := fmt.Sprintf("re-adopt %s: the hive's earlier copy #%d was closed without merging, so the adoption is re-run from the same head as a fresh copy", ref, o.Dead)
	return delegatedecision.Decision{
		Verdict: delegatedecision.VerdictAdoptSupersede, Rule: delegatedRuleAdoptionOrphaned, Text: text,
		Rationale: "the recorded supersede decision no longer stands once its copy is closed unmerged (hivecommons/hive#15052)",
		Actions:   []delegatedecision.Action{{Kind: delegatedecision.ActionCommentDecision, Text: text}, {Kind: delegatedecision.ActionAdoptSupersede, Target: o.Dead}},
	}
}

// delegatedOrphanOutcome is the row result text for a re-adoption.
func delegatedOrphanOutcome(dead int) string {
	return fmt.Sprintf("Re-adopting: hive's earlier copy #%d was closed by mistake; opening a fresh copy.", dead)
}
