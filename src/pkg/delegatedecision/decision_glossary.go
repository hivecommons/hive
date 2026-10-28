package delegatedecision

import (
	"encoding/json"
	"sort"
	"strings"
)

// The decision glossary: every rule slug, verdict/action and blocker reason
// the delegate (or the merge gate it hands off to) can name, in plain words
// an operator who has never read the hive's source can act on. This is the
// ONE source for operator-facing text — the dashboard toast and popover,
// the PR comment, the tooltip and the JS mirror in index.html
// (STAGE_TRACKER_DECISION_GLOSSARY, checked against GlossaryJSON by test)
// all read it — so a slug such as `contributor-offline` or `supersede` is
// never shown bare. Owner evidence (2026-10-28, hivecommons/hive#14822):
// "I have no idea what I need to do from the language and comments in
// there. a) why couldn't hive decide, b) what does supersede mean?"

// GlossaryKind classifies an entry.
type GlossaryKind string

// Glossary entry kinds.
const (
	// GlossaryRule is a Decision.Rule slug (why the delegate chose).
	GlossaryRule GlossaryKind = "rule"
	// GlossaryAction is a verdict or action the delegate takes (what it chose).
	GlossaryAction GlossaryKind = "action"
	// GlossaryReason is a lane / park reason that stops the chosen action
	// from taking effect (why it is stuck).
	GlossaryReason GlossaryKind = "reason"
)

// GlossaryEntry is one slug in plain words. Plain is a short phrase that
// fits a toast headline in parentheses ("adopted (the author's clanker hasn't
// been connected since the nudge)"); Detail is one full sentence for a
// popover, tooltip or PR comment. Aliases are other spellings of the same
// slug (lane tokens, label names) that resolve to this entry.
type GlossaryEntry struct {
	Slug    string       `json:"slug"`
	Kind    GlossaryKind `json:"kind"`
	Plain   string       `json:"plain"`
	Detail  string       `json:"detail"`
	Aliases []string     `json:"aliases,omitempty"`
}

// Slugs the glossary and its callers share. Rule slugs live beside the
// rules in policy.go / llm.go; these are the action and reason slugs the
// stuck-decision path coins.
const (
	GlossarySupersede      = "supersede"
	GlossaryAdopt          = "adopt"
	GlossaryChangesRequest = "changes_requested"
	GlossarySentinel       = "sentinel"
	GlossaryConflicting    = "conflicting"
	GlossaryCIRed          = "ci-red"
	GlossaryAuthorOnline   = "author-online"
	GlossaryHold           = "hold"
	GlossaryDCO            = "dco"
	GlossaryNeedsHuman     = "needs-human"
	GlossaryNoEffect       = "no-effect"
	GlossaryAdoptionOff    = "adoption-disabled"
	GlossaryStillParked    = "still-parked"
	GlossaryBehind         = "behind"
	GlossaryNotMergedYet   = "not-merged-yet"
	GlossaryNotClosedYet   = "not-closed-yet"
	GlossaryNoNewCommit    = "no-new-commit"
	GlossarySelfDecision   = "self-decision-read-as-changes"
	GlossaryEffectUnknown  = "effect-unknown"
	GlossaryHumanMerge     = "human-merge" // 090: no lane vouches for the author (hivecommons/hive#15078)
	// Adoption-execution reasons (hivecommons/hive#15188): what the
	// adoption engine did with a recorded adopt-supersede.
	GlossaryWorkflowPath      = "workflow-path"
	GlossaryAdoptionExecuting = "adoption-executing"
	GlossaryAdoptionUndone    = "adoption-undone"
	GlossaryBlocker           = "blocker"
	glossaryFallbackUnknown   = "the hive did not say why"
)

