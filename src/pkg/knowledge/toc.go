package knowledge

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Table-of-contents caps keep a TOC small enough to sit in an agent prompt.
const (
	DefaultTOCLimit = 50
	MaxTOCLimit     = 200
)

// DefaultTOCPromptChars is the default character budget for FormatTOCForPrompt.
const DefaultTOCPromptChars = 4000

const (
	maxTOCTitleRunes  = 100
	maxTOCTags        = 6
	maxTOCTagRunes    = 40
	maxTOCOriginRunes = 160
	repoTagPrefix     = "repo:"
)

// tocFooterReserve leaves room for the "more entries" footer.
const tocFooterReserve = 40

// TOCEntry is one compact row of the knowledge table of contents. It carries
// enough metadata to choose an entry and cite it, never the body.
type TOCEntry struct {
	ID           string         `json:"id"`
	Title        string         `json:"title"`
	Type         string         `json:"type"`
	Layer        LayerType      `json:"layer"`
	Repo         string         `json:"repo,omitempty"`
	Tags         []string       `json:"tags,omitempty"`
	Status       LifecycleState `json:"status"`
	Confidence   float64        `json:"confidence,omitempty"`
	Updated      *time.Time     `json:"updated,omitempty"`
	SizeBytes    int            `json:"size_bytes,omitempty"`
	Source       string         `json:"source,omitempty"`
	SupersededBy string         `json:"superseded_by,omitempty"`
}

// TOC is a capped table of contents. Total counts every in-scope entry before
// the cap so a caller can tell it was truncated.
type TOC struct {
	Entries   []TOCEntry `json:"entries"`
	Total     int        `json:"total"`
	Returned  int        `json:"returned"`
	Truncated bool       `json:"truncated"`
}

// TOCScope restricts which entries are visible. Empty fields are unrestricted.
// An entry with no repo:<name> tag is org-wide and passes any Repos filter.
type TOCScope struct {
	Layers []LayerType
	Repos  []string
	Types  []string
	Tags   []string
	// IncludeStates admits non-approved lifecycle states; approved entries are
	// always visible.
	IncludeStates []LifecycleState
}

// AgentScope builds the scope an agent is bound to by knowledge.agent_scopes
// (#11201). Empty fields stay unrestricted. An empty includeStates admits every
// state, so the request's or primer's own lifecycle filter decides; a listed
// state is admitted only when that filter admits it too. Unknown state names
// are dropped, and a list with none left admits approved knowledge only.
func AgentScope(layers, repos, types, tags, includeStates []string) TOCScope {
	sc := TOCScope{Repos: repos, Types: types, Tags: tags}
	for _, l := range layers {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			sc.Layers = append(sc.Layers, LayerType(l))
		}
	}
	if len(includeStates) == 0 {
		sc.IncludeStates = append([]LifecycleState(nil), AllLifecycleStates...)
		return sc
	}
	for _, st := range includeStates {
		if strings.EqualFold(strings.TrimSpace(st), "all") {
			sc.IncludeStates = append([]LifecycleState(nil), AllLifecycleStates...)
			return sc
		}
		if parsed, err := ParseLifecycleState(st); err == nil {
			sc.IncludeStates = append(sc.IncludeStates, parsed)
		}
	}
	return sc
}

// Filter returns the facts that pass the scope. Applying an agent scope with
// Filter before a request scope intersects the two: an entry is kept only when
// both admit it, so the request can never widen the agent's scope.
func (sc TOCScope) Filter(facts []Fact) []Fact {
	out := make([]Fact, 0, len(facts))
	for _, f := range facts {
		if sc.InScope(f) {
			out = append(out, f)
		}
	}
	return out
}

// FactRepo returns the repository a fact is scoped to via a repo:<name> tag.
func FactRepo(f Fact) string {
	for _, t := range f.Tags {
		t = strings.TrimSpace(t)
		if len(t) > len(repoTagPrefix) && strings.EqualFold(t[:len(repoTagPrefix)], repoTagPrefix) {
			return t[len(repoTagPrefix):]
		}
	}
	return ""
}

