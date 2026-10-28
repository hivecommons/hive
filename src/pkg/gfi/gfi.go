// Package gfi is hive-curated `good first issue` selection: the hive picks a
// small subset of its open, unclaimed issues, labels them with GitHub's
// canonical `good first issue` (plus `help wanted`) so they surface in the
// repository's Contribute page and in good-first-issue aggregators, and leaves
// ONE welcoming comment that points a new contributor — one who brings their
// own coding agent — at this hive's contributor relay to claim the issue.
//
// The label is advertising, not a hold. The issue stays in the hive's own
// queue; if a hive agent starts on it first the label is released and the
// welcome comment is edited to say so (Release). A claim through the relay on
// a labelled issue is a conversion and is counted (Stats).
//
// The package is deliberately stdlib-only: the scorer, the comment template
// and the sweep take plain Candidates and a small Forge interface so the
// decision logic is testable without a GitHub client.
package gfi

import (
	"context"
	"errors"
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

const (
	// Label is GitHub's canonical good-first-issue label. The exact spelling
	// matters: github.com/<repo>/contribute and the topic aggregators key on
	// it, so a `good-first-issue` or `beginner` spelling would be invisible
	// to the people this feature exists to attract.
	Label = "good first issue"
	// LabelColor is GitHub's default colour for the label.
	LabelColor = "7057ff"
	// LabelDescription seeds the label when the repository lacks it.
	LabelDescription = "Good for newcomers"
	// HelpWantedLabel is applied alongside Label: both are what the Contribute
	// page and aggregators look for.
	HelpWantedLabel = "help wanted"
	// HelpWantedColor is GitHub's default colour for `help wanted`.
	HelpWantedColor = "008672"
	// HelpWantedDescription seeds the label when the repository lacks it.
	HelpWantedDescription = "Extra attention is needed"

	// Marker opens every comment this package writes; Supersede edits the
	// same comment rather than posting a second one.
	Marker = "<!-- hive:gfi -->"
	// SupersededMarker replaces Marker when the hive took the issue itself.
	SupersededMarker = "<!-- hive:gfi superseded=true -->"

	// DefaultMaxPerRepo caps how many issues may carry a hive-applied label
	// in one repository at once.
	DefaultMaxPerRepo = 5
	// DefaultMaxPerSweep caps selections per repository per pass so one
	// sweep never floods a tracker with welcome comments.
	DefaultMaxPerSweep = 2
	// DefaultMinAge keeps the hive from racing a fresh issue its reporter may
	// still be editing or about to fix. An hour is enough for that; a day
	// meant the scanner had long since claimed anything worth advertising.
	DefaultMinAge = time.Hour
	// DefaultMinBodyChars / DefaultMaxBodyChars bound the body length: too
	// short has no acceptance criteria, too long is rarely a first issue.
	DefaultMinBodyChars = 80
	DefaultMaxBodyChars = 4000
	// DefaultMaxPackages is how many distinct source directories an issue may
	// mention and still count as scoped.
	DefaultMaxPackages = 2
	// DefaultMinScore is the positive-signal count an issue needs.
	DefaultMinScore = 2
	// DefaultReselectCooldown is how long a released issue stays ineligible.
	DefaultReselectCooldown = 7 * 24 * time.Hour

	// ReasonClaimedByHive is the release reason when a hive agent started on
	// the issue.
	ReasonClaimedByHive = "claimed-by-hive"
)

// AlwaysExcludeLabels disqualify an issue whatever Policy.ExcludeLabels or
// Policy.HardSuppressLabels say: an issue held for a person (`needs-human`,
// hivecommons/hive#12680 was stamped `good first issue` minutes after its
// owner said "Agents stand down"), parked on a maintainer decision
// (`stage/045-needs-decision`, the pre-045 `stage/085-needs-decision` and
// the retired word spellings), or waiting on a
// spec is never a newcomer's first issue. `approved-direction` on its own
// is fine — it is the maintainer's go-ahead — but next to `needs-human` the
// go-ahead is for people, and `needs-human` still wins.
var AlwaysExcludeLabels = []string{
	"needs-human", "stage/045-needs-decision", "stage/085-needs-decision", "needs-decision", "needs-direction", "needs-spec",
}

// ClaimedByPrefix is the hive's issue-claim label namespace; any label under
// it disqualifies the issue (an agent owns it).
const ClaimedByPrefix = "hive/claimed-by-"

// DefaultExcludeLabels are labels that disqualify an issue outright, beyond
// the hard-suppress set the caller supplies.
var DefaultExcludeLabels = []string{
	"kind/epic", "epic", "meta", "meta-tracker", "tracker", "roadmap", "security", "kind/security",
	"blocked", "hold", "do-not-merge", "wontfix", "duplicate", "invalid",
	// Claimed / in-progress / PR-covered issues, under the stage labels
	// (hivecommons/hive#12436) and the retired spellings they replaced.
	"stage/020-claimed", "stage/030-pr-in-progress", "stage/040-covered-by-pr",
	"hive/in-progress", "hive/already-done", "hive/covered-by-pr", "🐝 covered by pr",
	// The hive's own reports to itself are never a newcomer's first issue.
	"hive/advisory", "hive/selfreport", "hive/sentinel", "hive/fleet", "fleet",
}

// sizeLabels are labels that already say "small".
var sizeLabels = []string{"size/xs", "size/s", "good-first-issue", "good first issue", "beginner", "easy", "starter", "first-timers-only", "low-hanging-fruit"}

// bugLabels mark a concrete, reproducible report.
var bugLabels = []string{"bug", "kind/bug", "type/bug"}

// epicTitlePrefixes disqualify by title shape. They are matched against the
// title after NormalizeTitle strips emoji, brackets and punctuation, so
// "📝 Epic: …", "[RFC]: …" and "✨ [v6] proposal …" all match.
var epicTitlePrefixes = []string{"epic", "tracking", "umbrella", "rfc", "proposal", "hive advisory report", "self-report", "selfreport", "fleet report"}

// epicTitleWords disqualify wherever they appear as a whole word ("v6 epic
// + readiness bar", "[meta] …" once the bracket is unwrapped).
var epicTitleWords = map[string]bool{"epic": true, "rfc": true, "umbrella": true, "meta": true}

// securityTitleWords disqualify an issue whose title says it is a security
// item even when nobody applied a security label ("Review backlog (security): …").
var securityTitleWords = regexp.MustCompile(`(?i)\b(security|vulnerab\w*|cve-\d+|exploit|xss|csrf|rce|injection)\b`)

// bracketTag turns a bracketed tag such as [RFC] or [v6] into a bare word
// and leadingNoise strips everything before the first letter or digit
// (emoji, variation selectors, punctuation, whitespace).
var (
	bracketTag   = regexp.MustCompile(`\[([^\]]*)\]`)
	leadingNoise = regexp.MustCompile(`^[^\p{L}\p{N}]+`)
)

// NormalizeTitle lowercases a title, unwraps bracketed tags and strips
// leading emoji and punctuation so shape checks see the first real word.
// "[RFC]: Evidence pipeline" → "rfc : evidence pipeline"; "📝 v6 epic + …" →
// "v6 epic + …".
func NormalizeTitle(title string) string {
	t := strings.ToLower(strings.TrimSpace(title))
	t = bracketTag.ReplaceAllString(t, " $1 ")
	t = leadingNoise.ReplaceAllString(t, "")
	return strings.Join(strings.Fields(t), " ")
}

var (
	// pathPattern finds source paths mentioned in a body: at least one slash,
	// or a bare filename with a code extension.
	pathPattern = regexp.MustCompile("(?i)\\b(?:src/|pkg/|cmd/|internal/|lib/|app/|docs/)?[a-z0-9_.-]+(?:/[a-z0-9_.-]+)+(?:\\.[a-z]{1,5})?\\b")
	// urlPattern strips links before path extraction.
	urlPattern = regexp.MustCompile(`https?://\S+`)
	// fileExt marks a path segment as a file rather than a directory.
	fileExt = regexp.MustCompile(`(?i)\.(go|md|ts|tsx|js|jsx|py|rs|java|rb|yaml|yml|json|html|css|sh|txt|toml|proto|sql)$`)
	// headingOrBullet finds structure that reads as steps or acceptance
	// criteria.
	headingOrBullet = regexp.MustCompile(`(?m)^\s*(?:#{1,6}\s+\S|[-*+]\s+\S|\d+[.)]\s+\S|- \[[ xX]\]\s+\S)`)
	// criteriaWords are phrases that signal a definition of done.
	criteriaWords = regexp.MustCompile(`(?i)acceptance criteria|definition of done|steps to reproduce|expected behavio|how to verify|to reproduce|should:|must:`)
)

// Candidate is the issue-shaped input to the scorer. The caller (cmd/hive)
// projects it from the hive's enumeration envelope so this package does not
// import the GitHub client.
type Candidate struct {
	// Repo is owner/name. Number is the issue number.
	Repo   string
	Number int
	Title  string
	Body   string
	// URL is the issue's html_url; derived from Repo/Number when empty.
	URL       string
	Labels    []string
	Assignees []string
	CreatedAt time.Time
	// IsPR marks a pull request wearing the issue shape.
	IsPR bool
	// ClaimedBy is a live claimant (hive:claim marker or assignee).
	ClaimedBy string
	// HasLinkedPR says an open PR already references the issue.
	HasLinkedPR bool
}

// Key is the owner/name#number identity.
func (c Candidate) Key() string { return c.Repo + "#" + strconv.Itoa(c.Number) }

// IssueURL returns URL or the github.com form.
func (c Candidate) IssueURL() string {
	if strings.TrimSpace(c.URL) != "" {
		return c.URL
	}
	return "https://github.com/" + c.Repo + "/issues/" + strconv.Itoa(c.Number)
}

// Policy is the tunable half of the scorer and sweep. Zero values mean the
// package defaults; see Normalized.
type Policy struct {
	MaxPerRepo       int
	MaxPerSweep      int
	MinAge           time.Duration
	MinBodyChars     int
	MaxBodyChars     int
	MaxPackages      int
	MinScore         int
	ReselectCooldown time.Duration
	// ExcludeLabels replaces DefaultExcludeLabels when non-empty.
	ExcludeLabels []string
	// HardSuppressLabels are the hive's escalation labels; always excluded.
	HardSuppressLabels []string
}

// Normalized fills zero fields with the defaults.
func (p Policy) Normalized() Policy {
	if p.MaxPerRepo <= 0 {
		p.MaxPerRepo = DefaultMaxPerRepo
	}
	if p.MaxPerSweep <= 0 {
		p.MaxPerSweep = DefaultMaxPerSweep
	}
	if p.MinAge <= 0 {
		p.MinAge = DefaultMinAge
	}
	if p.MinBodyChars <= 0 {
		p.MinBodyChars = DefaultMinBodyChars
	}
	if p.MaxBodyChars <= 0 {
		p.MaxBodyChars = DefaultMaxBodyChars
	}
	if p.MaxPackages <= 0 {
		p.MaxPackages = DefaultMaxPackages
	}
	if p.MinScore <= 0 {
		p.MinScore = DefaultMinScore
	}
	if p.ReselectCooldown <= 0 {
		p.ReselectCooldown = DefaultReselectCooldown
	}
	if len(p.ExcludeLabels) == 0 {
		p.ExcludeLabels = append([]string(nil), DefaultExcludeLabels...)
	}
	return p
}

// Decision is the scorer's verdict for one Candidate.
type Decision struct {
	// Eligible is true when every gate passed and Score >= MinScore.
	Eligible bool
	// Score counts positive signals.
	Score int
	// Reasons are the human-readable positive signals, in a form that can be
	// quoted in the welcome comment ("scoped to one package").
	Reasons []string
	// Skip names the first failed gate when Eligible is false.
	Skip string
	// AlreadyLabelled is true when the issue already carries Label.
	AlreadyLabelled bool
}

// Score applies the qualification heuristic. It never calls out; everything
// it needs is on the Candidate.
func Score(c Candidate, p Policy, now time.Time) Decision {
	p = p.Normalized()
	labels := lowerSet(c.Labels)
	d := Decision{AlreadyLabelled: labels[Label]}
	switch {
	case c.IsPR:
		return skip(d, "pull-request")
	case c.Number <= 0 || strings.TrimSpace(c.Repo) == "":
		return skip(d, "no-identity")
	case strings.TrimSpace(c.ClaimedBy) != "":
		return skip(d, "claimed")
	case len(c.Assignees) > 0:
		return skip(d, "assigned")
	case c.HasLinkedPR:
		return skip(d, "linked-pr")
	}
	for _, l := range AlwaysExcludeLabels {
		if labels[l] {
			return skip(d, "hard-suppress:"+l)
		}
	}
	for _, l := range p.HardSuppressLabels {
		if labels[strings.ToLower(strings.TrimSpace(l))] {
			return skip(d, "hard-suppress:"+l)
		}
	}
	for _, l := range p.ExcludeLabels {
		if labels[strings.ToLower(strings.TrimSpace(l))] {
			return skip(d, "excluded-label:"+l)
		}
	}
	for _, l := range c.Labels {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(l)), ClaimedByPrefix) {
			return skip(d, "excluded-label:"+l)
		}
	}
	if why, ok := TrackingReason(c.Title, c.Labels, c.Body); ok {
		return skip(d, SkipTracking+":"+why)
	}
	if why, ok := OpsTrackingReason(c.Title, c.Body); ok {
		return skip(d, SkipOpsTracking+":"+why)
	}
	if _, ok := ParseRecheckAfter(c.Body); ok {
		return skip(d, SkipTimeGated)
	}
	title := NormalizeTitle(c.Title)
	for _, prefix := range epicTitlePrefixes {
		if strings.HasPrefix(title, prefix) {
			return skip(d, "title:"+prefix)
		}
	}
	for _, w := range strings.Fields(title) {
		if epicTitleWords[w] {
			return skip(d, "title:"+w)
		}
	}
	if securityTitleWords.MatchString(c.Title) {
		return skip(d, "title:security")
	}
	if c.CreatedAt.IsZero() || now.Sub(c.CreatedAt) < p.MinAge {
		return skip(d, "too-fresh")
	}
	body := strings.TrimSpace(c.Body)
	if len(body) < p.MinBodyChars {
		return skip(d, "body-too-short")
	}
	if len(body) > p.MaxBodyChars {
		return skip(d, "body-too-long")
	}
	pkgs := MentionedPackages(body)
	if len(pkgs) > p.MaxPackages {
		return skip(d, fmt.Sprintf("touches-%d-packages", len(pkgs)))
	}

	if hasAny(labels, sizeLabels) {
		d.Reasons = append(d.Reasons, "already sized small by a maintainer")
	}
	if hasAny(labels, bugLabels) {
		d.Reasons = append(d.Reasons, "a concrete, reproducible bug report")
	}
	if labels[HelpWantedLabel] {
		d.Reasons = append(d.Reasons, "maintainers asked for help on it")
	}
	if len(headingOrBullet.FindAllStringIndex(body, -1)) >= 2 || criteriaWords.MatchString(body) {
		d.Reasons = append(d.Reasons, "has clear steps or acceptance criteria")
	}
	switch len(pkgs) {
	case 1:
		d.Reasons = append(d.Reasons, "scoped to one package ("+pkgs[0]+")")
	case 2:
		d.Reasons = append(d.Reasons, "scoped to two packages ("+pkgs[0]+", "+pkgs[1]+")")
	}
	if len(body) <= p.MaxBodyChars/2 {
		d.Reasons = append(d.Reasons, "short, focused description")
	}
	d.Score = len(d.Reasons)
	if d.Score < p.MinScore {
		return skip(d, fmt.Sprintf("score-%d-below-%d", d.Score, p.MinScore))
	}
	d.Eligible = true
	return d
}

