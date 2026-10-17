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
	// and the covering PR's own progress is narrated in the issue's stage
	// reason/comment, never mirrored as a second label (hivecommons/hive#12451).
	// Enforce clamps a stage to the item's kind (Stage.For) so a PR-only
	// stage can never land on an issue and vice versa.
	Issue, PR bool
}

// Item is the kind of forge item a stage is written to.
type Item int

const (
	// IssueItem: an issue. Only Issue:true stages (010–040, and 085 for an
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
// s. An applicable stage is returned unchanged. A PR-only stage asked of an
// issue clamps to 040-covered-by-pr (the PR exists, that is all the issue
// says — never to 085, which an issue wears only when parked on a person).
// An issue-only stage asked of a PR has no PR meaning and yields None.
// Unknown stages yield None.
func (s Stage) For(k Item) Stage {
	if s.Applies(k) {
		return s
	}
	if k == PRItem || !s.Known() {
		return None
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
	{Stage: ReviewPending, Number: 50, Slug: "review-pending", Description: "Review swarm requested; awaiting a verdict for the current head", Dwell: time.Hour, PR: true},
	{Stage: Reviewed, Number: 60, Slug: "reviewed", Description: "Review swarm verdict approve; a gate is still pending (CI running, soak not started)", Dwell: 30 * time.Minute, PR: true},
	{Stage: Fixing, Number: 70, Slug: "fixing", Description: "Hive-authored PR: CI red, changes requested or needs rebase; a hive agent is fixing it", Dwell: 2 * time.Hour, PR: true},
	{Stage: ReturnedToContributor, Number: 72, Slug: "returned-to-contributor", Description: "Contributor PR: CI red, changes requested or conflicting; diagnosis posted, author must act", Dwell: 24 * time.Hour, PR: true},
	{Stage: AdoptedByHive, Number: 74, Slug: "adopted-by-hive", Description: "Contributor PR adopted: author unavailable after red CI; the hive is finishing it (takeover path)", Dwell: 3 * time.Hour, PR: true},
	{Stage: AwaitingMergeAdmission, Number: 80, Slug: "awaiting-merge-admission", Description: "CI green, verdict approve, no modifiers; the automerge lane has not admitted it yet", Dwell: 30 * time.Minute, StallNote: true, PR: true},
	{Stage: NeedsDecision, Number: 85, Slug: "needs-decision", Description: "A human decision is owed: sentinel alert, hold/needs-decision label, human-only rule or parked issue", Issue: true, PR: true},
	{Stage: ReadyForHumanMerge, Number: 90, Slug: "ready-for-human-merge", Description: "Hive would merge this but trust/role blocks it; a maintainer's /lgtm /approve is all that is missing", PR: true},
	{Stage: MergeQueue, Number: MergeQueueNumber, Slug: "merge-queue", Description: "Lane admitted: soaking, merge-eligible or waiting for tide / the merge window", Dwell: 3 * time.Hour, PR: true},
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

// StageBySlug resolves a slug (as used in config dwell overrides) to a stage.
func StageBySlug(slug string) (Stage, bool) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	for _, d := range Table {
		if d.Slug == slug {
			return d.Stage, true
		}
	}
	return None, false
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
	return out
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
}

// Plan returns the labels to add and remove so current carries exactly the
// desired hive-owned set. It is idempotent: an unchanged state yields two
// empty slices, so a caller that only writes non-empty plans never churns.
// Any `stage/*`, `role/*` or `author/*` label not wanted — including a
// label from a previous table and the retired `author/<tier>` role
// spellings — is removed. With Kind unset, `author/<kind>` labels are kept
// as found. Non-hive labels are never touched.
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
	have := map[string]bool{}
	for _, l := range current {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		have[l] = true
		if !Owned(l) || wanted[l] {
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
