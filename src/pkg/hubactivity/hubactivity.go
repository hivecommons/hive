// Package hubactivity is the wire contract for the bounded, read-only
// activity digest a spoke attaches to its hub heartbeat (RFC
// hivecommons/hive#15861, first slice #15951). Both ends import it, so the
// spoke's builder and the hub's re-validation share one set of ceilings and
// one urgency order.
//
// The digest is visibility-only: it names items (repo, number, title,
// stage, modifiers) the spoke tracker already shows, never tokens, pane text
// or settings. Private-repository items are dropped by the builder unless
// the spoke opts in, but they are always counted in Totals so the numbers
// stay honest. The hub enforces the same ceilings again (Sanitize) because a
// spoke's payload is untrusted input.
package hubactivity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	// DefaultMaxItems is the item cap when the spoke configures none.
	DefaultMaxItems = 100
	// MaxItemsCeiling is the finite server-side ceiling: no configuration
	// can make a digest carry more items, and the hub truncates anything
	// beyond it on receipt.
	MaxItemsCeiling = 250
	// MaxTitleRunes bounds an item title.
	MaxTitleRunes = 200
	// MaxModsPerItem bounds the modifier pills on one item.
	MaxModsPerItem = 8
	// MaxTotalsKeys bounds each Totals breakdown map.
	MaxTotalsKeys = 64
	// maxTokenRunes bounds short identifier fields (repo, kind, stage, mod).
	maxTokenRunes = 120
	// maxCount bounds every spoke-claimed integer so totals cannot
	// overflow or go negative.
	maxCount = 1_000_000_000
)

var (
	// repoPattern is GitHub's owner/name shape; rows naming anything else
	// are dropped so a consumer can build links from Repo safely.
	repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
	// slugPattern bounds stage and modifier slugs.
	slugPattern        = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)
	allowedKinds       = []string{"issue", "pr"}
	allowedAuthorKinds = []string{"human", "clanker", "hive"}
)

// Modifier names that drive urgency ordering. They match the stage
// tracker's modifier pills.
const (
	ModNeedsHuman    = "needs-human"
	ModNeedsDecision = "needs-decision"
	ModStalled       = "stalled"
)

