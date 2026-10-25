// Package delegatedecision is the hive's owner-delegate decider: the pure
// rules behind the activity monitor's "Let hive decide" action (Decide… ›
// Let hive decide) on an issue or pull request parked at
// stage/085-needs-decision or stage/045-needs-decision. The hive makes the
// decision itself, acting as the owner's delegate under the owner's standing
// operating rules (PolicyText), and the dashboard records it exactly as a
// human decision would be recorded — a `/hive decision <text>` comment, the
// park labels lifted, the auto-merge queue approval when a PR is released —
// stamped with Marker so the audit log can tell a delegated decision from a
// human one.
//
// Everything here is deterministic and side-effect free: Decide classifies
// an Input into a Decision whose Actions are drawn from the closed allowed
// set (ActionKind), and ApplyRails strips whatever the safety rails forbid.
// Executing the actions is the dashboard's job
// (pkg/dashboard/api_repo_item_decide_delegated.go). The non-mechanical
// cases — a reviewer question with no rule to answer it — fall to the
// decisive default the owner gives his assistant ("go with the
// recommendation; pick the reversible option"). When the LLM step is enabled,
// RunWithLLM (llm.go) asks the owner-delegate decider turn about exactly
// those default cases and runs its validated answer through the same rails.
package delegatedecision

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// PolicyName is the policy identifier the audit marker carries.
const PolicyName = "owner-delegate"

// MarkerPrefix opens the HTML comment stamped on every delegated decision.
const MarkerPrefix = "<!-- hive:delegated-decision"

// PolicyText is the owner's standing operating rules, verbatim: the single
// place the delegate's policy lives. The docs
// (src/docs/dashboard-sections.md, "Let hive decide") quote it from here and
// the rules in Decide implement it one clause at a time.
const PolicyText = `Owner-delegate policy (how the owner decides; the hive applies these as his delegate):

- Be decisive; never ask the owner a question back. Pick the option that keeps work moving and is reversible.
- PRs: prefer an adequate human/contributor PR over a hive duplicate — if both exist, keep the contributor's if it is mergeable and current, else supersede it with the hive's and close theirs with credit. Approve small, necessary, low-risk changes (CI pins, RBAC read-only adds, warning text, workflow hygiene) once the sentinel finding is checked and explained as intended. Sensitive-path sentinel alerts on trusted-author PRs: approve with a one-line "reviewed, intended" rationale. Clear reporter-trust holds when the fixing PR is sound.
- Contributors offline: adopt now in supersede mode — do not wait for a deadline (hivecommons/hive#13975).
- Issues: dedupe aggressively (close the newer duplicate with "Duplicate of #N", cross-link overlaps); secrets / PAT / org-permission items are owner-only → label needs-owner + hold with one comment stating the exact owner action, and stop opening more failure issues for that root cause; flaky tests → quarantine with retry + a deflake task rather than block; CI hygiene gate proposals (actionlint, duplication ratchets, link-checker allowlists) → approve as ratchets (baseline, fail only on increases; warning-only briefly, then enforce); design proposals with concrete acceptance criteria → accept as proposed with explicit defaults; "review backlog items blocked on their PR" → never auto-close when the PR closes unmerged (drop the relation, comment, re-triage).
- Record the decision as a "/hive decision <text>" comment on the item (and the auto-merge queue approval on the PR head when the decision is to release a PR), remove stage/085-needs-decision, add triage/accepted where applicable, and stamp the delegated-decision marker so it is auditable.
- Safety rails: never merge a PR (the auto-merge engine does that per repo settings); never act on an item carrying hold; never grant org membership, secrets or permissions (always the owner); leave an item untouched when the question is a policy or scope change to the hive itself (settings, governance docs, security posture) — post "hive recommends: …; owner to confirm" instead and keep 085.`

// Kind is the item kind.
type Kind string

// Item kinds.
const (
	KindIssue Kind = "issue"
	KindPR    Kind = "pr"
)

