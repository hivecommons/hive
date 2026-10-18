// Package stagelabels is the hive-owned pipeline state machine rendered as
// GitHub labels (hivecommons/hive#12429).
//
// One ordered, mutually exclusive `stage/<NN>-<slug>` family is shared by
// issues and PRs. The number is sparse (10, 20, …) so a stage can be inserted
// without renumbering, and alphabetical label sort equals pipeline order.
// Everything about a stage — number, slug, colour, description, expected
// dwell — comes from the single ordered Table below; code keys stages by the
// Stage enum, never by label text. A `stage/*` label on an item that is not
// in the current table is stale and the reconciler removes it, so renaming
// or inserting a stage is an edit to Table: the hive relabels every open item
// on its next cycle and the old labels can be deleted from the repo.
//
// Three orthogonal, hive-owned families ride alongside the stage:
//   - `role/<tier>`: the PR author's admission tier as the contributor lane
//     actually computed it, so a reader can tell whether the hive would
//     auto-merge the PR. At most one per PR. (Spelled `author/<tier>` before
//     hivecommons/hive#12581; the reconciler migrates the old spelling.)
//   - `author/<kind>`: WHO wrote the PR — a person by hand (`author/human`),
//     a contributor's own hive under that contributor's login
//     (`author/clanker`), or this repo's own hive (`author/hive`). At most
//     one per PR; orthogonal to role: a trusted contributor whose hive
//     opened the PR carries `role/trusted` + `author/clanker`.
//   - `stage/stalled`: the one health marker. It means NOBODY IS WORKING
//     THE ITEM: no new head, comment, review/verdict, CI state change or
//     non-hive label change for the stage's expected dwell — and never while
//     the item is waiting on the hive itself (required checks running, the
//     reviewer verdict pending for the current head). Any activity clears
//     it at once (hivecommons/hive#12534).
//
// A backward stage move (050 → 060 → 072 → 070 → 050 is the review loop
// working as designed) is NOT a health problem: it is narrated once in the
// stage comment and kept in the store's transition history, but carries no
// label. The former `stage/regressed` label is retired; the reconciler
// strips it from any item still wearing it.
//
// `hold`, `needs-human`, `needs-decision`, `needs-rebase`, `blocked`,
// `role/*` and `author/*` are stage MODIFIERS, not stages: they stay
// whatever the stage is. A closed, merged or superseded item is terminal and carries no stage.
package stagelabels

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
)

func init() { config.RegisterStageSlugs(Slugs()) }

// Stage is the pipeline position. The zero value None means "no stage":
// the item is terminal, out of the pipeline, or the hive has no opinion.
type Stage int

const (
	None Stage = iota
	Actionable
	Claimed
	PRInProgress
	CoveredByPR
	ReviewPending
	Reviewed
	Fixing
	ReturnedToContributor
	AdoptedByHive
	AwaitingMergeAdmission
	// ReadyForHumanMerge is stage/090 (hivecommons/hive#12528): the hive
	// would merge this itself — verdict approve at the current head, checks
	// green, DCO, soak, no modifiers — and only trust/role blocks it.
	ReadyForHumanMerge
	MergeQueue
	// NeedsDecision is stage/085: a human decision (not a merge) is owed.
	// Declared after MergeQueue so persisted records (which store the enum
	// value) written before it existed keep their meaning; pipeline order
	// comes from the Table's numbers, never from this enum.
	NeedsDecision
	// NeedsAuthor is stage/071: a human-authored PR that is red or blocked
	// on something only its author can do (CI, rebase, DCO, changes
	// requested). The clanker counterpart is 072 returned-to-contributor,
	// the hive's own is 070 fixing — see FixingStageFor. Appended for the
	// same persisted-enum reason as NeedsDecision.
	NeedsAuthor
	// IssueNeedsDecision is stage/045: an ISSUE parked on a person — a
	// maintainer decision (needs-human, needs-spec, the decision park
	// label) is owed before any agent may pick it up. It is the issue-side
	// counterpart of 085, which is PR-only; the two never cross kinds
	// (Stage.For clamps 085 asked of an issue to 045). Appended for the
	// same persisted-enum reason as NeedsDecision.
	IssueNeedsDecision
)

