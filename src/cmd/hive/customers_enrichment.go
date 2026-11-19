package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/colony/gitdm"
	"github.com/hivecommons/colony/resolver"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/customers"
)

// Customers enrichment (customers.enrichment, off by default; see
// src/docs/customers-enrichment.md). pkg/customers stores the facts and
// decides how they are shown; this file runs the hivecommons/colony
// providers and the sweep. gitdm downloads the public cncf/gitdm files and
// matches on the hive, so no lookup sends anything about a person.

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
)

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
}

func newCustomersEnricher(dataDir string) *customersEnricher {
	return &customersEnricher{
		cacheDir: filepath.Join(dataDir, customersEnrichmentCacheDir),
		newGitdm: func(cacheDir string, refresh time.Duration) gitdmLookup {
			return gitdm.New(gitdm.Options{CacheDir: cacheDir, Refresh: refresh})
		},
	}
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
		e := customers.EnrichedAffiliation{Org: strings.TrimSpace(a.Org), Source: customers.EnrichSourceGitdm}
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
	if b.logger != nil && (res.Looked > 0 || res.Changed > 0 || res.Pruned > 0 || res.Dropped > 0 || res.Errors > 0) {
		b.logger.Info("[customers] enrichment", "looked_up", res.Looked, "changed", res.Changed,
			"pruned", res.Pruned, "dropped", res.Dropped, "errors", res.Errors)
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
	if !cfg.CustomersEnrichmentGitdmEnabled() {
		if n, err := e.DropSource(customers.EnrichSourceGitdm); err == nil {
			res.Dropped = n
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
	return res
}
