package fleetreport

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/logscrub"
)

// Affected-hives block (hivecommons/hive#12529 follow-up). Each affected
// hive renders on its own bullet with the identity a maintainer needs to
// reach WHO is affected: the hive name, its anonymous instance digest, the
// GitHub login(s) of its owner(s), how many times it reported and when it
// was last seen. Pre-existing inline blocks ("**Affected hives (2):** a, b")
// still parse, so an older issue gains the per-hive lines on its next join.

// AffectedOwnersAnonymous is rendered in place of owner logins when the
// reporting hive opted out of naming its owners
// (governor.fleet_report.include_owners: false).
const AffectedOwnersAnonymous = "(anonymous)"

// affectedOwnersUnknown is rendered when a hive never told us its owners
// (a pre-owners spoke or a hive with no GitHub owner identity configured).
const affectedOwnersUnknown = "unknown"

// maxAffectedInstanceLen bounds the instance digest rendered per hive.
const maxAffectedInstanceLen = 16

// maxGitHubLoginLen is GitHub's documented username limit.
const maxGitHubLoginLen = 39

// AffectedHive is one entry of the affected block.
type AffectedHive struct {
	Hive string
	// Name is the hive's human display name and URL its public dashboard
	// (#13211); both optional. The line renders "<name> (<hive>)" and an
	// "[open](<url>)" cell when they are known.
	Name     string
	URL      string
	Instance string
	// Owners are the GitHub logins of the hive's owner(s), without "@".
	Owners []string
	// OwnersHidden is true when the hive opted out of naming its owners;
	// the line renders "(anonymous)" and never inherits owners from a
	// later report.
	OwnersHidden bool
	// Seen counts this hive's reports onto the issue; Last is its most
	// recent one (zero when the legacy block carried no timestamp).
	Seen int
	Last time.Time
}

// AffectedBlock is the parsed affected-hives block.
type AffectedBlock struct {
	Hives []AffectedHive
	Repos []string
	Seen  int
}

// NormalizeLogin lower-cases a GitHub login and strips the "@" and the hub's
// "github:" wire prefix. Anything that is not a plausible GitHub username
// (alphanumerics and single hyphens, at most 39 characters) or that names a
// non-GitHub identity provider ("ibmid:…") renders as "" so it never reaches
// the public issue body.
func NormalizeLogin(login string) string {
	login = strings.ToLower(strings.TrimSpace(logscrub.ScrubString(login)))
	login = strings.TrimPrefix(login, "@")
	login = strings.TrimPrefix(login, "github:")
	if login == "" || len(login) > maxGitHubLoginLen || strings.Contains(login, ":") {
		return ""
	}
	if strings.HasPrefix(login, "-") || strings.HasSuffix(login, "-") || strings.Contains(login, "--") {
		return ""
	}
	for _, r := range login {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !ok {
			return ""
		}
	}
	return login
}