var glossary = []GlossaryEntry{
	// Rules — why the delegate chose what it chose.
	{Slug: "contributor-offline", Kind: GlossaryRule, Plain: "the author's clanker hasn't been connected since the nudge",
		Detail: "The PR's author has been offline since the hive nudged them, so the hive takes the work over instead of waiting."},
	{Slug: "dedupe", Kind: GlossaryRule, Plain: "an older issue already tracks this",
		Detail: "An older issue already tracks the same work; one thread keeps it moving."},
	{Slug: "fix-then-merge", Kind: GlossaryRule, Plain: "the branch isn't mergeable yet",
		Detail: "The branch cannot merge as it is (conflicts, red CI or a missing sign-off); the hive told the author agent exactly what to fix."},
	{Slug: RuleReturnToAuthor, Kind: GlossaryRule, Plain: "a person pushed this PR; the hive asked them, not adopted it",
		Detail: "The PR was pushed by a person (author/human or one of this hive's owners), so the hive never adopts it. It told the author by name what to fix (rebase, sign-off, CI) and relayed the reviewer's question."},
	{Slug: "owner-only", Kind: GlossaryRule, Plain: "only the hive owner can do this",
		Detail: "This needs a secret, token or organization permission that only the hive owner holds; the hive cannot do it for you.", Aliases: []string{"owner_only"}},
	{Slug: "prefer-contributor", Kind: GlossaryRule, Plain: "a contributor's PR and a hive PR do the same work",
		Detail: "A contributor's PR and a hive-authored PR cover the same work; the hive prefers the contributor's when it is mergeable and current."},
	{Slug: "rail-hold", Kind: GlossaryRule, Plain: "the item is on hold",
		Detail: "The item carries the hold label; the hive never acts on a held item until a person releases the hold."},
	{Slug: "sentinel-trusted", Kind: GlossaryRule, Plain: "security-sensitive files changed, but the author is trusted",
		Detail: "Security-sensitive files changed; because the author is trusted the hive approved, but a person must still remove the sentinel label before it merges."},
	{Slug: "sentinel-untrusted", Kind: GlossaryRule, Plain: "security-sensitive files changed; a maintainer must approve",
		Detail: "Security-sensitive files changed in a PR from an author without write access; a maintainer must review and approve it."},
	{Slug: "hive-policy", Kind: GlossaryRule, Plain: "this changes the hive's own rules",
		Detail: "This changes the hive's own settings, governance or security posture; the hive only recommends and the owner confirms."},
	{Slug: "capability-gap", Kind: GlossaryRule, Plain: "the GitHub App lacks a permission; deciding cannot clear it",
		Detail: "The blocker is a missing GitHub App permission (for example `workflows`). Approving a direction would only re-apply needs-human, so the park stays until the owner grants the permission or a person applies the change."},
	{Slug: "review-backlog", Kind: GlossaryRule, Plain: "the PR closed without merging, so the issue stays open",
		Detail: "The fixing PR closed unmerged; the issue stays open and is re-triaged rather than auto-closed."},
	{Slug: "flaky-test", Kind: GlossaryRule, Plain: "a flaky test, quarantined with a retry",
		Detail: "The failure is a flaky test: it is quarantined with a retry and a separate deflake task is filed, so it blocks nothing."},
	{Slug: "ci-ratchet", Kind: GlossaryRule, Plain: "a CI hygiene gate, landed as a ratchet",
		Detail: "A new CI hygiene gate lands as a ratchet: it records today's baseline and fails only on increases, so existing work is never blocked."},
	{Slug: "design-proposal", Kind: GlossaryRule, Plain: "a proposal with clear acceptance criteria, accepted as written",
		Detail: "The proposal states concrete acceptance criteria, so it is accepted as written; anything not stated keeps today's behaviour."},
	{Slug: "reporter-trust", Kind: GlossaryRule, Plain: "the hold on an unknown reporter is cleared because the fix is sound",
		Detail: "The PR was held until the reporter's trust was known; its fix is green and mergeable, so the hold is cleared."},
	{Slug: "small-low-risk", Kind: GlossaryRule, Plain: "a small, low-risk change on a green branch",
		Detail: "The change is small and low-risk (docs, CI pins, text, workflow hygiene) and CI is green, so it is approved."},
	{Slug: "default-approve", Kind: GlossaryRule, Plain: "no special rule applies; the hive took its own recommendation",
		Detail: "No owner rule singles this issue out, so the hive went with its own recommendation — the reversible choice that keeps work moving."},
	{Slug: "default-release", Kind: GlossaryRule, Plain: "no special rule applies; a green branch is approved",
		Detail: "No owner rule holds this PR back; its branch is green and a merge is reversible, so it is approved."},
	{Slug: RuleLLM, Kind: GlossaryRule, Plain: "decided by the hive's model under the owner's rules",
		Detail: "No mechanical rule applied, so the hive's model decided under the owner's standing rules; the same safety rails applied."},

	// Actions / verdicts — what the delegate chose.
	{Slug: GlossarySupersede, Kind: GlossaryAction, Plain: "close this PR and open a fresh hive-authored PR with the same commits",
		Detail:  "Supersede means the hive closes this PR and opens a fresh hive-authored PR carrying the same commits and sign-off, then finishes it itself.",
		Aliases: []string{string(VerdictAdoptSupersede), string(ActionAdoptSupersede), "adopted", "supersede-now", LabelAdoptSupersede}},
	{Slug: GlossaryAdopt, Kind: GlossaryAction, Plain: "hive takes over finishing this PR",
		Detail: "Adopt means the hive takes over finishing this PR: it rebases, fixes CI and merges it, keeping the author's commits and sign-off."},
	{Slug: string(VerdictApprove), Kind: GlossaryAction, Plain: "approved for merge",
		Detail: "The hive approved it; the merge gate merges it once every check passes.", Aliases: []string{"approved", string(ActionApproveReview)}},
	{Slug: string(VerdictDirection), Kind: GlossaryAction, Plain: "told the author agent what to change",
		Detail: "The hive recorded direction for the author agent, which pushes a new commit with that change.", Aliases: []string{"directed"}},
	{Slug: string(VerdictCloseDuplicate), Kind: GlossaryAction, Plain: "closed as a duplicate",
		Detail: "Closed because an older item already tracks the same work.", Aliases: []string{string(ActionCloseDuplicate)}},
	{Slug: string(VerdictCloseSuperseded), Kind: GlossaryAction, Plain: "closed because another PR carries the work",
		Detail: "Closed because another PR carries the same work; the author is credited there.", Aliases: []string{"closed", "superseded"}},
	{Slug: string(VerdictQuarantineFlaky), Kind: GlossaryAction, Plain: "quarantined the flaky test",
		Detail: "The flaky test is quarantined with a retry and a deflake task is filed."},
	{Slug: string(VerdictAcceptRatchet), Kind: GlossaryAction, Plain: "accepted as a ratchet",
		Detail: "Accepted as a ratchet: baseline recorded now, failures only on increases."},
	{Slug: string(VerdictAcceptProposal), Kind: GlossaryAction, Plain: "accepted the proposal as written",
		Detail: "The proposal is accepted as written with its stated defaults."},
	{Slug: string(VerdictRecommendOnly), Kind: GlossaryAction, Plain: "recommended only; a person confirms",
		Detail: "The hive only recommended; the item stays parked until a person confirms.", Aliases: []string{"recommended"}},
	{Slug: string(VerdictRefused), Kind: GlossaryAction, Plain: "did not decide",
		Detail: "The hive did not decide this item; its safety rails stopped it (for example, the item is on hold)."},
	{Slug: string(ActionCommentDecision), Kind: GlossaryAction, Plain: "recorded the decision on GitHub",
		Detail: "The hive posted the decision as a `/hive decision` comment, the way a maintainer would."},
	{Slug: string(ActionLabelsAdd), Kind: GlossaryAction, Plain: "added labels", Detail: "The hive added labels to the item."},
	{Slug: string(ActionLabelsRemove), Kind: GlossaryAction, Plain: "removed labels", Detail: "The hive removed labels from the item."},
	{Slug: string(ActionLink), Kind: GlossaryAction, Plain: "cross-linked a related item", Detail: "The hive left a comment linking a related issue or PR."},

	// Reasons — why the chosen action has not taken effect.
	{Slug: GlossaryChangesRequest, Kind: GlossaryReason, Plain: "a reviewer asked for changes; a new commit is needed",
		Detail:  "A reviewer asked for changes on this branch. Approving cannot override that: only a new commit, or a maintainer merging by hand, moves it.",
		Aliases: []string{"changes-requested", "contributor-lane:changes-requested", "human-changes-requested", "reviewer-verdict:request_changes", "request_changes"}},
	{Slug: GlossarySelfDecision, Kind: GlossaryReason, Plain: "hive's own decision comment was read as a reviewer asking for changes",
		Detail: "The merge gate read the hive's own `/hive decision` comment as a reviewer asking for changes, so the decision blocked the PR it meant to advance (hivecommons/hive#14822). A new commit or a maintainer merge clears it."},
	{Slug: GlossarySentinel, Kind: GlossaryReason, Plain: "security-sensitive files changed; a maintainer must approve",
		Detail:  "Security-sensitive files changed. The hive never merges until a maintainer reviews and removes the sentinel label.",
		Aliases: []string{"sentinel-alert", "contributor-lane:sentinel-blocked", "sentinel-blocked"}},
	{Slug: GlossaryConflicting, Kind: GlossaryReason, Plain: "the branch conflicts with the base branch",
		Detail: "The branch has merge conflicts with the base branch; it needs a rebase before anything can merge.", Aliases: []string{"dirty", "conflict"}},
	{Slug: GlossaryCIRed, Kind: GlossaryReason, Plain: "CI is failing on this branch",
		Detail: "Required checks are failing on this branch; nothing merges until they are green.", Aliases: []string{"ci-failing", "checks-failing", "red"}},
	{Slug: GlossaryBehind, Kind: GlossaryReason, Plain: "the branch is behind the base branch",
		Detail: "The branch is behind the base branch and must be updated before it can merge."},
	{Slug: GlossaryAuthorOnline, Kind: GlossaryReason, Plain: "the author's clanker is connected, so hive won't take the PR over",
		Detail: "Adoption is for authors whose clanker has gone offline; this author's clanker is connected, so the hive leaves the PR with them."},
	{Slug: GlossaryHold, Kind: GlossaryReason, Plain: "on hold; the hive does nothing until it is released",
		Detail: "The item is on hold. The hive does nothing with it until a person releases the hold."},
	{Slug: GlossaryDCO, Kind: GlossaryReason, Plain: "a commit is missing its sign-off",
		Detail: "A commit on the branch is missing its Developer Certificate of Origin sign-off; the author must sign it off.", Aliases: []string{"dco-missing"}},
	{Slug: GlossaryNeedsHuman, Kind: GlossaryReason, Plain: "marked needs-human; a person must remove the label",
		Detail:  "The item is marked needs-human: automated fixes ran out or a person flagged it. The hive waits until the label is removed.",
		Aliases: []string{"label:needs-human", "requires_human"}},
	{Slug: GlossaryWorkflowPath, Kind: GlossaryReason, Plain: "the PR edits .github/workflows; the hive's App lacks the `workflows` permission, so it cannot adopt it",
		Detail:  "The PR changes files under .github/workflows. The hive's GitHub App has no `workflows` permission, so the adoption fence refuses to publish those edits from the hive. Grant the App the `workflows` permission and re-run the decision, or have a person rebase and merge it on GitHub, or close it as superseded.",
		Aliases: []string{"contributor-lane:workflow-path", "adoption-stalled:contributor-lane:workflow-path", "capability:workflows-permission"}},
	{Slug: GlossaryAdoptionExecuting, Kind: GlossaryReason, Plain: "the adoption engine is carrying it out",
		Detail: "The decision is recorded and the adoption engine picks it up on its next stage pass (a few minutes): it pushes the commits to a hive branch and opens the hive's copy. Nothing to do yet."},
	{Slug: GlossaryAdoptionUndone, Kind: GlossaryReason, Plain: "the hive's copy was closed without merging and the adoption was undone",
		Detail: "The hive opened its copy, but the copy was closed without merging and a fresh one could not be opened, so the adoption was undone and the PR is back in its normal flow. Re-run the decision to adopt it again, or act on GitHub."},
	{Slug: GlossaryAdoptionOff, Kind: GlossaryReason, Plain: "adoption is switched off on this hive",
		Detail: "Stage adoption is disabled on this hive (governor.labels.stage.adoption), so a request to take a PR over is never carried out."},
	{Slug: GlossaryBlocker, Kind: GlossaryReason, Plain: "the hive can't do this itself",
		Detail:  "The item is blocked on something the hive cannot do: a permission the GitHub App lacks, or a step only a person may take. No decision clears it; grant the permission (and re-check) or do the step by hand (hivecommons/hive#15099).",
		Aliases: []string{"capability", "human-only"}},
	{Slug: GlossaryStillParked, Kind: GlossaryReason, Plain: "the item is still waiting on a person",
		Detail: "The item is still parked for a person's decision; the recorded decision did not move it."},
	{Slug: GlossaryNotMergedYet, Kind: GlossaryReason, Plain: "the merge gate hasn't merged it",
		Detail: "The hive approved it but the merge gate has not merged it; its lane reason says what still blocks it."},
	{Slug: GlossaryNotClosedYet, Kind: GlossaryReason, Plain: "the close didn't land",
		Detail: "The hive decided to close it, but it is still open; the close call did not land."},
	{Slug: GlossaryNoNewCommit, Kind: GlossaryReason, Plain: "no new commit has been pushed",
		Detail: "The hive gave direction, but no new commit has been pushed yet."},
	{Slug: GlossaryHumanMerge, Kind: GlossaryReason, Plain: "no merge lane vouches for this PR's author; a maintainer merges",
		Detail:  "The PR is ready, but its author is not a collaborator and holds no trusted or merger tier, so no merge lane vouches for them and the hive never merges it unattended. A maintainer merges it (Approve & merge as maintainer, or Approve — let hive merge), or trusts the author under 👥 Contributors.",
		Aliases: []string{"untrusted-author", "ready-for-human-merge", "contributor-lane:author-association", "author-association"}},
	{Slug: GlossaryNoEffect, Kind: GlossaryReason, Plain: "the decision hasn't taken effect",
		Detail: "The hive recorded this decision, but what it was supposed to cause has not happened within the expected time."},
	{Slug: GlossaryEffectUnknown, Kind: GlossaryReason, Plain: glossaryFallbackUnknown,
		Detail: "The hive could not tell from the lane reason why the decision has not taken effect."},
}