// Prefix is the stage label family prefix. Every label under it is
// hive-owned: the reconciler removes any it does not currently emit.
const Prefix = "stage/"

// RolePrefix is the admission-tier family prefix (same ownership rule).
const RolePrefix = "role/"

// AuthorPrefix is the authorship-kind family prefix (authorkind.Prefix; same
// ownership rule).
// Until hivecommons/hive#12581 the admission tier lived here too; those
// spellings are LegacyRoleLabels now and the reconciler replaces them.
const AuthorPrefix = authorkind.Prefix

// StalledLabel is the health label; it coexists with the stage label.
const StalledLabel = Prefix + "stalled"

// RegressedLabel is the RETIRED backward-move marker (hivecommons/hive#12534).
// It is no longer provisioned or emitted; being under Prefix it is Owned, so
// Plan removes it from every item the reconciler touches.
const RegressedLabel = Prefix + "regressed"

// Family colours. Every stage/NN label shares StageColor (the hive-amber the
// retired `🐝 covered by PR` label used) so the eye tracks the family across
// lists; the health marker is red; role/* is neutral grey except the one
// tier that means "the hive will merge this". The author/* kinds each get
// their own hue so the three read apart at a glance: human is the same
// neutral grey as the non-trusted roles, clanker (a contributor's hive)
// is purple, hive (this repo's own hive) is blue.
const (
	StageColor         = "f5a623"
	StalledColor       = "d73a4a"
	RoleColor          = "6e7781"
	RoleTrustedColor   = "0e8a16"
	AuthorHumanColor   = RoleColor
	AuthorClankerColor = "8250df"
	AuthorHiveColor    = "1d76db"
)

// Pre-#12581 names of the role colours, kept for readers that tie another
// label's colour to the trusted green.
const (
	AuthorColor        = RoleColor
	AuthorTrustedColor = RoleTrustedColor
)

// Definition is one row of the ordered stage table.
type Definition struct {
	Stage Stage
	// Number is the sparse ordinal rendered into the label.
	Number int
	// Slug is the human-readable suffix.
	Slug string
	// Description is the GitHub label description.
	Description string
	// Dwell is how long an item may go without activity in this stage
	// before it is marked stalled. 0 means never (the stage is open-ended).
	Dwell time.Duration
	// StallNote asks the reconciler to post a one-line comment (with the
	// lane's last skip reason) when the item stalls here: a stall in such a
	// stage points at the hive, not the author, so it must be legible.
	StallNote bool
	// Issue/PR report which item kinds the stage may be written to. The
	// two sets are disjoint: an issue's pipeline ends at 040-covered-by-pr
	// (045-needs-decision is its parking bay) and the covering PR's own
	// progress is narrated in the issue's stage reason/comment, never
	// mirrored as a second label (hivecommons/hive#12451).
	// Enforce clamps a stage to the item's kind (Stage.For) so a PR-only
	// stage can never land on an issue and vice versa.
	Issue, PR bool
}

// Item is the kind of forge item a stage is written to.
type Item int

const (
	// IssueItem: an issue. Only Issue:true stages (010–045; 045 is the
	// issue parked on a person) may land on it.
	IssueItem Item = iota
	// PRItem: a pull request. Only PR:true stages (050–100) may land on it.
	PRItem
)

// String is "issue" or "pr".
func (k Item) String() string {
	if k == PRItem {
		return "pr"
	}
	return "issue"
}

// Applies reports whether the stage may be written to an item of kind k.
// None applies everywhere (it clears the stage).
func (s Stage) Applies(k Item) bool {
	if s == None {
		return true
	}
	d, ok := definition(s)
	if !ok {
		return false
	}
	if k == PRItem {
		return d.PR
	}
	return d.Issue
}