func skip(d Decision, reason string) Decision {
	d.Eligible = false
	d.Skip = reason
	return d
}

// MentionedPackages returns the distinct source directories (first two path
// segments, with a leading src/ dropped) named in body, sorted.
func MentionedPackages(body string) []string {
	seen := map[string]bool{}
	body = urlPattern.ReplaceAllString(body, " ")
	for _, m := range pathPattern.FindAllString(body, -1) {
		m = strings.Trim(m, ".")
		if !strings.Contains(m, "/") {
			continue
		}
		parts := strings.Split(m, "/")
		if parts[0] == "src" && len(parts) > 1 {
			parts = parts[1:]
		}
		// pkg/file.go → pkg; pkg/paginate.Pages → pkg/paginate.
		last := len(parts) - 1
		if fileExt.MatchString(parts[last]) {
			parts = parts[:last]
		} else if i := strings.Index(parts[last], "."); i > 0 {
			parts[last] = parts[last][:i]
		}
		// A bare top-level file (README.md) is not a package.
		if len(parts) == 0 {
			continue
		}
		n := 2
		if len(parts) < n {
			n = len(parts)
		}
		seen[strings.Join(parts[:n], "/")] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func lowerSet(labels []string) map[string]bool {
	out := make(map[string]bool, len(labels))
	for _, l := range labels {
		out[strings.ToLower(strings.TrimSpace(l))] = true
	}
	return out
}

func hasAny(set map[string]bool, labels []string) bool {
	for _, l := range labels {
		if set[l] {
			return true
		}
	}
	return false
}

// Forge is the slice of the GitHub client the sweep needs.
type Forge interface {
	EnsureIssueLabel(ctx context.Context, repo, name, color, description string) error
	AddLabels(ctx context.Context, repo string, number int, labels []string) error
	RemoveLabel(ctx context.Context, repo string, number int, label string) error
	// UpsertIssueMarkerComment finds the first comment containing marker,
	// edits it to body when it differs, and — only when create is true —
	// creates it when none exists. It reports what it found.
	UpsertIssueMarkerComment(ctx context.Context, repo string, number int, marker, body string, create bool) (MarkerComment, error)
}

// LabelReader is the optional half of Forge the sweep uses to re-read an
// issue's labels immediately before advertising it. Candidates come from
// the last enumeration, which can be many minutes old; hivecommons/hive
// #13267 picked up `blocked` (and lost its claim labels) between the
// enumeration and the sweep, and the stale snapshot was what got labelled.
type LabelReader interface {
	IssueLabels(ctx context.Context, repo string, number int) ([]string, error)
}

// MarkerComment is what UpsertIssueMarkerComment found before it wrote.
type MarkerComment struct {
	// Found is true when a comment containing the marker already existed.
	Found bool
	// Previous is that comment's body before the call ("" when !Found).
	Previous string
	// Created is true when the call posted a new comment.
	Created bool
}

// Links are the hive-specific URLs the comment template renders.
type Links struct {
	// Origin is this dashboard's public origin (https://host), no path.
	Origin string
	// HiveID names the hive in the comment.
	HiveID string
	// DocsBase is the base URL for the hive docs tree; the default template
	// links contributor-relay.md and hive-merge.md under it.
	DocsBase string
}

// DefaultDocsBase is where the hive's own contributor docs live.
const DefaultDocsBase = "https://github.com/hivecommons/hive/blob/v5/src/docs"

// ClaimURL is the deep link into this hive's relay page for one issue:
// <origin>/contribute?issue=<owner/name>%23<n>. The landing page reads the
// parameter and shows the issue the visitor arrived from.
func ClaimURL(origin, repo string, number int) string {
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	if origin == "" {
		return ""
	}
	return origin + "/contribute?issue=" + url.QueryEscape(repo+"#"+strconv.Itoa(number))
}

// Stats are process-lifetime counters.
type Stats struct {
	Selected  int `json:"selected"`
	Released  int `json:"released"`
	Converted int `json:"converted"`
	Labelled  int `json:"labelled"`
	// Withdrawn counts advertisements taken back from issues that turned
	// out to be blocked, held, parked on a person, ops tracking or
	// time-gated while labelled.
	Withdrawn int `json:"withdrawn"`
}

// Sweeper owns the in-process memory a sweep needs: which issues the hive
// labelled (so Release knows which labels it added), when an issue was last
// released (reselect cooldown), and the counters.
type Sweeper struct {
	mu       sync.Mutex
	selected map[string]selection
	released map[string]time.Time
	stats    Stats
}

type selection struct {
	added []string
	at    time.Time
}

// NewSweeper returns an empty Sweeper.
func NewSweeper() *Sweeper {
	return &Sweeper{selected: map[string]selection{}, released: map[string]time.Time{}}
}

// Stats returns a copy of the counters.
func (s *Sweeper) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Selected reports whether the hive labelled key in this process.
func (s *Sweeper) Selected(key string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.selected[key]
	return ok
}

// SweepOptions parameterise one pass.
type SweepOptions struct {
	Policy   Policy
	Links    Links
	Template string
	Now      time.Time
	Logger   *slog.Logger
	// AgentsMD, when set, returns the URL of the repository's AGENTS.md (or
	// "" when it has none); the template points agents at it.
	AgentsMD func(ctx context.Context, repo string) string
	// Audit, when set, receives one event per label applied or released.
	Audit func(Event)
}

// Event is one sweep action for the audit log.
type Event struct {
	Action  string // "select" | "release" | "convert" | "withdraw"
	Repo    string
	Number  int
	Reasons []string
	Reason  string
}

// Result summarises one pass.
type Result struct {
	Scanned  int
	Selected []string
	// Withdrawn lists the labelled issues whose advertisement this pass
	// took back (see Withdrawable).
	Withdrawn []string
	Skipped   map[string]int
	Errors    int
}

// Sweep scores candidates, selects up to MaxPerSweep per repo (and MaxPerRepo
// in total per repo, counting issues already carrying Label), labels them and
// posts the welcome comment. It is idempotent: an issue already labelled by a
// person gets the comment (once) but no second label; one labelled by this
// hive earlier is skipped.
func (s *Sweeper) Sweep(ctx context.Context, forge Forge, candidates []Candidate, opts SweepOptions) Result {
	res := Result{Skipped: map[string]int{}}
	if forge == nil {
		return res
	}
	p := opts.Policy.Normalized()
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	tmpl, err := ParseTemplate(opts.Template)
	if err != nil {
		logger.Warn("gfi: comment template invalid; using the default", "error", err)
		tmpl, _ = ParseTemplate("")
	}

	type scored struct {
		c Candidate
		d Decision
	}
	perRepo := map[string][]scored{}
	labelledPerRepo := map[string]int{}
	for _, c := range candidates {
		res.Scanned++
		if lowerSet(c.Labels)[Label] {
			labelledPerRepo[c.Repo]++
		}
		if s.Selected(c.Key()) {
			res.Skipped["already-selected"]++
			continue
		}
		if until, ok := s.releasedUntil(c.Key(), p.ReselectCooldown); ok && now.Before(until) {
			res.Skipped["reselect-cooldown"]++
			logger.Debug("gfi skip", "issue", c.Key(), "reason", "reselect-cooldown", "until", until)
			continue
		}
		d := Score(c, p, now)
		if !d.Eligible {
			res.Skipped[d.Skip]++
			logger.Debug("gfi skip", "issue", c.Key(), "reason", d.Skip)
			if d.AlreadyLabelled && Withdrawable(d.Skip) {
				if _, released := s.releasedUntil(c.Key(), p.ReselectCooldown); !released {
					if err := s.Withdraw(ctx, forge, c, d.Skip, opts); err != nil {
						res.Errors++
					} else {
						res.Withdrawn = append(res.Withdrawn, c.Key())
					}
				}
			}
			continue
		}
		perRepo[c.Repo] = append(perRepo[c.Repo], scored{c, d})
	}

	repos := make([]string, 0, len(perRepo))
	for r := range perRepo {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		list := perRepo[repo]
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].d.Score != list[j].d.Score {
				return list[i].d.Score > list[j].d.Score
			}
			return list[i].c.CreatedAt.Before(list[j].c.CreatedAt)
		})
		picked := 0
		for _, sc := range list {
			if picked >= p.MaxPerSweep {
				res.Skipped["sweep-cap"]++
				logger.Debug("gfi skip", "issue", sc.c.Key(), "reason", "sweep-cap")
				continue
			}
			if !sc.d.AlreadyLabelled && labelledPerRepo[repo] >= p.MaxPerRepo {
				res.Skipped["repo-cap"]++
				logger.Info("gfi skip", "issue", sc.c.Key(), "reason", "repo-cap", "labelled", labelledPerRepo[repo], "max", p.MaxPerRepo)
				continue
			}
			if stale, reason := s.staleSkip(ctx, forge, &sc.c, opts, p, now); stale {
				res.Skipped["stale:"+reason]++
				logger.Info("gfi skip", "issue", sc.c.Key(), "reason", "stale:"+reason)
				continue
			}
			agentsMD := ""
			if opts.AgentsMD != nil {
				agentsMD = opts.AgentsMD(ctx, repo)
			}
			body, err := RenderComment(tmpl, Vars{
				Candidate: sc.c, Links: opts.Links, Reasons: sc.d.Reasons, AgentsMDURL: agentsMD,
			})
			if err != nil {
				logger.Warn("gfi: rendering comment failed", "issue", sc.c.Key(), "error", err)
				res.Errors++
				continue
			}
			added, err := s.apply(ctx, forge, sc.c, body)
			if err != nil {
				if errors.Is(err, errSuperseded) {
					res.Skipped["previously-released"]++
					logger.Debug("gfi skip", "issue", sc.c.Key(), "reason", "previously-released")
					s.rememberRelease(sc.c.Key(), now)
					continue
				}
				logger.Warn("gfi: labelling failed", "issue", sc.c.Key(), "error", err)
				res.Errors++
				continue
			}
			picked++
			if !sc.d.AlreadyLabelled {
				labelledPerRepo[repo]++
			}
			s.rememberSelection(sc.c.Key(), added, now)
			res.Selected = append(res.Selected, sc.c.Key())
			logger.Info("gfi select", "issue", sc.c.Key(), "score", sc.d.Score, "reasons", strings.Join(sc.d.Reasons, "; "), "labels_added", strings.Join(added, ","), "claim_url", ClaimURL(opts.Links.Origin, sc.c.Repo, sc.c.Number))
			if opts.Audit != nil {
				opts.Audit(Event{Action: "select", Repo: sc.c.Repo, Number: sc.c.Number, Reasons: sc.d.Reasons})
			}
		}
	}
	return res
}

