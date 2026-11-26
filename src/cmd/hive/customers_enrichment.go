package main

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/colony/devstats"
	"github.com/hivecommons/colony/gitdm"
	"github.com/hivecommons/colony/resolver"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/customers"
)

// Customers enrichment (customers.enrichment, off by default; see
// src/docs/customers-enrichment.md). pkg/customers stores the facts and
// decides how they are shown; this file runs the hivecommons/colony
// providers and the sweep. gitdm downloads the public cncf/gitdm files and
// matches on the hive, so it sends nothing about a person. devstats sends one
// confirmed GitHub login per lookup, never an email, within an hourly limit.

const (
	// customersEnrichmentInterval is how often the enrichment sweep runs.
	// A person is looked up again only when the gitdm snapshot is newer
	// than their last lookup, so most passes do no work.
	customersEnrichmentInterval = 30 * time.Minute
	// customersEnrichmentTimeout bounds one pass, snapshot refresh included.
	customersEnrichmentTimeout = 5 * time.Minute
	// customersEnrichmentMaxPerPass bounds the people one pass looks up, so
	// a large customer list is enriched over several passes.
	customersEnrichmentMaxPerPass = 200
	// customersEnrichmentCacheDir holds colony's ETag cache of the public
	// gitdm files, beside the people store.
	customersEnrichmentCacheDir = "colony-cache"
	// customersEnrichmentAuditActor is the actor on enrichment audit lines.
	customersEnrichmentAuditActor = "hive"
	// customersEnrichmentAuditAction is the audit action for a change to a
	// person's enrichment.
	customersEnrichmentAuditAction = "customer_enrichment"
	// customersDevstatsRefresh is how long a person's devstats activity is
	// kept before it is looked up again.
	customersDevstatsRefresh = 24 * time.Hour
	// customersDevstatsWindow is the window the devstats hourly lookup
	// limit (customers.enrichment.devstats.hourly_limit) counts over.
	customersDevstatsWindow = time.Hour
)

// devstatsLookup is the part of the colony devstats provider the sweep uses.
// Each lookup sends one GitHub login, never an email, to devstats.cncf.io.
type devstatsLookup interface {
	Lookup(ctx context.Context, q resolver.Query) (resolver.Result, error)
}

// gitdmLookup is the part of the colony gitdm provider the sweep uses.
type gitdmLookup interface {
	Lookup(ctx context.Context, q resolver.Query) (resolver.Result, error)
	Refresh(ctx context.Context) error
	LoadedAt() time.Time
}

// customersEnricher owns the colony providers across passes, so the gitdm
// snapshot is held in memory and only revalidated on its refresh interval.
type customersEnricher struct {
	mu       sync.Mutex
	cacheDir string
	refresh  time.Duration
	gitdm    gitdmLookup
	// newGitdm builds the provider; tests replace it.
	newGitdm func(cacheDir string, refresh time.Duration) gitdmLookup
	devstats devstatsLookup
	// newDevstats builds the devstats provider; tests replace it.
	newDevstats func() devstatsLookup
	// devstatsSpent holds the start times of devstats lookups inside the
	// last customersDevstatsWindow, the hourly budget.
	devstatsSpent []time.Time
}

func newCustomersEnricher(dataDir string) *customersEnricher {
	return &customersEnricher{
		cacheDir: filepath.Join(dataDir, customersEnrichmentCacheDir),
		newGitdm: func(cacheDir string, refresh time.Duration) gitdmLookup {
			return gitdm.New(gitdm.Options{CacheDir: cacheDir, Refresh: refresh})
		},
		newDevstats: func() devstatsLookup { return devstats.New(devstats.Options{}) },
	}
}

// devstatsProvider returns the devstats provider, built on first use.
func (c *customersEnricher) devstatsProvider() devstatsLookup {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.devstats == nil {
		c.devstats = c.newDevstats()
	}
	return c.devstats
}

// reserveDevstats spends one lookup from the hourly budget, or reports that
// the budget is used up.
func (c *customersEnricher) reserveDevstats(now time.Time, limit int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.devstatsSpent[:0]
	for _, at := range c.devstatsSpent {
		if now.Sub(at) < customersDevstatsWindow {
			kept = append(kept, at)
		}
	}
	c.devstatsSpent = kept
	if len(kept) >= limit {
		return false
	}
	c.devstatsSpent = append(c.devstatsSpent, now)
	return true
}

