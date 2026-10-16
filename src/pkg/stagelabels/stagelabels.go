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
// Two orthogonal, hive-owned families ride alongside the stage:
//   - `author/<tier>`: the PR author's admission tier as the contributor lane
//     actually computed it, so a reader can tell whether the hive would
//     auto-merge the PR. At most one per PR.
//   - `stage/stalled` / `stage/regressed`: health markers. stalled means the
//     item sat in its stage longer than the stage's expected dwell;
//     regressed means the stage number went down (new head went red, PR
//     closed unmerged). At most one per item; both clear automatically.
//
// `hold`, `needs-human`, `needs-decision`, `needs-rebase`, `blocked` and
// `author/*` are stage MODIFIERS, not stages: they stay whatever the stage
// is. A closed, merged or superseded item is terminal and carries no stage.
package stagelabels

import (
	"fmt"
	"github.com/hivecommons/hive/pkg/claims"
	"sort"
	"strings"
	"time"

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
	NeedsHuman
	MergeQueue
)

// Prefix is the stage label family prefix. Every label under it is
// hive-owned: the reconciler removes any it does not currently emit.
const Prefix = "stage/"

// AuthorPrefix is the author-tier family prefix (same ownership rule).
const AuthorPrefix = "author/"

// Health labels. They coexist with the stage label; at most one applies.
const (
	StalledLabel   = Prefix + "stalled"
	RegressedLabel = Prefix + "regressed"
)

// Family colours. Every stage/NN label shares StageColor (the hive-amber the
// retired `🐝 covered by PR` label used) so the eye tracks the family across
// lists; health markers are red; author/* is neutral grey except the one
// tier that means "the hive will merge this".
const (
	StageColor         = "f5a623"
	StalledColor       = "d73a4a"
	RegressedColor     = "b60205"
	AuthorColor        = "6e7781"
	AuthorTrustedColor = "0e8a16"
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
	// Dwell is how long an item is expected to sit in this stage before it
	// is marked stalled. 0 means never (the stage is open-ended).
	Dwell time.Duration
	// StallNote asks the reconciler to post a one-line comment (with the
	// lane's last skip reason) when the item stalls here: a stall in such a
	// stage points at the hive, not the author, so it must be legible.
	StallNote bool
	// Issue/PR report which item kinds the stage is emitted for directly.
	// An issue covered by a PR mirrors the PR's stage, so PR stages also
	// appear on issues.
	Issue, PR bool
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
	{Stage: AdoptedByHive, Number: 74, Slug: "adopted-by-hive", Description: "Contributor PR adopted: author unavailable after red CI; the hive is finishing it via the takeover path", Dwell: 3 * time.Hour, PR: true},
	{Stage: AwaitingMergeAdmission, Number: 80, Slug: "awaiting-merge-admission", Description: "CI green, verdict approve, no modifiers; the automerge lane has not admitted it yet", Dwell: 30 * time.Minute, StallNote: true, PR: true},
	{Stage: NeedsHuman, Number: 90, Slug: "needs-human", Description: "Cannot auto-merge; a person must act (see author/* and hold labels)", PR: true},
	{Stage: MergeQueue, Number: MergeQueueNumber, Slug: "merge-queue", Description: "Lane admitted: soaking, merge-eligible or waiting for tide / the merge window", Dwell: 3 * time.Hour, PR: true},
}

// MergeQueueNumber is fixed: the merge queue is always the top stage. New
// stages are inserted between existing numbers, never above 100.
const MergeQueueNumber = 100

// NumberWidth is the zero-padded width of the number in a label.
const NumberWidth = 3

