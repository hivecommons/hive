package dashboard

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// StageTrackerActor is who (or what) holds the next move on a tracker row,
// derived server-side so the SPA draws the actor icon from Kind alone and
// never guesses from a login string (hivecommons/hive: docs#500 drew
// "Fixing" with no icon because its github-actions[bot] author matched
// none of the heuristics).
//
// Kind is one of StageActorHive (a hive lane or agent: honeycomb),
// StageActorClanker (a registered contributor agent: robot),
// StageActorHuman (a person: the same glyph as "Needs human decision"),
// StageActorBot (a non-hive GitHub bot such as github-actions[bot] or
// dependabot: cog) or StageActorNone (nobody is working on it right now —
// the SPA leads its tooltip with STAGE_TRACKER_NOBODY_LEAD, then Why). Login names the account or agent when
// known; Since is when the move became theirs (the stage entry time); Why
// is the plain-words reason, always set.
type StageTrackerActor struct {
	Kind  string     `json:"kind"`
	Login string     `json:"login,omitempty"`
	Since *time.Time `json:"since,omitempty"`
	Why   string     `json:"why"`
}

const (
	StageActorHive    = authorKindHive
	StageActorClanker = authorKindClanker
	StageActorHuman   = authorKindHuman
	StageActorBot     = "bot"
	StageActorNone    = "none"
)

// stageTrackerWorker is one busy agent on a current item, from the status
// payload the tracker already has (FrontendAgent.LastAction with Current
// set and Busy "working"); the SPA's stageTrackerWorkers reads the same
// fields.
type stageTrackerWorker struct {
	Agent  string
	Action string
	At     time.Time
}

// stageTrackerWorkersFromStatus keys the busy agents by
// lower-case owner/repo#number.
func stageTrackerWorkersFromStatus(status *StatusPayload) map[string]stageTrackerWorker {
	out := map[string]stageTrackerWorker{}
	if status == nil {
		return out
	}
	for _, a := range status.Agents {
		la := a.LastAction
		if la == nil || a.Busy != "working" || !la.Current || la.Repo == "" || la.Number <= 0 {
			continue
		}
		w := stageTrackerWorker{Agent: nonEmpty(a.Name, "an agent"), Action: la.Action}
		if t, err := time.Parse(time.RFC3339Nano, la.At); err == nil {
			w.At = t
		}
		out[strings.ToLower(la.Repo)+"#"+strconv.Itoa(la.Number)] = w
	}
	return out
}

// stageTrackerWorkerOn finds the agent on repo#number; a bare repo name in
// the audit ("hive") matches "hivecommons/hive" on its tail, as the SPA does.
func stageTrackerWorkerOn(workers map[string]stageTrackerWorker, repo string, number int) (stageTrackerWorker, bool) {
	if len(workers) == 0 || number <= 0 {
		return stageTrackerWorker{}, false
	}
	key := strings.ToLower(repo) + "#" + strconv.Itoa(number)
	if w, ok := workers[key]; ok {
		return w, true
	}
	suffix := "#" + strconv.Itoa(number)
	lr := strings.ToLower(repo)
	for k, w := range workers {
		if !strings.HasSuffix(k, suffix) {
			continue
		}
		wr := strings.TrimSuffix(k, suffix)
		if strings.HasSuffix(lr, "/"+wr) || strings.HasSuffix(wr, "/"+lr) {
			return w, true
		}
	}
	return stageTrackerWorker{}, false
}

// stageTrackerBotAuthor is true for a GitHub bot account the hive does not
// own: a "[bot]" login with no author kind resolved (the hive's own App
// PRs resolve to hive through AppAuthored / author/hive).
func stageTrackerBotAuthor(author, authorKind string) bool {
	return authorKind == "" && strings.HasSuffix(strings.ToLower(strings.TrimSpace(author)), botLoginSuffix)
}

// stageTrackerAuthorActorKind maps an item's author to an actor kind.
func stageTrackerAuthorActorKind(author, authorKind string) string {
	switch authorKind {
	case authorKindHive, authorKindClanker, authorKindHuman:
		return authorKind
	}
	if stageTrackerBotAuthor(author, authorKind) {
		return StageActorBot
	}
	return StageActorNone
}

// stageTrackerBotWhy says why nobody in the hive fixes a bot's PR.
func stageTrackerBotWhy(author string) string {
	return "bot-authored PR (@" + author + ") — the hive does not fix bot PRs; it waits for the bot's next run to replace it"
}