// gitdmProvider returns the provider for this refresh interval, building a
// new one when the interval changed.
func (c *customersEnricher) gitdmProvider(refresh time.Duration) gitdmLookup {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gitdm == nil || c.refresh != refresh {
		c.gitdm = c.newGitdm(c.cacheDir, refresh)
		c.refresh = refresh
	}
	return c.gitdm
}

// customersEnrichmentActive reports whether a source is switched on in the
// current config. The service asks it on every read, so switching a source
// off hides its facts at once; the next pass deletes them.
func customersEnrichmentActive(cfg func() *config.Config) func(source string) bool {
	return func(source string) bool {
		c := cfg()
		switch source {
		case customers.EnrichSourceGitdm:
			return c.CustomersEnrichmentGitdmEnabled()
		case customers.EnrichSourceDevstats:
			return c.CustomersEnrichmentDevstatsEnabled()
		default:
			return false
		}
	}
}

// wireCustomersEnrichment opens enrichment.json beside people.json and hands
// it to the service. A failure leaves enrichment unwired; Customers works
// as before.
func wireCustomersEnrichment(svc *customers.Service, dataDir string, cfg func() *config.Config, logger *slog.Logger) bool {
	e, err := customers.OpenEnrichment(dataDir)
	if err != nil {
		if logger != nil {
			logger.Warn("[customers] enrichment store unavailable; enrichment is off", "error", err)
		}
		return false
	}
	// In customers.store shadow or colony mode the enrichment results are
	// mirrored into colony.db too (#19855); in colony-only they are read
	// from it (#19972).
	if err := e.SetColony(svc.Store().Colony()); err != nil {
		if logger != nil {
			logger.Warn("[customers] enrichment could not be read from the colony store; enrichment is off", "error", err)
		}
		return false
	}
	svc.SetEnrichment(e, customersEnrichmentActive(cfg))
	return true
}

// enrichmentQuery is what a person may be looked up by: their confirmed
// GitHub login and confirmed email addresses. An unconfirmed claim is never
// used, and neither is an identity that was forgotten.
func enrichmentQuery(store *customers.Store, p customers.Person) resolver.Query {
	var q resolver.Query
	for _, id := range p.Identities {
		if id.Claimed || store.Forgotten(id.Kind, id.Value) {
			continue
		}
		switch id.Kind {
		case customers.KindGitHub:
			if q.GitHubLogin == "" {
				q.GitHubLogin = id.Value
			}
		case customers.KindEmail, customers.KindGitEmail:
			q.Emails = append(q.Emails, id.Value)
		}
	}
	return q
}

// enrichedAffiliations turns colony's gitdm affiliations into the stored
// shape: calendar dates, the gitdm source.
func enrichedAffiliations(res resolver.Result) []customers.EnrichedAffiliation {
	const day = "2006-01-02"
	var out []customers.EnrichedAffiliation
	for _, a := range res.Affiliations {
		if a.Source != "" && a.Source != gitdm.SourceName {
			continue
		}
		e := customers.EnrichedAffiliation{Org: strings.TrimSpace(a.Org), Source: customers.EnrichSourceGitdm,
			Confidence: string(a.Confidence), MatchedBy: a.MatchedBy}
		if !a.From.IsZero() {
			e.From = a.From.UTC().Format(day)
		}
		if !a.Until.IsZero() {
			e.Until = a.Until.UTC().Format(day)
		}
		out = append(out, e)
	}
	return out
}

// sameAffiliations reports whether a person's stored gitdm facts already
// equal what a lookup returned, so an unchanged lookup writes no audit line.
func sameAffiliations(pe customers.PersonEnrichment, next []customers.EnrichedAffiliation) bool {
	var cur []customers.EnrichedAffiliation
	for _, a := range pe.Affiliations {
		if a.Source == customers.EnrichSourceGitdm {
			cur = append(cur, a)
		}
	}
	if len(cur) != len(next) {
		return false
	}
	for i := range cur {
		if cur[i] != next[i] {
			return false
		}
	}
	return true
}

// customersEnrichmentResult counts one pass for the log line.
type customersEnrichmentResult struct {
	Looked, Changed, Pruned, Dropped, Errors int
	// Devstats counts devstats lookups; Budgeted the people left for a
	// later pass because the hourly limit was reached.
	Devstats, Budgeted int
	// AutoAccepted counts organizations the auto-accept policy filled.
	AutoAccepted int
}

