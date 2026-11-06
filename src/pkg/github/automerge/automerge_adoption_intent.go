package automerge

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Intent check for a hive push on an in-place adopted PR
// (projectbluefin/utah#1091). The adopted fixer is there to get the PR's own
// change merged. On utah#1091 it made CI green by re-adding the 25
// baselines/triage.toml entries the PR existed to remove, leaving a docs-only
// diff, and the PR was one green run from merging. Each hive push is checked
// before the head is taken as the hive's: the PR's net diff against its base
// must keep at least half of the lines it changed before the push.

// adoptionIntentMaxFiles is the compare page GitHub returns at most; a diff
// that fills it is not judged (its tail is unknown), and the push stands.
const adoptionIntentMaxFiles = 300

// adoptionIntent is one push's verdict: Own is how many changed lines the PR
// carried before the push, Kept how many of them its diff still carries.
type adoptionIntent struct {
	Own, Kept int
	// Unjudged is set when the diff could not be judged (too large).
	Unjudged bool
}

// Reverts reports whether the push undid most of the PR's own change.
func (a adoptionIntent) Reverts() bool {
	return !a.Unjudged && a.Own > 0 && a.Kept*2 < a.Own
}

// prChangeUnits is the PR's change as a multiset: one unit per added or
// removed line of each file's patch ("path\x00+text"). A file GitHub sends
// without a patch (binary, too large) counts as one unit for its status.
func prChangeUnits(files []*gh.CommitFile) map[string]int {
	units := map[string]int{}
	for _, f := range files {
		name := f.GetFilename()
		patch := f.GetPatch()
		if patch == "" {
			units[name+"\x00file:"+f.GetStatus()]++
			continue
		}
		for _, line := range strings.Split(patch, "\n") {
			if line == "" || strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
				continue
			}
			if line[0] == '+' || line[0] == '-' {
				units[name+"\x00"+line[:1]+strings.TrimSpace(line[1:])]++
			}
		}
	}
	return units
}

// judgeAdoptionIntent compares the PR's change before the push with its
// change after.
func judgeAdoptionIntent(before, after []*gh.CommitFile) adoptionIntent {
	if len(before) >= adoptionIntentMaxFiles || len(after) >= adoptionIntentMaxFiles {
		return adoptionIntent{Unjudged: true}
	}
	own, now := prChangeUnits(before), prChangeUnits(after)
	var out adoptionIntent
	for unit, n := range own {
		out.Own += n
		out.Kept += min(n, now[unit])
	}
	return out
}

// adoptedPushIntent reads the PR's diff against its base (three-dot, so a
// merge of the base into the branch is not counted) at the head before the
// push and at the pushed head.
func (c *Engine) adoptedPushIntent(ctx context.Context, obs stageObservation, before string) (adoptionIntent, error) {
	if obs.BaseRef == "" || before == "" || obs.HeadSHA == "" {
		return adoptionIntent{Unjudged: true}, nil
	}
	files := func(head string) ([]*gh.CommitFile, error) {
		cmp, _, err := c.gh.Repositories.CompareCommits(ctx, obs.Owner, obs.Repo, obs.BaseRef, head, &gh.ListOptions{PerPage: adoptionIntentMaxFiles})
		if err != nil {
			return nil, err
		}
		return cmp.Files, nil
	}
	was, err := files(before)
	if err != nil {
		return adoptionIntent{}, err
	}
	now, err := files(obs.HeadSHA)
	if err != nil {
		return adoptionIntent{}, err
	}
	return judgeAdoptionIntent(was, now), nil
}