// ActionKind is the closed set of actions a delegated decision may carry.
// Anything else is refused by ApplyRails. Merging, granting membership or
// secrets and clearing the sentinel label have no kind at all, so they can
// never be requested.
type ActionKind string

// The allowed action kinds.
const (
	// ActionCommentDecision posts the `/hive decision <Text>` comment (with
	// the rationale and Marker) and lifts the park on an issue (parking
	// labels off, approved-direction on).
	ActionCommentDecision ActionKind = "comment-decision"
	// ActionApproveReview releases a PR the way the dashboard's queue action
	// does: an Approve review on the head plus the auto-merge label, so the
	// auto-merge engine merges it per repo settings. The delegate never
	// merges.
	ActionApproveReview ActionKind = "approve-review"
	// ActionLabelsAdd adds Labels.
	ActionLabelsAdd ActionKind = "labels-add"
	// ActionLabelsRemove removes Labels (never hold).
	ActionLabelsRemove ActionKind = "labels-remove"
	// ActionCloseDuplicate closes the item as a duplicate of / superseded by Target.
	ActionCloseDuplicate ActionKind = "close-as-duplicate"
	// ActionLink cross-links the item to Target with a comment.
	ActionLink ActionKind = "link"
	// ActionAdoptSupersede queues "adopt now in supersede mode" for a
	// contributor PR whose author is offline.
	ActionAdoptSupersede ActionKind = "adopt-supersede"
	// ActionRecommendOnly posts "hive recommends: …; owner to confirm" and
	// keeps the park.
	ActionRecommendOnly ActionKind = "recommend-only"
)

// AllowedActions is the allowed set in a fixed order.
var AllowedActions = []ActionKind{
	ActionCommentDecision, ActionApproveReview, ActionLabelsAdd, ActionLabelsRemove,
	ActionCloseDuplicate, ActionLink, ActionAdoptSupersede, ActionRecommendOnly,
}

// Action is one step of a decision.
type Action struct {
	Kind   ActionKind `json:"kind"`
	Labels []string   `json:"labels,omitempty"`
	Target int        `json:"target,omitempty"`
	Text   string     `json:"text,omitempty"`
}

// Verdict names the decision outcome.
type Verdict string

// Verdicts.
const (
	VerdictApprove         Verdict = "approve"
	VerdictDirection       Verdict = "direction"
	VerdictCloseDuplicate  Verdict = "close-duplicate"
	VerdictCloseSuperseded Verdict = "close-superseded"
	VerdictOwnerOnly       Verdict = "owner-only"
	VerdictQuarantineFlaky Verdict = "quarantine-flaky"
	VerdictAcceptRatchet   Verdict = "accept-ratchet"
	VerdictAcceptProposal  Verdict = "accept-proposal"
	VerdictAdoptSupersede  Verdict = "adopt-supersede"
	VerdictRecommendOnly   Verdict = "recommend-only"
	VerdictRefused         Verdict = "refused"
)

// Labels the delegate writes or reads.
const (
	// LabelAdoptSupersede is the durable request consumed by the adoption engine.
	LabelAdoptSupersede = "adoption/supersede"
	LabelNeedsOwner     = "needs-owner"
	LabelHold           = "hold"
	LabelTriageAccepted = "triage/accepted"
	LabelFlakyTest      = "flaky-test"
	LabelNeedsDecision  = "stage/085-needs-decision"
)

// CompetingPR is another open PR for the same work.
type CompetingPR struct {
	Number       int
	HiveAuthored bool
	Mergeable    bool
	Current      bool
}

