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

	"github.com/hivecommons/hive/pkg/blocker"
)

// PolicyName is the policy identifier the audit marker carries.
const PolicyName = "owner-delegate"

// MarkerPrefix opens the HTML comment stamped on every delegated decision.
const MarkerPrefix = "<!-- hive:delegated-decision"

// PolicyText is the owner's standing operating rules, verbatim, including
// the contributor-adequacy rail adopted in hivecommons/hive#13998: the single
// place the delegate's policy lives. The docs
// (src/docs/dashboard-sections.md, "Let hive decide") quote it from here and
// the rules in Decide implement it one clause at a time.
const PolicyText = `Owner-delegate policy (how the owner decides; the hive applies these as his delegate):

- Be decisive; except for the safety rails below, never ask the owner a question back. Pick the option that keeps work moving and is reversible.
- PRs: prefer an adequate human/contributor PR over a hive duplicate — if both exist and the contributor's is mergeable and current, recommend a human comparison of issue coverage, checks and review approval; keep the hive PR open and parked until adequacy is confirmed, and never close it solely on an issue reference. Otherwise supersede the contributor's with the hive's and close theirs with credit. Approve small, necessary, low-risk changes (CI pins, RBAC read-only adds, warning text, workflow hygiene) once the sentinel finding is checked and explained as intended. Sensitive-path sentinel alerts on trusted-author PRs: approve with a one-line "reviewed, intended" rationale. Clear reporter-trust holds when the fixing PR is sound.
- Contributors offline: adopt now in supersede mode — do not wait for a deadline (hivecommons/hive#13975).
- Issues: dedupe aggressively (close the newer duplicate with "Duplicate of #N", cross-link overlaps); secrets / PAT / org-permission items are owner-only → label needs-owner + hold with one comment stating the exact owner action, and stop opening more failure issues for that root cause; flaky tests → quarantine with retry + a deflake task rather than block; CI hygiene gate proposals (actionlint, duplication ratchets, link-checker allowlists) → approve as ratchets (baseline, fail only on increases; warning-only briefly, then enforce); design proposals with concrete acceptance criteria → accept as proposed with explicit defaults; "review backlog items blocked on their PR" → never auto-close when the PR closes unmerged (drop the relation, comment, re-triage); a fleet self-report whose remedy is the operator's (an expired provider login, credentials, disk, network, App setup on the reporting spoke) is not code work → tell that spoke's owner(s) exactly what to do, no approved-direction, no implementer, keep it out of the actionable set, and never post the same direction twice; a hive-filed proposal is paid for by evidence → approve it when it cites an incident it would have prevented or prior instances of the same class fixed one at a time, otherwise decline it for now (comment the reason, close as not planned; re-propose with evidence) — a person's proposal is never declined this way; a hive advisory (a dedupe checklist, a triage note) is never work for a person → the hive ticks its items as they close and closes it as completed when all are resolved; while items remain open nothing is written.
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
	// ActionCloseNotNow posts the "not now" reason and closes the issue as
	// not planned: the established way a declined proposal leaves the
	// tracker (the dashboard's Won't-fix path).
	ActionCloseNotNow ActionKind = "close-not-now"
	// ActionCloseCompleted closes a hive advisory whose checklist items are
	// all resolved, as completed (hivecommons/hive#15754).
	ActionCloseCompleted ActionKind = "close-completed"
)

// AllowedActions is the allowed set in a fixed order.
var AllowedActions = []ActionKind{
	ActionCommentDecision, ActionApproveReview, ActionLabelsAdd, ActionLabelsRemove,
	ActionCloseDuplicate, ActionLink, ActionAdoptSupersede, ActionRecommendOnly, ActionCloseNotNow, ActionCloseCompleted,
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
	// VerdictOperatorAction: not code work — the reporting spoke's owner
	// acts (re-authenticates, grants, frees disk). The item keeps its park
	// and never reaches an implementer lane.
	VerdictOperatorAction Verdict = "operator-action"
	// VerdictNotNow declines a hive-filed proposal for now: no incident or
	// recurrence justifies its cost yet; closed as not planned.
	VerdictNotNow Verdict = "not-now"
	// VerdictClose closes a hive advisory as completed: its checklist
	// items are all resolved, so nothing is left for a person to read.
	VerdictClose Verdict = "close"
)

// Advisory rule names (hivecommons/hive#15754).
const (
	// RuleAdvisoryResolved: every item of the advisory's checklist is
	// resolved; the delegate closes it as completed.
	RuleAdvisoryResolved = "advisory-resolved"
	// RuleAdvisoryInProgress: items remain open; the hive's sweep is
	// working them, so the delegate writes nothing and keeps it open.
	RuleAdvisoryInProgress = "advisory-in-progress"
)

// AdvisoryChecklist is the progress of a hive advisory's checklist: how
// many of its items are resolved. Total 0 is an advisory without items.
type AdvisoryChecklist struct {
	Resolved int
	Total    int
}

// Labels the delegate writes or reads.
const (
	// LabelAdoptSupersede is the durable request consumed by the adoption engine.
	LabelAdoptSupersede = "adoption/supersede"
	// RuleCapabilityGap is the recommend-only rule for a blocker no decision
	// can clear: the GitHub App lacks a permission (hivecommons/hive#15099).
	RuleCapabilityGap   = "capability-gap"
	LabelNeedsOwner     = "needs-owner"
	LabelHold           = "hold"
	LabelTriageAccepted = "triage/accepted"
	LabelFlakyTest      = "flaky-test"
	LabelNeedsDecision  = "stage/085-needs-decision"
	// LabelIssueNeedsDecision is the issue-side park (hivecommons/hive#15021).
	LabelIssueNeedsDecision = "stage/045-needs-decision"
)

// CompetingPR is another open PR for the same work.
type CompetingPR struct {
	Number       int
	HiveAuthored bool
	Mergeable    bool
	Current      bool
	// HeldDuplicate: the merge queue holds this competitor out of the
	// train as a duplicate of the item (same `Fixes #N`,
	// hivecommons/hive#15395); the item is the PR the queue kept.
	HeldDuplicate bool
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
	// Author is the PR's / issue's GitHub login, for plain-words answers.
	Author string
	// HiveAuthored: the hive opened the PR / filed the issue.
	HiveAuthored bool
	// TrustedAuthor: the author is the hive or holds write on the repo.
	TrustedAuthor bool
	// SentinelAlert: the sentinel label is on; SentinelReason says why.
	SentinelAlert  bool
	SentinelReason string
	// CIGreen / Conflicting / DCOMissing describe a PR head. CIPending
	// refines a head that is not green: its checks are still running and
	// none has failed, so no fix is owed and "CI is red" is not true of it
	// (hivecommons/hive#15722's preview said so of a green head whose only
	// pending context was tide).
	CIGreen     bool
	CIPending   bool
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
	// HumanAuthor: the PR carries the author/human label — a person pushed
	// it themselves. OwnerAuthor: the author is one of this hive's owners
	// (dashboard.authorized_users). Neither has a hive that can be "offline",
	// and the delegate never adopts their work (hivecommons/hive#15202).
	HumanAuthor bool
	OwnerAuthor bool
	// DuplicateOf is the older issue this one duplicates (0 = none known).
	DuplicateOf int
	// Overlaps lists issues that overlap without duplicating.
	Overlaps []int
	// Competing lists the other open PRs for the same work.
	Competing []CompetingPR
	// TrainDuplicateOf is the open PR the merge queue kept for the same
	// issue when it held this one as a duplicate (0 = not held);
	// TrainDuplicateIssue is that issue (hivecommons/hive#15395).
	TrainDuplicateOf    int
	TrainDuplicateIssue int
	// ClaimConflictWith is the open PR in another merge lane that claims
	// the same issue as this one while no decision is recorded, so the
	// queue admits neither (0 = none); ClaimConflictIssue is that issue
	// (hivecommons/hive#15810).
	ClaimConflictWith  int
	ClaimConflictIssue int
	// ReporterTrustHold: the park is the reporter-trust gate on the fixing PR.
	ReporterTrustHold bool
	// AdoptionStalled: a contributor PR whose adoption parked at 085.
	AdoptionStalled bool
	// AdoptionGraceElapsed: a contributor PR at 072 whose offline grace
	// has run out and whose adoption the engine holds (maintainer policy,
	// or nothing moved). The delegate picks Adopt now
	// (RuleAdoptNow) rather than wait any longer (hivecommons/hive#15038).
	AdoptionGraceElapsed bool
	// WorkflowPath: the PR edits `.github/workflows/*` (read from its file
	// list or from an `adoption-stalled:…workflow-path` park reason). The
	// hive's App has no `workflows` permission, so supersede-adoption is
	// refused by the adoption fence: deciding to adopt cannot take effect
	// (hivecommons/hive#15188).
	WorkflowPath bool
	// BlockedOnPRClosed: a review-backlog item whose PR closed unmerged.
	BlockedOnPRClosed bool
	// SupersededBy names the merged PR that already delivered this PR's
	// work (its issue is closed by that merge, or that PR declares
	// `Supersedes #N`); nil when none is known (hivecommons/hive#15289).
	SupersededBy *MergedCloser
	// DiffEmpty: the PR's diff against its base is empty (+0/−0, no
	// changed files) — with SupersededBy that is the already-landed case
	// (hivecommons/hive#15732), and the preview says so instead of
	// talking about the author's trust.
	DiffEmpty bool
	// AuthorTooling: what the author drives (hive / clanker / person);
	// picks the words a decision uses for them (AuthorWord).
	AuthorTooling AuthorTooling
	// ReworkPickedUp: the author's relay has acknowledged, claimed or
	// pushed the requested rework (hivecommons/hive#15280's signal), so
	// they are responsive even without recent activity.
	ReworkPickedUp bool
	// RepoPaused: the repo is under an operator pause; a decision that
	// would write (close) is recommend-only instead.
	RepoPaused bool
	// Advisory is set for a hive advisory issue (triage/advisory, a dedupe
	// checklist): not work, the hive's sweep ticks and closes it itself.
	Advisory *AdvisoryChecklist
	// AdoptedByHive: the hive adopted this contributor PR in place (stage
	// 074, contributor-lane:adopted-by-hive) and its fix lane owns the
	// branch, so a direction is the hive's own fixer's to carry out — not
	// the contributor's (hivecommons/hive#15722: "send #15722 back to its
	// author" on a PR the hive had adopted).
	AdoptedByHive bool
	// ReviewerRequiresHuman: the recorded park reason is the reviewer
	// swarm's requires_human on the current head; ReviewerFinding is what
	// the review found ("the reviewer marked this change high-risk"), and
	// SecurityFinding says that finding is the security review's — a
	// policy call no fixer can make for a person.
	ReviewerRequiresHuman bool
	ReviewerFinding       string
	SecurityFinding       bool
}