// For returns the stage to write on an item of kind k when the caller wants
// s. An applicable stage is returned unchanged. 085 needs-decision asked of
// an issue clamps to its issue-side twin 045 (the park survives, on the
// issue's own stage); any other PR-only stage asked of an issue clamps to
// 040-covered-by-pr (the PR exists, that is all the issue says). An
// issue-only stage asked of a PR has no PR meaning and yields None.
// Unknown stages yield None.
func (s Stage) For(k Item) Stage {
	if s.Applies(k) {
		return s
	}
	if k == PRItem || !s.Known() {
		return None
	}
	if s == NeedsDecision {
		return IssueNeedsDecision
	}
	return CoveredByPR
}

// Table is THE ordered stage table. Edit it to insert, rename or re-time a
// stage; nothing else in the hive hard-codes a stage label string.
var Table = []Definition{
	{Stage: Actionable, Number: 10, Slug: "actionable", Description: "Triaged; eligible for the hive to pick up", Dwell: 24 * time.Hour, Issue: true},
	{Stage: Claimed, Number: 20, Slug: "claimed", Description: "Claimed by the scanner or a relay contributor", Dwell: 2 * time.Hour, Issue: true},
	{Stage: PRInProgress, Number: 30, Slug: "pr-in-progress", Description: "An agent kick is running; no PR yet", Dwell: 90 * time.Minute, Issue: true},
	{Stage: CoveredByPR, Number: 40, Slug: "covered-by-pr", Description: "An open PR covers this issue", Issue: true},
	{Stage: IssueNeedsDecision, Number: 45, Slug: "needs-decision", Description: "Issue parked on a person: needs-human or a maintainer decision is owed before the hive picks it up", Issue: true},
	{Stage: ReviewPending, Number: 50, Slug: "review-pending", Description: "Review swarm requested; awaiting a verdict for the current head", Dwell: time.Hour, PR: true},
	{Stage: Reviewed, Number: 60, Slug: "reviewed", Description: "Review swarm verdict approve; a gate is still pending (CI running, soak not started)", Dwell: 30 * time.Minute, PR: true},
	{Stage: Fixing, Number: 70, Slug: "fixing", Description: "Hive-authored PR: CI red, changes requested or needs rebase; a hive agent is fixing it", Dwell: 2 * time.Hour, PR: true},
	{Stage: NeedsAuthor, Number: 71, Slug: "needs-author", Description: "Human-authored PR: CI red, changes requested or conflicting; diagnosis posted, the author must act", Dwell: 24 * time.Hour, PR: true},
	{Stage: ReturnedToContributor, Number: 72, Slug: "returned-to-contributor", Description: "Clanker PR: CI red, changes requested or conflicting; diagnosis posted, its hive must act", Dwell: 24 * time.Hour, PR: true},
	{Stage: AdoptedByHive, Number: 74, Slug: "adopted-by-hive", Description: "Contributor PR adopted: author unavailable after red CI; the hive is finishing it (takeover path)", Dwell: 3 * time.Hour, PR: true},
	{Stage: AwaitingMergeAdmission, Number: 80, Slug: "awaiting-merge-admission", Description: "CI green, verdict approve, no modifiers; the automerge lane has not admitted it yet", Dwell: 30 * time.Minute, StallNote: true, PR: true},
	{Stage: NeedsDecision, Number: 85, Slug: "needs-decision", Description: "PR parked on a human decision: sentinel alert, hold/needs-decision label or human-only rule", PR: true},
	{Stage: ReadyForHumanMerge, Number: 90, Slug: "ready-for-human-merge", Description: "Hive would merge this but trust/role blocks it; a maintainer's /lgtm /approve is all that is missing", PR: true},
	{Stage: MergeQueue, Number: MergeQueueNumber, Slug: "merge-queue", Description: "Lane admitted: soaking, merge-eligible or waiting for tide / the merge window", Dwell: 3 * time.Hour, PR: true},
}

