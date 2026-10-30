package dashboard

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The 072 adoption state on a tracker row (hivecommons/hive#14524: PR
// #14524 sat at stage/072 for 26h, nudged twice, its author's clanker gone,
// while every cycle logged "author maintains the repository; not
// adopting" and nothing in the UI said so). A nudged contributor PR
// carries `adoption`: whether the engine found adoption due, what holds it
// (`blocked_by`), since when the author has been nudged, how many nudges
// went out, and the author's live hub presence — read through the same
// presence source the contributor-offline adoption itself consults
// (automerge.ContributorPresenceSource, the contribute hub). When the hold
// is the owner's to lift, `action` names the row action that lifts it
// under the context menu's own id vocabulary ("decide:adopt").

// StageTrackerAdoption is the `adoption` field of a 072 row.
type StageTrackerAdoption struct {
	// Author is the PR author the row waits on (the nested PR's, not the
	// issue's, when the row is an issue carrying its covering PR).
	Author string `json:"author,omitempty"`
	// Due is true once the adoption engine judged adoption due at the
	// current head and held it (BlockedBy says why).
	Due bool `json:"due"`
	// BlockedBy is the hold reason ("author-is-maintainer"), "" when the
	// engine holds nothing.
	BlockedBy string `json:"blocked_by,omitempty"`
	// Since is when the author was first nudged at this head.
	Since *time.Time `json:"since,omitempty"`
	// Nudges counts the nudges sent for the head.
	Nudges int `json:"nudges"`
	// AuthorOnline / AuthorRegistered are the author's live hub presence;
	// AuthorLastSeen the freshest heartbeat (zero when never seen).
	AuthorOnline     bool       `json:"author_online"`
	AuthorRegistered bool       `json:"author_registered"`
	AuthorLastSeen   *time.Time `json:"author_last_seen,omitempty"`
	// AuthorMaintainer is true when the hold reason is the author's
	// repository role.
	AuthorMaintainer bool `json:"author_maintainer,omitempty"`
	// Policy is the live governor.labels.stage.adoption.maintainer_policy.
	Policy string `json:"policy,omitempty"`
	// Action is the row action that lifts the hold ("decide:adopt"), ""
	// when nothing is the owner's to decide.
	Action string `json:"action,omitempty"`
	// QueuedBy is the operator whose Adopt now is waiting for the sweep:
	// the row is in its acted state ("Adopting… (queued by @x)") and no
	// blocker or adopt action is shown.
	QueuedBy string `json:"queued_by,omitempty"`
	// Text is the plain-words pill/tooltip: "waiting on @hanthor
	// (maintainer, offline 26h) — adopt?".
	Text string `json:"text"`
}

// BlockerReason is the hold reason, or "policy" when the engine recorded none.
func (a StageTrackerAdoption) BlockerReason() string {
	if a.BlockedBy != "" {
		return a.BlockedBy
	}
	return "policy"
}

// decisionActionAdopt is the row action id for the owner's Adopt, in the
// context menu's "decide:<verb>" vocabulary (POST …/decision adopt).
const decisionActionAdopt = "decide:adopt"

// stageTrackerPresence returns the live presence lookup the tracker uses
// for 072 rows: the contribute hub when the server has one. Tests inject
// presenceLookup.
func (s *Server) stageTrackerPresence() func(login string) (automerge.ContributorPresence, bool) {
	if s == nil {
		return nil
	}
	if s.presenceLookup != nil {
		return s.presenceLookup
	}
	if s.contributeHub == nil {
		return nil
	}
	hub := s.contributeHub
	return func(login string) (automerge.ContributorPresence, bool) {
		return hub.Presence(context.Background(), login), true
	}
}

// stageTrackerMaintainerPolicy is the live maintainer policy ("adopt" when
// the config is unreadable).
func (s *Server) stageTrackerMaintainerPolicy() string {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return config.DefaultAdoptionMaintainerPolicy
	}
	return s.deps.Config.Governor.Labels.Stage.Adoption.EffectiveMaintainerPolicy()
}