var errSuperseded = errors.New("gfi: issue was released earlier; welcome comment is superseded")

// staleSkip re-reads the issue's labels when the forge can, and re-scores
// with them. It reports true, with the fresh skip reason, when the live
// labels disqualify an issue the enumeration snapshot had admitted. On
// success the candidate's labels are replaced so apply sees the truth. A
// failed read skips with "label-read": advertising on a snapshot the hive
// could not confirm is the failure mode this guard exists to stop.
func (s *Sweeper) staleSkip(ctx context.Context, forge Forge, c *Candidate, opts SweepOptions, p Policy, now time.Time) (bool, string) {
	lr, ok := forge.(LabelReader)
	if !ok {
		return false, ""
	}
	fresh, err := lr.IssueLabels(ctx, c.Repo, c.Number)
	if err != nil {
		return true, "label-read"
	}
	live := *c
	live.Labels = fresh
	if d := Score(live, p, now); !d.Eligible {
		if d.AlreadyLabelled && Withdrawable(d.Skip) {
			if _, released := s.releasedUntil(c.Key(), p.ReselectCooldown); !released {
				_ = s.Withdraw(ctx, forge, live, d.Skip, opts)
			}
		}
		return true, d.Skip
	}
	c.Labels = fresh
	return false, ""
}