// FixingStageFor is the "author must act" stage for a red or blocked PR by
// who wrote it: the hive's own PR (selfAuthored, or author/hive) goes to 070
// fixing — a hive agent reworks it; an author/clanker PR goes to 072
// returned-to-contributor — the contributor's hive is handed it back; a
// human (or unknown-kind) PR goes to 071 needs-author — the person must act.
// The three carry the same evidence and leave through the same transitions;
// only who is on the hook differs (docs: "Stage 07x by author kind").
func FixingStageFor(selfAuthored bool, kind authorkind.Kind) Stage {
	switch {
	case selfAuthored, kind == authorkind.Hive:
		return Fixing
	case kind == authorkind.Clanker:
		return ReturnedToContributor
	}
	return NeedsAuthor
}

// AuthorBlocked reports whether s is one of the three "author must act"
// repair stages (070 fixing, 071 needs-author, 072 returned-to-contributor).
func AuthorBlocked(s Stage) bool {
	return s == Fixing || s == NeedsAuthor || s == ReturnedToContributor
}

// MergeQueueNumber is fixed: the merge queue is always the top stage. New
// stages are inserted between existing numbers, never above 100.
const MergeQueueNumber = 100

// NumberWidth is the zero-padded width of the number in a label.
const NumberWidth = 3

var healthDefs = []LabelDef{
	{Name: StalledLabel, Color: StalledColor, Description: "No activity (head, comment, review, CI, label) longer than the stage expects; nobody is working it"},
}

func definition(s Stage) (Definition, bool) {
	for _, d := range Table {
		if d.Stage == s {
			return d, true
		}
	}
	return Definition{}, false
}

// Label renders the stage label, or "" for None / an unknown stage.
func (s Stage) Label() string {
	d, ok := definition(s)
	if !ok {
		return ""
	}
	return Prefix + numberString(d.Number) + "-" + d.Slug
}

// numberString zero-pads to NumberWidth so GitHub's lexical label sort
// matches pipeline order (100 must sort after 090).
func numberString(n int) string {
	return fmt.Sprintf("%0*d", NumberWidth, n)
}

// Number is the sparse ordinal, or 0 for None.
func (s Stage) Number() int {
	d, _ := definition(s)
	return d.Number
}

// Slug is the label suffix, or "" for None.
func (s Stage) Slug() string {
	d, _ := definition(s)
	return d.Slug
}

// String is the "NN-slug" form used in comments and logs ("none" for None).
func (s Stage) String() string {
	d, ok := definition(s)
	if !ok {
		return "none"
	}
	return numberString(d.Number) + "-" + d.Slug
}

// Description is the GitHub label description, or "" for None.
func (s Stage) Description() string {
	d, _ := definition(s)
	return d.Description
}

// Dwell is the table's expected dwell for the stage (0 = open-ended).
func (s Stage) Dwell() time.Duration {
	d, _ := definition(s)
	return d.Dwell
}

// StallNote reports whether a stall in s should be explained in a comment.
func (s Stage) StallNote() bool {
	d, _ := definition(s)
	return d.StallNote
}

// Known reports whether s is a stage in the current table.
func (s Stage) Known() bool {
	_, ok := definition(s)
	return ok
}

// Before reports whether s is earlier in the pipeline than other. None is
// before everything.
func (s Stage) Before(other Stage) bool {
	return s.Number() < other.Number()
}

// YieldsToAdoption reports whether a live adoption (Record.AdoptionLive)
// replaces s with AdoptedByHive. Adoption is who owns the PR, not what CI
// says, so it outranks every review-side stage — 050 review-pending, 060
// reviewed and the 07x repair stages alike — and gives way only at the
// merge gate (080 and later), where the PR is leaving the hive's hands.
// None (draft, out of the pipeline) is never replaced.
func (s Stage) YieldsToAdoption() bool {
	return s != None && s.Before(AwaitingMergeAdmission)
}

