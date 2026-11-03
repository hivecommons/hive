// Package rfccoverage points new RFC / feature-request issues at the
// existing settings that may already cover them (hivecommons/hive#13575).
//
// Many RFCs ask for something the hive already does behind a setting. At
// triage the hive matches the issue's title and body, by keyword, against
// the dashboard's Settings search index (the SETTINGS_SEARCH_STATIC_HITS
// table in pkg/dashboard/static/index.html) and leaves ONE comment listing
// the best matches, each linking to Settings with the search pre-filled.
//
// The match is deterministic token overlap — no model call — and the
// product is advisory: the package never labels, closes or decides. The
// comment carries Marker, so a restart never posts a second copy.
//
// Like pkg/firsttimer the package is stdlib-only: the caller (cmd/hive)
// projects issues into Candidates and supplies a small Forge.
package rfccoverage

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Marker opens every comment this package writes.
const Marker = "<!-- hive:rfc-existing-coverage -->"

// DocsRepo is where the hive's own docs live; a setting's docs link is a
// code search for its key under src/docs there.
const DocsRepo = "hivecommons/hive"

// MinScore is the lowest score a setting needs to be listed: at least two
// distinct shared keywords, or one in the title plus one in the body.
const MinScore = 3

// Setting is one Settings search index entry.
type Setting struct {
	Tab     string
	Section string
	Label   string
	Key     string
	Text    string
}

var (
	staticHitsStartRE = regexp.MustCompile(`const\s+SETTINGS_SEARCH_STATIC_HITS\s*=\s*\[`)
	staticHitFieldRE  = regexp.MustCompile(`(\w+):\s*'((?:[^'\\]|\\.)*)'`)
)

