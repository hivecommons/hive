package hiveadvisor

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Readable needs-human advice. clear-human-gate-prs and unblock-human-issues
// used to say "59 of 94 open PRs (62%) are your turn (1 INSPECT, 24 DECIDE,
// 34 REVIEW)" and list five items with their whole ownership reason, which for
// a parked review is every reviewer perspective's summary run together. An
// owner could not tell from it what was holding the queue or what to do. The
// advice now groups the your-turn items by why they wait, in plain words and
// with where they sit (Recommendation.Why), turns the biggest groups into one
// "how to help" line (Recommendation.Help), and keeps each listed item's note
// to one short clause (shortOwnershipNote).

// gateReason is how the advice words one ownership reason code. why completes
// "<count> …" and reads for one item or many; help is the action, where a
// trailing " in %s" names the group's main repositories, or is dropped when
// the group is spread too thin for two names to be the place to start.
type gateReason struct {
	why  string
	help string
}

// gateReasons covers every reason code the dashboard's your-turn decision
// produces (pkg/dashboard ownershipReasonVerbs; a dashboard test keeps the two
// in step). review_decision_required is split further by gateReasonFor.
var gateReasons = map[string]gateReason{
	"sentinel_flagged":              {"flagged by the sentinel, to inspect before anything merges", "inspect what the sentinel flagged"},
	"parked_needs_human":            {"parked for a person to review", "review the parked items in %s"},
	"parked_needs_direction":        {"parked until a maintainer gives direction", "give direction on the parked items in %s"},
	"parked_needs_decision":         {"parked until a maintainer decides", "answer the parked decisions in %s"},
	"parked_needs_spec":             {"parked until someone writes a spec", "write the missing specs in %s"},
	"untrusted_reporter":            {"from reporters the hive does not trust yet", "admit or close reports from new reporters"},
	"reporter_confirmation_stale":   {"waiting on a reporter who went quiet", "close reports whose reporter went quiet"},
	"verified_done":                 {"verified done but not closed", "close what the hive verified done"},
	"agent_filed_unacknowledged":    {"filed by agents and not yet acknowledged", "acknowledge or close agent-filed issues in %s"},
	"level_gate_hold":               {"held by the ACMM level gate", "review the level-gate holds and release the ones you agree with"},
	"outreach_hold":                 {"held by the outreach lane for review", "review the outreach holds"},
	"policy_hold":                   {"held by hive policy for review", "review the policy holds in %s"},
	"hold_source_unknown":           {"held with no recorded reason", "release or explain the unexplained holds"},
	"hive_pr_needs_human":           {"hive PRs escalated for a person", "review the escalated hive PRs in %s"},
	"adjudicator_wait_elapsed":      {"escalated with no verdict after the adjudicator's wait", "decide the escalations the adjudicator never answered"},
	"reviewer_unavailable":          {"escalated with no adjudicator available", "decide the escalations no adjudicator could take"},
	"superseded_recommended_close":  {"superseded by another PR", "close the superseded PRs"},
	"superseded_extra_files_review": {"superseded but adding files the replacement did not", "check the extra files on superseded PRs"},
	"adjudicator_recommended_close": {"the adjudicator recommended closing", "close what the adjudicator recommended closing"},
	"adjudicator_reescalated":       {"re-escalated after the adjudicator's pass", "decide the re-escalated items"},
	"adjudicator_handed_off":        {"handed to a person by the adjudicator", "pick up what the adjudicator handed off"},
	"person_pr_needs_human":         {"people's PRs that need a human review", "review people's PRs in %s"},
	"review_requires_human":         {"handed to a person by the hive's review to decide", "read the hive's review and decide in %s"},
	"review_rejected":               {"rejected by the hive's review", "close or rework what the hive's review rejected"},
	"review_dispatch_hold":          {"with the hive's review parked for a person", "read the parked hive reviews in %s"},
	"review_loop_cap":               {"with the hive's review and fix loop stopped", "finish the PRs whose review and fix loop stopped"},
	"stale_safety_net":              {"untouched long enough that the safety net handed them to a person", "look at the long-untouched items in %s"},
	"blocked_unexplained":           {"labelled blocked with no blocker named", "name the blocker or lift the label"},
	"pr_gate_label":                 {"held by a gate label", "act on the gate labels or remove them"},
	"hand_merge_required":           {"that only a person can merge", "merge the reviewed PRs by hand in %s"},
	"review_decision_required":      {"waiting for an approving review that branch protection requires", "approve PRs in %s"},
	"run_checkpoint":                {"runs waiting at a checkpoint", "answer the run checkpoints"},
	"run_stale_plan":                {"runs with a stale plan", "re-plan, approve or abandon the stale runs"},
	"run_stage_exhausted":           {"runs whose stage used up its retries", "reset or abandon the stuck runs"},
	"approval_pending":              {"waiting in Approvals", "approve or reject what waits in Approvals"},
}