var healthDefs = []LabelDef{
	{Name: StalledLabel, Color: StalledColor, Description: "In its current stage longer than expected"},
	{Name: RegressedLabel, Color: RegressedColor, Description: "Stage moved backwards (see the hive's stage comment)"},
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

// Author is the PR author's admission tier as the lane computed it.
type Author int

const (
	AuthorNone Author = iota
	// AuthorTrusted: the hive would auto-merge this author's PR (trusted-author
	// role, registry/claim tier in require_tiers, or the App/trusted bot).
	AuthorTrusted
	// AuthorContributor: registered with the hive at the contributor tier,
	// below require_tiers; needs a human lgtm/approve.
	AuthorContributor
	// AuthorNewcomer: registered at the newcomer tier (self-registration).
	AuthorNewcomer
	// AuthorUntrusted: no registry profile or verified claim, or revoked.
	AuthorUntrusted
)

var authorDefs = map[Author]LabelDef{
	AuthorTrusted:     {Name: AuthorPrefix + "trusted", Color: AuthorTrustedColor, Description: "Author is trusted for unattended merge (trusted-author role or registry/claim tier in require_tiers)"},
	AuthorContributor: {Name: AuthorPrefix + "contributor", Color: AuthorColor, Description: "Author holds the contributor tier in this hive's registry; a human must lgtm/approve"},
	AuthorNewcomer:    {Name: AuthorPrefix + "newcomer", Color: AuthorColor, Description: "Author holds the newcomer tier (self-registered); a human must lgtm/approve"},
	AuthorUntrusted:   {Name: AuthorPrefix + "untrusted", Color: AuthorColor, Description: "Author has no registry profile or verified hive claim (or is revoked); never auto-merged"},
}

// Label renders the author-tier label, or "" for AuthorNone.
func (a Author) Label() string { return authorDefs[a].Name }

// String is the bare tier name ("trusted", …) or "" for AuthorNone.
func (a Author) String() string { return strings.TrimPrefix(authorDefs[a].Name, AuthorPrefix) }

// Merge combines two lanes' opinions of the same author into the tier to
// show. The lanes see different evidence — the trusted-author lane the
// maintainer role, the contributor lane the registry/claim tier — and a
// maintainer with no registry profile is "untrusted" to the latter, so:
// trusted always wins; any known tier replaces none or untrusted; between
// two equally specific tiers the first recorded is kept.
func (a Author) Merge(next Author) Author {
	switch {
	case next == AuthorNone:
		return a
	case a == AuthorNone, next == AuthorTrusted, a == AuthorUntrusted:
		return next
	}
	return a
}

// AuthorFromTier maps a registry/claim tier string to an Author given the
// lane's require_tiers set. Unknown or empty tiers are untrusted.
func AuthorFromTier(tier string, requireTiers map[string]bool) Author {
	tier = strings.ToLower(strings.TrimSpace(tier))
	switch {
	case tier != "" && requireTiers[tier]:
		return AuthorTrusted
	case tier == "contributor":
		return AuthorContributor
	case tier == "newcomer":
		return AuthorNewcomer
	default:
		return AuthorUntrusted
	}
}

// LabelDef is what the ensure-label step needs to create or recolour a label.
type LabelDef struct {
	Name, Color, Description string
}

// AllLabelDefs lists every label the families may emit, in a stable order,
// so a repo can be provisioned (and recoloured) in one pass.
func AllLabelDefs() []LabelDef {
	out := make([]LabelDef, 0, len(Table)+len(healthDefs)+len(authorDefs))
	for _, d := range Table {
		out = append(out, LabelDef{Name: d.Stage.Label(), Color: StageColor, Description: d.Description})
	}
	out = append(out, healthDefs...)
	authors := make([]Author, 0, len(authorDefs))
	for a := range authorDefs {
		authors = append(authors, a)
	}
	sort.Slice(authors, func(i, j int) bool { return authors[i] < authors[j] })
	for _, a := range authors {
		out = append(out, authorDefs[a])
	}
	return out
}

// Desired is the full hive-owned label set wanted on one item.
type Desired struct {
	Stage     Stage
	Author    Author
	Stalled   bool
	Regressed bool
}

// Plan returns the labels to add and remove so current carries exactly the
// desired hive-owned set. It is idempotent: an unchanged state yields two
// empty slices, so a caller that only writes non-empty plans never churns.
// Any `stage/*` or `author/*` label not wanted — including a label from a
// previous table — is removed. Non-hive labels are never touched.
func Plan(current []string, want Desired) (add, remove []string) {
	wanted := map[string]bool{}
	if l := want.Stage.Label(); l != "" {
		wanted[l] = true
	}
	if l := want.Author.Label(); l != "" {
		wanted[l] = true
	}
	// Both health flags true is a caller bug; regressed is the louder signal.
	switch {
	case want.Regressed:
		wanted[RegressedLabel] = true
	case want.Stalled:
		wanted[StalledLabel] = true
	}
	have := map[string]bool{}
	for _, l := range current {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		have[l] = true
		if Owned(l) && !wanted[l] {
			remove = append(remove, l)
		}
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
// covered-by-PR label": a PR-covered issue carries 040 or a mirrored PR stage.
func CarriesStageAtLeast(labels []string, min Stage) bool {
	cur := Current(labels)
	return cur != None && !cur.Before(min)
}

// Owned reports whether label belongs to a hive-owned family this package
// reconciles (including the retired labels it replaces).
func Owned(label string) bool {
	return strings.HasPrefix(label, Prefix) || strings.HasPrefix(label, AuthorPrefix) || IsLegacy(label)
}

// Current returns the stage a label set carries, or None. An unknown
// `stage/*` label (stale table entry, health marker) yields None. Should an
// item transiently carry two stage labels (mid-migration, before the
// reconciler's next pass), the furthest along wins.
func Current(labels []string) Stage {
	cur := None
	for _, l := range labels {
		for _, d := range Table {
			if l == d.Stage.Label() && cur.Before(d.Stage) {
				cur = d.Stage
			}
		}
	}
	return cur
}