// devstatsActivity turns colony's devstats activity into the stored shape.
func devstatsActivity(res resolver.Result) []customers.EnrichedActivity {
	var out []customers.EnrichedActivity
	for _, a := range res.Activity {
		if a.Source != "" && a.Source != devstats.SourceName {
			continue
		}
		out = append(out, customers.EnrichedActivity{Project: a.Project, Contributions: a.Contributions,
			Period: a.Period, URL: a.URL, Source: customers.EnrichSourceDevstats})
	}
	return out
}

// sameActivity reports whether a person's stored devstats activity already
// equals a lookup's answer.
func sameActivity(pe customers.PersonEnrichment, next []customers.EnrichedActivity) bool {
	var cur []customers.EnrichedActivity
	for _, a := range pe.Activity {
		if a.Source == customers.EnrichSourceDevstats {
			cur = append(cur, a)
		}
	}
	if len(cur) != len(next) {
		return false
	}
	for i := range cur {
		if cur[i] != next[i] {
			return false
		}
	}
	return true
}

// devstatsLogin is the confirmed GitHub login a person may be looked up by
// on devstats, or "". Emails are never used.
func devstatsLogin(store *customers.Store, p customers.Person) string {
	for _, id := range p.Identities {
		if id.Kind == customers.KindGitHub && !id.Claimed && !store.Forgotten(id.Kind, id.Value) {
			return id.Value
		}
	}
	return ""
}

// runDevstatsStep looks up public activity for confirmed customers with a
// GitHub login whose activity is older than customersDevstatsRefresh, within
// the hourly budget. With devstats off it deletes stored activity.
func runDevstatsStep(ctx context.Context, svc *customers.Service, cfg *config.Config, enr *customersEnricher,
	people []customers.Person, now func() time.Time, audit func(person, provider string), res *customersEnrichmentResult) {
	e, store := svc.Enrichment(), svc.Store()
	if !cfg.CustomersEnrichmentDevstatsEnabled() {
		if n, err := e.DropSource(customers.EnrichSourceDevstats); err == nil {
			res.Dropped += n
		} else {
			res.Errors++
		}
		return
	}
	limit := cfg.CustomersEnrichmentDevstatsHourlyLimit()
	ds := enr.devstatsProvider()
	for _, p := range people {
		if ctx.Err() != nil {
			return
		}
		pe, _ := e.Get(p.ID)
		if at, ok := pe.FetchedAt[customers.EnrichSourceDevstats]; ok && now().Sub(at) < customersDevstatsRefresh {
			continue
		}
		login := devstatsLogin(store, p)
		if login == "" {
			continue
		}
		if !enr.reserveDevstats(now(), limit) {
			res.Budgeted++
			continue
		}
		out, err := ds.Lookup(ctx, resolver.Query{GitHubLogin: login})
		res.Devstats++
		if err != nil {
			res.Errors++
			continue
		}
		acts := devstatsActivity(out)
		if _, err := store.Get(p.ID); err != nil {
			continue
		}
		changed := !sameActivity(pe, acts)
		if err := e.SetSource(p.ID, customers.EnrichSourceDevstats, nil, acts, now()); err != nil {
			res.Errors++
			continue
		}
		if changed {
			res.Changed++
			if audit != nil {
				audit(p.ID, customers.EnrichSourceDevstats)
			}
		}
	}
}

// runCustomersEnrichment is one enrichment pass. With the source off it only
// tidies: facts of a switched-off source and of people who are gone are
// deleted. With gitdm on it refreshes the snapshot when due and looks up
// confirmed customers whose last lookup predates the snapshot.
func (b *boot) runCustomersEnrichment(ctx context.Context, cfg *config.Config) {
	if b.customers == nil || cfg == nil || !cfg.CustomersEnabled() {
		return
	}
	if b.customersEnricher == nil {
		b.customersEnricher = newCustomersEnricher(customersDataDir)
	}
	ctx, cancel := context.WithTimeout(ctx, customersEnrichmentTimeout)
	defer cancel()
	audit := func(person, provider string) {
		if b.dashSrv != nil {
			b.dashSrv.AuditLog(customersEnrichmentAuditActor, customersEnrichmentAuditAction,
				"person="+person+", provider="+provider, "")
		}
	}
	res := runCustomersEnrichmentPass(ctx, b.customers.svc, cfg, b.customersEnricher, time.Now, audit, b.logger)
	if b.logger != nil && (res.Looked > 0 || res.Devstats > 0 || res.Changed > 0 || res.Pruned > 0 || res.Dropped > 0 ||
		res.AutoAccepted > 0 || res.Errors > 0) {
		b.logger.Info("[customers] enrichment", "looked_up", res.Looked, "devstats", res.Devstats, "budgeted", res.Budgeted,
			"changed", res.Changed, "pruned", res.Pruned, "dropped", res.Dropped, "auto_accepted", res.AutoAccepted, "errors", res.Errors)
	}
}