// NormalizeLogins normalises, dedupes and sorts owner logins.
func NormalizeLogins(logins []string) []string {
	uniq := map[string]bool{}
	var out []string
	for _, l := range logins {
		l = NormalizeLogin(l)
		if l == "" || uniq[l] {
			continue
		}
		uniq[l] = true
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// OwnerMentions renders "@a, @b" for the evidence table and the affected
// block; "(anonymous)" when hidden, "—" when unknown.
func OwnerMentions(owners []string, hidden bool) string {
	if hidden {
		return AffectedOwnersAnonymous
	}
	owners = NormalizeLogins(owners)
	if len(owners) == 0 {
		return "—"
	}
	parts := make([]string, len(owners))
	for i, o := range owners {
		parts[i] = "@" + o
	}
	return strings.Join(parts, ", ")
}

func normalizeInstance(instance string) string {
	instance = safeToken(instance)
	if len(instance) > maxAffectedInstanceLen {
		instance = instance[:maxAffectedInstanceLen]
	}
	return instance
}

// normalize canonicalises an entry so renders are idempotent.
func (h AffectedHive) normalize() AffectedHive {
	h.Hive = labelValue(h.Hive)
	h.Name = NormalizeHiveName(h.Name)
	if h.Name == h.Hive {
		h.Name = ""
	}
	h.URL = NormalizeSpokeURL(h.URL)
	h.Instance = normalizeInstance(h.Instance)
	if h.Instance == h.Hive {
		h.Instance = ""
	}
	h.Owners = NormalizeLogins(h.Owners)
	if h.OwnersHidden {
		h.Owners = nil
	}
	if h.Seen < 1 {
		h.Seen = 1
	}
	if !h.Last.IsZero() {
		h.Last = h.Last.UTC().Truncate(time.Second)
	}
	return h
}

// ownersLabel is the owner cell of a bullet line.
func (h AffectedHive) ownersLabel() string {
	if h.OwnersHidden {
		return AffectedOwnersAnonymous
	}
	if len(h.Owners) == 0 {
		return affectedOwnersUnknown
	}
	return OwnerMentions(h.Owners, false)
}

// renderLine renders
// "- <name> (<hive>) (instance <id>) — [open](<url>) — owner(s): @a, @b — seen N, last <ts>".
// The name and the open link are omitted when unknown, which keeps a line
// written before #13211 byte-identical.
func (h AffectedHive) renderLine() string {
	var b strings.Builder
	b.WriteString("- ")
	if h.Name != "" {
		fmt.Fprintf(&b, "%s (%s)", h.Name, h.Hive)
	} else {
		b.WriteString(h.Hive)
	}
	if h.Instance != "" {
		fmt.Fprintf(&b, " (instance %s)", h.Instance)
	}
	if h.URL != "" {
		fmt.Fprintf(&b, " — [%s](%s)", affectedOpenLabel, h.URL)
	}
	fmt.Fprintf(&b, " — owner(s): %s — seen %d", h.ownersLabel(), h.Seen)
	if !h.Last.IsZero() {
		fmt.Fprintf(&b, ", last %s", h.Last.UTC().Format(time.RFC3339))
	}
	return b.String()
}

// parseAffectedLine is the inverse of renderLine; ok is false for a line
// that is not a hive bullet.
func parseAffectedLine(line string) (AffectedHive, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "- ") {
		return AffectedHive{}, false
	}
	parts := strings.Split(strings.TrimPrefix(line, "- "), " — ")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return AffectedHive{}, false
	}
	var h AffectedHive
	head := strings.TrimSpace(parts[0])
	if i := strings.Index(head, " (instance "); i >= 0 {
		h.Instance = strings.TrimSuffix(strings.TrimSpace(head[i+len(" (instance "):]), ")")
		head = strings.TrimSpace(head[:i])
	}
	// "<name> (<hive>)": the hive id is the last parenthesised token; a
	// display name may itself contain parentheses, so split on the last one.
	if strings.HasSuffix(head, ")") {
		if i := strings.LastIndex(head, " ("); i >= 0 {
			h.Name = strings.TrimSpace(head[:i])
			head = strings.TrimSpace(strings.TrimSuffix(head[i+len(" ("):], ")"))
		}
	}
	h.Hive = head
	for _, p := range parts[1:] {
		p = strings.TrimSpace(p)
		switch {
		case strings.HasPrefix(p, "["+affectedOpenLabel+"](") && strings.HasSuffix(p, ")"):
			h.URL = strings.TrimSuffix(strings.TrimPrefix(p, "["+affectedOpenLabel+"]("), ")")
		case strings.HasPrefix(p, "owner(s):"):
			v := strings.TrimSpace(strings.TrimPrefix(p, "owner(s):"))
			switch v {
			case AffectedOwnersAnonymous:
				h.OwnersHidden = true
			case affectedOwnersUnknown, "", "—":
			default:
				h.Owners = NormalizeLogins(strings.Split(v, ","))
			}
		case strings.HasPrefix(p, "seen "):
			rest := strings.TrimPrefix(p, "seen ")
			seenPart, lastPart, _ := strings.Cut(rest, ",")
			_, _ = fmt.Sscanf(strings.TrimSpace(seenPart), "%d", &h.Seen)
			if ts := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lastPart), "last ")); ts != "" {
				if t, err := time.Parse(time.RFC3339, ts); err == nil {
					h.Last = t
				}
			}
		}
	}
	return h.normalize(), true
}

// Render renders the idempotent affected block: one bullet per hive (sorted
// by hive), the union of the repos they watch, the repo owners derived from
// those repos, and seen_count. The repos and repo-owners lines are omitted
// when no hive contributed repos.
func (blk AffectedBlock) Render() string {
	hives := blk.normalizedHives()
	seen := blk.Seen
	if seen < len(hives) {
		seen = len(hives)
	}
	repos := NormalizeRepos(blk.Repos)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n**Affected hives (%d):**\n", AffectedBlockStart, len(hives))
	for _, h := range hives {
		b.WriteString(h.renderLine())
		b.WriteByte('\n')
	}
	if len(repos) > 0 {
		owners := OwnersOf(repos)
		fmt.Fprintf(&b, "**Affected repos (%d):** %s\n", len(repos), strings.Join(repos, ", "))
		fmt.Fprintf(&b, "**Affected repo owners (%d):** %s\n", len(owners), strings.Join(owners, ", "))
	}
	fmt.Fprintf(&b, "seen_count: %d\n%s", seen, AffectedBlockEnd)
	return b.String()
}