// review_decision_required splits by the rule GitHub is waiting on
// (github.PullRequest.HumanReviewBlocksMerge): a reviewer's requested
// changes and a code owner's review ask different people for different things.
// The reason says "changes requested by @a" when it knows who asked, and "a
// reviewer requested changes" when it does not; both are the changes group.
var (
	gateChangesRequested = gateReason{"with changes a reviewer requested", "follow up on the requested changes in %s"}
	gateCodeOwnerReview  = gateReason{"waiting for a code owner's review", "code owners: review PRs in %s"}
)

// DescribesReasonCode reports whether the advice has its own wording for an
// ownership reason code. Unknown codes still group, under their reason text.
func DescribesReasonCode(code string) bool {
	_, ok := gateReasons[code]
	return ok
}

// gateReasonFor returns the group key and wording for one your-turn item.
func gateReasonFor(o Ownership) (string, gateReason) {
	if o.ReasonCode == "review_decision_required" {
		reason := strings.ToLower(o.Reason)
		switch {
		case strings.Contains(reason, "changes requested"), strings.Contains(reason, "requested changes"):
			return o.ReasonCode + "/changes", gateChangesRequested
		case strings.Contains(reason, "code-owner"), strings.Contains(reason, "code owner"):
			return o.ReasonCode + "/code-owner", gateCodeOwnerReview
		}
	}
	if g, ok := gateReasons[o.ReasonCode]; ok {
		return o.ReasonCode, g
	}
	label := firstClause(o.Reason)
	if label == "" {
		label = nonEmpty(o.ReasonCode, o.Verb)
	}
	return "other/" + label, gateReason{why: "waiting for a person: " + strings.ToLower(label)}
}

// maxWhyGroups is how many groups the Why list names; the rest fold into one
// "more" line, unless only one group is left over.
const maxWhyGroups = 5

// maxHelpActions is how many of the largest groups the Help line names.
const maxHelpActions = 3

type gateGroup struct {
	key   string
	words gateReason
	count int
	repos map[string]int
}

