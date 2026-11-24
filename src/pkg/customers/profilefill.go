package customers

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// The profile fill (hivecommons/hive#19302): a bounded pass that reads each
// confirmed customer's own public profile on the hive's forge (GitHub, GitHub
// Enterprise, GitLab or Gitea/Forgejo) and, when a Linear workspace is
// connected, their workspace member name, and fills the profile fields nobody
// set by hand. The network calls are the caller's ProfileLookup sources; this
// file decides who is asked, when, within what budget, and what an answer may
// change (Store.ApplyProfileFill).
//
// Rules, each pinned by profilefill_test.go:
//   - Off unless Enabled says on: off makes no request at all.
//   - Only confirmed customers (origin hive_user, manual or accepted), never a
//     suggestion. A forge is asked only about a login, Linear only about an
//     email address the person already has as a confirmed identity.
//   - A manual or hub field is never overwritten; see ApplyProfileFill.
//   - Every request comes out of one rolling hourly budget; a source that
//     says its shared quota is low is skipped; a rate-limit refusal stops the
//     pass and backs off, doubling up to ProfileFillBackoffMax.
//   - Nothing here logs a login, an email or a value: the result holds counts.

// Pacing. Hard limits; the hourly budget comes from configuration.
const (
	// ProfileFillInterval is how often the periodic pass runs.
	ProfileFillInterval = 15 * time.Minute
	// ProfileFillRefreshAge is how old a successful lookup gets before the
	// periodic pass refreshes it.
	ProfileFillRefreshAge = 7 * 24 * time.Hour
	// ProfileFillFailureRetry is how soon a failed lookup (a forge that could
	// not be reached, a server error) is tried again.
	ProfileFillFailureRetry = 24 * time.Hour
	// ProfileFillMaxPerPass bounds the people one pass looks up, so the
	// backfill of a large hive is spread across passes.
	ProfileFillMaxPerPass = 20
	// ProfileFillMaxRequestsPerLookup is what one source lookup may spend:
	// GitLab needs two requests, GitHub one plus an organization name.
	ProfileFillMaxRequestsPerLookup = 2
	// ProfileFillWindow is the rolling window of the hourly budget.
	ProfileFillWindow = time.Hour
	// ProfileFillBackoffBase is the first wait after a rate-limit refusal.
	ProfileFillBackoffBase = 15 * time.Minute
	// ProfileFillBackoffMax caps the doubling backoff.
	ProfileFillBackoffMax = 6 * time.Hour
)

// Skip reasons of a profile-fill pass. Fixed words, safe to log.
const (
	FillSkipDisabled  = "disabled"
	FillSkipNoSources = "no_sources"
	FillSkipBusy      = "pass_running"
	FillSkipBackoff   = "backoff"
	FillSkipBudget    = "hourly_budget_spent"
	FillSkipNotFound  = "no_such_customer"
)

var (
	// ErrProfileNotFound is what a source returns when the forge has no such
	// user or the workspace no such member.
	ErrProfileNotFound = errors.New("customers: no public profile")
	// ErrProfileRateLimited is what a source returns when the forge refused
	// for a rate-limit reason.
	ErrProfileRateLimited = errors.New("customers: profile source rate limited")
)

// ForgeProfile is the public profile a source read: only what the person
// published themselves. Website and AvatarURL are read but not stored.
type ForgeProfile struct {
	Login     string
	Name      string
	Company   string
	Location  string
	Website   string
	AvatarURL string
	// Requests is how many requests the lookup made; 0 means the full
	// ProfileFillMaxRequestsPerLookup.
	Requests int
}

// ProfileLookup is one profile source.
type ProfileLookup interface {
	// Source is the ProfileSource* rule its values carry.
	Source() string
	// Kind is the identity it looks up: KindGitHub (a forge login; for a
	// non-GitHub forge the hive's login identity holds that forge's
	// username) or KindEmail (Linear).
	Kind() IdentityKind
	// StandDown returns a non-empty fixed reason when its shared quota says
	// optional work must wait.
	StandDown() string
	// Lookup makes at most ProfileFillMaxRequestsPerLookup requests.
	Lookup(ctx context.Context, key string) (ForgeProfile, error)
}

// ProfileFillResult counts what one pass did: counts only, safe to log.
type ProfileFillResult struct {
	Skipped  string `json:"skipped,omitempty"`
	Due      int    `json:"due"`
	People   int    `json:"people"`
	Requests int    `json:"requests"`
	Filled   int    `json:"filled"`
	NotFound int    `json:"not_found"`
	Failed   int    `json:"failed"`
	// StoodDown counts lookups skipped because a source's quota was low.
	StoodDown int `json:"stood_down"`
	// Truncated is true when the pass stopped with people still due: the
	// per-pass cap, the hourly budget or a rate-limit backoff.
	Truncated bool `json:"truncated"`
}