// InScope reports whether f passes the scope, including the lifecycle filter.
func (sc TOCScope) InScope(f Fact) bool {
	return sc.ExclusionReason(f) == ""
}

// Exclusion reason codes reported by ExclusionReason, in check order.
const (
	ExcludedLifecycle = "lifecycle"
	ExcludedLayer     = "layer"
	ExcludedRepo      = "repo"
	ExcludedType      = "type"
	ExcludedTag       = "tag"
)

// exclusionOrder ranks reason codes so the first failing check wins when
// several scopes are combined.
var exclusionOrder = []string{ExcludedLifecycle, ExcludedLayer, ExcludedRepo, ExcludedType, ExcludedTag}

// ExclusionReason is the explain variant of InScope: it returns "" when f
// passes the scope, otherwise the reason code of the first failing check in
// the order lifecycle, layer, repo, type, tag.
func (sc TOCScope) ExclusionReason(f Fact) string {
	if !lifecycleAllowed(f.EffectiveState(), sc.IncludeStates) {
		return ExcludedLifecycle
	}
	if len(sc.Layers) > 0 && !containsLayer(sc.Layers, f.Layer) {
		return ExcludedLayer
	}
	if len(sc.Repos) > 0 {
		if repo := FactRepo(f); repo != "" && !containsFold(sc.Repos, repo) {
			return ExcludedRepo
		}
	}
	if len(sc.Types) > 0 && !containsFold(sc.Types, string(f.Type)) {
		return ExcludedType
	}
	if len(sc.Tags) > 0 {
		for _, t := range f.Tags {
			if containsFold(sc.Tags, t) {
				return ""
			}
		}
		return ExcludedTag
	}
	return ""
}

// StatesAdmitted lists the lifecycle states every scope admits, in
// AllLifecycleStates order. Approved is always admitted.
func StatesAdmitted(scopes ...TOCScope) []LifecycleState {
	var out []LifecycleState
	for _, st := range AllLifecycleStates {
		ok := true
		for _, sc := range scopes {
			if !lifecycleAllowed(st, sc.IncludeStates) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, st)
		}
	}
	return out
}

// ExcludedTOCEntry is a TOC entry filtered out of an effective view, with the
// reason code of the first check it failed.
type ExcludedTOCEntry struct {
	TOCEntry
	Reason string `json:"reason"`
}

// TOCExplanation is the effective view of a set of scopes (#11202): the TOC
// entries they admit and the entries they filter out, each list capped.
type TOCExplanation struct {
	Included         []TOCEntry         `json:"included"`
	Excluded         []ExcludedTOCEntry `json:"excluded"`
	IncludedTotal    int                `json:"included_total"`
	ExcludedTotal    int                `json:"excluded_total"`
	IncludedReturned int                `json:"included_returned"`
	ExcludedReturned int                `json:"excluded_returned"`
	Truncated        bool               `json:"truncated"`
}

// ExplainTOC reports which facts the intersection of scopes admits (built
// exactly like BuildTOC) and which it excludes and why. When scopes disagree
// the earliest reason in check order wins. Each list holds at most
// ClampTOCLimit(limit) entries; Truncated is set when either was capped.
// A fact whose slug is admitted from another layer is neither: it is shadowed
// by precedence, not filtered by the scope.
func ExplainTOC(facts []Fact, limit int, scopes ...TOCScope) TOCExplanation {
	limit = ClampTOCLimit(limit)
	var admitted []Fact
	type rejected struct {
		f      Fact
		reason string
	}
	var out []rejected
	for _, f := range facts {
		if f.Slug == "" {
			continue
		}
		if reason := combinedExclusion(f, scopes); reason != "" {
			out = append(out, rejected{f, reason})
			continue
		}
		admitted = append(admitted, f)
	}
	toc := BuildTOC(admitted, TOCScope{IncludeStates: AllLifecycleStates}, limit)
	ex := TOCExplanation{
		Included:         toc.Entries,
		Excluded:         []ExcludedTOCEntry{},
		IncludedTotal:    toc.Total,
		IncludedReturned: toc.Returned,
		Truncated:        toc.Truncated,
	}

	included := make(map[string]bool, len(admitted))
	for _, f := range admitted {
		included[f.Slug] = true
	}
	kept := out[:0]
	for _, r := range out {
		if !included[r.f.Slug] {
			kept = append(kept, r)
		}
	}
	sort.Slice(kept, func(i, j int) bool {
		a, b := kept[i].f, kept[j].f
		if pa, pb := a.Layer.Precedence(), b.Layer.Precedence(); pa != pb {
			return pa < pb
		}
		return a.Slug < b.Slug
	})
	ex.ExcludedTotal = len(kept)
	for _, r := range kept {
		if len(ex.Excluded) >= limit {
			ex.Truncated = true
			break
		}
		ex.Excluded = append(ex.Excluded, ExcludedTOCEntry{TOCEntry: tocEntry(r.f), Reason: r.reason})
	}
	ex.ExcludedReturned = len(ex.Excluded)
	return ex
}