// WithdrawnComment is the correction left on an issue whose advertisement
// was withdrawn: the hive explains why the issue is not newcomer work and
// where other issues are. It carries SupersededMarker so the issue is never
// re-advertised.
func WithdrawnComment(c Candidate, reason string, l Links, at time.Time) string {
	hive := strings.TrimSpace(l.HiveID)
	if hive == "" {
		hive = "this hive"
	}
	var why string
	switch {
	case strings.HasPrefix(reason, SkipTracking+":"):
		why = "it is a persistent tracking/archive issue (" + TrackingReasonText(strings.TrimPrefix(reason, SkipTracking+":")) + "), not work"
	case strings.HasPrefix(reason, SkipOpsTracking):
		why = "it is a maintainer's operations checklist, not a code change"
	case reason == SkipTimeGated:
		why = "its checklist cannot be completed until a deadline has passed"
	case strings.HasPrefix(reason, "hard-suppress:"):
		why = "it is waiting on a person (`" + strings.TrimPrefix(reason, "hard-suppress:") + "`)"
	case strings.HasPrefix(reason, "excluded-label:"):
		why = "it carries the `" + strings.TrimPrefix(reason, "excluded-label:") + "` label, so it cannot be worked right now"
	default:
		why = reason
	}
	var b strings.Builder
	b.WriteString(SupersededMarker + "\n")
	fmt.Fprintf(&b, "~~Good first issue~~ — **withdrawn** by %s: this issue is not open to new contributors — %s. The `%s` and `%s` labels have been removed (%s). Sorry for the noise!",
		hive, why, Label, HelpWantedLabel, at.UTC().Format(time.RFC3339))
	if origin := strings.TrimRight(strings.TrimSpace(l.Origin), "/"); origin != "" {
		fmt.Fprintf(&b, " Issues open to new contributors are listed on the relay page: %s/contribute", origin)
	}
	b.WriteString("\n")
	return b.String()
}