// humanGateWhy groups the your-turn items by why they wait and returns the
// Why lines (largest group first) and the Help actions for the largest few.
func humanGateWhy(owns []Ownership, repos []string) (why []string, help string) {
	groups := map[string]*gateGroup{}
	for i, o := range owns {
		key, words := gateReasonFor(o)
		g := groups[key]
		if g == nil {
			g = &gateGroup{key: key, words: words, repos: map[string]int{}}
			groups[key] = g
		}
		g.count++
		g.repos[repos[i]]++
	}
	sorted := make([]*gateGroup, 0, len(groups))
	for _, g := range groups {
		sorted = append(sorted, g)
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].count != sorted[j].count {
			return sorted[i].count > sorted[j].count
		}
		return sorted[i].key < sorted[j].key
	})

	rest := 0
	for i, g := range sorted {
		if i >= maxWhyGroups && len(sorted) > maxWhyGroups+1 {
			rest += g.count
			continue
		}
		why = append(why, fmt.Sprintf("%d %s (%s).", g.count, g.words.why, whereText(g.repos, g.count)))
	}
	if rest > 0 {
		why = append(why, fmt.Sprintf("%d more for other reasons; the full list has each one.", rest))
	}

	var actions []string
	seen := map[string]bool{}
	for _, g := range sorted {
		if len(actions) == maxHelpActions {
			break
		}
		if g.words.help == "" {
			continue
		}
		action := g.words.help
		if strings.HasSuffix(action, " in %s") {
			top := topRepos(g.repos, 2)
			held := 0
			for _, name := range top {
				held += g.repos[name]
			}
			if len(top) > 0 && 2*held > g.count {
				action = fmt.Sprintf(action, joinAnd(top))
			} else {
				action = strings.TrimSuffix(action, " in %s")
			}
		}
		if !seen[action] {
			seen[action] = true
			actions = append(actions, action)
		}
	}
	return why, strings.Join(actions, "; ")
}

// topRepos returns up to n repositories by item count, ties by name.
func topRepos(counts map[string]int, n int) []string {
	names := make([]string, 0, len(counts))
	for name := range counts {
		if strings.TrimSpace(name) != "" {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		if counts[names[i]] != counts[names[j]] {
			return counts[names[i]] > counts[names[j]]
		}
		return names[i] < names[j]
	})
	if len(names) > n {
		names = names[:n]
	}
	return names
}

// whereText says where a group's items sit: "all in common", or the three
// largest repositories with counts and how many are elsewhere.
func whereText(counts map[string]int, total int) string {
	top := topRepos(counts, 3)
	if len(top) == 0 {
		return "repository unknown"
	}
	if len(top) == 1 && counts[top[0]] == total {
		return "all in " + top[0]
	}
	parts := make([]string, 0, len(top)+1)
	listed := 0
	for _, name := range top {
		parts = append(parts, fmt.Sprintf("%d in %s", counts[name], name))
		listed += counts[name]
	}
	if other := total - listed; other > 0 {
		parts = append(parts, fmt.Sprintf("%d elsewhere", other))
	}
	return strings.Join(parts, ", ")
}

// joinAnd renders "a", "a and b".
func joinAnd(names []string) string {
	switch len(names) {
	case 0:
		return "your repositories"
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// maxNoteRunes bounds an item note. A longer reason is cut to its first
// clause ("Hive's review is parked for a person: correctness requires …"
// becomes "Hive's review is parked for a person"); the full reason stays in
// the inbox and the CSV.
const maxNoteRunes = 100

// noteDetailOnly are the reason codes whose reason opens with a clause the
// verb already says ("Waiting on a person's review: " beside REVIEW), so the
// item note keeps only what follows it: "REVIEW — a reviewer requested
// changes".
var noteDetailOnly = map[string]bool{"review_decision_required": true}

// shortOwnershipNote is the item note: the verb and a reason short enough to
// read in a list.
func shortOwnershipNote(o Ownership) string {
	reason := strings.TrimSpace(o.Reason)
	if head := firstClause(reason); noteDetailOnly[o.ReasonCode] && head != reason {
		reason = strings.TrimSpace(strings.TrimPrefix(reason, head+": "))
	}
	if utf8.RuneCountInString(reason) > maxNoteRunes {
		if head := firstClause(reason); head != "" && utf8.RuneCountInString(head) <= maxNoteRunes {
			reason = head
		} else {
			reason = string([]rune(reason)[:maxNoteRunes-1]) + "…"
		}
	}
	return ownershipNote(Ownership{Verb: o.Verb, Reason: reason})
}

// firstClause is the text before the first ": " (or the whole text).
func firstClause(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ": "); i > 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func nonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