// stageTrackerAdoption builds the `adoption` field for a PR row at
// stage/072 from its stage record and the author's live presence; nil
// when the row was never nudged at its current head (nothing to adopt
// yet) or is already adopted.
func stageTrackerAdoption(it StageTrackerItem, rec stagelabels.Record, hasRec bool, presence func(login string) (automerge.ContributorPresence, bool), policy string, now time.Time) *StageTrackerAdoption {
	if it.Kind != "pr" || !hasRec {
		return nil
	}
	if rec.AdoptionBlocked(it.Head) == stagelabels.AdoptBlockedAuthorPerson {
		// A person's PR was asked to be adopted and refused: say so on the
		// row even though no nudge clock ever ran (hivecommons/hive#15202).
		out := &StageTrackerAdoption{Author: it.Author, Policy: policy, Due: true, BlockedBy: stagelabels.AdoptBlockedAuthorPerson}
		if !rec.AdoptBlockedAt.IsZero() {
			since := rec.AdoptBlockedAt
			out.Since = &since
		}
		out.Text = stageTrackerAdoptionText(it.Author, *out, now)
		return out
	}
	if rec.AdoptedHead != "" && (it.Head == "" || rec.Adopted(it.Head) || rec.AdoptedInPlace()) {
		return nil
	}
	if by, ok := rec.AdoptionForced(); ok {
		// An operator pressed Adopt now and the sweep has not landed yet:
		// the row reads as acted on, whatever held adoption before.
		out := &StageTrackerAdoption{Author: it.Author, Policy: policy, Due: true, QueuedBy: by, Nudges: rec.NudgeCount}
		if !rec.AdoptForcedAt.IsZero() {
			since := rec.AdoptForcedAt
			out.Since = &since
		}
		out.Text = stageTrackerAdoptionText(it.Author, *out, now)
		return out
	}
	if rec.NudgeCount == 0 || rec.NudgeStart().IsZero() {
		return nil
	}
	if it.Head != "" && rec.NudgeHead != "" && rec.NudgeHead != it.Head {
		// The head moved since the nudge: the 072 clock restarts on the
		// next sweep; nothing to say about adoption yet.
		return nil
	}
	since := rec.NudgeStart()
	out := &StageTrackerAdoption{Author: it.Author, Since: &since, Nudges: rec.NudgeCount, Policy: policy}
	if presence != nil && it.Author != "" {
		if p, ok := presence(it.Author); ok {
			out.AuthorOnline, out.AuthorRegistered = p.Online, p.Registered
			if !p.LastSeen.IsZero() {
				t := p.LastSeen
				out.AuthorLastSeen = &t
			}
		}
	}
	head := it.Head
	if head == "" {
		head = rec.NudgeHead
	}
	if by := rec.AdoptionBlocked(head); by != "" {
		out.Due, out.BlockedBy = true, by
		out.AuthorMaintainer = by == stagelabels.AdoptBlockedAuthorMaintainer
		if by == stagelabels.AdoptBlockedAuthorMaintainer && policy == config.AdoptionMaintainerPolicyAsk {
			out.Action = decisionActionAdopt
		}
	}
	out.Text = stageTrackerAdoptionText(it.Author, *out, now)
	return out
}

// stageTrackerAdoptionText is the plain-words pill/tooltip for a 072 row:
//
//	waiting on @hanthor (maintainer, offline 26h) — adopt?
//	waiting on @hanthor (offline 40m, 2 nudges) — adoption due in a moment
//	waiting on @hanthor (online, 1 nudge)
func stageTrackerAdoptionText(author string, a StageTrackerAdoption, now time.Time) string {
	who := "the author"
	if author != "" {
		who = "@" + author
	}
	if a.BlockedBy == stagelabels.AdoptBlockedAuthorPerson {
		return who + " pushed this PR themselves — not adopted: the hive asks them, not adopts"
	}
	if a.QueuedBy != "" {
		return "Adopting… (queued by @" + a.QueuedBy + "; the hive opens its copy on the next stage pass)"
	}
	var facts []string
	if a.AuthorMaintainer {
		facts = append(facts, "maintainer")
	}
	if a.AuthorOnline {
		facts = append(facts, "online")
	} else {
		facts = append(facts, "offline"+stageTrackerAdoptionOfflineFor(a, now))
	}
	if a.Nudges > 0 && !(a.Due && a.BlockedBy != "") {
		facts = append(facts, plural(a.Nudges, "nudge"))
	}
	text := "waiting on " + who + " (" + strings.Join(facts, ", ") + ")"
	switch {
	case a.BlockedBy == stagelabels.AdoptBlockedAuthorMaintainer && a.Policy == config.AdoptionMaintainerPolicyAsk:
		text += " — adopt?"
	case a.BlockedBy == stagelabels.AdoptBlockedAuthorMaintainer:
		text += " — not adopted: maintainer_policy is never"
	case a.BlockedBy == stagelabels.AdoptBlockedOperatorAdmission:
		text += " — not adopted: a maintainer approved this head; the hive merges it as-is"
	case a.BlockedBy != "":
		text += " — not adopted: " + a.BlockedBy
	}
	return text
}