// Withdraw takes the advertisement back from an issue that is not newcomer
// work (see Withdrawable): both labels come off whoever applied them, and
// the welcome comment is edited in place to a correction — or, when the
// labels arrived without a welcome comment, one short correction is
// posted. Idempotent: an issue already carrying the superseded note gets no
// second comment, and the release is remembered so the reselect cooldown
// keeps the sweep from re-advertising it.
func (s *Sweeper) Withdraw(ctx context.Context, forge Forge, c Candidate, reason string, opts SweepOptions) error {
	if forge == nil {
		return nil
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	have := lowerSet(c.Labels)
	var removed []string
	var firstErr error
	for _, l := range []string{Label, HelpWantedLabel} {
		if !have[l] {
			continue
		}
		if err := forge.RemoveLabel(ctx, c.Repo, c.Number, l); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("remove label %q: %w", l, err)
			}
			continue
		}
		removed = append(removed, l)
	}
	peek, err := forge.UpsertIssueMarkerComment(ctx, c.Repo, c.Number, SupersededMarker, "", false)
	if err != nil && firstErr == nil {
		firstErr = fmt.Errorf("withdraw comment lookup: %w", err)
	}
	if err == nil && !peek.Found {
		body := WithdrawnComment(c, reason, opts.Links, now)
		if _, err := forge.UpsertIssueMarkerComment(ctx, c.Repo, c.Number, Marker, body, true); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("withdraw comment: %w", err)
		}
	}
	s.mu.Lock()
	delete(s.selected, c.Key())
	s.released[c.Key()] = now
	s.stats.Withdrawn++
	s.mu.Unlock()
	logger.Info("gfi withdraw", "issue", c.Key(), "reason", reason, "labels_removed", strings.Join(removed, ","), "error", firstErr)
	if opts.Audit != nil {
		opts.Audit(Event{Action: "withdraw", Repo: c.Repo, Number: c.Number, Reason: reason})
	}
	return firstErr
}