// runCustomersEnrichmentPass is the pass itself, with its collaborators
// passed in so tests can drive it.
func runCustomersEnrichmentPass(ctx context.Context, svc *customers.Service, cfg *config.Config, enr *customersEnricher,
	now func() time.Time, audit func(person, provider string), logger *slog.Logger) customersEnrichmentResult {
	var res customersEnrichmentResult
	e := svc.Enrichment()
	if e == nil {
		return res
	}
	store := svc.Store()
	people := store.People()
	alive := make(map[string]customers.Person, len(people))
	for _, p := range people {
		alive[p.ID] = p
	}
	if n, err := e.Prune(func(id string) bool { _, ok := alive[id]; return ok }); err == nil {
		res.Pruned = n
	} else {
		res.Errors++
	}
	runDevstatsStep(ctx, svc, cfg, enr, people, now, audit, &res)
	if !cfg.CustomersEnrichmentGitdmEnabled() {
		if n, err := e.DropSource(customers.EnrichSourceGitdm); err == nil {
			res.Dropped += n
		} else {
			res.Errors++
		}
		return res
	}
	refresh := cfg.CustomersEnrichmentGitdmRefresh()
	gd := enr.gitdmProvider(refresh)
	// The snapshot refresh is its own step: one download (or a 304) of the
	// public files, never per person.
	if loaded := gd.LoadedAt(); loaded.IsZero() || now().Sub(loaded) >= refresh {
		if err := gd.Refresh(ctx); err != nil {
			res.Errors++
			if logger != nil {
				logger.Warn("[customers] gitdm snapshot refresh failed", "error", err)
			}
			if gd.LoadedAt().IsZero() {
				return res
			}
		}
	}
	snapshot := gd.LoadedAt()
	for _, p := range people {
		if res.Looked >= customersEnrichmentMaxPerPass || ctx.Err() != nil {
			break
		}
		pe, _ := e.Get(p.ID)
		if at, ok := pe.FetchedAt[customers.EnrichSourceGitdm]; ok && !at.Before(snapshot) {
			continue
		}
		q := enrichmentQuery(store, p)
		if q.Empty() {
			continue
		}
		out, err := gd.Lookup(ctx, q)
		res.Looked++
		if err != nil {
			res.Errors++
			continue
		}
		affs := enrichedAffiliations(out)
		// The person may have been forgotten while we looked.
		if _, err := store.Get(p.ID); err != nil {
			continue
		}
		changed := !sameAffiliations(pe, affs)
		if err := e.SetSource(p.ID, customers.EnrichSourceGitdm, affs, nil, now()); err != nil {
			res.Errors++
			continue
		}
		if changed {
			res.Changed++
			if audit != nil {
				audit(p.ID, customers.EnrichSourceGitdm)
			}
		}
	}
	runAutoAcceptStep(svc, cfg, people, audit, &res)
	return res
}

// customersAutoAcceptAuditProvider names the auto-accept policy on the
// enrichment audit line of a person whose organization it filled.
const customersAutoAcceptAuditProvider = "gitdm_auto_accept"

// runAutoAcceptStep applies customers.enrichment.auto_accept after the gitdm
// lookups. With the default policy, off, it does nothing. With gitdm_high a
// current gitdm affiliation matched by GitHub login fills an organization
// that is empty or was auto-accepted before (customers.AutoAcceptCandidate);
// it never replaces a value a person set, one shared from the hub, or the
// person's own forge profile company.
func runAutoAcceptStep(svc *customers.Service, cfg *config.Config, people []customers.Person,
	audit func(person, provider string), res *customersEnrichmentResult) {
	if cfg.CustomersEnrichmentAutoAccept() != config.CustomersAutoAcceptGitdmHigh {
		return
	}
	e := svc.Enrichment()
	store := svc.Store()
	for _, p := range people {
		pe, ok := e.Get(p.ID)
		if !ok {
			continue
		}
		changed, err := store.AutoAcceptAffiliation(p.ID, pe.Affiliations)
		if err != nil {
			if !errors.Is(err, customers.ErrNotFound) {
				res.Errors++
			}
			continue
		}
		if changed {
			res.AutoAccepted++
			if audit != nil {
				audit(p.ID, customersAutoAcceptAuditProvider)
			}
		}
	}
}