// stageTrackerStageActor derives the row's actor from its finished fields.
// Issues at 030/040 and mirrored issues inherit their covering PR's actor
// in stageTrackerResolveActors once the PR rows are known; here they get
// the fallback for a PR not in the snapshot.
func stageTrackerStageActor(it StageTrackerItem, workers map[string]stageTrackerWorker, now time.Time) *StageTrackerActor {
	a := &StageTrackerActor{Since: it.StageSince}
	set := func(kind, login, why string) *StageTrackerActor {
		a.Kind, a.Login, a.Why = kind, login, why
		return a
	}
	mods := it.Modifiers
	if slices.Contains(mods, modHold) {
		return set(StageActorNone, "", "on hold — the owner paused it on purpose; nothing moves until the hold is lifted")
	}
	if it.Superseded != nil && it.Superseded.CloserPR > 0 && !stageTrackerBotAuthor(it.Author, it.AuthorKind) {
		return set(StageActorHuman, "", "superseded by #"+strconv.Itoa(it.Superseded.CloserPR)+" — a maintainer decides whether to close it")
	}
	stage := stageByNumber(it.Stage)
	if it.Kind == "issue" && it.Mirrored {
		return set(StageActorNone, "", "covered by PR #"+strconv.Itoa(it.LinkedPR)+", which is not in this snapshot")
	}
	switch stage {
	case stagelabels.None:
		switch {
		case slices.Contains(mods, modBlocked):
			why := "blocked"
			if it.BlockedOnPR != nil && it.BlockedOnPR.Number > 0 {
				why += " on PR #" + strconv.Itoa(it.BlockedOnPR.Number)
			} else if len(it.BlockedOn) > 0 {
				why += " on " + strings.Join(it.BlockedOn, ", ")
			}
			return set(StageActorNone, "", why+" — nothing moves until the dependency closes")
		case slices.Contains(mods, modDraft):
			k := stageTrackerAuthorActorKind(it.Author, it.AuthorKind)
			if k == StageActorNone {
				return set(k, it.Author, "draft — the author has not marked it ready for review")
			}
			return set(k, it.Author, "draft — waits for the author to mark it ready for review")
		}
		return set(StageActorNone, "", "not in any stage yet — the stage engine has not classified it")
	case stagelabels.Actionable:
		return set(StageActorHive, "", "waiting for the scanner to pick it up")
	case stagelabels.TriageNeeded:
		return set(StageActorHuman, "", "no maintainer has triaged it yet")
	case stagelabels.PossiblyExisting:
		k := stageTrackerAuthorActorKind(it.Author, it.AuthorKind)
		if k == StageActorNone || k == StageActorBot {
			k = StageActorHuman
		}
		return set(k, it.Author, "prior art found — the author must confirm the gap or close it")
	case stagelabels.Claimed:
		switch it.ClaimantKind {
		case authorKindHive, authorKindClanker, authorKindHuman:
			return set(it.ClaimantKind, "", "holds the live claim; work is about to start")
		}
		return set(StageActorNone, "", "claimed, but no live claim is in the worker ledger")
	case stagelabels.PRInProgress:
		return set(StageActorHive, "", "an agent has a PR under way; not yet opened")
	case stagelabels.CoveredByPR:
		return set(StageActorNone, "", "covered by PR #"+strconv.Itoa(it.LinkedPR)+", which is not in this snapshot")
	case stagelabels.ReviewPending:
		return set(StageActorHive, "", "review swarm — waiting for a verdict on the current head")
	case stagelabels.Reviewed:
		return set(StageActorHive, "", "approved; a required check is still pending")
	case stagelabels.Fixing, stagelabels.NeedsAuthor, stagelabels.ReturnedToContributor:
		return stageTrackerAuthorStageActor(a, it, stage, workers, now)
	case stagelabels.AdoptedByHive:
		if w, ok := stageTrackerWorkerOn(workers, it.Repo, it.Number); ok {
			return set(StageActorHive, w.Agent, stageTrackerWorkerWhy(w, now))
		}
		return set(StageActorHive, "", "adopted — the hive carries it from here")
	case stagelabels.AwaitingMergeAdmission:
		return set(StageActorHive, "", "automerge lane — waiting to be admitted")
	case stagelabels.NeedsDecision, stagelabels.IssueNeedsDecision:
		if step, _ := stageTrackerUnauthorized(it.Reason); step != "" {
			return set(StageActorHuman, "", "the hive is not permitted to "+step+"; a person must")
		}
		return set(StageActorHuman, "", "a human decision is owed")
	case stagelabels.ReadyForHumanMerge:
		// The chevron and the popover already carry HumanMergeReasonText;
		// the next-move line says only who moves (hivecommons/hive#15394).
		return set(StageActorHuman, it.HumanApprover, "a maintainer merges it")
	case stagelabels.PreMergeSoak:
		return set(StageActorHive, "", "automerge lane — the pre-merge soak window is running")
	case stagelabels.MergeQueue:
		return set(StageActorHive, "", "in the merge queue — tide or the merge window takes it")
	}
	return set(StageActorNone, "", "no stage recorded yet")
}