// ParseSettingsIndex extracts the SETTINGS_SEARCH_STATIC_HITS entries from
// the dashboard page. Entries without a key are dropped: the comment links
// each setting by key.
func ParseSettingsIndex(page []byte) []Setting {
	loc := staticHitsStartRE.FindIndex(page)
	if loc == nil {
		return nil
	}
	rest := string(page[loc[1]:])
	if end := strings.Index(rest, "];"); end >= 0 {
		rest = rest[:end]
	}
	var out []Setting
	for _, line := range strings.Split(rest, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var s Setting
		for _, m := range staticHitFieldRE.FindAllStringSubmatch(line, -1) {
			v := strings.ReplaceAll(strings.ReplaceAll(m[2], `\'`, `'`), `\\`, `\`)
			switch m[1] {
			case "tab":
				s.Tab = v
			case "section":
				s.Section = v
			case "label":
				s.Label = v
			case "key":
				s.Key = v
			case "text":
				s.Text = v
			}
		}
		if s.Key != "" {
			out = append(out, s)
		}
	}
	return out
}

var tokenSplitRE = regexp.MustCompile(`[^a-z0-9]+`)

// stopwords are words too common in issues or in the index to say anything
// about which setting an issue is asking for.
var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`the and for are not but can all any its our you your they them then than
		will just more some such other each been being does doing done way get set has had was were who how why
		this that these those there their what which when where with without into from about like also only make
		need needs able allow allows option options setting settings enable enabled disable disabled new use used
		using want wants would should could please hive hives issue issues feature features add adds added support
		rfc enhancement proposal request requests agent agents currently today already instead maybe still even
		very much many lot lots thing things time true false key keys default defaults http https www com github
		after before back hand`) {
		stopwords[w] = true
	}
}

// Tokens lowercases s, splits it on anything that is not a letter or digit
// (so "duplicate_sweep.enabled" is "duplicate sweep") and drops stopwords
// and words shorter than three letters. A trailing plural "s" is trimmed so
// "labels" and "label" match.
func Tokens(s string) []string {
	var out []string
	for _, t := range tokenSplitRE.Split(strings.ToLower(s), -1) {
		if len(t) > 4 && strings.HasSuffix(t, "s") && !strings.HasSuffix(t, "ss") {
			t = strings.TrimSuffix(t, "s")
		}
		if len(t) < 3 || stopwords[t] {
			continue
		}
		out = append(out, t)
	}
	return out
}

func tokenSet(parts ...string) map[string]bool {
	set := map[string]bool{}
	for _, p := range parts {
		for _, t := range Tokens(p) {
			set[t] = true
		}
	}
	return set
}

// Match is one setting an issue may already be covered by.
type Match struct {
	Setting
	Score int
	// Shared are the keywords the issue and the setting have in common,
	// sorted.
	Shared []string
}

// FindMatches ranks settings by keyword overlap with the issue: each shared
// keyword scores 1, plus 1 more when it appears in the title, and an issue
// that names a setting's key verbatim scores 5 more for it. Settings below
// MinScore are dropped; at most limit are returned (limit <= 0: all).
func FindMatches(title, body string, settings []Setting, limit int) []Match {
	titleSet := tokenSet(title)
	issueSet := tokenSet(title, body)
	lowered := strings.ToLower(title + "\n" + body)
	best := map[string]Match{}
	for _, s := range settings {
		score := 0
		var shared []string
		for t := range tokenSet(s.Key, s.Label, s.Section, s.Text) {
			if !issueSet[t] {
				continue
			}
			shared = append(shared, t)
			score++
			if titleSet[t] {
				score++
			}
		}
		if strings.Contains(lowered, strings.ToLower(s.Key)) {
			score += 5
		}
		if score < MinScore || len(shared) == 0 {
			continue
		}
		sort.Strings(shared)
		if prev, ok := best[s.Key]; ok && prev.Score >= score {
			continue
		}
		best[s.Key] = Match{Setting: s, Score: score, Shared: shared}
	}
	out := make([]Match, 0, len(best))
	for _, m := range best {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Key < out[j].Key
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// SettingsURL is the dashboard deep link that opens Settings on the
// setting's tab with the search box holding its key
// (#config/governor/<tab>/<query>). It is "" when origin is unknown.
func SettingsURL(origin string, s Setting) string {
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	if origin == "" {
		return ""
	}
	return origin + "/#config/governor/" + url.PathEscape(s.Tab) + "/" + url.PathEscape(s.Key)
}

// DocsSearchURL is a code search for the key in the hive's docs.
func DocsSearchURL(key string) string {
	return "https://github.com/search?type=code&q=" + url.QueryEscape("repo:"+DocsRepo+" path:src/docs "+strconv.Quote(key))
}

// RenderComment builds the comment body. With no matches it says nothing
// was found. origin is the dashboard's public origin ("" renders the key
// without a Settings link).
func RenderComment(origin string, matches []Match) string {
	var b strings.Builder
	b.WriteString(Marker + "\n")
	b.WriteString("**Triage: does this already exist?**\n\n")
	if len(matches) == 0 {
		b.WriteString("Nothing found: no existing setting in the Settings search index matches this request's keywords.\n")
		b.WriteString("\nThis is an automated keyword check, not a decision: no labels were changed and the issue stays in triage as usual.\n")
		return b.String()
	}
	b.WriteString("These existing settings and features look related to this request. If one already covers it, please say so here; if not, a note on what is missing helps whoever picks this up.\n\n")
	for _, m := range matches {
		where := strings.TrimSpace(m.Tab)
		if m.Section != "" && m.Section != m.Tab {
			where += " → " + m.Section
		}
		name := m.Label
		if name == "" {
			name = m.Key
		}
		if link := SettingsURL(origin, m.Setting); link != "" {
			fmt.Fprintf(&b, "- [%s](%s) — `%s` (Settings → %s)", name, link, m.Key, where)
		} else {
			fmt.Fprintf(&b, "- **%s** — `%s` (Settings → %s; search Settings for the key)", name, m.Key, where)
		}
		fmt.Fprintf(&b, " · [docs](%s) · matched: %s\n", DocsSearchURL(m.Key), strings.Join(m.Shared, ", "))
	}
	b.WriteString("\nThis is an automated keyword check against the Settings search index, not a decision: no labels were changed and the issue stays in triage as usual.\n")
	return b.String()
}

// Candidate is an issue to check.
type Candidate struct {
	Repo      string
	Number    int
	Title     string
	Body      string
	Labels    []string
	CreatedAt time.Time
}

// Key is the per-issue identity.
func (c Candidate) Key() string { return c.Repo + "#" + strconv.Itoa(c.Number) }

// Forge is the slice of the GitHub client the sweep needs.
type Forge interface {
	// FindMarkerComment reports whether a comment containing marker exists.
	FindMarkerComment(ctx context.Context, repo string, number int, marker string) (bool, error)
	// CreateComment posts body on the issue.
	CreateComment(ctx context.Context, repo string, number int, body string) error
}

// Options parameterise one pass.
type Options struct {
	Settings []Setting
	// Origin is the dashboard's public origin for Settings links.
	Origin string
	// Labels select the issues to check (lowercase).
	Labels []string
	// MaxMatches caps the settings listed per comment.
	MaxMatches int
	// MaxAge skips issues opened longer ago than this (0: no limit).
	MaxAge time.Duration
	// PostWhenNothingFound posts the "nothing found" comment.
	PostWhenNothingFound bool
	Now                  time.Time
	Logger               *slog.Logger
}

// Skip reasons counted in Result.Skipped.
const (
	SkipNotRFC       = "not-rfc"
	SkipTooOld       = "too-old"
	SkipDone         = "already-checked"
	SkipNothingFound = "nothing-found"
)

// Result summarises one pass.
type Result struct {
	Scanned   int
	Commented []string
	Skipped   map[string]int
	Errors    int
}

// Sweeper remembers which issues were checked so a later pass costs no
// GitHub call for them.
type Sweeper struct {
	mu   sync.Mutex
	done map[string]bool
}

// NewSweeper returns an empty Sweeper.
func NewSweeper() *Sweeper { return &Sweeper{done: map[string]bool{}} }

func (s *Sweeper) checked(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done[key]
}

func (s *Sweeper) remember(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done[key] = true
}

func hasAnyLabel(labels, want []string) bool {
	for _, l := range labels {
		l = strings.ToLower(strings.TrimSpace(l))
		for _, w := range want {
			if l == w {
				return true
			}
		}
	}
	return false
}

// Sweep comments once on every RFC candidate that has no Marker comment yet.
// It only reads issues and writes that one comment: it never labels, closes
// or edits anything.
func (s *Sweeper) Sweep(ctx context.Context, forge Forge, candidates []Candidate, opts Options) Result {
	res := Result{Skipped: map[string]int{}}
	if s == nil || forge == nil {
		return res
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	for _, c := range candidates {
		res.Scanned++
		key := c.Key()
		switch {
		case !hasAnyLabel(c.Labels, opts.Labels):
			res.Skipped[SkipNotRFC]++
			continue
		case opts.MaxAge > 0 && !c.CreatedAt.IsZero() && now.Sub(c.CreatedAt) > opts.MaxAge:
			res.Skipped[SkipTooOld]++
			continue
		case s.checked(key):
			res.Skipped[SkipDone]++
			continue
		}
		matches := FindMatches(c.Title, c.Body, opts.Settings, opts.MaxMatches)
		if len(matches) == 0 && !opts.PostWhenNothingFound {
			s.remember(key)
			res.Skipped[SkipNothingFound]++
			continue
		}
		found, err := forge.FindMarkerComment(ctx, c.Repo, c.Number, Marker)
		if err != nil {
			res.Errors++
			logger.Warn("rfc existing-coverage: marker lookup failed", "issue", key, "error", err)
			continue
		}
		if found {
			s.remember(key)
			res.Skipped[SkipDone]++
			continue
		}
		if err := forge.CreateComment(ctx, c.Repo, c.Number, RenderComment(opts.Origin, matches)); err != nil {
			res.Errors++
			logger.Warn("rfc existing-coverage: comment failed", "issue", key, "error", err)
			continue
		}
		s.remember(key)
		res.Commented = append(res.Commented, key)
		logger.Info("rfc existing-coverage comment", "issue", key, "matches", len(matches))
	}
	return res
}