// stageTrackerAdoptionOfflineFor renders " 26h" for the time the author's
// hive has been gone: since its last heartbeat when one is known and later
// than the first nudge, else since the first nudge.
func stageTrackerAdoptionOfflineFor(a StageTrackerAdoption, now time.Time) string {
	var from time.Time
	if a.Since != nil {
		from = *a.Since
	}
	if a.AuthorLastSeen != nil && a.AuthorLastSeen.After(from) {
		from = *a.AuthorLastSeen
	}
	if from.IsZero() || !now.After(from) {
		return ""
	}
	d := now.Sub(from)
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf(" %dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf(" %dh", int(d/time.Hour))
	default:
		return fmt.Sprintf(" %dm", max(1, int(d/time.Minute)))
	}
}

// stageTrackerAdoptionCaption folds the adoption line into the 072 caption:
// "Returned to @hanthor — waiting on @hanthor (maintainer, offline 26h) —
// adopt?" replaces the bare "@hanthor must act (2 nudges sent)".
func stageTrackerAdoptionCaption(caption string, a *StageTrackerAdoption) string {
	if a == nil || a.Text == "" {
		return caption
	}
	// Only the trailing "— @x must act (N nudges sent)" clause is replaced;
	// the reason clause before it ("review swarm requested changes") stays.
	head := caption
	if i := strings.LastIndex(caption, " — "); i >= 0 && strings.Contains(caption[i:], " must act") {
		head = caption[:i]
	}
	return head + " — " + a.Text
}

// stageTrackerSupersedesIndex maps each superseding PR's store key to the
// closed original it replaced (hivecommons/hive#15135). The original's
// record carries SupersededBy; the tracker never renders that record as a
// row (addPR drops it, and a closed PR has no open item), so the live
// PR's chip is the one trace the tracker keeps. When several originals
// name the same superseder the lowest-numbered one is kept.
func stageTrackerSupersedesIndex(store *stagelabels.Store, baseURL string) map[string]StageTrackerSupersedes {
	out := map[string]StageTrackerSupersedes{}
	if store == nil {
		return out
	}
	for _, key := range store.Keys() {
		rec, ok := store.Get(key)
		if !ok || !rec.Superseded() {
			continue
		}
		repo, numText, found := strings.Cut(key, "#")
		number, err := strconv.Atoi(numText)
		if !found || err != nil || number <= 0 || repo == "" {
			continue
		}
		by := stagelabels.Key(repo, rec.SupersededBy)
		if prev, dup := out[by]; dup && prev.Number <= number {
			continue
		}
		sup := StageTrackerSupersedes{Number: number, URL: overviewItemURL("", repo, number, "pr", baseURL), Open: rec.SupersededOpen}
		if !rec.SupersededAt.IsZero() {
			t := rec.SupersededAt
			sup.ClosedAt = &t
		}
		out[by] = sup
	}
	return out
}

// stageTrackerLinkedPRFinished reports whether a PR state the caches
// resolved (stageTrackerPRStateResolver) means the PR is no longer open
// work: closed unmerged or merged. Unknown ("") is not finished.
func stageTrackerLinkedPRFinished(state string) bool {
	return state == blockedOnPRStateClosed || state == blockedOnPRStateMerged
}