var glossaryIndex = func() map[string]*GlossaryEntry {
	idx := make(map[string]*GlossaryEntry, len(glossary)*2)
	for i := range glossary {
		e := &glossary[i]
		idx[normalizeGlossarySlug(e.Slug)] = e
		for _, a := range e.Aliases {
			idx[normalizeGlossarySlug(a)] = e
		}
	}
	return idx
}()

func normalizeGlossarySlug(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// Glossary returns every entry, sorted by kind then slug.
func Glossary() []GlossaryEntry {
	out := append([]GlossaryEntry(nil), glossary...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Slug < out[j].Slug
	})
	return out
}

// Explain looks a slug (or alias) up. The lane-reason prefix the adoption
// engine adds ("adoption-stalled:…") and a trailing ":detail" are stripped
// before the lookup so a composed lane token still resolves.
func Explain(slug string) (GlossaryEntry, bool) {
	key := normalizeGlossarySlug(slug)
	if key == "" {
		return GlossaryEntry{}, false
	}
	if e, ok := glossaryIndex[key]; ok {
		return *e, true
	}
	if rest, ok := strings.CutPrefix(key, "adoption-stalled:"); ok {
		if e, found := glossaryIndex[rest]; found {
			return *e, true
		}
		key = rest
	}
	if i := strings.LastIndex(key, ":"); i > 0 {
		if e, ok := glossaryIndex[key[i+1:]]; ok {
			return *e, true
		}
	}
	// A family token ("blocker:capability:workflows-permission") resolves
	// to its family the way the SPA's stageTrackerPlainWords does.
	if i := strings.Index(key, ":"); i > 0 {
		if e, ok := glossaryIndex[key[:i]]; ok {
			return *e, true
		}
	}
	return GlossaryEntry{}, false
}

