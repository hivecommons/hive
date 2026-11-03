// Package firsttimer welcomes first-time contributors. When a PERSON opens
// their first pull request to a repository this hive governs — GitHub's
// author_association is FIRST_TIME_CONTRIBUTOR or FIRST_TIMER — the hive
// leaves ONE comment that thanks them and explains they can run their own
// hive (a "clanker") and let it do the contribution loop for them, listing
// what that earns: a contributor profile, a leaderboard place, trust-tier
// progression that unlocks auto-merge, and review fixes / rebases handled
// while they are offline.
//
// The comment is advertising, not a gate: it changes nothing about how the
// PR is reviewed or merged. It is posted at most once per author per
// repository, found again by its marker (Marker), so a restart never posts a
// second copy on the same PR and a second PR by the same author gets nothing.
//
// Like pkg/gfi — whose good-first-issue welcome this mirrors — the package is
// stdlib-only: the decision, the template and the sweep take plain
// Candidates and a small Forge interface so the logic is testable without a
// GitHub client.
package firsttimer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Marker opens every comment this package writes. The sweep searches for
	// it before posting, so a comment template must contain it.
	Marker = "<!-- hive:first-timer-welcome -->"

	// AssociationFirstTimeContributor is GitHub's author_association for an
	// author with no merged contribution to this repository yet.
	AssociationFirstTimeContributor = "FIRST_TIME_CONTRIBUTOR"
	// AssociationFirstTimer is GitHub's author_association for an author
	// whose PR is their first contribution to GitHub at all.
	AssociationFirstTimer = "FIRST_TIMER"

	// MaxAuthorPRsChecked bounds how many of the author's other PRs the
	// sweep peeks at for an earlier welcome: a first-timer has few, and an
	// author with more than this many PRs was welcomed long ago.
	MaxAuthorPRsChecked = 10
)

// Skip reasons reported by Decide and counted by Sweep.
const (
	SkipNoAuthor              = "no-author"
	SkipNonHuman              = "non-human-author"
	SkipHiveAttributed        = "hive-attributed"
	SkipClankerLabel          = "clanker-author"
	SkipRegisteredContributor = "registered-contributor"
	SkipDraft                 = "draft"
	SkipNotFirstTimer         = "not-first-timer"
	SkipAlreadyWelcomed       = "already-welcomed"
	SkipRepoNotAllowed        = "repo-not-allowed"
)

// clankerLabels are the author-kind labels (src/pkg/authorkind,
// docs/labels-and-control-signals.md) that say a hive — this one or another
// contributor's — authored the PR. Such an author already runs a hive.
var clankerLabels = []string{"author/clanker", "author/hive"}

// Candidate is the PR-shaped input to the decision. The caller (cmd/hive)
// projects it from the hive's enumeration envelope so this package does not
// depend on the GitHub client types.
type Candidate struct {
	// Repo is owner/name. Number is the PR number.
	Repo   string
	Number int
	Title  string
	URL    string
	// Author is the PR author's GitHub login.
	Author string
	// AuthorAssociation is GitHub's author_association for the PR.
	AuthorAssociation string
	// AuthorNonHuman is true when the author is a Bot account, the hive's
	// own App login or one of its identities.
	AuthorNonHuman bool
	// HiveAttributed is true when the PR body carries the `— hive:`
	// attribution trailer: a hive authored it on a person's credentials, so
	// that person already runs one.
	HiveAttributed bool
	Labels         []string
	Draft          bool
	CreatedAt      time.Time
}

// Key is the per-PR identity.
func (c Candidate) Key() string { return c.Repo + "#" + strconv.Itoa(c.Number) }

// AuthorKey is the per-author-per-repo identity the once-only rule uses.
func (c Candidate) AuthorKey() string { return AuthorKey(c.Repo, c.Author) }

// AuthorKey builds the per-author-per-repo identity.
func AuthorKey(repo, author string) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "@" + strings.ToLower(strings.TrimSpace(author))
}

// PRURL returns URL or the github.com form.
func (c Candidate) PRURL() string {
	if strings.TrimSpace(c.URL) != "" {
		return c.URL
	}
	return "https://github.com/" + c.Repo + "/pull/" + strconv.Itoa(c.Number)
}