// normalizedHives dedupes entries by hive (merging duplicates) and sorts.
func (blk AffectedBlock) normalizedHives() []AffectedHive {
	byName := map[string]int{}
	var out []AffectedHive
	for _, h := range blk.Hives {
		h = h.normalize()
		if h.Hive == "" {
			continue
		}
		if i, ok := byName[h.Hive]; ok {
			out[i] = mergeAffected(out[i], h, false)
			continue
		}
		byName[h.Hive] = len(out)
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hive < out[j].Hive })
	return out
}

// mergeAffected folds a new report of the same hive into an existing entry.
// bump adds one to Seen and moves Last forward; otherwise the newer facts
// (instance, owners, visibility) are merged without touching the counters.
func mergeAffected(have, in AffectedHive, bump bool) AffectedHive {
	if have.Instance == "" {
		have.Instance = in.Instance
	}
	// Name and URL follow the newest report so a rename or a new vanity
	// host shows up on the next join; an empty incoming value keeps the old.
	if in.Name != "" {
		have.Name = in.Name
	}
	if in.URL != "" {
		have.URL = in.URL
	}
	switch {
	case in.OwnersHidden:
		have.OwnersHidden, have.Owners = true, nil
	case len(in.Owners) > 0:
		have.OwnersHidden = false
		have.Owners = NormalizeLogins(append(append([]string(nil), have.Owners...), in.Owners...))
	}
	if bump {
		have.Seen++
		if in.Last.After(have.Last) {
			have.Last = in.Last
		}
	} else if have.Last.IsZero() && !in.Last.IsZero() {
		have.Last = in.Last
	}
	return have
}

// ParseAffected extracts the block from a body; ok is false when the body
// has no block. Both the per-hive bullet form and the legacy inline list
// are understood; repo owners are never parsed, they are re-derived.
func ParseAffected(body string) (AffectedBlock, bool) {
	start := strings.Index(body, AffectedBlockStart)
	end := strings.Index(body, AffectedBlockEnd)
	if start < 0 || end < 0 || end < start {
		return AffectedBlock{}, false
	}
	var blk AffectedBlock
	block := body[start+len(AffectedBlockStart) : end]
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "**Affected hives"):
			for _, name := range affectedLineItems(line) {
				blk.Hives = append(blk.Hives, AffectedHive{Hive: name})
			}
		case strings.HasPrefix(line, "**Affected repos"):
			blk.Repos = append(blk.Repos, affectedLineItems(line)...)
		case strings.HasPrefix(line, "seen_count:"):
			_, _ = fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(line, "seen_count:")), "%d", &blk.Seen)
		case strings.HasPrefix(line, "- "):
			if h, ok := parseAffectedLine(line); ok {
				blk.Hives = append(blk.Hives, h)
			}
		}
	}
	blk.Hives = blk.normalizedHives()
	blk.Repos = NormalizeRepos(blk.Repos)
	return blk, true
}

// UpdateAffected returns body with the reporting hive merged into the
// affected block (appending a block when the body has none) and repos
// merged into the union. bump counts this as a new sighting: seen_count and
// the hive's own seen go up and its last-seen moves to in.Last. changed is
// false when nothing about the block would differ, so a re-report never
// edits the issue for nothing.
func UpdateAffected(body string, in AffectedHive, repos []string, bump bool) (string, bool) {
	blk, ok := ParseAffected(body)
	in = in.normalize()
	found := false
	for i, h := range blk.Hives {
		if h.Hive == in.Hive {
			blk.Hives[i] = mergeAffected(h, in, bump)
			found = true
			break
		}
	}
	if !found && in.Hive != "" {
		blk.Hives = append(blk.Hives, in)
	}
	blk.Repos = NormalizeRepos(append(append([]string(nil), blk.Repos...), repos...))
	if bump {
		blk.Seen++
	}
	block := blk.Render()
	if !ok {
		return strings.TrimRight(body, "\n") + "\n\n" + block + "\n", true
	}
	start := strings.Index(body, AffectedBlockStart)
	end := strings.Index(body, AffectedBlockEnd) + len(AffectedBlockEnd)
	if body[start:end] == block {
		return body, false
	}
	return body[:start] + block + body[end:], true
}