// CIRed reports a head that is not green because a check failed (or none
// ever reported), as opposed to one still waiting on running checks.
func (in Input) CIRed() bool { return !in.CIGreen && !in.CIPending }

// RuleAdoptNow is the rule name of the grace-elapsed Adopt now decision;
// the dashboard runs it as a forced adoption (Adopt now), not a queued one.
const RuleAdoptNow = "grace-elapsed-adopt-now"

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
	// Ladder is the contributor-PR rung the decision landed on and why
	// the rungs above it did not apply (hivecommons/hive#15289).
	Ladder *Ladder `json:"ladder,omitempty"`
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
	secretsRE    = regexp.MustCompile(`(?i)\b(secrets?|PATs?|personal access tokens?|org(?:anization)? (?:membership|permissions?|admin|owner)|token scopes?|GITHUB_TOKEN|credentials?|app installation|billing)\b`)
	flakyRE      = regexp.MustCompile(`(?i)\b(flak(?:y|e|iness)|intermittent(?:ly)? fail\w*|non-?deterministic test)\b`)
	ratchetRE    = regexp.MustCompile(`(?i)\b(actionlint|ratchet|duplication (?:gate|threshold|check)|link[- ]?checker|allow-?list|lint gate|hygiene gate|shellcheck|yamllint|markdownlint)\b`)
	proposalRE   = regexp.MustCompile(`(?i)\bacceptance criteria\b`)
	hivePolicyRE = regexp.MustCompile(`(?i)\b(hive\.ya?ml|governance|security posture|hive (?:settings?|config(?:uration)?|policy|policies)|branch protection|CODEOWNERS|required checks?|autonomy level|acmm level|issue[_ ]filter|hive-filed|unacknowledged proposals?)\b`)
	// appPermissionRE preserves the park for App permissions that have no
	// specific capability kind in the blocker ledger, and for free-text
	// workflow headlines ("the App cannot push workflows") that name no
	// permission or path shape the ledger's workflows rule recognises.
	appPermissionRE = regexp.MustCompile(`(?i)\b(?:app(?: token)?|installation|github app)\b[^\n]{0,80}\b(?:cannot|can't|lacks?|missing|without|no)\b[^\n]{0,60}\b(?:workflows?|permissions?)\b`)
	duplicateOfRE   = regexp.MustCompile(`(?i)\bduplicate of #(\d+)`)
	blockedOnPRRE   = regexp.MustCompile(`(?i)\b(review backlog|blocked on (?:its|their|the) PR)\b`)
)