// StageBySlug resolves a slug (as used in config dwell overrides) to a stage.
// The two parking stages share the slug "needs-decision" (045 on issues,
// 085 on PRs); the first table row — 045 — wins. Both are open-ended, so a
// dwell override keyed on that slug is a no-op either way.
func StageBySlug(slug string) (Stage, bool) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	for _, d := range Table {
		if d.Slug == slug {
			return d.Stage, true
		}
	}
	return None, false
}

// StageBySlugFor resolves a slug to the stage of that name that applies to
// items of kind k (045 vs 085 for "needs-decision"), or None.
func StageBySlugFor(slug string, k Item) Stage {
	slug = strings.ToLower(strings.TrimSpace(slug))
	for _, d := range Table {
		if d.Slug == slug && d.Stage.Applies(k) {
			return d.Stage
		}
	}
	return None
}

// Slugs lists every stage slug in pipeline order (for config/docs).
func Slugs() []string {
	out := make([]string, 0, len(Table))
	for _, d := range Table {
		out = append(out, d.Slug)
	}
	return out
}

// Role is the PR author's admission tier as the lane computed it, rendered
// as `role/<tier>`.
type Role int

const (
	RoleNone Role = iota
	// RoleTrusted: the hive would auto-merge this author's PR (trusted-author
	// role, registry/claim tier in require_tiers, or the App/trusted bot).
	RoleTrusted
	// RoleContributor: registered with the hive at the contributor tier,
	// below require_tiers; needs a human lgtm/approve.
	RoleContributor
	// RoleNewcomer: registered at the newcomer tier (self-registration).
	RoleNewcomer
	// RoleUntrusted: no registry profile or verified claim, or revoked.
	RoleUntrusted
)

var roleDefs = map[Role]LabelDef{
	RoleTrusted:     {Name: RolePrefix + "trusted", Color: RoleTrustedColor, Description: "Author is trusted for unattended merge (trusted-author role or registry/claim tier in require_tiers)"},
	RoleContributor: {Name: RolePrefix + "contributor", Color: RoleColor, Description: "Author holds the contributor tier in this hive's registry; a human must lgtm/approve"},
	RoleNewcomer:    {Name: RolePrefix + "newcomer", Color: RoleColor, Description: "Author holds the newcomer tier (self-registered); a human must lgtm/approve"},
	RoleUntrusted:   {Name: RolePrefix + "untrusted", Color: RoleColor, Description: "Author has no registry profile or verified hive claim (or is revoked); never auto-merged"},
}

// Label renders the role label, or "" for RoleNone.
func (r Role) Label() string { return roleDefs[r].Name }

// LegacyLabel is the pre-#12581 `author/<tier>` spelling of the role label,
// or "" for RoleNone. Readers accept it during the migration; Plan removes it.
func (r Role) LegacyLabel() string {
	if r == RoleNone {
		return ""
	}
	return AuthorPrefix + r.String()
}

// String is the bare tier name ("trusted", …) or "" for RoleNone.
func (r Role) String() string { return strings.TrimPrefix(roleDefs[r].Name, RolePrefix) }

// Merge combines two lanes' opinions of the same author into the tier to
// show. The lanes see different evidence — the trusted-author lane the
// maintainer role, the contributor lane the registry/claim tier — and a
// maintainer with no registry profile is "untrusted" to the latter, so:
// trusted always wins; any known tier replaces none or untrusted; between
// two equally specific tiers the first recorded is kept.
func (r Role) Merge(next Role) Role {
	switch {
	case next == RoleNone:
		return r
	case r == RoleNone, next == RoleTrusted, r == RoleUntrusted:
		return next
	}
	return r
}

// RoleFromTier maps a registry/claim tier string to a Role given the
// lane's require_tiers set. Unknown or empty tiers are untrusted.
func RoleFromTier(tier string, requireTiers map[string]bool) Role {
	tier = strings.ToLower(strings.TrimSpace(tier))
	switch {
	case tier != "" && requireTiers[tier]:
		return RoleTrusted
	case tier == "contributor":
		return RoleContributor
	case tier == "newcomer":
		return RoleNewcomer
	default:
		return RoleUntrusted
	}
}