// stageTrackerAuthorStageActor: 070 / 071 / 072 park the PR on its
// author. A hive PR is the hive's (an agent on it is named; otherwise the
// hive re-kicks it), a clanker's is re-kicked by its contributor hive, a
// human must act, a foreign bot's PR is nobody's to fix (StageActorBot:
// icon of the bot, caption says it waits for the bot's next run), and an
// unknown author is StageActorNone.
func stageTrackerAuthorStageActor(a *StageTrackerActor, it StageTrackerItem, stage stagelabels.Stage, workers map[string]stageTrackerWorker, now time.Time) *StageTrackerActor {
	kind := stageTrackerAuthorActorKind(it.Author, it.AuthorKind)
	a.Kind, a.Login = kind, it.Author
	switch kind {
	case StageActorHive:
		if w, ok := stageTrackerWorkerOn(workers, it.Repo, it.Number); ok {
			a.Login, a.Why = w.Agent, stageTrackerWorkerWhy(w, now)
			return a
		}
		if stage == stagelabels.Fixing {
			a.Why = "hive PR — no agent is on it this moment; the hive re-kicks it on its next cycle"
		} else {
			a.Why = "hive PR — the hive re-kicks it on its next cycle"
		}
	case StageActorClanker:
		if it.Adoption != nil && it.Adoption.Text != "" {
			a.Why = it.Adoption.Text
		} else {
			a.Why = "contributor agent's PR — its hive re-kicks it; adopted by this hive if it goes quiet"
		}
	case StageActorHuman:
		if stage == stagelabels.Fixing {
			a.Why = "@" + it.Author + " must push a fix; the hive does not edit a person's PR"
		} else {
			a.Why = "@" + it.Author + " must act"
		}
	case StageActorBot:
		a.Why = stageTrackerBotWhy(it.Author)
	default:
		a.Login = ""
		a.Why = "author kind unknown — no author/<kind> label and no hive marker on the PR"
	}
	return a
}

func stageTrackerWorkerWhy(w stageTrackerWorker, now time.Time) string {
	why := w.Agent + " is on it"
	if word := stageTrackerWorkWord(w.Action); word != "" {
		why = w.Agent + " " + word
	}
	if !w.At.IsZero() && now.After(w.At) {
		why += " · " + stageTrackerShortAgoText(now.Sub(w.At)) + " ago"
	}
	return why
}

// stageTrackerWorkWord mirrors the SPA's STAGE_TRACKER_WORK_WORDS.
func stageTrackerWorkWord(action string) string {
	switch action {
	case "agent_pr_created":
		return "opened a pull request"
	case "agent_issue_created":
		return "opened an issue"
	case "agent_comment_created":
		return "commented"
	case "agent_issue_claimed":
		return "claimed it"
	case "agent_label_applied":
		return "changed labels"
	case "agent_review_requested":
		return "asked for review"
	case "agent_issue_closed", "pr_closed":
		return "closed it"
	case "agent_pr_reviewed":
		return "reviewed it"
	case "pr_merged":
		return "merged it"
	}
	return ""
}

func stageTrackerShortAgoText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// stageTrackerBotFixingCaption is the honest caption for a foreign bot's
// PR the lane filed under 070-fixing: nobody is fixing it. Red or
// conflicting is named from the modifiers; a supersession names the PR
// that replaced it.
func stageTrackerBotFixingCaption(it StageTrackerItem) string {
	lead := "Red or changes requested"
	if slices.Contains(it.Modifiers, modNeedsRebase) {
		lead = "Conflicting with the base branch"
	}
	c := lead + "; " + stageTrackerBotWhy(it.Author)
	if it.Superseded != nil && it.Superseded.CloserPR > 0 {
		c += " (superseded by #" + strconv.Itoa(it.Superseded.CloserPR) + ")"
	}
	return c
}

// stageTrackerResolveActors runs after stageTrackerResolveLinkedPRs: an
// issue riding its covering PR (including stale intake narration replaced
// after link resolution) takes that PR's actor, and each nested sub-row
// carries its PR's actor so the SPA draws the same icon on both.
func stageTrackerResolveActors(items []StageTrackerItem) {
	prs := map[string]*StageTrackerItem{}
	for i := range items {
		if items[i].Kind == "pr" {
			prs[items[i].Repo+"#"+strconv.Itoa(items[i].Number)] = &items[i]
		}
	}
	for i := range items {
		it := &items[i]
		if it.Kind != "issue" {
			continue
		}
		for j := range it.LinkedPRs {
			if pr := prs[it.Repo+"#"+strconv.Itoa(it.LinkedPRs[j].Number)]; pr != nil && pr.StageActor != nil {
				cp := *pr.StageActor
				it.LinkedPRs[j].StageActor = &cp
			}
		}
		if it.StageActor == nil || slices.Contains(it.Modifiers, modHold) {
			continue
		}
		rides := it.Mirrored || it.Stage == trackerNumberPRInProgress || it.Stage == trackerNumberCoveredByPR || it.Actor == "its PR"
		if !rides || len(it.LinkedPRs) == 0 {
			continue
		}
		lead := it.LinkedPRs[0]
		for _, lp := range it.LinkedPRs {
			if lp.DrawnUnder == 0 || lp.DrawnUnder == it.Number {
				lead = lp
				break
			}
		}
		if lead.StageActor != nil {
			cp := *lead.StageActor
			it.StageActor = &cp
		}
	}
}
