package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/customers"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/linearagent"
)

// The Customers profile fill (#19302). pkg/customers decides who is asked,
// when, within what budget, and what an answer may change; this file adapts
// the hive's existing forge clients to customers.ProfileLookup and runs the
// passes. Off unless customers.profile_fill.enabled.
//
// The lookups reuse the clients every other forge call uses: the shared
// GitHub client (its transport, proxy, pacer and rate-limit accounting) for
// github.com and GitHub Enterprise, the pkg/forge GitLab and Gitea adapters,
// and the Linear connector's GraphQL helper with the connected workspace's
// token. They only read; the website and avatar URLs a profile names are never
// fetched.

const (
	// customersProfileFillFirstDelay is the wait after boot before the first
	// pass, which is the backfill on a hive that has never filled profiles.
	customersProfileFillFirstDelay = 2 * time.Minute
	// customersProfileFillPassTimeout bounds one periodic pass.
	customersProfileFillPassTimeout = 2 * time.Minute
	// customersProfileFillOneTimeout bounds a one-person refresh.
	customersProfileFillOneTimeout = 30 * time.Second
	// customersOrgNameTTL is how long a GitHub organization display name is
	// kept before it is asked for again.
	customersOrgNameTTL = 24 * time.Hour
	// customersOrgNameCacheMax bounds the organization name cache.
	customersOrgNameCacheMax = 512
)

// customersDashboard answers "Refresh from profiles".
var _ dashboard.CustomersProfileRefresher = (*customersDashboard)(nil)

// customersOrgNames caches GitHub organization display names by lower-case
// handle, so "@acme" on many profiles costs one request a day.
type customersOrgNames struct {
	mu      sync.Mutex
	entries map[string]customersOrgName
	now     func() time.Time
}

type customersOrgName struct {
	name string // "" = not an organization, or no display name
	at   time.Time
}

func newCustomersOrgNames() *customersOrgNames {
	return &customersOrgNames{entries: map[string]customersOrgName{}, now: time.Now}
}

func (c *customersOrgNames) get(handle string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[strings.ToLower(handle)]
	if !ok || c.now().Sub(e.at) >= customersOrgNameTTL {
		return "", false
	}
	return e.name, true
}