// ProfileFill is the profile-fill engine. Build one with NewProfileFill.
type ProfileFill struct {
	Store *Store
	// Sources returns the lookups for the current configuration, forge
	// first, in priority order: a field takes the first non-empty value.
	Sources func() []ProfileLookup
	// Enabled and HourlyLimit are read on every pass. A nil Enabled is off.
	Enabled     func() bool
	HourlyLimit func() int

	now func() time.Time

	pass sync.Mutex // one pass at a time

	mu           sync.Mutex
	spent        []time.Time
	backoffUntil time.Time
	backoffStep  time.Duration
}

// NewProfileFill builds an engine over store.
func NewProfileFill(store *Store) *ProfileFill {
	return &ProfileFill{Store: store, now: time.Now}
}

// SetClock replaces the clock, for tests.
func (f *ProfileFill) SetClock(now func() time.Time) { f.now = now }

// reserve takes want requests from the rolling hourly budget, all or none.
func (f *ProfileFill) reserve(want, limit int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	kept := f.spent[:0]
	for _, t := range f.spent {
		if now.Sub(t) < ProfileFillWindow {
			kept = append(kept, t)
		}
	}
	f.spent = kept
	if limit-len(f.spent) < want {
		return false
	}
	for i := 0; i < want; i++ {
		f.spent = append(f.spent, now)
	}
	return true
}

func (f *ProfileFill) refund(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ; n > 0 && len(f.spent) > 0; n-- {
		f.spent = f.spent[:len(f.spent)-1]
	}
}

// backingOff reports whether a rate-limit backoff is in force.
func (f *ProfileFill) backingOff() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now().Before(f.backoffUntil)
}

// rateLimited starts or doubles the backoff.
func (f *ProfileFill) rateLimited() {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.backoffStep == 0:
		f.backoffStep = ProfileFillBackoffBase
	case f.backoffStep < ProfileFillBackoffMax:
		f.backoffStep *= 2
	}
	if f.backoffStep > ProfileFillBackoffMax {
		f.backoffStep = ProfileFillBackoffMax
	}
	f.backoffUntil = f.now().Add(f.backoffStep)
}

// answered resets the backoff after a lookup that went through.
func (f *ProfileFill) answered() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.backoffStep = 0
	f.backoffUntil = time.Time{}
}

// BackoffUntil is when the current rate-limit backoff ends (zero when none).
func (f *ProfileFill) BackoffUntil() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.backoffUntil
}

// profileLookupKey is the identity value a source of kind asks about, or "".
// A forge is asked about the person's first confirmed login identity, else a
// dashboard user name shaped like a login ("github:" prefix dropped). Linear
// is asked about the first confirmed email address.
func profileLookupKey(p Person, kind IdentityKind) string {
	for _, id := range p.Identities {
		if id.Claimed || id.Kind != kind {
			continue
		}
		v := strings.TrimSpace(id.Value)
		switch kind {
		case KindGitHub:
			v = strings.TrimPrefix(v, "@")
			if forgeLoginShaped(v) && !IsBotLogin(v) {
				return v
			}
		case KindEmail:
			if strings.Contains(v, "@") {
				return v
			}
		}
	}
	if kind != KindGitHub {
		return ""
	}
	for _, id := range p.Identities {
		if id.Claimed || id.Kind != KindHiveUser {
			continue
		}
		v := NormalizeIdentity(KindHiveUser, id.Value)
		if LooksLikeGitHubLogin(v) && !IsBotLogin(v) {
			return v
		}
	}
	return ""
}

// forgeLoginMax bounds a forge username sent in a lookup.
const forgeLoginMax = 255