// LegacyRoleLabels are the pre-#12581 `author/<tier>` spellings of the role
// family. Plan removes them from any item it writes (the `role/<tier>` label
// goes on in the same write), so the rename completes one reconcile cycle
// after upgrade; RoleOf still reads them so nothing is forgotten in between.
func LegacyRoleLabels() []string {
	out := make([]string, 0, len(roleDefs))
	for _, r := range sortedRoles() {
		out = append(out, r.LegacyLabel())
	}
	return out
}

// IsLegacyRole reports whether label is a retired `author/<tier>` spelling.
func IsLegacyRole(label string) bool {
	label = strings.TrimSpace(label)
	for r := range roleDefs {
		if strings.EqualFold(label, r.LegacyLabel()) {
			return true
		}
	}
	return false
}

func sortedRoles() []Role {
	roles := make([]Role, 0, len(roleDefs))
	for r := range roleDefs {
		roles = append(roles, r)
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i] < roles[j] })
	return roles
}

// Author-kind labels (`author/<kind>`) are decided by pkg/authorkind; this
// package only renders and reconciles them. Defs lists the three for
// provisioning under the configured names.
func authorKindDefs(names authorkind.Names) []LabelDef {
	n := names.Normalize()
	return []LabelDef{
		{Name: n.Human, Color: AuthorHumanColor, Description: "A person pushed this PR themselves"},
		{Name: n.Clanker, Color: AuthorClankerColor, Description: "A contributor's own hive authored this PR under their login"},
		{Name: n.Hive, Color: AuthorHiveColor, Description: "This repository's own hive (the App or one of its agent lanes) authored this PR"},
	}
}

// MaxLabelDescription is GitHub's hard cap on a label description; a longer
// one makes label create/edit fail with 422, which aborted the whole stage
// reconcile (and with it adoption) on every repo after the 085/090 rewording.
const MaxLabelDescription = 100

// ClampLabelDescription trims d to MaxLabelDescription runes.
func ClampLabelDescription(d string) string {
	r := []rune(d)
	if len(r) <= MaxLabelDescription {
		return d
	}
	return strings.TrimSpace(string(r[:MaxLabelDescription]))
}

// LabelDef is what the ensure-label step needs to create or recolour a label.
type LabelDef struct {
	Name, Color, Description string
}

// AllLabelDefs lists every label the families may emit, in a stable order,
// so a repo can be provisioned (and recoloured) in one pass.
func AllLabelDefs() []LabelDef { return LabelDefsFor(authorkind.DefaultNames) }

// LabelDefsFor is AllLabelDefs with the author/<kind> names an operator
// configured (governor.labels.author).
func LabelDefsFor(kinds authorkind.Names) []LabelDef {
	out := make([]LabelDef, 0, len(Table)+len(healthDefs)+len(roleDefs)+3)
	for _, d := range Table {
		out = append(out, LabelDef{Name: d.Stage.Label(), Color: StageColor, Description: d.Description})
	}
	out = append(out, healthDefs...)
	for _, r := range sortedRoles() {
		out = append(out, roleDefs[r])
	}
	out = append(out, authorKindDefs(kinds)...)
	out = append(out, dcoDefs[DCOYes], dcoDefs[DCONo])
	return out
}

// DCO labels (hivecommons/hive#12542). The operator wanted "the label and
// the green colour" instead of Prow's `dco-signoff: yes` pill, and red for
// the failing case with as little text as possible. GitHub cannot colour one
// label name two ways, so the pair is `dco` (green) / `no-dco` (red), mutually
// exclusive, written only from the commit check — never copied from Prow.
const (
	DCOLabel   = "dco"
	NoDCOLabel = "no-dco"
)

// DCOColor is the same green as `role/trusted` (the operator wants the two
// pills to read as one family); NoDCOColor is the engine's one red.
const (
	DCOColor   = RoleTrustedColor
	NoDCOColor = StalledColor
)