// Input is everything the delegate reads about one parked item.
type Input struct {
	Repo   string
	Number int
	Kind   Kind
	Title  string
	Body   string
	Labels []string
	// Reasons are the park reasons; Question is the reviewer's decision
	// question, when one was recorded.
	Reasons  []string
	Question string
	// HiveAuthored: the hive opened the PR / filed the issue.
	HiveAuthored bool
	// TrustedAuthor: the author is the hive or holds write on the repo.
	TrustedAuthor bool
	// SentinelAlert: the sentinel label is on; SentinelReason says why.
	SentinelAlert  bool
	SentinelReason string
	// CIGreen / Conflicting / DCOMissing describe a PR head.
	CIGreen     bool
	Conflicting bool
	DCOMissing  bool
	// Small: a size/XS or size/S PR (also read from the labels).
	Small bool
	// LowRiskPaths: every changed file is CI config, workflow, docs, text or
	// read-only RBAC.
	LowRiskPaths bool
	// ContributorOnline: the author is reachable (relay heartbeat / recent
	// activity); false for an external author who went quiet.
	ContributorOnline bool
	// DuplicateOf is the older issue this one duplicates (0 = none known).
	DuplicateOf int
	// Overlaps lists issues that overlap without duplicating.
	Overlaps []int
	// Competing lists the other open PRs for the same work.
	Competing []CompetingPR
	// ReporterTrustHold: the park is the reporter-trust gate on the fixing PR.
	ReporterTrustHold bool
	// AdoptionStalled: a contributor PR whose adoption parked at 085.
	AdoptionStalled bool
	// BlockedOnPRClosed: a review-backlog item whose PR closed unmerged.
	BlockedOnPRClosed bool
}

// Decision is what the delegate decided and how to record it.
type Decision struct {
	Verdict   Verdict  `json:"verdict"`
	Rule      string   `json:"rule"`
	Text      string   `json:"text"`
	Rationale string   `json:"rationale"`
	Actions   []Action `json:"actions"`
	// Refusals lists the actions ApplyRails removed and why.
	Refusals []string `json:"refusals,omitempty"`
	// KeepsPark is true when the item stays at 085/045 (recommend-only,
	// owner-only, refused).
	KeepsPark bool `json:"keeps_park"`
}

// Marker is the audit stamp every delegated comment carries.
func Marker(hiveID string) string {
	hiveID = strings.TrimSpace(hiveID)
	if hiveID == "" {
		hiveID = "unknown"
	}
	return fmt.Sprintf("%s by=%s policy=%s -->", MarkerPrefix, hiveID, PolicyName)
}

// IsDelegatedComment reports whether a comment body carries the marker.
func IsDelegatedComment(body string) bool {
	return strings.Contains(body, MarkerPrefix)
}

var (
	secretsRE     = regexp.MustCompile(`(?i)\b(secrets?|PATs?|personal access tokens?|org(?:anization)? (?:membership|permissions?|admin|owner)|token scopes?|GITHUB_TOKEN|credentials?|app installation|billing)\b`)
	flakyRE       = regexp.MustCompile(`(?i)\b(flak(?:y|e|iness)|intermittent(?:ly)? fail\w*|non-?deterministic test)\b`)
	ratchetRE     = regexp.MustCompile(`(?i)\b(actionlint|ratchet|duplication (?:gate|threshold|check)|link[- ]?checker|allow-?list|lint gate|hygiene gate|shellcheck|yamllint|markdownlint)\b`)
	proposalRE    = regexp.MustCompile(`(?i)\bacceptance criteria\b`)
	hivePolicyRE  = regexp.MustCompile(`(?i)\b(hive\.ya?ml|governance|security posture|hive (?:settings?|config(?:uration)?|policy|policies)|branch protection|CODEOWNERS|required checks?|autonomy level|acmm level)\b`)
	duplicateOfRE = regexp.MustCompile(`(?i)\bduplicate of #(\d+)`)
	blockedOnPRRE = regexp.MustCompile(`(?i)\b(review backlog|blocked on (?:its|their|the) PR)\b`)
)

// Classifier labels.
const (
	labelKindProposal = "kind/proposal"
	labelProposal     = "proposal"
	labelRFC          = "rfc"
	labelGovernance   = "governance"
	labelFlaky        = "flaky"
	labelSizeXS       = "size/xs"
	labelSizeS        = "size/s"
)

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), want) {
			return true
		}
	}
	return false
}