func (c *customersOrgNames) put(handle, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= customersOrgNameCacheMax {
		// Drop the oldest entry; the cache is small.
		oldest, at := "", time.Time{}
		for k, e := range c.entries {
			if oldest == "" || e.at.Before(at) {
				oldest, at = k, e.at
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[strings.ToLower(handle)] = customersOrgName{name: name, at: c.now()}
}

// customersOrgHandle returns the organization handle of a company field that
// is exactly "@handle", or "".
func customersOrgHandle(company string) string {
	c := strings.TrimSpace(company)
	if !strings.HasPrefix(c, "@") || strings.ContainsAny(c, " \t,;/") {
		return ""
	}
	h := strings.TrimPrefix(c, "@")
	if !customers.LooksLikeGitHubLogin(h) {
		return ""
	}
	return h
}

// githubProfileLookup reads public profiles through the shared GitHub client:
// github.com, or the GitHub Enterprise server the hive is configured for.
type githubProfileLookup struct {
	client func() *github.Client
	source string
	orgs   *customersOrgNames
}

func (g githubProfileLookup) Source() string               { return g.source }
func (g githubProfileLookup) Kind() customers.IdentityKind { return customers.KindGitHub }

// StandDown is the shared GitHub quota gate: below-normal API budget, the
// secondary-limit caution window, or a low core bucket.
func (g githubProfileLookup) StandDown() string { return g.client().OptionalRESTStandDown() }

func (g githubProfileLookup) Lookup(ctx context.Context, login string) (customers.ForgeProfile, error) {
	c := g.client()
	if c == nil {
		return customers.ForgeProfile{}, github.ErrNoGitHubClient
	}
	p, err := c.GetPublicProfile(ctx, login)
	switch {
	case errors.Is(err, github.ErrMentionSearchRateLimited):
		return customers.ForgeProfile{Requests: 1}, customers.ErrProfileRateLimited
	case errors.Is(err, github.ErrProfileNotFound):
		return customers.ForgeProfile{Requests: 1}, customers.ErrProfileNotFound
	case err != nil:
		return customers.ForgeProfile{Requests: 1}, err
	}
	out := customers.ForgeProfile{Login: p.Login, Name: p.Name, Company: p.Company, Location: p.Location,
		Website: p.Blog, AvatarURL: p.AvatarURL, Requests: 1}
	if h := customersOrgHandle(p.Company); h != "" && g.orgs != nil {
		name, ok := g.orgs.get(h)
		if !ok {
			// One more request: the organization's public display name. A
			// handle that is not an organization, or any failure, keeps the
			// handle as the person wrote it.
			out.Requests++
			n, oerr := c.GetPublicOrgName(ctx, h)
			if oerr == nil || errors.Is(oerr, github.ErrProfileNotFound) {
				g.orgs.put(h, n)
			}
			name = n
		}
		if name != "" {
			out.Company = name
		}
	}
	return out, nil
}

// forgeProfileLookup reads public profiles through a pkg/forge adapter
// (GitLab or Gitea/Forgejo). Those forges have no shared quota tracker in
// the hive; a 429 backs the fill off.
type forgeProfileLookup struct {
	profiler forge.UserProfiler
	source   string
	requests int
}

func (f forgeProfileLookup) Source() string               { return f.source }
func (f forgeProfileLookup) Kind() customers.IdentityKind { return customers.KindGitHub }
func (f forgeProfileLookup) StandDown() string            { return "" }

func (f forgeProfileLookup) Lookup(ctx context.Context, login string) (customers.ForgeProfile, error) {
	p, err := f.profiler.UserProfile(ctx, login)
	switch {
	case errors.Is(err, forge.ErrRateLimited):
		return customers.ForgeProfile{Requests: f.requests}, customers.ErrProfileRateLimited
	case errors.Is(err, forge.ErrUserNotFound):
		return customers.ForgeProfile{Requests: f.requests}, customers.ErrProfileNotFound
	case err != nil:
		return customers.ForgeProfile{Requests: f.requests}, err
	}
	return customers.ForgeProfile{Login: p.Login, Name: p.Name, Company: p.Company, Location: p.Location,
		Website: p.Website, AvatarURL: p.AvatarURL, Requests: f.requests}, nil
}

// linearProfileLookup reads a connected Linear workspace's member name by a
// confirmed email address. Linear has no company or location for a member.
type linearProfileLookup struct {
	token      func() string
	graphqlURL string
}

func (l linearProfileLookup) Source() string               { return customers.ProfileSourceLinear }
func (l linearProfileLookup) Kind() customers.IdentityKind { return customers.KindEmail }
func (l linearProfileLookup) StandDown() string            { return "" }

func (l linearProfileLookup) Lookup(ctx context.Context, email string) (customers.ForgeProfile, error) {
	m, err := linearagent.FetchMemberByEmail(ctx, nil, l.graphqlURL, l.token(), email)
	switch {
	case errors.Is(err, linearagent.ErrRateLimited):
		return customers.ForgeProfile{Requests: 1}, customers.ErrProfileRateLimited
	case errors.Is(err, linearagent.ErrMemberNotFound):
		return customers.ForgeProfile{Requests: 1}, customers.ErrProfileNotFound
	case err != nil:
		return customers.ForgeProfile{Requests: 1}, err
	}
	return customers.ForgeProfile{Name: m.Name, AvatarURL: m.AvatarURL, Requests: 1}, nil
}

// customersProfileFillDeps are what the sources are built from, read per pass
// so a config reload or a reconnected workspace takes effect at once.
type customersProfileFillDeps struct {
	cfg         func() *config.Config
	client      func() *github.Client
	linearToken func() string
	orgs        *customersOrgNames
	// newForge builds the GitLab or Gitea adapter; a test fake replaces it.
	newForge func(kind forge.Kind, token string, opts forge.Options) (forge.Forge, error)
	logger   *slog.Logger
}

// customersProfileSources are the lookups for the hive's project-wide forge,
// then Linear when a workspace is connected. A forge that cannot be built
// (no URL, a typo) is left out with a warning, never a failure.
func customersProfileSources(d customersProfileFillDeps) []customers.ProfileLookup {
	cfg := d.cfg()
	if cfg == nil {
		return nil
	}
	var out []customers.ProfileLookup
	switch kind := cfg.ForgeKindForRepo(""); kind {
	case config.ForgeGitLab, config.ForgeGitea:
		fk, gov, source, requests := forge.KindGitLab, cfg.GitLab.Credentials().Governor(), customers.ProfileSourceGitLab, forge.UserProfileMaxRequests
		if kind == config.ForgeGitea {
			fk, gov, source, requests = forge.KindGitea, cfg.Gitea.Credentials().Governor(), customers.ProfileSourceGitea, 1
		}
		newForge := d.newForge
		if newForge == nil {
			newForge = forge.NewForge
		}
		token, _ := gov.Resolve()
		f, err := newForge(fk, token, forge.Options{BaseURL: cfg.ForgeURLForRepo(""), Org: cfg.Project.Org})
		if err != nil {
			if d.logger != nil {
				d.logger.Warn("[customers] profile fill: forge adapter unavailable", "forge", kind, "error", err)
			}
			break
		}
		if p, ok := f.(forge.UserProfiler); ok {
			out = append(out, forgeProfileLookup{profiler: p, source: source, requests: requests})
		}
	default:
		if d.client != nil && d.client() != nil {
			source := customers.ProfileSourceGitHub
			if cfg.GitHub.IsGHE() {
				source = customers.ProfileSourceGHE
			}
			out = append(out, githubProfileLookup{client: d.client, source: source, orgs: d.orgs})
		}
	}
	if d.linearToken != nil && d.linearToken() != "" {
		out = append(out, linearProfileLookup{token: d.linearToken})
	}
	return out
}

// newCustomersProfileFill builds the engine over the people store.
func newCustomersProfileFill(store *customers.Store, d customersProfileFillDeps) *customers.ProfileFill {
	f := customers.NewProfileFill(store)
	f.Enabled = func() bool { return d.cfg().CustomersProfileFillEnabled() }
	f.HourlyLimit = func() int {
		if c := d.cfg(); c != nil {
			return c.Customers.ProfileFill.RequestsPerHour()
		}
		return config.DefaultCustomersProfileFillMaxPerHour
	}
	f.Sources = func() []customers.ProfileLookup { return customersProfileSources(d) }
	return f
}

// logProfileFill logs one pass: counts and a fixed skip word only, never a
// login, an email or a value.
func logProfileFill(logger *slog.Logger, scope string, res customers.ProfileFillResult) {
	if logger == nil || res.Skipped == customers.FillSkipDisabled || (res.People == 0 && res.Skipped == "") {
		return
	}
	logger.Info("[customers] profile fill", "scope", scope, "due", res.Due, "people", res.People,
		"requests", res.Requests, "filled", res.Filled, "not_found", res.NotFound, "failed", res.Failed,
		"stood_down", res.StoodDown, "truncated", res.Truncated, "skipped", res.Skipped)
}

// startCustomersProfileFill runs the periodic pass (the backfill first, then
// refreshes) until the boot context ends. It does not go through the GitHub
// sweep workers, which need a GitHub client: a GitLab or Gitea hive has none.
// The GitHub source applies the shared quota gate itself.
func (c *customersDashboard) startCustomersProfileFill(ctx context.Context) func() {
	if c == nil || c.profileFill == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		timer := time.NewTimer(customersProfileFillFirstDelay)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			pass, done := context.WithTimeout(ctx, customersProfileFillPassTimeout)
			logProfileFill(c.logger, "periodic", c.profileFill.Run(pass, ""))
			done()
			timer.Reset(customers.ProfileFillInterval)
		}
	}()
	return cancel
}

// profileFillOnAccept fills the profile of a person just accepted. It never
// delays or fails the accept.
func (c *customersDashboard) profileFillOnAccept(person string) {
	if c.profileFill == nil || person == "" {
		return
	}
	run := func() {
		ctx, cancel := context.WithTimeout(context.Background(), customersProfileFillOneTimeout)
		defer cancel()
		logProfileFill(c.logger, "accept", c.profileFill.Run(ctx, person))
	}
	if c.spawn != nil {
		c.spawn(run)
		return
	}
	go run()
}

// RefreshProfile is POST /api/customers/{id}/refresh-profile: look one
// customer up now, ignoring the refresh age, within the hourly budget.
func (c *customersDashboard) RefreshProfile(ctx context.Context, v dashboard.CustomersViewer, id string) (dashboard.CustomerResult, any, error) {
	if c.profileFill == nil || c.profileFill.Enabled == nil || !c.profileFill.Enabled() {
		return dashboard.CustomerResult{}, nil, fmt.Errorf("%w: the profile fill is off (customers.profile_fill.enabled)", dashboard.ErrCustomerUnavailable)
	}
	before, err := customerResult(c.svc.Get(customersViewer(v), id))
	if err != nil {
		return dashboard.CustomerResult{}, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, customersProfileFillOneTimeout)
	defer cancel()
	res := c.profileFill.Run(ctx, before.ID)
	logProfileFill(c.logger, "refresh_one", res)
	after, err := customerResult(c.svc.Get(customersViewer(v), before.ID))
	return after, res, err
}

// RefreshAllProfiles is POST /api/customers/refresh-profiles: every customer
// becomes due, and a pass starts in the background; later passes continue
// within the hourly budget.
func (c *customersDashboard) RefreshAllProfiles(_ dashboard.CustomersViewer) (any, error) {
	if c.profileFill == nil || c.profileFill.Enabled == nil || !c.profileFill.Enabled() {
		return nil, fmt.Errorf("%w: the profile fill is off (customers.profile_fill.enabled)", dashboard.ErrCustomerUnavailable)
	}
	n, err := c.profileFill.RefreshAll()
	if err != nil {
		return nil, customersError(err)
	}
	run := func() {
		ctx, cancel := context.WithTimeout(context.Background(), customersProfileFillPassTimeout)
		defer cancel()
		logProfileFill(c.logger, "refresh_all", c.profileFill.Run(ctx, ""))
	}
	if c.spawn != nil {
		c.spawn(run)
	} else {
		go run()
	}
	return map[string]any{"queued": true, "reset": n, "max_per_pass": customers.ProfileFillMaxPerPass,
		"interval_minutes": int(customers.ProfileFillInterval / time.Minute)}, nil
}