// IsFirstTimerAssociation reports whether assoc is one GitHub uses for an
// author with no merged contribution to the repository.
func IsFirstTimerAssociation(assoc string) bool {
	switch strings.ToUpper(strings.TrimSpace(assoc)) {
	case AssociationFirstTimeContributor, AssociationFirstTimer:
		return true
	}
	return false
}

// Decision is the outcome of Decide.
type Decision struct {
	// Welcome is true when the PR deserves the comment (before the
	// once-per-author check the sweep does against the forge).
	Welcome bool
	// Skip names why not, "" when Welcome.
	Skip string
}

// Decide applies the author gates. known, when non-nil, reports whether the
// login is already a registered or relay contributor of this hive — someone
// who already runs a hive and needs no invitation.
func Decide(c Candidate, known func(login string) bool) Decision {
	author := strings.TrimSpace(c.Author)
	switch {
	case author == "":
		return Decision{Skip: SkipNoAuthor}
	case c.AuthorNonHuman || strings.HasSuffix(strings.ToLower(author), "[bot]"):
		return Decision{Skip: SkipNonHuman}
	case c.HiveAttributed:
		return Decision{Skip: SkipHiveAttributed}
	case hasAnyLabel(c.Labels, clankerLabels):
		return Decision{Skip: SkipClankerLabel}
	case c.Draft:
		return Decision{Skip: SkipDraft}
	case known != nil && known(author):
		return Decision{Skip: SkipRegisteredContributor}
	case !IsFirstTimerAssociation(c.AuthorAssociation):
		return Decision{Skip: SkipNotFirstTimer}
	}
	return Decision{Welcome: true}
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

// Forge is the slice of the GitHub client the sweep needs.
type Forge interface {
	// UpsertIssueMarkerComment finds the first comment containing marker,
	// edits it to body when it differs, and — only when create is true —
	// creates it when none exists. With create false and an empty body it
	// is a pure lookup.
	UpsertIssueMarkerComment(ctx context.Context, repo string, number int, marker, body string, create bool) (MarkerComment, error)
	// ListAuthorPullRequests returns the numbers of pull requests author
	// opened in repo (any state), at most limit.
	ListAuthorPullRequests(ctx context.Context, repo, author string, limit int) ([]int, error)
}

// MarkerComment is what UpsertIssueMarkerComment found before it wrote. It
// has the same shape as gfi.MarkerComment so one client method can serve
// both packages.
type MarkerComment struct {
	Found    bool
	Previous string
	Created  bool
}

// Links are the hive-specific URLs the comment template renders.
type Links struct {
	// Origin is this dashboard's public origin (https://host), no path.
	Origin string
	// HiveID names the hive in the comment.
	HiveID string
	// DocsBase is the base URL for the hive docs tree.
	DocsBase string
}

// Stats are process-lifetime counters.
type Stats struct {
	Welcomed int `json:"welcomed"`
	Skipped  int `json:"skipped"`
	Errors   int `json:"errors"`
}

// Sweeper owns the in-process memory: which author@repo pairs were welcomed
// (or found welcomed) so a later pass costs no GitHub call for them.
type Sweeper struct {
	mu       sync.Mutex
	welcomed map[string]time.Time
	stats    Stats
}

// NewSweeper returns an empty Sweeper.
func NewSweeper() *Sweeper {
	return &Sweeper{welcomed: map[string]time.Time{}}
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

// Welcomed reports whether this process welcomed (or found welcomed) the
// author in the repository.
func (s *Sweeper) Welcomed(repo, author string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.welcomed[AuthorKey(repo, author)]
	return ok
}

func (s *Sweeper) remember(key string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.welcomed[key] = at
}

func (s *Sweeper) count(f func(*Stats)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.stats)
}

// SweepOptions parameterise one pass.
type SweepOptions struct {
	Links    Links
	Template string
	Now      time.Time
	Logger   *slog.Logger
	// KnownContributor, when set, reports whether a login already runs a
	// hive against this one (relay claim or registry profile).
	KnownContributor func(login string) bool
	// RepoAllowed, when set, restricts the sweep to some repositories.
	RepoAllowed func(repo string) bool
	// Audit, when set, receives one event per comment posted.
	Audit func(Event)
}

// Event is one sweep action for the audit log.
type Event struct {
	Action string // "welcome"
	Repo   string
	Number int
	Author string
}

// Result summarises one pass.
type Result struct {
	Scanned  int
	Welcomed []string
	Skipped  map[string]int
	Errors   int
}

// Sweep welcomes every candidate that passes Decide and has not been welcomed
// before: not on this PR (marker), not in this process (memory) and not on
// another PR of theirs in the same repository (forge lookup).
func (s *Sweeper) Sweep(ctx context.Context, forge Forge, candidates []Candidate, opts SweepOptions) Result {
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
	tmpl, err := ParseTemplate(opts.Template)
	if err != nil {
		logger.Warn("first-timer welcome: comment template invalid; using the default", "error", err)
		tmpl, _ = ParseTemplate("")
	}

	// Oldest PR first so an author with two open first PRs is welcomed on
	// the one they opened first.
	sorted := append([]Candidate(nil), candidates...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
		}
		return sorted[i].Key() < sorted[j].Key()
	})

	skip := func(c Candidate, reason string) {
		res.Skipped[reason]++
		s.count(func(st *Stats) { st.Skipped++ })
		logger.Debug("first-timer welcome skip", "pr", c.Key(), "author", c.Author, "reason", reason)
	}
	fail := func(c Candidate, what string, err error) {
		res.Errors++
		s.count(func(st *Stats) { st.Errors++ })
		logger.Warn("first-timer welcome: "+what+" failed", "pr", c.Key(), "error", err)
	}
	for _, c := range sorted {
		res.Scanned++
		if opts.RepoAllowed != nil && !opts.RepoAllowed(c.Repo) {
			skip(c, SkipRepoNotAllowed)
			continue
		}
		d := Decide(c, opts.KnownContributor)
		if !d.Welcome {
			skip(c, d.Skip)
			continue
		}
		if s.Welcomed(c.Repo, c.Author) {
			skip(c, SkipAlreadyWelcomed)
			continue
		}
		welcomed, err := s.alreadyWelcomed(ctx, forge, c)
		if err != nil {
			fail(c, "lookup", err)
			continue
		}
		if welcomed {
			s.remember(c.AuthorKey(), now)
			skip(c, SkipAlreadyWelcomed)
			continue
		}
		body, err := RenderComment(tmpl, Vars{Candidate: c, Links: opts.Links})
		if err != nil {
			fail(c, "rendering", err)
			continue
		}
		if _, err := forge.UpsertIssueMarkerComment(ctx, c.Repo, c.Number, Marker, body, true); err != nil {
			fail(c, "comment", err)
			continue
		}
		s.remember(c.AuthorKey(), now)
		s.count(func(st *Stats) { st.Welcomed++ })
		res.Welcomed = append(res.Welcomed, c.Key())
		logger.Info("first-timer welcome", "pr", c.Key(), "author", c.Author, "association", c.AuthorAssociation)
		if opts.Audit != nil {
			opts.Audit(Event{Action: "welcome", Repo: c.Repo, Number: c.Number, Author: c.Author})
		}
	}
	return res
}

var errNilForge = errors.New("firsttimer: nil forge")

// alreadyWelcomed reports whether the marker exists on this PR or on another
// PR by the same author in the repository.
func (s *Sweeper) alreadyWelcomed(ctx context.Context, forge Forge, c Candidate) (bool, error) {
	if forge == nil {
		return false, errNilForge
	}
	peek, err := forge.UpsertIssueMarkerComment(ctx, c.Repo, c.Number, Marker, "", false)
	if err != nil {
		return false, fmt.Errorf("marker lookup on %s: %w", c.Key(), err)
	}
	if peek.Found {
		return true, nil
	}
	others, err := forge.ListAuthorPullRequests(ctx, c.Repo, c.Author, MaxAuthorPRsChecked)
	if err != nil {
		return false, fmt.Errorf("listing %s's pull requests in %s: %w", c.Author, c.Repo, err)
	}
	for _, n := range others {
		if n == c.Number || n <= 0 {
			continue
		}
		peek, err := forge.UpsertIssueMarkerComment(ctx, c.Repo, n, Marker, "", false)
		if err != nil {
			return false, fmt.Errorf("marker lookup on %s#%d: %w", c.Repo, n, err)
		}
		if peek.Found {
			return true, nil
		}
	}
	return false, nil
}