var dcoDefs = map[DCOState]LabelDef{
	DCOYes: {Name: DCOLabel, Color: DCOColor, Description: "All commits are DCO signed-off"},
	DCONo:  {Name: NoDCOLabel, Color: NoDCOColor, Description: "A commit is missing Signed-off-by — run `git rebase --signoff`"},
}

// LegacyDCOLabels are the Prow dco plugin's labels. They are retired on
// hive-managed repos: once the hive has read a head's commits it removes
// them so a PR shows exactly one DCO pill. Readers still ACCEPT them during
// the migration (DCOFromLabels).
var LegacyDCOLabels = []string{LegacyDCOYes, LegacyDCONo}

const (
	LegacyDCOYes = "dco-signoff: yes"
	LegacyDCONo  = "dco-signoff: no"
)

// Label renders the DCO label, or "" for DCOUnknown.
func (d DCOState) Label() string { return dcoDefs[d].Name }

// String is "yes", "no" or "unknown" (the merge-eligible export's spelling).
func (d DCOState) String() string {
	switch d {
	case DCOYes:
		return "yes"
	case DCONo:
		return "no"
	}
	return "unknown"
}

// IsLegacyDCO reports whether label is one of Prow's dco-signoff labels.
func IsLegacyDCO(label string) bool {
	for _, l := range LegacyDCOLabels {
		if strings.EqualFold(strings.TrimSpace(label), l) {
			return true
		}
	}
	return false
}

// dcoOwned reports whether label belongs to the DCO family the hive
// reconciles (its own pair plus the retired Prow pair).
func dcoOwned(label string) bool {
	return label == DCOLabel || label == NoDCOLabel || IsLegacyDCO(label)
}

// DCOFromLabels reads the DCO verdict a label set carries. The hive's own
// labels win; Prow's legacy pair is honoured only when neither is present,
// so a reader keeps working through the migration cycle. `no-dco` beats
// `dco` should an item transiently carry both.
func DCOFromLabels(labels []string) DCOState {
	own, legacy := DCOUnknown, DCOUnknown
	for _, l := range labels {
		switch strings.TrimSpace(l) {
		case NoDCOLabel:
			own = DCONo
		case DCOLabel:
			if own == DCOUnknown {
				own = DCOYes
			}
		case LegacyDCONo:
			legacy = DCONo
		case LegacyDCOYes:
			if legacy == DCOUnknown {
				legacy = DCOYes
			}
		}
	}
	if own != DCOUnknown {
		return own
	}
	return legacy
}

// Desired is the full hive-owned label set wanted on one item.
type Desired struct {
	Stage Stage
	Role  Role
	// Kind is the authorship kind to show; authorkind.Unknown means "no
	// opinion" and leaves an existing author/<kind> label in place.
	Kind authorkind.Kind
	// KindLabels are the configured author/<kind> names; zero = defaults.
	KindLabels authorkind.Names
	Stalled    bool
	// DCO is the commit check's verdict for the head; DCOUnknown leaves the
	// item's existing DCO labels (own or legacy) untouched.
	DCO DCOState
}

// Plan returns the labels to add and remove so current carries exactly the
// desired hive-owned set. It is idempotent: an unchanged state yields two
// empty slices, so a caller that only writes non-empty plans never churns.
// Any `stage/*`, `role/*` or `author/*` label not wanted — including a
// label from a previous table and the retired `author/<tier>` role
// spellings — is removed. With Kind unset, `author/<kind>` labels are kept
// as found. The `dco` / `no-dco` pair (and Prow's retired `dco-signoff: *`)
// is reconciled only when want.DCO is known. Non-hive labels are never
// touched.
func Plan(current []string, want Desired) (add, remove []string) {
	wanted := map[string]bool{}
	if l := want.Stage.Label(); l != "" {
		wanted[l] = true
	}
	if l := want.Role.Label(); l != "" {
		wanted[l] = true
	}
	if l := want.KindLabels.Label(want.Kind); l != "" {
		wanted[l] = true
	}
	if want.Stalled {
		wanted[StalledLabel] = true
	}
	if l := want.DCO.Label(); l != "" {
		wanted[l] = true
	}
	have := map[string]bool{}
	for _, l := range current {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		have[l] = true
		if wanted[l] {
			continue
		}
		// The DCO family (own pair + Prow's legacy pair) is reconciled only
		// once the commit check has spoken; an unread head keeps its pills.
		if dcoOwned(l) {
			if want.DCO != DCOUnknown {
				remove = append(remove, l)
			}
			continue
		}
		if !Owned(l) {
			continue
		}
		if want.Kind == authorkind.Unknown && strings.HasPrefix(l, AuthorPrefix) && !IsLegacyRole(l) {
			continue
		}
		remove = append(remove, l)
	}
	for l := range wanted {
		if !have[l] {
			add = append(add, l)
		}
	}
	sort.Strings(add)
	sort.Strings(remove)
	return add, remove
}