// apply ensures the labels and the comment on one issue. It returns the
// labels it added (those the issue lacked).
func (s *Sweeper) apply(ctx context.Context, forge Forge, c Candidate, body string) ([]string, error) {
	// A superseded marker means the hive released this issue in an earlier
	// process; it must not be re-advertised. The welcome comment is written
	// first so the labels never appear without it.
	peek, err := forge.UpsertIssueMarkerComment(ctx, c.Repo, c.Number, SupersededMarker, "", false)
	if err != nil {
		return nil, fmt.Errorf("welcome comment: %w", err)
	}
	if peek.Found {
		return nil, errSuperseded
	}
	if _, err := forge.UpsertIssueMarkerComment(ctx, c.Repo, c.Number, Marker, body, true); err != nil {
		return nil, fmt.Errorf("welcome comment: %w", err)
	}
	have := lowerSet(c.Labels)
	var want []string
	if !have[Label] {
		if err := forge.EnsureIssueLabel(ctx, c.Repo, Label, LabelColor, LabelDescription); err != nil {
			return nil, fmt.Errorf("ensure label %q: %w", Label, err)
		}
		want = append(want, Label)
	}
	if !have[HelpWantedLabel] {
		if err := forge.EnsureIssueLabel(ctx, c.Repo, HelpWantedLabel, HelpWantedColor, HelpWantedDescription); err != nil {
			return nil, fmt.Errorf("ensure label %q: %w", HelpWantedLabel, err)
		}
		want = append(want, HelpWantedLabel)
	}
	if len(want) > 0 {
		if err := forge.AddLabels(ctx, c.Repo, c.Number, want); err != nil {
			return nil, fmt.Errorf("add labels: %w", err)
		}
	}
	return want, nil
}

