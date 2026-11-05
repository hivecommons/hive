package dashboard

import (
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// Remediation evidence kinds on the wire. A red review or CI result is a
// request for remediation, never proof that anybody is remediating; the
// PR in progress chevron reads "Remediating" only on one of these.
const (
	// remediationKindKick: a hive agent holds a fix kick for this head (fix
	// dispatch record, or the agent's current action is on the PR since the
	// red signal), or the claim ledger names a hive agent since then.
	remediationKindKick = "kick"
	// remediationKindClaim: a contributor's clanker took the fix task
	// (relay claim) or a clanker claim landed on the PR since the signal.
	remediationKindClaim = "claim"
	// remediationKindPush: a new head landed after the red signal.
	remediationKindPush = "push"
	// remediationKindComment is reserved for an author's acknowledging
	// comment; the tracker has no per-comment attribution yet, so no
	// derivation emits it (a bare updated_at bump is not evidence).
	remediationKindComment = "comment"
)

// Code activity states the remediation derivation adds.
const (
	// codeStateUnassigned: red review / CI on the current head and no
	// pickup evidence — the truthful waiting word.
	codeStateUnassigned = "unassigned"
	// codeStateRemediating: pickup evidence exists (kick / claim).
	codeStateRemediating = "remediating"
	// codeStateFixPushed: a head landed after the red signal; CI and the
	// re-review for it are the open work.
	codeStateFixPushed = "fix_pushed"
)

// StageTrackerRemediation is the evidence that somebody picked a red PR up:
// who, what kind of evidence, when it started and which record it rests on.
type StageTrackerRemediation struct {
	Actor  string     `json:"actor"`
	Kind   string     `json:"kind"`
	Since  *time.Time `json:"since,omitempty"`
	Source string     `json:"source"`
	// Head is the pushed head on push evidence.
	Head string `json:"head,omitempty"`
}

// stageTrackerRedSince reports when the current head's latest red signal
// (review changes requested, CI failing) began. A 07x row with no red
// activity recorded on its head is red since it entered the stage.
func stageTrackerRedSince(it *StageTrackerItem) (time.Time, string, bool) {
	var since time.Time
	word := ""
	if a := it.CIActivity; a != nil && strings.EqualFold(a.Head, it.Head) && a.State == "failing" {
		word = "CI failing"
		if a.Since != nil {
			since = *a.Since
		}
	}
	if a := it.ReviewActivity; a != nil && strings.EqualFold(a.Head, it.Head) && a.State == "changes_requested" {
		word = "Changes requested"
		if a.Since != nil && a.Since.After(since) {
			since = *a.Since
		}
	}
	if word != "" {
		return since, word, true
	}
	if it.Stage == trackerNumberFixing || it.Stage == trackerNumberNeedsAuthor || it.Stage == trackerNumberReturned {
		if it.StageSince != nil {
			since = *it.StageSince
		}
		return since, stageTrackerFixRequestedWord, true
	}
	return time.Time{}, "", false
}

// stageTrackerFixRequestedWord is the red word for a 07x row whose current
// head has no review / CI verdict recorded yet.
const stageTrackerFixRequestedWord = "Fix requested"

// PR in progress chevron words for a red PR nobody has picked up. They
// name the PR's lifecycle state — who owes the next head — never the CI
// or review verdict, which is the CI / Review chevron's information and
// stays in the detail (hivecommons/hive#16643).
const (
	prProgressUnassignedWord = "Unassigned"
	prProgressFixOwedWord    = "fix owed"
)

// stageTrackerRemediationEvidence is the one place pickup evidence is read.
// Each source is independent; the most recent evidence wins so a kick that
// follows a push (the agent iterating) reads as remediating again.
func stageTrackerRemediationEvidence(it *StageTrackerItem, dispatch review.DispatchState, status *StatusPayload, redSince, now time.Time) *StageTrackerRemediation {
	var best *StageTrackerRemediation
	consider := func(c *StageTrackerRemediation) {
		if c == nil || c.Actor == "" {
			return
		}
		if c.Since != nil && c.Since.After(now) {
			t := now
			c.Since = &t
		}
		if best == nil || (c.Since != nil && (best.Since == nil || c.Since.After(*best.Since))) {
			best = c
		}
	}
	consider(remediationFromFixDispatch(it, dispatch, status, now))
	consider(remediationFromAgentNow(it, status, redSince, now))
	consider(remediationFromClaimant(it, redSince))
	consider(remediationFromRework(it))
	consider(remediationFromPush(it, redSince))
	return best
}

func remediationFromFixDispatch(it *StageTrackerItem, dispatch review.DispatchState, status *StatusPayload, now time.Time) *StageTrackerRemediation {
	qualify := stageTrackerRepoQualifier(status)
	for _, fix := range dispatch.Fixes {
		if fix.Agent == "" || !trackerHeadMatches(it.Repo, it.Number, it.Head, qualify(fix.Repo), fix.Number, fix.HeadSHA) ||
			fix.Dispatched.IsZero() || fix.Dispatched.After(now) || now.Sub(fix.Dispatched) > review.DefaultPendingFixTTL {
			continue
		}
		return &StageTrackerRemediation{Actor: fix.Agent, Kind: remediationKindKick, Since: timePtr(fix.Dispatched), Source: "fix-dispatch"}
	}
	return nil
}

// remediationFromAgentNow: a working hive agent whose current action is on
// this PR and landed after the red signal. Review actions are the review
// swarm's, not a fix.
func remediationFromAgentNow(it *StageTrackerItem, status *StatusPayload, redSince, now time.Time) *StageTrackerRemediation {
	if status == nil {
		return nil
	}
	qualify := stageTrackerRepoQualifier(status)
	for _, worker := range status.Agents {
		action := worker.LastAction
		if worker.Busy != "working" || worker.Paused || action == nil || !action.Current || !agentCausedAuditActions[action.Action] ||
			action.Action == github.AuditActionPRReviewed || action.Action == github.AuditActionAgentReviewRequested ||
			action.Number != it.Number || !strings.EqualFold(qualify(action.Repo), it.Repo) {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, action.At)
		if err != nil || at.After(now) || at.Before(redSince) {
			continue
		}
		return &StageTrackerRemediation{Actor: worker.Name, Kind: remediationKindKick, Since: &at, Source: "agent-now"}
	}
	return nil
}

// remediationFromClaimant: a claim-ledger / claim-comment claim that landed
// after the red signal. Authorship (source author) is not pickup.
func remediationFromClaimant(it *StageTrackerItem, redSince time.Time) *StageTrackerRemediation {
	c := it.Claimant
	if c == nil || c.Since == nil || c.Since.Before(redSince) {
		return nil
	}
	switch {
	case c.Kind == claimantKindHiveAgent && (c.Source == claimantSourceClaimLedger || c.Source == claimantSourceKickHistory) && c.Agent != "":
		return &StageTrackerRemediation{Actor: c.Agent, Kind: remediationKindKick, Since: c.Since, Source: c.Source}
	case c.Kind == claimantKindClanker && (c.Source == claimantSourceClaimLedger || c.Source == claimantSourceClaimComment) && c.Login != "":
		return &StageTrackerRemediation{Actor: "@" + c.Login + "'s clanker", Kind: remediationKindClaim, Since: c.Since, Source: c.Source}
	}
	return nil
}

// remediationFromRework: the contributor relay holds the fix task
// (in_progress) or the author's head landed (pushed).
func remediationFromRework(it *StageTrackerItem) *StageTrackerRemediation {
	r := it.Rework
	if r == nil || r.Actor == "" {
		return nil
	}
	switch r.State {
	case reworkStateInProgress:
		return &StageTrackerRemediation{Actor: "@" + r.Actor + "'s clanker", Kind: remediationKindClaim, Since: r.Since, Source: "contributor-fix-relay"}
	case reworkStatePushed:
		return &StageTrackerRemediation{Actor: "@" + r.Actor, Kind: remediationKindPush, Since: r.Since, Source: "rework-commit", Head: r.Head}
	}
	return nil
}

// remediationFromPush: the recorded push is newer than the red signal. A
// push the red review was given on is the reviewed head, not a fix.
func remediationFromPush(it *StageTrackerItem, redSince time.Time) *StageTrackerRemediation {
	p := it.Push
	if p == nil || p.State != pushStatePushed || p.Since == nil || !p.Since.After(redSince) {
		return nil
	}
	actor := p.Login
	if actor == "" {
		actor = it.Author
	}
	if actor == "" {
		actor = "author"
	}
	head := p.Sha
	if head == "" {
		head = it.Head
	}
	return &StageTrackerRemediation{Actor: "@" + actor, Kind: remediationKindPush, Since: p.Since, Source: "recorded-push", Head: head}
}

// stageTrackerNextRemediator names who is expected to pick a red PR up when
// nobody has: the configured fixer for hive-owned PRs, the author otherwise.
func stageTrackerNextRemediator(it *StageTrackerItem, fixer string) string {
	hiveOwned := it.AuthorKind == authorKindHive || (it.Claimant != nil && it.Claimant.Kind == claimantKindHiveAgent)
	if it.Stage == trackerNumberNeedsAuthor || it.Stage == trackerNumberReturned {
		hiveOwned = false
	}
	if hiveOwned {
		if fixer == "" {
			fixer = review.DefaultFixerAgent
		}
		return fixer
	}
	if it.Author != "" {
		return "@" + it.Author
	}
	return "author"
}

// stageTrackerFixerAgent is the configured fix agent (review.fixer_agent),
// falling back to the dispatcher's default.
func stageTrackerFixerAgent(cfg *config.Config) string {
	if cfg != nil && strings.TrimSpace(cfg.Review.FixerAgent) != "" {
		return strings.TrimSpace(cfg.Review.FixerAgent)
	}
	return review.DefaultFixerAgent
}

func stageTrackerShortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func stageTrackerShortAge(since *time.Time, now time.Time) string {
	if since == nil || since.After(now) {
		return ""
	}
	d := now.Sub(*since).Round(time.Minute)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// stageTrackerApplyRemediation runs after stageTrackerApplyCodeActivity: on
// a PR whose current head is red (or that sits in a 07x fix stage) it
// replaces the code activity with the evidence-based remediation word and
// records the evidence on the item. Observed code work (fixing) and a
// same-head CI retry keep their own, more specific words.
func stageTrackerApplyRemediation(it *StageTrackerItem, dispatch review.DispatchState, status *StatusPayload, fixer string, now time.Time) {
	it.Remediation = nil
	if it.Kind != "pr" || it.Head == "" || it.Superseded != nil || it.Stage == trackerNumberNeedsDecision || it.Stage >= trackerNumberAwaitingMerge {
		return
	}
	stageTrackerAdoptHumanDecision(it)
	redSince, redWord, red := stageTrackerRedSince(it)
	if !red {
		return
	}
	out := it.CodeActivity
	if out == nil {
		out = trackerUnknownActivity(it.Head, "Code activity unknown")
	}
	// A 07x row whose head carries no red verdict yet and a push marker: the
	// push IS the new head (stageTrackerPushFor binds it to the current
	// head). Its native word stands; the push is the evidence.
	if redWord == stageTrackerFixRequestedWord && out.State == "pushed" && it.Push != nil {
		it.Remediation = remediationFromPush(it, time.Time{})
		return
	}
	ev := stageTrackerRemediationEvidence(it, dispatch, status, redSince, now)
	it.Remediation = ev
	if out.State == "fixing" && out.Active {
		return
	}
	if out.State == "waiting_ci" && out.Source == "recorded-ci-retry" {
		return
	}
	// 074: the hive adopted the repair; the stage word stands.
	if out.State == "adopted" {
		return
	}
	redAt := ""
	if !redSince.IsZero() {
		redAt = " at " + redSince.UTC().Format(time.RFC3339)
	}
	switch {
	case ev != nil && ev.Kind == remediationKindPush:
		out = &StageTrackerActivity{Head: it.Head, State: codeStateFixPushed, Actor: ev.Actor, Source: ev.Source, Since: ev.Since}
		out.Caption = "Fix pushed " + stageTrackerShortSHA(ev.Head) + " · re-review pending"
		if a := it.CIActivity; a != nil && strings.EqualFold(a.Head, it.Head) && (a.State == "queued" || a.State == "running" || a.State == "unknown") {
			out.Caption = "Fix pushed " + stageTrackerShortSHA(ev.Head) + " · CI pending"
		}
		out.Detail = ev.Actor + " pushed " + stageTrackerShortSHA(ev.Head) + " after the " + strings.ToLower(redWord) + " signal" + redAt +
			"; CI and the re-review on the new head decide whether it stands (evidence: push, " + ev.Source + ")."
		it.DisplayColumn = trackerNumberReviewPending
	case ev != nil:
		out = &StageTrackerActivity{Head: it.Head, State: codeStateRemediating, Actor: ev.Actor, Source: ev.Source, Since: ev.Since, ObservedAt: timePtr(now), Active: true}
		out.Caption = "Remediating · " + ev.Actor
		if age := stageTrackerShortAge(ev.Since, now); age != "" {
			out.Caption += " · " + age
		}
		if it.CodeActivity != nil && it.CodeActivity.State == "waiting_ci" {
			out.Caption += " · waiting for CI"
		}
		what := map[string]string{remediationKindKick: "a hive fix kick is running", remediationKindClaim: "a contributor clanker claimed the fix", remediationKindComment: "the author acknowledged"}[ev.Kind]
		out.Detail = redWord + redAt + "; " + what + " (evidence: " + ev.Kind + ", " + ev.Source
		if ev.Since != nil {
			out.Detail += ", since " + ev.Since.UTC().Format(time.RFC3339)
		}
		out.Detail += ")."
		it.DisplayColumn = trackerNumberPRInProgress
	case out.State == "waiting_owner":
		// The PR in progress chevron speaks the PR's own lifecycle; the red
		// word (CI failing / changes requested) is the CI or Review
		// chevron's and rides only in the detail (hivecommons/hive#16643).
		out.Caption = strings.Replace(out.Caption, " · not observed coding", " · "+prProgressFixOwedWord, 1)
		out.Detail = redWord + redAt + "; no push, claim or fix kick seen since. A new head from @" + it.Author + ", a clanker claim or a hive adoption would move it."
		it.DisplayColumn = trackerNumberPRInProgress
	default:
		next := stageTrackerNextRemediator(it, fixer)
		out = &StageTrackerActivity{Head: it.Head, State: codeStateUnassigned, Source: "no-pickup-evidence", Since: timePtr(redSince)}
		if redSince.IsZero() {
			out.Since = nil
		}
		out.Caption = prProgressUnassignedWord
		if r := it.Rework; r != nil && r.Evidence == reworkEvidenceQueued && r.Actor != "" {
			out.Caption += fmt.Sprintf(" · fixer queued, %d ahead", r.Ahead)
			next = "@" + r.Actor + "'s clanker"
		}
		out.Caption += " · next: " + next
		out.Detail = redWord + redAt + "; no fix kick, claim or new head seen since. It switches to Remediating when a hive fix kick for this head runs, a clanker claims it or " + next + " pushes."
		if it.CodeActivity != nil && it.CodeActivity.Blocker != "" {
			out.Blocker = it.CodeActivity.Blocker
		}
		it.DisplayColumn = trackerNumberPRInProgress
	}
	it.CodeActivity = out
	if it.Stage == trackerNumberFixing && it.Superseded == nil &&
		(it.Caption == "" || strings.HasPrefix(it.Caption, "Waiting for fixer") || strings.HasPrefix(it.Caption, "New head pushed") ||
			strings.HasPrefix(it.Caption, "Remediating") || strings.HasPrefix(it.Caption, "Fixing")) {
		it.Caption = out.Caption
	}
}

// stageTrackerAdoptHumanDecision: a review verdict that asked for a human
// decision stops reading "awaiting human decision" once the row is back in
// PR in progress with the hive on it — adopted (074) or in a fix stage —
// and nobody is actually parked on a person. The findings are then changes
// the hive is addressing; the decision question rides in the detail.
func stageTrackerAdoptHumanDecision(it *StageTrackerItem) {
	a := it.ReviewActivity
	if a == nil || a.State != "human_decision" || !strings.EqualFold(a.Head, it.Head) {
		return
	}
	if it.NeedsHuman || it.OnHold || it.Hold != nil || it.Stage == trackerNumberNeedsDecision {
		return
	}
	switch it.Stage {
	case trackerNumberAdopted, trackerNumberFixing, trackerNumberNeedsAuthor, trackerNumberReturned:
	default:
		return
	}
	who := "the hive adopted the repair"
	if it.Stage != trackerNumberAdopted {
		who = "the row is under repair"
	}
	a.State, a.Caption, a.Active = "changes_requested", "Review findings · changes requested", false
	a.Source += "+adopted-decision"
	detail := "The review swarm asked for a human decision; " + who + ", so its findings are the changes to make."
	if a.Blocker != "" {
		detail += " Question: " + a.Blocker
	}
	a.Detail = strings.TrimSpace(detail + " " + a.Detail)
}