// Plain is the short phrase for a slug. An unknown slug is humanised
// (dashes and underscores become spaces) so a bare token is never shown;
// an empty slug is "".
func Plain(slug string) string {
	if e, ok := Explain(slug); ok {
		return e.Plain
	}
	return humanizeSlug(slug)
}

// Detail is the one-sentence explanation for a slug; falls back to Plain.
func Detail(slug string) string {
	if e, ok := Explain(slug); ok {
		return e.Detail
	}
	return humanizeSlug(slug)
}

// RulePlain is Plain for a Decision.Rule; "" for an empty rule.
func RulePlain(rule string) string {
	if strings.TrimSpace(rule) == "" {
		return ""
	}
	return Plain(rule)
}

// VerdictPlain is Plain for a verdict.
func VerdictPlain(v Verdict) string {
	return Plain(string(v))
}

func humanizeSlug(slug string) string {
	s := strings.TrimSpace(slug)
	if s == "" {
		return ""
	}
	s = strings.NewReplacer("-", " ", "_", " ", ":", ": ").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// GlossaryJSON renders the glossary as the JSON object the SPA mirrors
// (STAGE_TRACKER_DECISION_GLOSSARY in index.html): slug → {kind, plain,
// detail}, with every alias as its own key pointing at the same words, so
// the browser resolves a token exactly the way Explain does.
func GlossaryJSON() string {
	type entry struct {
		Kind   GlossaryKind `json:"kind"`
		Plain  string       `json:"plain"`
		Detail string       `json:"detail"`
	}
	m := map[string]entry{}
	for _, e := range glossary {
		m[e.Slug] = entry{Kind: e.Kind, Plain: e.Plain, Detail: e.Detail}
		for _, a := range e.Aliases {
			m[a] = entry{Kind: e.Kind, Plain: e.Plain, Detail: e.Detail}
		}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}