func combinedExclusion(f Fact, scopes []TOCScope) string {
	best := -1
	for _, sc := range scopes {
		reason := sc.ExclusionReason(f)
		if reason == "" {
			continue
		}
		for i, r := range exclusionOrder {
			if r == reason && (best < 0 || i < best) {
				best = i
			}
		}
	}
	if best < 0 {
		return ""
	}
	return exclusionOrder[best]
}

func containsLayer(layers []LayerType, l LayerType) bool {
	for _, x := range layers {
		if x == l {
			return true
		}
	}
	return false
}

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), strings.TrimSpace(v)) {
			return true
		}
	}
	return false
}

// ClampTOCLimit applies the default and maximum entry caps.
func ClampTOCLimit(limit int) int {
	if limit <= 0 {
		return DefaultTOCLimit
	}
	if limit > MaxTOCLimit {
		return MaxTOCLimit
	}
	return limit
}

// BuildTOC filters facts by scope and returns at most ClampTOCLimit(limit)
// compact entries, ordered by layer precedence, then most recently updated,
// then slug so output is deterministic. Duplicate slugs keep the entry from
// the higher-precedence layer.
func BuildTOC(facts []Fact, scope TOCScope, limit int) TOC {
	limit = ClampTOCLimit(limit)
	best := make(map[string]Fact, len(facts))
	for _, f := range facts {
		if f.Slug == "" || !scope.InScope(f) {
			continue
		}
		if prev, ok := best[f.Slug]; ok && prev.Layer.Precedence() <= f.Layer.Precedence() {
			continue
		}
		best[f.Slug] = f
	}
	in := make([]Fact, 0, len(best))
	for _, f := range best {
		in = append(in, f)
	}
	sort.Slice(in, func(i, j int) bool {
		a, b := in[i], in[j]
		if pa, pb := a.Layer.Precedence(), b.Layer.Precedence(); pa != pb {
			return pa < pb
		}
		if !a.Updated.Equal(b.Updated) {
			return a.Updated.After(b.Updated)
		}
		return a.Slug < b.Slug
	})

	toc := TOC{Entries: []TOCEntry{}, Total: len(in)}
	for _, f := range in {
		if len(toc.Entries) >= limit {
			toc.Truncated = true
			break
		}
		toc.Entries = append(toc.Entries, tocEntry(f))
	}
	toc.Returned = len(toc.Entries)
	return toc
}

func tocEntry(f Fact) TOCEntry {
	typ := string(f.Type)
	if typ == "" {
		typ = "general"
	}
	e := TOCEntry{
		ID:           f.Slug,
		Title:        truncateRunes(f.Title, maxTOCTitleRunes),
		Type:         typ,
		Layer:        f.Layer,
		Repo:         FactRepo(f),
		Status:       f.EffectiveState(),
		SizeBytes:    f.BodySize,
		Source:       truncateRunes(f.Origin, maxTOCOriginRunes),
		SupersededBy: f.SupersededBy,
	}
	if f.ConfidenceScored {
		e.Confidence = f.Confidence
	}
	if !f.Updated.IsZero() {
		u := f.Updated.UTC()
		e.Updated = &u
	}
	for _, t := range f.Tags {
		if len(e.Tags) >= maxTOCTags {
			break
		}
		if t = strings.TrimSpace(t); t != "" {
			e.Tags = append(e.Tags, truncateRunes(t, maxTOCTagRunes))
		}
	}
	return e
}