// LegacyLabels are retired hive labels whose meaning the stage family now
// carries: `🐝 covered by PR` (and its pre-#11888 spelling) → 40-covered-by-pr,
// `claimed` → 20-claimed, `hive/in-progress` → 20-claimed / 30-pr-in-progress.
// The reconciler removes them from any open item it writes a stage for, so
// the migration completes one cycle after upgrade. `hive/claimed-by-<who>`
// is NOT retired: it is the "who" modifier the stage cannot carry.
var LegacyLabels = []string{LegacyCoveredByPR, LegacyCoveredByPRASCII, claims.LegacyLabelClaimed, claims.LegacyLabelInProgress}

// Retired label spellings the stage family replaced (kept as constants so
// readers that still have to RECOGNISE them for the migration cycle do not
// re-type the strings). The claim-side pair lives in pkg/claims.
const (
	LegacyCoveredByPR      = "🐝 covered by PR"
	LegacyCoveredByPRASCII = "hive/covered-by-pr"
)

// IsLegacy reports whether label is one of the retired labels (case-insensitive).
func IsLegacy(label string) bool {
	for _, legacy := range LegacyLabels {
		if strings.EqualFold(label, legacy) {
			return true
		}
	}
	return false
}

// CarriesStageAtLeast reports whether labels carry a current stage label at
// or past min. It is the migration-safe replacement for "has the
// covered-by-PR label": a PR-covered issue carries 040 (or, from an older
// build, a PR stage past it that Enforce clamps back on its next pass).
func CarriesStageAtLeast(labels []string, min Stage) bool {
	cur := Current(labels)
	return cur != None && !cur.Before(min)
}

// Owned reports whether label belongs to a hive-owned family this package
// reconciles (including the retired labels it replaces).
func Owned(label string) bool {
	return strings.HasPrefix(label, Prefix) || strings.HasPrefix(label, RolePrefix) || strings.HasPrefix(label, AuthorPrefix) || IsLegacy(label)
}

// Renames maps retired stage label spellings to their current name. The
// reconciler renames the GitHub label in place (one PATCH per repo) so every
// item carrying the old name keeps its stage through the upgrade, and
// Current reads the old spelling as the new stage until then.
var Renames = map[string]Stage{
	"stage/090-needs-human": ReadyForHumanMerge,
	// #12713 briefly renamed 072 to `stage/072-needs-author`; that spelling
	// was withdrawn when 071 took the "needs author" meaning for human PRs.
	"stage/072-needs-author": ReturnedToContributor,
}

// Current returns the stage a label set carries, or None. An unknown
// `stage/*` label (stale table entry, health marker) yields None. Should an
// item transiently carry two stage labels (mid-migration, before the
// reconciler's next pass), the furthest along wins.
func Current(labels []string) Stage {
	cur := None
	for _, l := range labels {
		if s, ok := Renames[l]; ok && cur.Before(s) {
			cur = s
		}
		for _, d := range Table {
			if l == d.Stage.Label() && cur.Before(d.Stage) {
				cur = d.Stage
			}
		}
	}
	return cur
}