// Classifier labels.
const (
	labelKindProposal = "kind/proposal"
	labelProposal     = "proposal"
	labelRFC          = "rfc"
	labelKindDesign   = "kind/design"
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

// CapabilityGap reports a needs-human whose blocker is a missing GitHub App
// capability rather than an open question. Shared ledger inference handles
// known capability kinds; a generic App-permission guard keeps other missing
// permissions parked. Human-only blockers are handled by the ledger.
func CapabilityGap(in Input) bool {
	isCapability := func(text string) bool {
		k, ok := blocker.Infer(text)
		return ok && k.IsCapability() || appPermissionRE.MatchString(text)
	}
	return isCapability(in.headline()) ||
		in.Kind == KindIssue && isCapability(firstLines(in.Body, 12))
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
	if in.Advisory != nil {
		return advisoryDecision(ref, *in.Advisory)
	}
	// A self-report whose remedy is the spoke operator's comes first: its
	// body names tokens and credentials, which would otherwise read as an
	// owner-only secrets item and hold it forever.
	if sr, ok := ParseSelfReport(in.Title, in.Body); ok {
		if remedy, ops := selfreportOpsClass(sr.Class); ops {
			return operatorActionDecision(sr, remedy)
		}
	}
	if HivePolicyChange(in) {
		return recommendOnly("hive-policy", "this changes the hive's own policy or scope (settings, governance, security posture); the owner confirms those", hivePolicyRecommendation(in))
	}
	if CapabilityGap(in) {
		return recommendOnly(RuleCapabilityGap, "the blocker is a missing GitHub App permission; approving a direction would un-park the item only for the same needs-human to be re-applied, so the park stays until the permission changes",
			"grant the App the missing permission (for example `workflows`) or have a human apply the change, then re-check; hive cannot clear this by deciding")
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
	if IsProposal(in.Title, in.Labels) {
		if kind, detail, ok := ProposalEvidence(in.Title, in.Body); ok {
			return approveIssue("proposal-approve-evidence", "approve as proposed: "+detail+", so the cost of the gate is already paid for",
				"the proposal carries "+kind+" evidence — "+detail+" — which is what justifies a new gate")
		}
		if hiveFiled(in) {
			return notNowDecision(ref)
		}
	}
	d := approveIssue("default-approve", "go with the recommendation; where options remain, take the one that keeps work moving and is reversible",
		"no owner rule singles this out; the standing default is the hive's own recommendation")
	for _, n := range sortedInts(in.Overlaps) {
		d.Actions = append(d.Actions, Action{Kind: ActionLink, Target: n, Text: fmt.Sprintf("overlaps #%d", n)})
	}
	return d
}

// hivePolicyRecommendation is the sentence the owner reads on a hive-policy
// item: a real recommendation, in the owner's terms, not "no rule applies".
func hivePolicyRecommendation(in Input) string {
	if in.Kind != KindIssue {
		return "accept the proposal as written with its stated defaults, or close it as won't-fix if it widens the hive's write scope"
	}
	rec := "approve `" + strings.TrimSpace(in.Title) + "` as proposed"
	lower := strings.ToLower(in.Body)
	if strings.Contains(lower, "proposal") && (strings.Contains(lower, "acknowledg") || strings.Contains(lower, "approved-direction")) {
		rec += " — proposals nobody has acknowledged should not be actionable"
	}
	if strings.Contains(lower, "opt-in") {
		rec += "; make it default-on for the lanes it names rather than opt-in"
	}
	return rec
}

// operatorActionDecision answers a self-report whose fix is the spoke
// owner's: one comment naming who does what, no approved-direction, no
// implementer, the park kept so the actionable set never sees it.
func operatorActionDecision(sr SelfReport, remedy string) Decision {
	text := operatorActionPrefix + sr.OperatorInstruction(remedy)
	return Decision{
		Verdict: VerdictOperatorAction, Rule: "selfreport-operator-action", Text: text,
		Rationale: "the `" + sr.Class + "` self-report is not code work: the remedy is on the reporting spoke, so handing it to an implementer agent only loops back here; it closes itself once the report stops recurring (or `/fixed`)",
		KeepsPark: true,
		Actions: []Action{
			{Kind: ActionLabelsAdd, Labels: []string{LabelNeedsOwner}},
			{Kind: ActionRecommendOnly, Text: text},
		},
	}
}

// notNowDecision declines a hive-filed proposal that shows neither an
// incident nor a recurrence: the reason is posted and the issue is closed
// as not planned, the way a declined proposal leaves the tracker.
// advisoryDecision decides a hive advisory (hivecommons/hive#15754). Its
// items are the hive's own work: while any is open nothing is written and
// the advisory stays open (not parked — the tracker never parks one); once
// all are resolved it is closed as completed.
func advisoryDecision(ref string, adv AdvisoryChecklist) Decision {
	if adv.Total > 0 && adv.Resolved < adv.Total {
		return Decision{
			Verdict: VerdictRefused, Rule: RuleAdvisoryInProgress,
			Text:      fmt.Sprintf("keep %s open: the hive is working its items (%d/%d resolved)", ref, adv.Resolved, adv.Total),
			Rationale: fmt.Sprintf("the hive is working the items (%d/%d resolved); the advisory closes itself when all are", adv.Resolved, adv.Total),
			KeepsPark: true,
		}
	}
	text := fmt.Sprintf("close %s: its items are tracked by the hive", ref)
	rationale := "an advisory is a note, not work; nothing waits on it"
	if adv.Total > 0 {
		text = fmt.Sprintf("close %s: all %d items are resolved", ref, adv.Total)
		rationale = fmt.Sprintf("all %d of the advisory's items are resolved; nothing is left to read", adv.Total)
	}
	return Decision{
		Verdict: VerdictClose, Rule: RuleAdvisoryResolved, Text: text, Rationale: rationale,
		Actions: []Action{{Kind: ActionCloseCompleted, Text: text}},
	}
}

func notNowDecision(ref string) Decision {
	text := "not now: no incident or recurrence justifies the cost of " + ref + " yet; re-propose with the incident or the repeated instances it would have prevented"
	return Decision{
		Verdict: VerdictNotNow, Rule: "proposal-not-now", Text: text,
		Rationale: "a hive-filed proposal is paid for by evidence; without an incident it would have prevented or a class it would end, the cost is speculative",
		Actions:   []Action{{Kind: ActionCloseNotNow, Text: text}},
	}
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
	// Two open PRs for one issue in different lanes (hivecommons/hive#15810):
	// the queue holds both and the hive only names the choice.
	if d, ok := decideClaimConflict(in, ref); ok {
		return d
	}
	// Two open PRs for one issue (hivecommons/hive#15395): the merge
	// queue already picked the survivor (green first, then earlier); the
	// held one closes as superseded, the kept one is released.
	if d, ok := decideTrainDuplicate(in, ref); ok {
		return d
	}
	// A head parked on DCO or the reporter-trust gate must clear that gate
	// before a competing-PR rule can release or supersede it.
	if !in.DCOMissing && !in.ReporterTrustHold {
		if d, ok := decideCompeting(in, ref); ok {
			return d
		}
	}
	// A PR the hive adopted in place is the hive's own to fix: the
	// reviewer's finding and the head's blockers go to the fixer, never
	// "back to the author" (hivecommons/hive#15722).
	if d, ok := decideAdoptedByHive(in, ref); ok {
		return d
	}
	// The contributor-PR ladder (hivecommons/hive#15289): already fixed →
	// mergeable as-is → rebase-then-merge → return to contributor →
	// adopt-supersede, in that order, so a PR whose issue a merged PR
	// already closed is closed as superseded rather than adopted. A
	// person's PR leaves the ladder after the already-fixed rung.
	if d, ok := decideLadder(in, ref); ok {
		return d
	}
	// A person pushed this PR themselves (an owner, or author/human):
	// it is theirs to fix, never the hive's to adopt (hivecommons/hive#15202).
	if in.PersonAuthored() {
		if d, ok := decideReturnToAuthor(in, ref); ok {
			return d
		}
	}
	if !in.HiveAuthored && !in.PersonAuthored() && !in.ContributorOnline && in.WorkflowPath && (in.AdoptionStalled || in.AdoptionGraceElapsed || in.Conflicting || in.CIRed() || in.DCOMissing) {
		// The adoption engine refuses to publish a contributor's workflow
		// edits from the App (contributor-lane:workflow-path), so an
		// adopt-supersede here is decided and never executed
		// (hivecommons/hive#15188). Same rule as the issue-side gap
		// (#15127) so the tracker routes it to the capability blocker.
		return recommendOnly(RuleCapabilityGap, "the PR edits .github/workflows and the hive's GitHub App lacks the `workflows` permission, so the adoption fence refuses to supersede it; deciding to adopt cannot take effect",
			"grant the App the `workflows` permission and re-run the decision, or have a person rebase and merge "+ref+" on GitHub, or close it as superseded")
	}
	if in.ReporterTrustHold && in.CIGreen && !in.Conflicting && !in.DCOMissing {
		d := releasePR("reporter-trust", "approve "+ref+": the fixing PR is sound, so the reporter-trust hold is cleared", "a reporter-trust hold is cleared when the fixing PR is sound")
		d.Actions = append(d.Actions, Action{Kind: ActionLabelsAdd, Labels: []string{LabelTriageAccepted}})
		return d
	}
	if fix := fixList(in); len(fix) > 0 {
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

// RuleAdoptedFixer: the hive adopted the PR in place, so the reviewer's
// finding and the head's blockers are its own fixer's to address; no person
// and no contributor is asked. RuleAdoptedSecurityFinding is the one case
// the fixer cannot settle alone: the security review withheld approval,
// and that policy call is a person's (hivecommons/hive#15722).
const (
	RuleAdoptedFixer           = "adopted-fixer"
	RuleAdoptedSecurityFinding = "adopted-security-finding"
)

// decideAdoptedByHive is the adopted-in-place branch of decidePR. The hive
// owns the branch, so "send it back to its author" is wrong — the hive is
// the author now. A reviewer requires_human or a head blocker becomes a
// direction the hive's fixer carries out; a security finding is named in
// full and left to a person. A green, unquestioned adopted head falls
// through to the ordinary rules.
func decideAdoptedByHive(in Input, ref string) (Decision, bool) {
	if in.Kind != KindPR || !in.AdoptedByHive || in.HiveAuthored || in.PersonAuthored() {
		return Decision{}, false
	}
	finding := strings.TrimSpace(in.ReviewerFinding)
	if in.ReviewerRequiresHuman && in.SecurityFinding {
		what := "the security review withheld approval"
		if finding != "" {
			what = finding
		}
		return recommendOnly(RuleAdoptedSecurityFinding,
			"the hive adopted "+ref+" and owns its branch, but the reviewer's finding is a security one — "+what+" — and that policy call is a person's, not the fixer's",
			"read the finding on "+ref+" ("+what+") and reply `/hive decision <instruction>` for the hive's fixer to carry out, or close it"), true
	}
	var fix []string
	if in.ReviewerRequiresHuman {
		item := "address the reviewer's finding"
		if finding != "" {
			item += " (" + finding + ")"
		}
		fix = append(fix, item)
	}
	fix = append(fix, fixList(in)...)
	if len(fix) == 0 {
		return Decision{}, false
	}
	text := "hive's fixer: " + strings.Join(fix, ", ") + " on " + ref + "; then it merges"
	return Decision{
		Verdict: VerdictDirection, Rule: RuleAdoptedFixer, Text: text,
		Rationale: "the hive adopted " + ref + " in place and owns its branch, so the reviewer's finding is the hive's own fixer's to address — not the contributor's and not a person's (hivecommons/hive#15722)",
		Actions:   []Action{decisionComment(text)},
	}, true
}

// RuleOnePRPerIssue: the merge queue carries one PR per issue; the one it
// held as a duplicate closes as superseded by the one it kept.
const RuleOnePRPerIssue = "one-pr-per-issue"

// RuleSameIssueClaimConflict: two open PRs in different merge lanes claim
// one issue and nobody has decided which lands; the queue admits neither
// and the hive names the choice without making it (hivecommons/hive#15810).
const RuleSameIssueClaimConflict = "same-issue-claim-conflict"

// decideClaimConflict is the recommend-only answer for a PR the merge
// queue holds on an undecided same-issue claim: the hive's own PR and a
// trusted human's PR for hivecommons/hive#15773 both marched toward merge
// because each lane saw only its own. The hive does not pick — it names
// the two PRs and the two ways out; Approve on one row admits that PR,
// Close as superseded on the other ends the conflict.
func decideClaimConflict(in Input, ref string) (Decision, bool) {
	if in.Kind != KindPR || in.ClaimConflictWith <= 0 {
		return Decision{}, false
	}
	other := fmt.Sprintf("#%d", in.ClaimConflictWith)
	issue := "the same issue"
	if in.ClaimConflictIssue > 0 {
		issue = fmt.Sprintf("issue #%d", in.ClaimConflictIssue)
	}
	return recommendOnly(RuleSameIssueClaimConflict,
		fmt.Sprintf("%s and %s both claim %s from different merge lanes; the merge queue admits neither until one is closed or approved, so two implementations of one fix never both land", ref, other, issue),
		fmt.Sprintf("keep %s, close %s — or keep %s, close %s; the hive will not pick", other, ref, ref, other)), true
}

// decideTrainDuplicate resolves a same-issue pair the merge queue split
// (hivecommons/hive#15395). The queue's pick stands: a green head beats a
// red one, the earlier-opened wins among equals. Deciding on the held PR
// closes it as superseded by the kept one, with a comment; deciding on
// the kept PR releases it and names the duplicate the queue holds. A
// paused repo only gets the recommendation.
func decideTrainDuplicate(in Input, ref string) (Decision, bool) {
	if in.Kind != KindPR {
		return Decision{}, false
	}
	issue := ""
	if in.TrainDuplicateIssue > 0 {
		issue = fmt.Sprintf(" for issue #%d", in.TrainDuplicateIssue)
	}
	if in.TrainDuplicateOf > 0 {
		winner := fmt.Sprintf("#%d", in.TrainDuplicateOf)
		text := fmt.Sprintf("superseded by %s: both PRs fix the same issue%s; the merge queue carries one PR per issue and kept %s (green first, then the earlier-opened)", winner, issue, winner)
		rationale := fmt.Sprintf("%s and %s declare the same closing issue%s; the merge queue holds one PR per issue and kept %s, so %s closes as superseded with credit", ref, winner, issue, winner, ref)
		if in.RepoPaused {
			return recommendOnly(RuleOnePRPerIssue, rationale+"; the repo is paused, so the hive only recommends", "close "+ref+" as superseded by "+winner), true
		}
		note := fmt.Sprintf("Thank you for this PR. %s fixes the same issue%s and the merge queue carries one PR per issue; it kept %s (green first, then the earlier-opened), so the hive is closing this one as superseded. Your work is credited on the issue.", winner, issue, winner)
		return Decision{
			Verdict: VerdictCloseSuperseded, Rule: RuleOnePRPerIssue, Text: text,
			Rationale: rationale,
			Actions:   []Action{{Kind: ActionCloseDuplicate, Target: in.TrainDuplicateOf, Text: note}},
		}, true
	}
	for _, other := range in.Competing {
		if !other.HeldDuplicate {
			continue
		}
		held := fmt.Sprintf("#%d", other.Number)
		if in.Conflicting || in.CIRed() || in.DCOMissing {
			return recommendOnly(RuleOnePRPerIssue,
				fmt.Sprintf("the merge queue kept %s over %s%s, but %s is not mergeable now (%s); the queue re-picks on its next pass", ref, held, issue, ref, mergeBlockers(in)),
				"fix "+ref+" ("+mergeBlockers(in)+"), or decide on "+held+" to let it supersede this one"), true
		}
		return releasePR(RuleOnePRPerIssue,
			fmt.Sprintf("approve %s: the merge queue carries one PR per issue%s and kept this one; %s is held as its duplicate and closes as superseded once this merges", ref, issue, held),
			"one PR per issue in the merge queue; the kept PR merges, the held duplicate closes as superseded"), true
	}
	return Decision{}, false
}

// decideCompeting applies "prefer an adequate contributor PR over a hive
// duplicate" when the item has a competing PR. Mergeability alone cannot
// establish adequacy, so closing a hive PR requires a human comparison.
func decideCompeting(in Input, ref string) (Decision, bool) {
	for _, other := range in.Competing {
		switch {
		case in.HiveAuthored && !other.HiveAuthored && other.Mergeable && other.Current:
			return recommendOnly("prefer-contributor",
				"mergeability and an issue reference do not establish that a competing PR adequately replaces the hive's work",
				fmt.Sprintf("compare #%d with %s for issue coverage, passing checks and review approval; keep %s open until that comparison confirms the contributor's PR is an adequate replacement", other.Number, ref, ref)), true
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

// PersonAuthored: a human or one of this hive's owners pushed the PR
// themselves. Such a PR is never a "contributor" with an expected connected
// hive, so the contributor-offline rule and adoption do not apply to it.
func (in Input) PersonAuthored() bool { return in.HumanAuthor || in.OwnerAuthor }

// RuleReturnToAuthor hands a person's PR back to that person with the exact
// fix list and the reviewer's question, instead of adopting it.
const RuleReturnToAuthor = "return-to-author"

// decideReturnToAuthor is the person-authored branch of decidePR: a head that
// is not mergeable, or a reviewer question still open, goes back to the
// author by name. A green, mergeable head with no open question falls
// through to the ordinary approve rules.
// ReturnToAuthor is decideReturnToAuthor for callers outside the policy
// (the 090 human-merge decider): the person's PR goes back to them with
// the fix list and the reviewer's question; ok=false when nothing is owed.
func ReturnToAuthor(in Input) (Decision, bool) {
	return decideReturnToAuthor(in, "#"+strconv.Itoa(in.Number))
}

func decideReturnToAuthor(in Input, ref string) (Decision, bool) {
	fix := fixList(in)
	question := strings.TrimSpace(in.Question)
	if len(fix) == 0 && question == "" {
		return Decision{}, false
	}
	who := "the author"
	if a := strings.TrimSpace(in.Author); a != "" {
		who = "@" + strings.TrimPrefix(a, "@")
	}
	var parts []string
	if len(fix) > 0 {
		parts = append(parts, who+": "+strings.Join(fix, ", ")+" on "+ref+"; then it merges")
	}
	if question != "" {
		parts = append(parts, who+", the reviewer asked: "+question)
	}
	text := strings.Join(parts, " — ")
	return Decision{
		Verdict: VerdictDirection, Rule: RuleReturnToAuthor, Text: text,
		Rationale: "a person pushed this PR themselves, so the hive asks that person instead of adopting the work (hivecommons/hive#15202)",
		Actions:   []Action{decisionComment(text)},
	}, true
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
	// PersonAuthored: a human or owner pushed the PR; adopt-supersede is
	// refused whatever the rule chose (hivecommons/hive#15202).
	PersonAuthored bool
}

// RefusalAdoptPersonAuthored is the refusal ApplyRails records when a rule
// asked to adopt a person's PR.
const RefusalAdoptPersonAuthored = "adopt-supersede: a person pushed this PR themselves; the hive asks the author instead of adopting it"

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
		if a.Kind == ActionAdoptSupersede && r.PersonAuthored {
			out.Refusals = append(out.Refusals, RefusalAdoptPersonAuthored)
			continue
		}
		out.Actions = append(out.Actions, a)
	}
	if len(out.Actions) == 0 && d.Verdict != VerdictRefused {
		// A decision that already refuses with no actions (an advisory the
		// hive is still working) stays a no-write refusal, not a
		// recommendation comment.
		out.Verdict = VerdictRecommendOnly
		out.KeepsPark = true
		out.Actions = []Action{{Kind: ActionRecommendOnly, Text: "hive recommends: " + d.Text + "; owner to confirm"}}
	}
	return out
}

// Run is Decide followed by ApplyRails with the hold rail read from the labels.
func Run(in Input) Decision {
	return ApplyRails(Decide(in), Rails{Held: hasLabel(in.Labels, LabelHold), PersonAuthored: in.PersonAuthored()})
}

// CommentBody renders the comment the delegate posts for a decision: the
// `/hive decision <text>` line a maintainer would type (or the
// recommendation when the park is kept), the rationale, the plain-words
// block (PlainWords), the attribution and the audit marker. Callers that
// know the item kind and the effect timeout use CommentBodyWith.
func CommentBody(d Decision, hiveID, requester string) string {
	return CommentBodyWith(d, hiveID, requester, CommentOptions{})
}