func truncateRunes(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max-1]) + "…"
}

// FormatTOCForPrompt renders the TOC as a markdown list for an agent kick,
// stopping before maxChars is exceeded (DefaultTOCPromptChars when <= 0) and
// saying how many entries were left out so the agent can narrow its request.
func FormatTOCForPrompt(toc TOC, maxChars int) string {
	if len(toc.Entries) == 0 {
		return ""
	}
	if maxChars <= 0 {
		maxChars = DefaultTOCPromptChars
	}
	const header = "# Available Knowledge\n\nRead an entry in full by id before relying on it.\n\n"
	var b strings.Builder
	b.WriteString(header)
	shown := 0
	for _, e := range toc.Entries {
		line := tocLine(e)
		if b.Len()+len(line)+tocFooterReserve > maxChars {
			break
		}
		b.WriteString(line)
		shown++
	}
	if omitted := toc.Total - shown; omitted > 0 {
		fmt.Fprintf(&b, "\n(%d more entries not listed)\n", omitted)
	}
	return b.String()
}

func tocLine(e TOCEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- `%s` — %s [%s, %s", e.ID, e.Title, e.Type, e.Layer)
	if e.Repo != "" {
		b.WriteString(", repo " + e.Repo)
	}
	if e.Status != StateApproved {
		b.WriteString(", " + string(e.Status))
	}
	if e.Updated != nil {
		b.WriteString(", updated " + e.Updated.Format("2006-01-02"))
	}
	b.WriteString("]")
	if e.Source != "" {
		b.WriteString(" (source: " + e.Source + ")")
	}
	b.WriteString("\n")
	return b.String()
}

// RenderFactMarkdown renders a fact as a complete markdown document: YAML
// front-matter followed by the body.
func RenderFactMarkdown(f Fact) string {
	var b strings.Builder
	b.WriteString("---\n")
	writeFM := func(k, v string) {
		if v = sanitizeFrontmatterValue(v); v != "" {
			b.WriteString(k + ": " + v + "\n")
		}
	}
	writeFM("title", f.Title)
	writeFM("type", string(f.Type))
	writeFM("layer", string(f.Layer))
	writeFM("state", string(f.EffectiveState()))
	if f.ConfidenceScored {
		writeFM("confidence", fmt.Sprintf("%.2f", f.Confidence))
	}
	if len(f.Tags) > 0 {
		writeFM("tags", "["+strings.Join(f.Tags, ", ")+"]")
	}
	if len(f.Related) > 0 {
		writeFM("related", "["+strings.Join(f.Related, ", ")+"]")
	}
	writeFM("supersedes", f.Supersedes)
	writeFM("superseded_by", f.SupersededBy)
	writeFM("source", f.Origin)
	if !f.Updated.IsZero() {
		writeFM("updated", f.Updated.UTC().Format(time.RFC3339))
	}
	b.WriteString("---\n\n")
	b.WriteString(f.Body)
	if !strings.HasSuffix(f.Body, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// ReadEntry returns the complete fact for slug from any source the TOC lists:
// wiki layers, vaults, then git sources. It returns nil, nil when no source
// has the slug.
func (k *KnowledgeAPI) ReadEntry(ctx context.Context, slug string) (*Fact, error) {
	if f, err := k.ReadFact(ctx, slug); err != nil || f != nil {
		return f, err
	}
	k.mu.RLock()
	sources := k.gitSources
	k.mu.RUnlock()
	for _, gs := range sources {
		if !gs.Ready() {
			continue
		}
		if f, err := gs.Store().ReadPage(slug); err == nil {
			f.Layer = gs.Config().Layer
			return f, nil
		}
	}
	return nil, nil
}