// forgeLoginShaped reports whether v can be a forge username: GitHub logins
// plus the dots and underscores GitLab and Gitea allow. Anything else (an
// address, a path, whitespace) is never sent.
func forgeLoginShaped(v string) bool {
	if v == "" || len(v) > forgeLoginMax || strings.HasPrefix(v, ".") {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// profileFillDue reports whether the periodic pass should look p up now.
func profileFillDue(p Person, now time.Time) bool {
	fe := p.Profile.Fetched
	if fe == nil {
		return true
	}
	age := now.Sub(fe.At)
	if fe.Outcome == FetchFailed {
		return age >= ProfileFillFailureRetry
	}
	return age >= ProfileFillRefreshAge
}

// fillCandidate is one customer with the keys each source would ask about.
type fillCandidate struct {
	person Person
	keys   []string // parallel to sources; "" = not asked
}

// candidates are the customers the pass may look up, never-fetched first,
// then the stalest. only, when set, names the one person to look up
// regardless of age.
func (f *ProfileFill) candidates(sources []ProfileLookup, only string) []fillCandidate {
	now := f.now()
	var out []fillCandidate
	for _, p := range f.Store.People() {
		if p.Status != StatusCustomer || (only != "" && p.ID != only) {
			continue
		}
		if only == "" && !profileFillDue(p, now) {
			continue
		}
		c := fillCandidate{person: p, keys: make([]string, len(sources))}
		asked := false
		for i, src := range sources {
			if c.keys[i] = profileLookupKey(p, src.Kind()); c.keys[i] != "" {
				asked = true
			}
		}
		if asked {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].person.Profile.Fetched, out[j].person.Profile.Fetched
		switch {
		case a == nil && b == nil:
			return out[i].person.ID < out[j].person.ID
		case a == nil:
			return true
		case b == nil:
			return false
		}
		return a.At.Before(b.At)
	})
	return out
}

// Run is one pass. only names one customer for "Refresh from profiles" or an
// accept; "" is the periodic pass over everyone due. It never returns a
// login, an email or a value.
func (f *ProfileFill) Run(ctx context.Context, only string) ProfileFillResult {
	var res ProfileFillResult
	switch {
	case f == nil || f.Store == nil:
		res.Skipped = FillSkipNoSources
		return res
	case f.Enabled == nil || !f.Enabled():
		res.Skipped = FillSkipDisabled
		return res
	}
	var sources []ProfileLookup
	if f.Sources != nil {
		sources = f.Sources()
	}
	if len(sources) == 0 {
		res.Skipped = FillSkipNoSources
		return res
	}
	if !f.pass.TryLock() {
		res.Skipped = FillSkipBusy
		return res
	}
	defer f.pass.Unlock()
	cands := f.candidates(sources, only)
	res.Due = len(cands)
	if only != "" && len(cands) == 0 {
		if _, err := f.Store.Get(only); err != nil {
			res.Skipped = FillSkipNotFound
		}
		return res
	}
	if f.backingOff() {
		res.Skipped = FillSkipBackoff
		res.Truncated = res.Due > 0
		return res
	}
	limit := 0
	if f.HourlyLimit != nil {
		limit = f.HourlyLimit()
	}
	for n, c := range cands {
		if n >= ProfileFillMaxPerPass || ctx.Err() != nil {
			res.Truncated = true
			break
		}
		stop := f.fillOne(ctx, sources, c, limit, &res)
		res.People++
		if stop != "" {
			res.Skipped = stop
			res.Truncated = true
			break
		}
	}
	return res
}

// fillOne looks one person up in every source that has a key for them and
// applies the answers. It returns a skip reason when the pass must stop.
func (f *ProfileFill) fillOne(ctx context.Context, sources []ProfileLookup, c fillCandidate, limit int, res *ProfileFillResult) string {
	values := map[string]ProfileFillValue{}
	var answeredBy []string
	failed, notFound := false, false
	put := func(field, value, source, login string) {
		if _, ok := values[field]; ok || strings.TrimSpace(value) == "" {
			return
		}
		values[field] = ProfileFillValue{Value: value, Source: source, Login: login}
	}
	for i, src := range sources {
		key := c.keys[i]
		if key == "" {
			continue
		}
		if reason := src.StandDown(); reason != "" {
			res.StoodDown++
			continue
		}
		if !f.reserve(ProfileFillMaxRequestsPerLookup, limit) {
			return FillSkipBudget
		}
		prof, err := src.Lookup(ctx, key)
		used := prof.Requests
		if used <= 0 || used > ProfileFillMaxRequestsPerLookup {
			used = ProfileFillMaxRequestsPerLookup
		}
		f.refund(ProfileFillMaxRequestsPerLookup - used)
		res.Requests += used
		switch {
		case errors.Is(err, ErrProfileRateLimited):
			f.rateLimited()
			return FillSkipBackoff
		case errors.Is(err, ErrProfileNotFound):
			notFound = true
			f.answered()
			continue
		case err != nil:
			failed = true
			continue
		}
		f.answered()
		answeredBy = append(answeredBy, src.Source())
		login := ""
		if src.Kind() == KindGitHub {
			login = strings.TrimPrefix(strings.TrimSpace(key), "@")
		}
		put(FieldRealName, prof.Name, src.Source(), login)
		put(FieldOrganization, NormalizeCompany(prof.Company), src.Source(), login)
		put(FieldLocation, prof.Location, src.Source(), login)
	}
	fetch := ProfileFetch{At: f.now().UTC(), Sources: answeredBy}
	switch {
	case len(answeredBy) > 0:
		fetch.Outcome = FetchUnchanged
	case failed:
		fetch.Outcome = FetchFailed
		res.Failed++
	case notFound:
		fetch.Outcome = FetchNotFound
		res.NotFound++
	default:
		// Every source stood down: nothing was asked, nothing is recorded,
		// so the person stays first in line.
		return ""
	}
	_, changed, err := f.Store.ApplyProfileFill(c.person.ID, values, fetch)
	if err != nil {
		res.Failed++
		return ""
	}
	if changed {
		res.Filled++
	}
	return ""
}

// RefreshAll forgets every customer's last lookup so the following passes
// look everyone up again, oldest first within the hourly budget. It returns
// how many records it cleared.
func (f *ProfileFill) RefreshAll() (int, error) {
	if f == nil || f.Store == nil {
		return 0, ErrNotFound
	}
	return f.Store.ClearProfileFetched()
}