func hasAnyLabel(labels []string, wants ...string) bool {
	for _, w := range wants {
		if hasLabel(labels, w) {
			return true
		}
	}
	return false
}

// text joins the fields the keyword rules read.
func (in Input) text() string {
	return strings.Join(append([]string{in.Title, in.Body, in.Question}, in.Reasons...), "\n")
}

// IsSmall reports a small PR from the input or its size labels.
func (in Input) IsSmall() bool {
	return in.Small || hasAnyLabel(in.Labels, labelSizeXS, labelSizeS)
}

// duplicateTarget is DuplicateOf, else the first "Duplicate of #N" found in
// the item's text.
func (in Input) duplicateTarget() int {
	if in.DuplicateOf > 0 {
		return in.DuplicateOf
	}
	if m := duplicateOfRE.FindStringSubmatch(in.text()); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// headline joins the title, the reviewer's question and the park reasons:
// the short fields the owner reads first.
func (in Input) headline() string {
	return strings.Join(append([]string{in.Title, in.Question}, in.Reasons...), "\n")
}

// HivePolicyChange reports whether the question is a policy or scope change
// to the hive itself: those are the owner's to confirm.
func HivePolicyChange(in Input) bool {
	if hasLabel(in.Labels, labelGovernance) {
		return true
	}
	return hivePolicyRE.MatchString(in.headline())
}

// OwnerOnly reports a secrets / PAT / org-permission item.
func OwnerOnly(in Input) bool {
	if secretsRE.MatchString(in.headline()) {
		return true
	}
	return in.Kind == KindIssue && secretsRE.MatchString(firstLines(in.Body, 12))
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// Decide applies the policy to one item. The result has not yet been run
// through ApplyRails; callers use Run.
func Decide(in Input) Decision {
	if in.Kind == KindPR {
		return decidePR(in)
	}
	return decideIssue(in)
}

func decisionComment(text string) Action { return Action{Kind: ActionCommentDecision, Text: text} }

func ownerOnlyDecision(in Input, rationale string) Decision {
	text := "owner action required: " + ownerAction(in)
	return Decision{
		Verdict: VerdictOwnerOnly, Rule: "owner-only", Text: text, Rationale: rationale, KeepsPark: true,
		Actions: []Action{
			{Kind: ActionLabelsAdd, Labels: []string{LabelNeedsOwner, LabelHold}},
			{Kind: ActionRecommendOnly, Text: text},
		},
	}
}

func decideIssue(in Input) Decision {
	ref := "#" + strconv.Itoa(in.Number)
	if HivePolicyChange(in) {
		return recommendOnly("hive-policy", "this changes the hive's own policy or scope (settings, governance, security posture); the owner confirms those", "accept the proposal as written with its stated defaults, or close it as won't-fix if it widens the hive's write scope")
	}
	if OwnerOnly(in) {
		return ownerOnlyDecision(in, "secrets, PATs and org permissions are the owner's alone; the hive stops opening further failure issues for this root cause")
	}
	if target := in.duplicateTarget(); target > 0 && target < in.Number {
		text := fmt.Sprintf("Duplicate of #%d", target)
		return Decision{
			Verdict: VerdictCloseDuplicate, Rule: "dedupe", Text: text,
			Rationale: fmt.Sprintf("the older issue #%d already tracks this; one thread keeps the work moving", target),
			Actions:   []Action{{Kind: ActionCloseDuplicate, Target: target, Text: text}},
		}
	}
	if in.BlockedOnPRClosed || blockedOnPRRE.MatchString(in.text()) && strings.Contains(strings.ToLower(in.text()), "unmerged") {
		return approveIssue("review-backlog", "keep "+ref+" open: drop the relation to the closed PR, note it here and re-triage; a review-backlog item is never auto-closed when its PR closes unmerged",
			"the PR closing unmerged leaves the work undone")
	}
	if hasLabel(in.Labels, labelFlaky) || flakyRE.MatchString(in.Title) || flakyRE.MatchString(firstLines(in.Body, 6)) {
		d := approveIssue("flaky-test", "quarantine the flaky test with a retry and file a deflake task; do not block on it",
			"a flaky test blocks nothing by policy — quarantine with retry, deflake separately")
		d.Verdict = VerdictQuarantineFlaky
		d.Actions = append([]Action{{Kind: ActionLabelsAdd, Labels: []string{LabelFlakyTest}}}, d.Actions...)
		return d
	}
	if ratchetRE.MatchString(in.Title) || ratchetRE.MatchString(firstLines(in.Body, 10)) {
		d := approveIssue("ci-ratchet", "approve as a ratchet: record the baseline now, fail only on increases, warning-only for one release then enforce",
			"CI hygiene gates land as ratchets so they never block existing work")
		d.Verdict = VerdictAcceptRatchet
		return d
	}
	if hasAnyLabel(in.Labels, labelKindProposal, labelProposal, labelRFC) && proposalRE.MatchString(in.Body) {
		d := approveIssue("design-proposal", "accept as proposed with the explicit defaults stated in the acceptance criteria; anything not stated keeps today's behaviour",
			"a proposal with concrete acceptance criteria is decided by accepting it as written")
		d.Verdict = VerdictAcceptProposal
		return d
	}
	d := approveIssue("default-approve", "go with the recommendation; where options remain, take the one that keeps work moving and is reversible",
		"no owner rule singles this out; the standing default is the hive's own recommendation")
	for _, n := range sortedInts(in.Overlaps) {
		d.Actions = append(d.Actions, Action{Kind: ActionLink, Target: n, Text: fmt.Sprintf("overlaps #%d", n)})
	}
	return d
}

func approveIssue(rule, text, rationale string) Decision {
	return Decision{
		Verdict: VerdictApprove, Rule: rule, Text: text, Rationale: rationale,
		Actions: []Action{decisionComment(text), {Kind: ActionLabelsAdd, Labels: []string{LabelTriageAccepted}}},
	}
}

func recommendOnly(rule, rationale, recommendation string) Decision {
	text := "hive recommends: " + recommendation + "; owner to confirm"
	return Decision{
		Verdict: VerdictRecommendOnly, Rule: rule, Text: text, Rationale: rationale, KeepsPark: true,
		Actions: []Action{{Kind: ActionRecommendOnly, Text: text}},
	}
}

// ownerAction names the exact thing the owner must do for an owner-only item.
func ownerAction(in Input) string {
	t := strings.ToLower(in.Title + " " + in.Question)
	switch {
	case strings.Contains(t, "org") && (strings.Contains(t, "member") || strings.Contains(t, "permission")):
		return "grant or adjust the organization membership/permission named in the title, then remove `hold` and `needs-owner`"
	case strings.Contains(t, "scope") || strings.Contains(t, "pat") || strings.Contains(t, "token"):
		return "rotate or re-scope the token named in the title in the hive's secret store, then remove `hold` and `needs-owner`"
	default:
		return "set or rotate the secret named in the title, then remove `hold` and `needs-owner`"
	}
}

func decidePR(in Input) Decision {
	ref := "#" + strconv.Itoa(in.Number)
	if HivePolicyChange(in) {
		return recommendOnly("hive-policy", "this PR changes the hive's own policy or scope; the owner confirms those", "approve "+ref+" as written if the change is intended, else request changes")
	}
	if OwnerOnly(in) {
		return ownerOnlyDecision(in, "secrets, PATs and org permissions are the owner's alone")
	}
	if in.SentinelAlert {
		if !in.TrustedAuthor {
			return recommendOnly("sentinel-untrusted", "a sentinel alert on a PR from an author without write access is a human review", "review the sentinel finding on "+ref+" and remove the sentinel label if intended")
		}
		reason := strings.TrimSpace(in.SentinelReason)
		if reason == "" {
			reason = "sensitive path"
		}
		text := "reviewed, intended: " + reason + " — trusted author, change is as intended"
		return Decision{
			Verdict: VerdictApprove, Rule: "sentinel-trusted", Text: text,
			Rationale: "a sensitive-path sentinel alert on a trusted author's PR is approved with a one-line rationale; the sentinel label itself is only cleared by a person, so the merge follows once it is removed",
			Actions:   []Action{decisionComment(text), {Kind: ActionApproveReview}},
		}
	}
	if d, ok := decideCompeting(in, ref); ok {
		return d
	}
	if !in.HiveAuthored && !in.ContributorOnline && (in.AdoptionStalled || in.Conflicting || !in.CIGreen || in.DCOMissing) {
		text := "supersede now: adopt " + ref + " in supersede mode, preserving the author's commits and sign-off; do not wait for the grace deadline"
		return Decision{
			Verdict: VerdictAdoptSupersede, Rule: "contributor-offline", Text: text,
			Rationale: "the contributor is offline; adoption in supersede mode keeps the work moving (hivecommons/hive#13975)",
			Actions:   []Action{decisionComment(text), {Kind: ActionAdoptSupersede}},
		}
	}
	if in.ReporterTrustHold && in.CIGreen && !in.Conflicting && !in.DCOMissing {
		d := releasePR("reporter-trust", "approve "+ref+": the fixing PR is sound, so the reporter-trust hold is cleared", "a reporter-trust hold is cleared when the fixing PR is sound")
		d.Actions = append(d.Actions, Action{Kind: ActionLabelsAdd, Labels: []string{LabelTriageAccepted}})
		return d
	}
	if in.Conflicting || in.DCOMissing || !in.CIGreen {
		var fix []string
		if in.Conflicting {
			fix = append(fix, "rebase onto the base branch")
		}
		if in.DCOMissing {
			fix = append(fix, "add the DCO sign-off")
		}
		if !in.CIGreen {
			fix = append(fix, "make CI green without widening scope")
		}
		text := strings.Join(fix, ", ") + "; keep the smallest reversible change, then it merges"
		return Decision{
			Verdict: VerdictDirection, Rule: "fix-then-merge", Text: text,
			Rationale: "the head is not mergeable yet; the direction names the fix and nothing else",
			Actions:   []Action{decisionComment(text)},
		}
	}
	if in.IsSmall() || in.LowRiskPaths {
		return releasePR("small-low-risk", "approve "+ref+": small, necessary, low-risk change on a green head", "small low-risk changes (CI pins, read-only RBAC, warning text, workflow hygiene) are approved once checked")
	}
	return releasePR("default-release", "approve "+ref+": green head, reviewer question answered with the reversible default — proceed as implemented", "no owner rule holds this back; a merge is reversible and keeps work moving")
}

// decideCompeting applies "prefer an adequate contributor PR over a hive
// duplicate" when the item has a competing PR.
func decideCompeting(in Input, ref string) (Decision, bool) {
	for _, other := range in.Competing {
		switch {
		case in.HiveAuthored && !other.HiveAuthored && other.Mergeable && other.Current:
			return Decision{
				Verdict: VerdictCloseSuperseded, Rule: "prefer-contributor",
				Text:      fmt.Sprintf("superseded by #%d: the contributor's PR is mergeable and current, so it lands and this hive duplicate closes", other.Number),
				Rationale: "an adequate contributor PR beats a hive duplicate",
				Actions:   []Action{{Kind: ActionCloseDuplicate, Target: other.Number, Text: fmt.Sprintf("superseded by #%d", other.Number)}},
			}, true
		case !in.HiveAuthored && other.HiveAuthored && in.CIGreen && !in.Conflicting:
			return releasePR("prefer-contributor", "approve "+ref+": the contributor's PR is mergeable and current, so it lands ahead of the hive's duplicate", "an adequate contributor PR beats a hive duplicate"), true
		case !in.HiveAuthored && other.HiveAuthored:
			return Decision{
				Verdict: VerdictCloseSuperseded, Rule: "prefer-contributor",
				Text:      fmt.Sprintf("superseded by #%d with credit: this contributor PR is not mergeable/current, the hive's carries the work", other.Number),
				Rationale: "the contributor's PR is neither mergeable nor current, so the hive's supersedes it with credit",
				Actions:   []Action{{Kind: ActionCloseDuplicate, Target: other.Number, Text: fmt.Sprintf("superseded by #%d (with credit to the original author)", other.Number)}},
			}, true
		}
	}
	return Decision{}, false
}

func releasePR(rule, text, rationale string) Decision {
	return Decision{
		Verdict: VerdictApprove, Rule: rule, Text: text, Rationale: rationale,
		Actions: []Action{decisionComment(text), {Kind: ActionApproveReview}},
	}
}

func sortedInts(in []int) []int {
	out := append([]int(nil), in...)
	sort.Ints(out)
	return out
}

// Rails is the safety-rail context ApplyRails reads.
type Rails struct {
	// Held: the item carries a hold label. The delegate never acts on it.
	Held bool
	// AllowedActions overrides the allowed set (nil = AllowedActions).
	AllowedActions []ActionKind
}

// ApplyRails enforces the safety rails on a decision: a held item is refused
// outright; actions outside the allowed set are dropped with a refusal; a
// hold label is never removed; a decision whose every action was dropped
// becomes recommend-only.
func ApplyRails(d Decision, r Rails) Decision {
	if r.Held {
		return Decision{
			Verdict: VerdictRefused, Rule: "rail-hold",
			Rationale: "the item carries `hold`; the delegate does not act on held items",
			KeepsPark: true, Refusals: []string{"held: all actions refused"},
		}
	}
	allowed := r.AllowedActions
	if allowed == nil {
		allowed = AllowedActions
	}
	ok := map[ActionKind]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	out := d
	out.Actions = nil
	for _, a := range d.Actions {
		if !ok[a.Kind] {
			out.Refusals = append(out.Refusals, fmt.Sprintf("%s: not in the allowed action set", a.Kind))
			continue
		}
		if a.Kind == ActionLabelsRemove && hasLabel(a.Labels, LabelHold) {
			out.Refusals = append(out.Refusals, "labels-remove hold: the delegate never lifts a hold")
			continue
		}
		out.Actions = append(out.Actions, a)
	}
	if len(out.Actions) == 0 {
		out.Verdict = VerdictRecommendOnly
		out.KeepsPark = true
		out.Actions = []Action{{Kind: ActionRecommendOnly, Text: "hive recommends: " + d.Text + "; owner to confirm"}}
	}
	return out
}

// Run is Decide followed by ApplyRails with the hold rail read from the labels.
func Run(in Input) Decision {
	return ApplyRails(Decide(in), Rails{Held: hasLabel(in.Labels, LabelHold)})
}

// CommentBody renders the comment the delegate posts for a decision: the
// `/hive decision <text>` line a maintainer would type (or the
// recommendation when the park is kept), the rationale, the attribution and
// the audit marker.
func CommentBody(d Decision, hiveID, requester string) string {
	var b strings.Builder
	switch {
	case d.Verdict == VerdictRefused:
		fmt.Fprintf(&b, "Hive (owner-delegate) did not decide this item: %s.", d.Rationale)
	case d.KeepsPark:
		fmt.Fprintf(&b, "%s\n\nRationale: %s. `%s` stays.", d.Text, d.Rationale, LabelNeedsDecision)
	default:
		fmt.Fprintf(&b, "/hive decision %s\n\nRationale: %s.", d.Text, d.Rationale)
	}
	by := "the hive"
	if r := strings.TrimSpace(requester); r != "" {
		by = "the hive at @" + r + "'s request"
	}
	fmt.Fprintf(&b, "\n\n_Decided by %s as the owner's delegate under the %s policy (rule `%s`)._\n%s", by, PolicyName, d.Rule, Marker(hiveID))
	return b.String()
}
