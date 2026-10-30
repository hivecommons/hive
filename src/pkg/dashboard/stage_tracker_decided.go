package dashboard

import (
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The post-decision state machine (hivecommons/hive#15126). The owner
// pressed "Let hive decide" on three 090 rows; the hive recorded
// adopt-supersede. The row then showed, all at once, a green ✓, a spinner
// "Hive is deciding…", the striped "Ready for human merge · untrusted
// author" pill and a toast — and later two sub-rows per issue once the
// hive's copy opened. Nothing on the row said which state it was in.
//
// The tracker now projects a recorded decision onto the row:
//
//	deciding   — the job runs: the row's pill reads "Hive deciding…"
//	            (SPA pending caption); no badge.
//	decided    — the ledger has the verdict (last_operator_action.verdict)
//	            and its effect has not landed yet: the row LEAVES 090 for
//	            the merge gate (080) with `decided{...}` naming the verdict,
//	            the sub-step ("Adopted · hive rebases & merges next pass")
//	            and when the next pass is due; the ✓ badge is the decision
//	            badge, never a merge celebration.
//	landed     — the effect shows (the item moved stage, or the hive's copy
//	            exists): `decided.effect: landed`, no projection; the
//	            adoption pair is explicit (adopted_from on the copy,
//	            adopted_by on the original).
//	timed-out  — past decisions.delegate.effect_timeout with no effect:
//	            back to the real stage with `decided.effect: timed-out`;
//	            the #15063 no-effect path offers the options.

// StageTrackerDecided is the `decided` field of a row with a recorded
// decision.
type StageTrackerDecided struct {
	// Action is the ledger action (let-hive-decide, approve, queue-merge,
	// merge-now); Verdict the delegate's verdict for let-hive-decide
	// (adopt-supersede, approve, …), else the action's own verb.
	Action  string `json:"action"`
	Verdict string `json:"verdict"`
	Rule    string `json:"rule,omitempty"`
	// Actor is who pressed the button; At when the decision was recorded.
	Actor string    `json:"actor,omitempty"`
	At    time.Time `json:"at"`
	// Effect is pending | landed | timed-out | none (a decision that keeps
	// the park expects nothing to happen).
	Effect string `json:"effect"`
	// StageBefore is the stage the row really wears while Effect is
	// pending (the projection moved it to Stage).
	StageBefore int `json:"stage_before,omitempty"`
	// Stage is the stage the projection moves the row onto while Effect
	// is pending (0 = the real stage stays).
	Stage int `json:"stage,omitempty"`
	stage stagelabels.Stage
	// Word is the pill's lead word ("Adopted", "Approved"); Text the full
	// sub-step ("Adopted · hive rebases & merges next pass"); Title the
	// badge tooltip ("Hive decided 17:51 — adopted (supersede); next:
	// adoption pass in 4m").
	Word  string `json:"word"`
	Text  string `json:"text"`
	Title string `json:"title"`
	// Acting is the in-flight caption the SPA shows while the effect is
	// pending ("Adopting · opening hive copy…"), never a verb from another
	// decision.
	Acting string `json:"acting,omitempty"`
	// NextAt is when the pass that carries the effect out is due.
	NextAt *time.Time `json:"next_at,omitempty"`
	// EffectTimeoutSeconds is the window Effect is judged against.
	EffectTimeoutSeconds int64 `json:"effect_timeout_s,omitempty"`
	// SupersededBy is the hive's copy once an adoption landed.
	SupersededBy int `json:"superseded_by,omitempty"`
}

// StageTrackerAdoptedFrom is the `adopted_from` field of the hive's copy
// in an adoption pair: the original PR and the owner's decision that led
// here, so the copy's chip reads "↳ adopted from #15029 · you approved
// 17:51".
type StageTrackerAdoptedFrom struct {
	Number int    `json:"number"`
	URL    string `json:"url,omitempty"`
	Author string `json:"author,omitempty"`
	// DecidedBy / DecidedAt / DecidedAction are the original's recorded
	// operator action when one led to the adoption.
	DecidedBy     string     `json:"decided_by,omitempty"`
	DecidedAt     *time.Time `json:"decided_at,omitempty"`
	DecidedAction string     `json:"decided_action,omitempty"`
	Verdict       string     `json:"verdict,omitempty"`
	// State is the original's state: "open" (still open; closes when the
	// copy merges) or "closed" (closed as superseded). The chip's hover
	// says which, since the original is not listed as a row of its own
	// (hivecommons/hive#15526).
	State string `json:"state,omitempty"`
	Text  string `json:"text"`
}

const (
	decidedEffectPending  = "pending"
	decidedEffectLanded   = "landed"
	decidedEffectTimedOut = "timed-out"
	decidedEffectNone     = "none"
)

// decidedVerb is the pill / caption vocabulary per verdict.
type decidedVerb struct {
	Word   string
	Text   string
	Acting string
	Plain  string
	// Stage is the stage the row is projected onto while the effect is
	// pending (None keeps the real stage).
	Stage stagelabels.Stage
}

var decidedVerbs = map[string]decidedVerb{
	string(delegatedecision.VerdictAdoptSupersede):  {Word: "Adopted", Text: "Adopted · hive rebases & merges next pass", Acting: "Adopting · opening hive copy…", Plain: "adopted (supersede)", Stage: stagelabels.AwaitingMergeAdmission},
	string(delegatedecision.VerdictApprove):         {Word: "Approved", Text: "Approved · merge queue takes it", Acting: "Approving…", Plain: "approved", Stage: stagelabels.AwaitingMergeAdmission},
	string(delegatedecision.VerdictDirection):       {Word: "Sent back", Text: "Sent back · waiting for a new commit", Acting: "Sending back…", Plain: "direction given", Stage: stagelabels.None},
	string(delegatedecision.VerdictCloseDuplicate):  {Word: "Closing", Text: "Closing · duplicate, leaves the tracker next refresh", Acting: "Closing…", Plain: "closed as duplicate", Stage: stagelabels.None},
	string(delegatedecision.VerdictCloseSuperseded): {Word: "Closing", Text: "Closing · superseded, leaves the tracker next refresh", Acting: "Closing…", Plain: "closed as superseded", Stage: stagelabels.None},
	string(delegatedecision.VerdictNotNow):          {Word: "Closing", Text: "Closing · declined for now, leaves the tracker next refresh", Acting: "Closing…", Plain: "declined for now", Stage: stagelabels.None},
	operatorActionQueueMerge:                        {Word: "Approved", Text: "Approved · hive will merge on the next tick once CI is green", Acting: "Recording approval…", Plain: "approved — hive will merge on the next tick once CI is green", Stage: stagelabels.AwaitingMergeAdmission},
	operatorActionMergeNow:                          {Word: "Merging", Text: "Merging · now as maintainer", Acting: "Merging…", Plain: "merged now as maintainer", Stage: stagelabels.MergeQueue},
}

// stageTrackerDecidedVerdict names the verdict a recorded action stands
// for: the delegate's verdict for let-hive-decide, the action itself for a
// direct approve / queue-merge / merge-now. "" when the action records no
// decision the row can project.
func stageTrackerDecidedVerdict(a *StageTrackerOperatorAction) string {
	if a == nil || a.Outcome != operatorActionOutcomeApplied {
		return ""
	}
	switch a.Action {
	case operatorActionLetHiveDecide:
		return a.Verdict
	case operatorActionApprove, operatorActionQueueMerge, operatorActionMergeNow:
		return a.Action
	}
	return ""
}

// stageTrackerDecided derives the decision projection for a PR row from
// its newest recorded action, or nil when none is a decision.
func stageTrackerDecided(it StageTrackerItem, sweep automerge.SweepState, now time.Time, timeout time.Duration) *StageTrackerDecided {
	a := it.LastOperatorAction
	verdict := stageTrackerDecidedVerdict(a)
	if verdict == "" {
		return nil
	}
	d := &StageTrackerDecided{Action: a.Action, Verdict: verdict, Rule: a.Rule, Actor: a.Actor, At: a.At, EffectTimeoutSeconds: int64(timeout / time.Second)}
	verb, known := decidedVerbs[verdict]
	if !known {
		// A decision that keeps the park (recommend-only, owner-only,
		// refused): nothing is expected to change.
		d.Effect = decidedEffectNone
		d.Word, d.Text = "Decided", "Hive decided: "+delegatedecision.VerdictPlain(delegatedecision.Verdict(verdict))+" — still yours to decide"
		d.Title = stageTrackerDecidedTitle(d, delegatedecision.VerdictPlain(delegatedecision.Verdict(verdict)), "", now)
		return d
	}
	d.Word, d.Text, d.Acting = verb.Word, verb.Text, verb.Acting
	moved := a.StageAt != 0 && it.Stage != a.StageAt
	switch {
	case it.AdoptedBy > 0 && verdict == string(delegatedecision.VerdictAdoptSupersede):
		d.Effect, d.SupersededBy = decidedEffectLanded, it.AdoptedBy
		d.Text = stageTrackerSupersededNote(it.AdoptedBy, false)
	case moved || a.State == operatorActionStateAddressed:
		d.Effect = decidedEffectLanded
	case !a.At.IsZero() && now.Sub(a.At) >= timeout:
		d.Effect = decidedEffectTimedOut
		d.Text = d.Word + " · no effect after " + formatETA(timeout) + " — options in the row menu"
	default:
		d.Effect = decidedEffectPending
		target := verb.Stage
		// Decision wins over a stale park label: a row still wearing 085
		// after a recorded direction sits where the hive really is — 070
		// Fixing — not on the ask it already answered (hivecommons/hive#15096 N13).
		if target == stagelabels.None && verdict == string(delegatedecision.VerdictDirection) && it.Stage == stagelabels.NeedsDecision.Number() {
			target = stagelabels.Fixing
			d.Text = "Fixing · hive is addressing " + stageTrackerReviewerPossessive(it) + " review"
		}
		if target != stagelabels.None {
			d.StageBefore, d.Stage, d.stage = it.Stage, target.Number(), target
		}
		if !sweep.NextFloorAt.IsZero() && sweep.NextFloorAt.After(now) {
			t := sweep.NextFloorAt
			d.NextAt = &t
		}
	}
	next := ""
	if d.NextAt != nil {
		pass := "next pass"
		if verdict == string(delegatedecision.VerdictAdoptSupersede) {
			pass = "adoption pass"
		}
		next = pass + " in " + formatETA(d.NextAt.Sub(now))
	}
	d.Title = stageTrackerDecidedTitle(d, verb.Plain, next, now)
	return d
}

// stageTrackerDecidedTitle is the badge tooltip: "Hive decided 17:51 —
// adopted (supersede); next: adoption pass in 4m" (you · Let hive decide ·
// hive chose adopt-supersede).
func stageTrackerDecidedTitle(d *StageTrackerDecided, plain, next string, now time.Time) string {
	who := "hive"
	if d.Action != operatorActionLetHiveDecide {
		who = "you"
	}
	title := who + " decided " + d.At.UTC().Format("15:04") + " UTC — " + plain
	if d.Action == operatorActionLetHiveDecide {
		// Leads with what was chosen and what hive decided, never with the
		// ask the decision already answered (hivecommons/hive#15096 N13).
		title = "You chose Let hive decide · hive decided: " + plain
		if d.Rule != "" {
			title += " (" + delegatedecision.RulePlain(d.Rule) + ")"
		}
		title += " · " + d.At.UTC().Format("15:04") + " UTC · hive chose " + d.Verdict
	}
	switch d.Effect {
	case decidedEffectPending:
		if next != "" {
			title += "; next: " + next
		}
	case decidedEffectLanded:
		if d.SupersededBy > 0 {
			title += "; hive's copy is #" + strconv.Itoa(d.SupersededBy)
		} else {
			title += "; in effect"
		}
	case decidedEffectTimedOut:
		title += "; not in effect after " + formatETA(time.Duration(d.EffectTimeoutSeconds)*time.Second) + " — see the row menu"
	case decidedEffectNone:
		title += "; nothing changes by itself"
	}
	return title
}

// coverReason is the issue lane's reason for riding a PR (stageengine
// derive_issue.go).
func coverReason(pr int, stage stagelabels.Stage) string {
	return "open PR #" + strconv.Itoa(pr) + " covers it (PR at " + stage.String() + ")"
}

// stageTrackerProjectDecided moves a row whose decision is pending onto
// the stage the decision leads to, so the chevron, pill and counts read
// the decided state instead of the 090 it still wears on GitHub.
func stageTrackerProjectDecided(it *StageTrackerItem, d *StageTrackerDecided, sweep automerge.SweepState, now time.Time) {
	if it == nil || d == nil || d.Effect != decidedEffectPending {
		return
	}
	stage := d.stage
	if stage == stagelabels.None {
		return
	}
	it.Stage, it.StageSlug, it.StageLabel = stage.Number(), stage.Slug(), stage.Label()
	t := d.At
	it.StageSince = &t
	it.HumanMergeReason, it.HumanMergeReasonText, it.ManualMerge, it.HumanApprover = "", "", false, ""
	it.DecisionNeeded = nil
	it.Modifiers = slices.DeleteFunc(it.Modifiers, func(m string) bool { return m == modNeedsHuman || m == modNeedsDecision || m == modStalled })
	it.Caption, it.Actor = d.Text, StageActorHive
	if it.Kind == "pr" {
		it.Reason = ""
		if stage == stagelabels.Fixing {
			it.MergeGate = nil
		} else {
			it.MergeGate = stageTrackerMergeGate(it.Repo, it.Number, stage, stagelabels.Record{}, false, sweep, now)
		}
	} else {
		it.Reason = coverReason(it.LinkedPR, stage)
		it.Mirrored = true
	}
}

// applyDecidedProjection stamps `decided` on every PR with a recorded
// decision, projects pending ones onto their decided stage, carries the
// projection to the issue rows riding those PRs, and makes adoption pairs
// explicit (adopted_from on the hive's copy). Runs after
// applyOperatorActions and before the linked-PR mirror.
func (s *Server) applyDecidedProjection(items []StageTrackerItem, now time.Time) {
	sweep := s.stageTrackerSweepState()
	timeout := s.delegatedEffectTimeout()
	store := s.stageStoreOrShared()
	prs := map[string]*StageTrackerItem{}
	for i := range items {
		it := &items[i]
		if it.Kind != "pr" {
			continue
		}
		prs[it.Repo+"#"+strconv.Itoa(it.Number)] = it
		if it.Decided = stageTrackerDecided(*it, sweep, now, timeout); it.Decided != nil {
			stageTrackerProjectDecided(it, it.Decided, sweep, now)
		}
	}
	for i := range items {
		it := &items[i]
		if it.Kind != "issue" {
			continue
		}
		// An issue riding a decided PR follows it; one riding the original
		// of a landed adoption follows the hive's copy.
		if it.LinkedPR == 0 && len(it.LinkedPRs) == 1 {
			it.LinkedPR = it.LinkedPRs[0].Number
		}
		pr := prs[it.Repo+"#"+strconv.Itoa(it.LinkedPR)]
		if pr == nil {
			s.stageTrackerClosedOriginal(it, prs, store)
			stageTrackerListClosedOriginals(it, prs)
			continue
		}
		stageTrackerListClosedOriginals(it, prs)
		if copy := prs[it.Repo+"#"+strconv.Itoa(pr.AdoptedBy)]; pr.AdoptedBy > 0 && copy != nil {
			stageTrackerFollowAdoption(it, pr, copy)
			continue
		}
		if pr.Decided != nil && pr.Decided.Effect == decidedEffectPending && it.Mirrored {
			it.Decided = pr.Decided
			stageTrackerProjectDecided(it, it.Decided, sweep, now)
			it.MergeGate = pr.MergeGate
		}
	}
	// Adoption pairs: the copy names its original and the decision that
	// led to it.
	for _, orig := range prs {
		if orig.AdoptedBy <= 0 {
			continue
		}
		copy := prs[orig.Repo+"#"+strconv.Itoa(orig.AdoptedBy)]
		if copy == nil {
			continue
		}
		copy.AdoptedFrom = stageTrackerAdoptedFrom(orig, now)
	}
}

// stageTrackerFollowAdoption re-points an issue row from the adopted
// original onto the hive's copy: the issue's pill follows the PR that will
// merge, and the original is listed as superseded.
func stageTrackerFollowAdoption(issue, orig, copy *StageTrackerItem) {
	issue.LinkedPR = copy.Number
	issue.Stage, issue.StageSlug, issue.StageLabel = copy.Stage, copy.StageSlug, copy.StageLabel
	issue.StageSince = copy.StageSince
	issue.Reason = coverReason(copy.Number, stagelabels.Stage(copy.Stage))
	issue.Caption, issue.Actor = copy.Caption, copy.Actor
	issue.MergeGate = copy.MergeGate
	issue.HumanMergeReason, issue.HumanMergeReasonText, issue.ManualMerge, issue.HumanApprover = "", "", false, ""
	issue.DecisionNeeded = nil
	issue.Mirrored = true
	issue.Modifiers = slices.DeleteFunc(issue.Modifiers, func(m string) bool { return m == modNeedsHuman || m == modNeedsDecision })
	if orig.Decided != nil {
		issue.Decided = orig.Decided
	}
}

// stageTrackerAdoptedFrom builds the copy's adopted_from from the original
// PR and its recorded decision.
func stageTrackerAdoptedFrom(orig *StageTrackerItem, now time.Time) *StageTrackerAdoptedFrom {
	af := &StageTrackerAdoptedFrom{Number: orig.Number, URL: orig.URL, Author: orig.Author, State: lineageFromStateOpen}
	text := "↳ adopted from #" + strconv.Itoa(orig.Number)
	if a := orig.LastOperatorAction; a != nil && stageTrackerDecidedVerdict(a) != "" {
		t := a.At
		af.DecidedBy, af.DecidedAt, af.DecidedAction, af.Verdict = a.Actor, &t, a.Action, stageTrackerDecidedVerdict(a)
		what := "you approved"
		if a.Action == operatorActionLetHiveDecide {
			what = "you let hive decide"
		}
		text += " · " + what + " " + a.At.UTC().Format("15:04") + " UTC"
	}
	af.Text = text
	return af
}

// stageTrackerClosedOriginal handles an issue whose covering PR is gone
// from the open-PR projection because it was closed as an adopted original
// (the operator closed #15029 once #15123 was open; close-first adoption
// makes this the normal case). The issue follows the hive's copy and lists
// the original as one closed line, never as a bare number.
func (s *Server) stageTrackerClosedOriginal(issue *StageTrackerItem, prs map[string]*StageTrackerItem, store *stagelabels.Store) {
	if issue.LinkedPR <= 0 || store == nil {
		return
	}
	rec, ok := store.Get(stagelabels.Key(issue.Repo, issue.LinkedPR))
	if !ok {
		return
	}
	copyNumber := rec.SupersededBy
	if copyNumber <= 0 {
		copyNumber = rec.AdoptedBy
	}
	copy := prs[issue.Repo+"#"+strconv.Itoa(copyNumber)]
	if copy == nil {
		return
	}
	orig := StageTrackerLinkedPR{Number: issue.LinkedPR, State: "closed", SupersededBy: copy.Number, Title: copy.Title,
		Caption: stageTrackerSupersededNote(copy.Number, true)}
	if a := s.stageTrackerLatestDecision(issue.Repo, issue.LinkedPR); a != nil {
		orig.LastOperatorAction = a
		copy.AdoptedFrom = stageTrackerAdoptedFrom(&StageTrackerItem{Number: issue.LinkedPR, LastOperatorAction: a}, time.Time{})
	} else if copy.AdoptedFrom == nil {
		copy.AdoptedFrom = &StageTrackerAdoptedFrom{Number: issue.LinkedPR, Text: "↳ adopted from #" + strconv.Itoa(issue.LinkedPR)}
	}
	copy.AdoptedFrom.State = lineageFromStateClosed
	stageTrackerFollowAdoption(issue, &StageTrackerItem{Number: issue.LinkedPR}, copy)
	issue.LinkedPRs = slices.DeleteFunc(issue.LinkedPRs, func(lp StageTrackerLinkedPR) bool { return lp.Number == orig.Number })
	issue.LinkedPRs = append(issue.LinkedPRs, orig)
}

// stageTrackerSupersededNote is the muted sub-row's trailing note for an
// adopted original (owner, hivecommons/hive#15511: one short line,
// "superseded · hive adopted → #15574"; "superseded · closed · hive adopted
// → #15574" once the close landed — the "re-authored with sign-off" part
// lives in the hover).
func stageTrackerSupersededNote(copy int, closed bool) string {
	if closed {
		return "superseded · closed · hive adopted → #" + strconv.Itoa(copy)
	}
	return "superseded · hive adopted → #" + strconv.Itoa(copy)
}

// stageTrackerListClosedOriginals lists, under an issue, the closed
// original of every hive copy the issue carries (the copy's lineage names
// it: from_number, from_state closed), as a muted "superseded · closed ·
// hive adopted → #M" line. Closed PRs are not in the open-PR projection, so
// without this the original vanished the moment the close-first close
// landed; the owner wants it visible in that muted form while its
// superseder is open (hivecommons/hive#15511). A copy that merged or closed
// is not listed, so its original goes with it.
func stageTrackerListClosedOriginals(issue *StageTrackerItem, prs map[string]*StageTrackerItem) {
	for _, lp := range slices.Clone(issue.LinkedPRs) {
		copy := prs[issue.Repo+"#"+strconv.Itoa(lp.Number)]
		if copy == nil || copy.Lineage == nil || copy.Lineage.Kind != lineageAdopted || copy.Lineage.FromState != lineageFromStateClosed || copy.Lineage.FromNumber <= 0 {
			continue
		}
		n := copy.Lineage.FromNumber
		if n == issue.LinkedPR || prs[issue.Repo+"#"+strconv.Itoa(n)] != nil {
			continue
		}
		closed := StageTrackerLinkedPR{Number: n, URL: copy.Lineage.FromURL, State: "closed", SupersededBy: copy.Number,
			Title: copy.Title, Author: copy.Lineage.FromAuthor, AuthorKind: copy.Lineage.FromAuthorKind, Caption: stageTrackerSupersededNote(copy.Number, true)}
		// The claims index may still list the original as open for a while
		// after the close: that bare entry becomes the muted line.
		if k := slices.IndexFunc(issue.LinkedPRs, func(x StageTrackerLinkedPR) bool { return x.Number == n }); k >= 0 {
			if issue.LinkedPRs[k].URL != "" {
				closed.URL = issue.LinkedPRs[k].URL
			}
			issue.LinkedPRs[k] = closed
			continue
		}
		issue.LinkedPRs = append(issue.LinkedPRs, closed)
	}
}

// stageTrackerLatestDecision reads the ledger for the newest decision
// recorded on repo#number, or nil.
func (s *Server) stageTrackerLatestDecision(repo string, number int) *StageTrackerOperatorAction {
	st := s.operatorActions()
	if st == nil {
		return nil
	}
	acts := st.List(repo, number)
	for i := len(acts); i > 0; i-- {
		a := stageTrackerOperatorAction(acts[:i], 0, nil, nil, time.Now(), operatorActionStuckAfter())
		if stageTrackerDecidedVerdict(a) != "" {
			// The original is closed: the decision is in effect.
			a.State = operatorActionStateAddressed
			return a
		}
	}
	return nil
}

// stageTrackerReviewerPossessive names the reviewer whose request the hive
// is addressing ("@hanthor's") when the row's decision question or caption
// carries a login, else "the".
func stageTrackerReviewerPossessive(it StageTrackerItem) string {
	texts := []string{it.Caption, it.Reason}
	if it.DecisionNeeded != nil {
		texts = append([]string{it.DecisionNeeded.Question, it.DecisionNeeded.Blocker}, texts...)
	}
	for _, t := range texts {
		if m := stageTrackerLoginRE.FindString(t); m != "" {
			return m + "'s"
		}
	}
	return "the"
}

var stageTrackerLoginRE = regexp.MustCompile(`@[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})`)