// Item is one open issue or PR in the digest.
type Item struct {
	Repo   string `json:"repo"`
	Number int    `json:"n"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	// AuthorKind is human / clanker / hive ("" = unknown).
	AuthorKind string `json:"author_kind,omitempty"`
	// Stage is the stage slug; StageNumber its pipeline number, used to
	// order "furthest stage first" when the cap bites.
	Stage       string     `json:"stage"`
	StageNumber int        `json:"stage_n,omitempty"`
	Since       *time.Time `json:"since,omitempty"`
	Mods        []string   `json:"mods,omitempty"`
	Private     bool       `json:"private,omitempty"`
}

// Totals counts every tracked item, including private ones and ones the
// cap dropped.
type Totals struct {
	Items      int            `json:"items"`
	ByStage    map[string]int `json:"by_stage,omitempty"`
	ByModifier map[string]int `json:"by_modifier,omitempty"`
}

// Digest is the heartbeat `activity` block. A digest carrying only ETag and
// GeneratedAt is an "unchanged" marker: the spoke's content matches the
// etag the hub last acknowledged.
type Digest struct {
	GeneratedAt time.Time `json:"generated_at"`
	ETag        string    `json:"etag"`
	Totals      *Totals   `json:"totals,omitempty"`
	Items       []Item    `json:"items,omitempty"`
	// Truncated is how many eligible items the cap left out.
	Truncated int `json:"truncated,omitempty"`
}

// IsUnchangedMarker reports whether d only re-asserts a previously sent
// etag.
func (d Digest) IsUnchangedMarker() bool {
	return d.ETag != "" && d.Totals == nil && len(d.Items) == 0 && d.Truncated == 0
}

// BuildOptions configures Build.
type BuildOptions struct {
	// MaxItems is the item cap; <= 0 means DefaultMaxItems and values above
	// MaxItemsCeiling are clamped to it.
	MaxItems int
	// SharePrivate keeps private-repository items (still marked Private).
	SharePrivate bool
	Now          time.Time
}

// EffectiveMaxItems clamps a configured cap to (0, MaxItemsCeiling].
func EffectiveMaxItems(configured int) int {
	if configured <= 0 {
		return DefaultMaxItems
	}
	return min(configured, MaxItemsCeiling)
}

// Build produces a bounded digest from the spoke's tracked items. It does
// not modify items.
func Build(items []Item, opts BuildOptions) Digest {
	totals := countTotals(items)
	eligible := make([]Item, 0, len(items))
	seen := map[string]bool{}
	for _, it := range items {
		if it.Private && !opts.SharePrivate {
			continue
		}
		if n, ok := normalizeItem(it); ok && !seen[itemKey(n)] {
			seen[itemKey(n)] = true
			eligible = append(eligible, n)
		}
	}
	slices.SortStableFunc(eligible, CompareUrgency)
	limit := EffectiveMaxItems(opts.MaxItems)
	truncated := 0
	if len(eligible) > limit {
		truncated = len(eligible) - limit
		eligible = eligible[:limit]
	}
	d := Digest{GeneratedAt: opts.Now.UTC(), Totals: totals, Items: eligible, Truncated: truncated}
	d.ETag = ContentETag(d)
	return d
}

// Sanitize re-applies every ceiling to a digest received from a spoke. It
// drops malformed rows (repo not owner/name, no number) and duplicate
// repo#n rows, truncates to MaxItemsCeiling and adds the overflow to
// Truncated, bounds titles, tokens, modifiers and every integer, and caps
// the Totals maps. The input is not modified.
func Sanitize(d Digest) Digest {
	out := Digest{GeneratedAt: d.GeneratedAt.UTC(), ETag: CleanETag(d.ETag), Truncated: clampCount(d.Truncated)}
	if d.Totals != nil {
		out.Totals = &Totals{
			Items:      clampCount(d.Totals.Items),
			ByStage:    boundCounts(d.Totals.ByStage),
			ByModifier: boundCounts(d.Totals.ByModifier),
		}
	}
	seen := map[string]bool{}
	for _, it := range d.Items {
		n, ok := normalizeItem(it)
		if !ok || seen[itemKey(n)] {
			continue
		}
		seen[itemKey(n)] = true
		if len(out.Items) == MaxItemsCeiling {
			out.Truncated = clampCount(out.Truncated + 1)
			continue
		}
		out.Items = append(out.Items, n)
	}
	return out
}

// CleanETag bounds a spoke-supplied etag the same way Sanitize stores it,
// so an unchanged marker compares against the stored form.
func CleanETag(etag string) string {
	return cleanToken(etag)
}

// ContentETag is a weak etag over a digest's content, independent of
// GeneratedAt so an unchanged tracker keeps its etag across beats.
func ContentETag(d Digest) string {
	body, err := json.Marshal(struct {
		Totals    *Totals `json:"t"`
		Items     []Item  `json:"i"`
		Truncated int     `json:"x"`
	}{d.Totals, d.Items, d.Truncated})
	if err != nil {
		// Marshalling plain structs cannot fail; never emit an etag that
		// could match a different body.
		return ""
	}
	sum := sha256.Sum256(body)
	return `W/"` + hex.EncodeToString(sum[:8]) + `"`
}

func countTotals(items []Item) *Totals {
	byStage := map[string]int{}
	byMod := map[string]int{}
	for _, it := range items {
		if s := slug(it.Stage); s != "" {
			byStage[s]++
		}
		for _, m := range it.Mods {
			if m = slug(m); m != "" {
				byMod[m]++
			}
		}
	}
	return &Totals{Items: len(items), ByStage: boundCounts(byStage), ByModifier: boundCounts(byMod)}
}

// boundCounts copies m keeping at most MaxTotalsKeys sanitized keys (the
// lexically smallest, so the result is deterministic).
func boundCounts(m map[string]int) map[string]int {
	keys := make([]string, 0, len(m))
	for k := range m {
		if cleanToken(k) != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	slices.Sort(keys)
	out := make(map[string]int, min(len(keys), MaxTotalsKeys))
	for _, k := range keys {
		ck := cleanToken(k)
		if _, seen := out[ck]; !seen && len(out) == MaxTotalsKeys {
			break
		}
		out[ck] = clampCount(out[ck] + clampCount(m[k]))
	}
	return out
}

func clampCount(n int) int {
	return min(max(n, 0), maxCount)
}

func itemKey(it Item) string {
	return strings.ToLower(it.Repo) + "#" + strconv.Itoa(it.Number)
}

// normalizeItem bounds and validates one row; ok is false for a row that
// must be dropped (repo not owner/name, or no positive number).
func normalizeItem(it Item) (Item, bool) {
	out := it
	out.Repo = cleanToken(it.Repo)
	if !repoPattern.MatchString(out.Repo) || it.Number <= 0 || it.Number > maxCount {
		return Item{}, false
	}
	out.Kind = oneOf(cleanToken(it.Kind), allowedKinds)
	out.AuthorKind = oneOf(cleanToken(it.AuthorKind), allowedAuthorKinds)
	out.Stage = slug(it.Stage)
	out.StageNumber = clampCount(it.StageNumber)
	out.Title = cleanText(it.Title, MaxTitleRunes)
	if it.Since != nil {
		s := it.Since.UTC()
		out.Since = &s
	}
	out.Mods = nil
	for _, m := range it.Mods {
		if len(out.Mods) == MaxModsPerItem {
			break
		}
		if m = slug(m); m != "" {
			out.Mods = append(out.Mods, m)
		}
	}
	return out, true
}

// oneOf returns v when it is one of allowed, else "".
func oneOf(v string, allowed []string) string {
	if slices.Contains(allowed, v) {
		return v
	}
	return ""
}

// slug returns s when it is a valid stage/modifier slug, else "".
func slug(s string) string {
	s = strings.TrimSpace(s)
	if slugPattern.MatchString(s) {
		return s
	}
	return ""
}

// urgencyRank: 0 needs a human or a decision, 1 stalled, 2 everything else.
func urgencyRank(it Item) int {
	rank := 2
	for _, m := range it.Mods {
		switch m {
		case ModNeedsHuman, ModNeedsDecision:
			return 0
		case ModStalled:
			rank = 1
		}
	}
	return rank
}

// CompareUrgency orders items most urgent first: needs-human or
// needs-decision, then stalled, then furthest stage, then oldest in stage,
// then repo and number so the order is total and deterministic.
func CompareUrgency(a, b Item) int {
	if ra, rb := urgencyRank(a), urgencyRank(b); ra != rb {
		return ra - rb
	}
	if a.StageNumber != b.StageNumber {
		return b.StageNumber - a.StageNumber
	}
	if c := compareSince(a.Since, b.Since); c != 0 {
		return c
	}
	if c := strings.Compare(a.Repo, b.Repo); c != 0 {
		return c
	}
	return a.Number - b.Number
}

// compareSince orders older first; an unknown time sorts last.
func compareSince(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	return a.Compare(*b)
}

func cleanToken(s string) string {
	return cleanText(strings.TrimSpace(s), maxTokenRunes)
}

// cleanText drops control and format characters and caps the rune count.
// Titles are rendered as text on the hub; this only bounds them.
func cleanText(s string, maxRunes int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == maxRunes {
			break
		}
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			continue
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