// checkAdoptedPushIntent runs when the hive advanced an in-place adopted PR
// to obs.HeadSHA. The push is judged against the PR's diff at the
// contributor's head the adoption started from (Record.IntentBaseHead), not
// the previous hive head: a refused head never becomes the baseline, and a
// chain of pushes that each keep most of the previous one cannot erode the
// change below half. A push that keeps the PR's change is taken as the
// hive's head (MarkAdoptedHead). One that undoes it is escalated to 085 with
// LaneReasonAdoptionFixerReverts: the contributor lane refuses to merge that
// head and the author or a maintainer is told why, once; the head is taken
// only after the note posts, so a failed post is re-checked and retried next
// sweep. An unreadable diff leaves the head unmarked, so no merge admits it,
// and is re-read next sweep.
func (c *Engine) checkAdoptedPushIntent(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, prev stagelabels.Record) (int, error) {
	base := prev.IntentBaseHead()
	intent, err := c.adoptedPushIntent(ctx, obs, base)
	if err != nil {
		c.warn("stage labels: adopted PR advanced by the hive; diff unreadable, intent check deferred", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "error", err)
		return 0, nil
	}
	if !intent.Reverts() {
		if intent.Unjudged {
			c.info("stage labels: adopted PR advanced by the hive; diff too large to judge intent", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA)
		}
		store.MarkAdoptedHead(key, obs.HeadSHA)
		return 0, nil
	}
	reason := stagelabels.LaneReasonAdoptionFixerReverts
	store.MarkAdoptionEscalated(key, obs.HeadSHA, reason)
	c.warn("stage labels: hive push undoes the adopted PR's own change; refusing it and escalating to needs-decision", "repo", obs.Display, "pr", obs.Number, "intent_base", base, "head", obs.HeadSHA, "own_lines", intent.Own, "kept_lines", intent.Kept)
	n := 0
	if rec, _ := store.Get(key); !rec.AdoptionEscalationNoted(obs.HeadSHA, reason) {
		n, err = c.postAdoptionEscalationNote(ctx, store, key, obs, reason, AdoptionFixerRevertsComment(obs, base, intent), "stage labels: fixer-reverts note failed")
		if err != nil {
			return n, err
		}
	}
	store.MarkAdoptedHead(key, obs.HeadSHA)
	return n, nil
}

// AdoptionFixerRevertsComment is the one note on a head whose hive push
// undid the PR's change.
func AdoptionFixerRevertsComment(obs stageObservation, before string, intent adoptionIntent) string {
	return fmt.Sprintf("**Fixing CI would undo this PR's change.** The hive adopted this PR (`%s`) and pushed `%s` to fix it, but that push leaves %d of the %d lines this PR changed at `%s`, so the PR would no longer do what it set out to do. The hive will not merge `%s`.\n\n"+
		"**Ask (author or maintainer):** decide what this PR should do: wait for what it depends on, revert the hive's commit and fix the cause another way, or close it. A new push re-checks.\n\n"+
		"Moving to `%s` so a person sees it.",
		stagelabels.AdoptedByHive.Label(), shortSHA(obs.HeadSHA), intent.Kept, intent.Own, shortSHA(before), shortSHA(obs.HeadSHA), stagelabels.NeedsDecision.Label())
}

// adoptionIntentRefused reports whether the stage store refuses headSHA as
// the hive's adopted head: its push undid the PR's change, or the hive
// pushed it and the intent check has not passed yet.
func adoptionIntentRefused(rec stagelabels.Record, headSHA string) string {
	if headSHA == "" || !rec.AdoptedInPlace() {
		return ""
	}
	if rec.AdoptionEscalated(headSHA) && rec.AdoptEscalatedReason == stagelabels.LaneReasonAdoptionFixerReverts {
		return stagelabels.LaneReasonAdoptionFixerReverts
	}
	if rec.HivePushedHead(headSHA) && rec.AdoptedHead != headSHA {
		return contributorLaneReasonAdoptionIntentPending
	}
	return ""
}

// contributorLaneReasonAdoptionIntentPending: the hive pushed this head on
// an adopted PR and has not yet checked it keeps the PR's change.
const contributorLaneReasonAdoptionIntentPending = "contributor-lane:adoption-intent-pending"

// adoptionIntentGate is the contributor lane's refusal for such a head,
// whatever else would admit it.
func (c *Engine) adoptionIntentGate(owner, repo string, number int, headSHA string) string {
	if c == nil {
		return ""
	}
	rec, ok := c.stageStoreOrShared().Get(stageKey(owner, repo, number))
	if !ok {
		return ""
	}
	return adoptionIntentRefused(rec, headSHA)
}