func (s *Sweeper) rememberSelection(key string, added []string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.selected[key] = selection{added: append([]string(nil), added...), at: at}
	s.stats.Selected++
	s.stats.Labelled += len(added)
}

func (s *Sweeper) rememberRelease(key string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.selected, key)
	s.released[key] = at
}

func (s *Sweeper) releasedUntil(key string, cooldown time.Duration) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.released[key]
	if !ok {
		return time.Time{}, false
	}
	return at.Add(cooldown), true
}

// Release withdraws the hive's advertisement because the hive is now working
// the issue itself: the `good first issue` label comes off (and `help wanted`
// when this process added it), and the welcome comment is edited in place to
// a short superseded note. Idempotent; safe on an issue that was never
// labelled (the label removal is a no-op and no comment is created unless a
// marker comment already exists).
func (s *Sweeper) Release(ctx context.Context, forge Forge, c Candidate, reason string, opts SweepOptions) error {
	if forge == nil {
		return nil
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	sel, known := s.selected[c.Key()]
	s.mu.Unlock()

	labels := []string{Label}
	if known {
		for _, l := range sel.added {
			if !strings.EqualFold(l, Label) {
				labels = append(labels, l)
			}
		}
	}
	var firstErr error
	for _, l := range labels {
		if err := forge.RemoveLabel(ctx, c.Repo, c.Number, l); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("remove label %q: %w", l, err)
		}
	}
	body := SupersededComment(c, reason, opts.Links, now)
	if _, err := forge.UpsertIssueMarkerComment(ctx, c.Repo, c.Number, Marker, body, false); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("supersede comment: %w", err)
	}
	s.mu.Lock()
	delete(s.selected, c.Key())
	s.released[c.Key()] = now
	s.stats.Released++
	s.mu.Unlock()
	logger.Info("gfi release", "issue", c.Key(), "reason", reason, "labels_removed", strings.Join(labels, ","), "error", firstErr)
	if opts.Audit != nil {
		opts.Audit(Event{Action: "release", Repo: c.Repo, Number: c.Number, Reason: reason})
	}
	return firstErr
}

// Convert records that an outside contributor claimed a labelled issue —
// the outcome the label exists for. Logs and counts; the label stays until
// the issue closes, as it would on any repository.
func (s *Sweeper) Convert(c Candidate, contributor, kind string, opts SweepOptions) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s.mu.Lock()
	s.stats.Converted++
	s.mu.Unlock()
	logger.Info("gfi converted", "issue", c.Key(), "contributor", contributor, "kind", kind)
	if opts.Audit != nil {
		opts.Audit(Event{Action: "convert", Repo: c.Repo, Number: c.Number, Reason: kind + ":" + contributor})
	}
}

// HasLabel reports whether labels carries Label (case-insensitive).
func HasLabel(labels []string) bool { return lowerSet(labels)[Label] }
