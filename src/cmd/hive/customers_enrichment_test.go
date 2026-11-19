package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/colony/resolver"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/customers"
)

// fakeGitdm stands in for the colony gitdm provider: it records every query
// and answers from a fixed table keyed by login.
type fakeGitdm struct {
	loaded     time.Time
	refreshErr error
	refreshes  int
	queries    []resolver.Query
	answers    map[string][]resolver.Affiliation
	now        func() time.Time
}

func (f *fakeGitdm) Lookup(_ context.Context, q resolver.Query) (resolver.Result, error) {
	f.queries = append(f.queries, q)
	return resolver.Result{Affiliations: f.answers[q.GitHubLogin]}, nil
}

func (f *fakeGitdm) Refresh(context.Context) error {
	f.refreshes++
	if f.refreshErr != nil {
		return f.refreshErr
	}
	f.loaded = f.now()
	return nil
}

func (f *fakeGitdm) LoadedAt() time.Time { return f.loaded }

func clearEnrichmentEnvCmd(t *testing.T) {
	t.Helper()
	for _, env := range []string{
		config.EnvCustomersEnabled, config.EnvCustomersEnrichmentEnabled, config.EnvCustomersEnrichmentGitdmEnabled,
		config.EnvCustomersEnrichmentGitdmRefreshInterval,
	} {
		t.Setenv(env, "")
	}
}

func enrichmentConfig(on bool) *config.Config {
	cfg := &config.Config{}
	cfg.Customers.Enrichment.Enabled = &on
	cfg.Customers.Enrichment.Gitdm.Enabled = &on
	return cfg
}

type enrichmentHarness struct {
	svc   *customersDashboard
	enr   *customersEnricher
	fake  *fakeGitdm
	now   time.Time
	audit []string
	cfg   *config.Config
}

func newEnrichmentHarness(t *testing.T, on bool) *enrichmentHarness {
	t.Helper()
	clearEnrichmentEnvCmd(t)
	h := &enrichmentHarness{svc: wireTestCustomers(t), now: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC), cfg: enrichmentConfig(on)}
	dir := filepath.Dir(h.svc.svc.Store().Path())
	if !wireCustomersEnrichment(h.svc.svc, dir, func() *config.Config { return h.cfg }, nil) {
		t.Fatal("enrichment store did not open")
	}
	h.fake = &fakeGitdm{now: func() time.Time { return h.now }, answers: map[string][]resolver.Affiliation{
		"ada": {
			{Org: "Acme", From: time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC), Source: "gitdm"},
			{Org: "Globex", From: time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC), Source: "gitdm"},
		},
	}}
	h.enr = &customersEnricher{cacheDir: t.TempDir(), newGitdm: func(string, time.Duration) gitdmLookup { return h.fake }}
	return h
}

func (h *enrichmentHarness) pass() customersEnrichmentResult {
	return runCustomersEnrichmentPass(context.Background(), h.svc.svc, h.cfg, h.enr, func() time.Time { return h.now },
		func(person, provider string) { h.audit = append(h.audit, person+"|"+provider) }, nil)
}

var ownerViewer = customers.Viewer{User: "olivia", Full: true, Owner: true}

func TestCustomersEnrichmentOffDoesNothing(t *testing.T) {
	h := newEnrichmentHarness(t, false)
	if _, err := h.svc.svc.Add(ownerViewer, "Ada", []customers.Identity{{Kind: customers.KindGitHub, Value: "ada"}}); err != nil {
		t.Fatal(err)
	}
	res := h.pass()
	if res.Looked != 0 || h.fake.refreshes != 0 || len(h.fake.queries) != 0 || len(h.audit) != 0 {
		t.Fatalf("off pass = %+v, refreshes %d, queries %v, audit %v", res, h.fake.refreshes, h.fake.queries, h.audit)
	}
	if ids := h.svc.svc.Enrichment().IDs(); len(ids) != 0 {
		t.Fatalf("off pass stored %v", ids)
	}
}

func TestCustomersEnrichmentGitdmPass(t *testing.T) {
	h := newEnrichmentHarness(t, true)
	ada, err := h.svc.svc.Add(ownerViewer, "Ada", []customers.Identity{{Kind: customers.KindGitHub, Value: "ada"}, {Kind: customers.KindEmail, Value: "ada@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.svc.Add(ownerViewer, "No ids", nil); err != nil {
		t.Fatal(err)
	}
	res := h.pass()
	if h.fake.refreshes != 1 || res.Looked != 1 || res.Changed != 1 {
		t.Fatalf("first pass = %+v, refreshes %d", res, h.fake.refreshes)
	}
	q := h.fake.queries[0]
	if q.GitHubLogin != "ada" || len(q.Emails) != 1 || q.Emails[0] != "ada@example.com" {
		t.Fatalf("query = %+v", q)
	}
	if len(h.audit) != 1 || h.audit[0] != ada.ID+"|gitdm" {
		t.Fatalf("audit = %v, want person id and provider only", h.audit)
	}
	got, err := h.svc.svc.Get(ownerViewer, ada.ID)
	if err != nil || got.Affiliation == nil || got.Affiliation.Value != "Globex" || got.Affiliation.Source != "gitdm" || got.Affiliation.From != "2022-01-01" {
		t.Fatalf("suggestion = %+v, %v", got.Affiliation, err)
	}
	if got.Profile.Organization.Value != "" {
		t.Fatal("enrichment wrote the organization")
	}
	// Nothing new in the snapshot: no lookup, no audit.
	if res := h.pass(); res.Looked != 0 || len(h.audit) != 1 {
		t.Fatalf("unchanged pass = %+v, audit %v", res, h.audit)
	}
	// Switching gitdm off hides the suggestion at once and the next pass
	// deletes the facts.
	off := false
	h.cfg.Customers.Enrichment.Gitdm.Enabled = &off
	// The offline email-domain rule may still offer something; gitdm must not.
	if got, _ := h.svc.svc.Get(ownerViewer, ada.ID); got.Affiliation != nil && got.Affiliation.Source != "" {
		t.Fatalf("gitdm off still suggests %+v", got.Affiliation)
	}
	if res := h.pass(); res.Dropped != 1 {
		t.Fatalf("off pass = %+v, want the facts dropped", res)
	}
	if _, ok := h.svc.svc.Enrichment().Get(ada.ID); ok {
		t.Fatal("gitdm facts survive the switch going off")
	}
}

func TestCustomersEnrichmentManualOrganizationWins(t *testing.T) {
	h := newEnrichmentHarness(t, true)
	ada, err := h.svc.svc.Add(ownerViewer, "Ada", []customers.Identity{{Kind: customers.KindGitHub, Value: "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.svc.Edit(ownerViewer, ada.ID, customers.Edit{Op: customers.OpProfile, Field: customers.FieldOrganization, Value: "Initech"}); err != nil {
		t.Fatal(err)
	}
	h.pass()
	got, _ := h.svc.svc.Get(ownerViewer, ada.ID)
	if got.Affiliation != nil || got.Profile.Organization.Value != "Initech" {
		t.Fatalf("manual organization challenged: %+v, %+v", got.Affiliation, got.Profile.Organization)
	}
}

func TestCustomersEnrichmentForgetAndPrune(t *testing.T) {
	h := newEnrichmentHarness(t, true)
	ada, err := h.svc.svc.Add(ownerViewer, "Ada", []customers.Identity{{Kind: customers.KindGitHub, Value: "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	h.pass()
	if _, ok := h.svc.svc.Enrichment().Get(ada.ID); !ok {
		t.Fatal("no enrichment stored")
	}
	if _, err := h.svc.svc.Forget(ownerViewer, ada.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.svc.svc.Enrichment().Get(ada.ID); ok {
		t.Fatal("Forget left enrichment behind")
	}
	// A stale entry for someone who no longer exists is pruned.
	if err := h.svc.svc.Enrichment().SetSource("p_gone", customers.EnrichSourceGitdm, []customers.EnrichedAffiliation{{Org: "X"}}, nil, h.now); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(48 * time.Hour)
	queries := len(h.fake.queries)
	if res := h.pass(); res.Pruned != 1 {
		t.Fatalf("prune pass = %+v", res)
	}
	if len(h.fake.queries) != queries {
		t.Fatal("a forgotten person was looked up again")
	}
}

func TestCustomersEnrichmentRefreshFailureWithoutSnapshot(t *testing.T) {
	h := newEnrichmentHarness(t, true)
	if _, err := h.svc.svc.Add(ownerViewer, "Ada", []customers.Identity{{Kind: customers.KindGitHub, Value: "ada"}}); err != nil {
		t.Fatal(err)
	}
	h.fake.refreshErr = errors.New("offline")
	if res := h.pass(); res.Errors != 1 || res.Looked != 0 {
		t.Fatalf("pass with no snapshot = %+v", res)
	}
}

func TestCustomersEnrichmentProviderFollowsRefresh(t *testing.T) {
	built := 0
	enr := &customersEnricher{newGitdm: func(string, time.Duration) gitdmLookup { built++; return &fakeGitdm{} }}
	enr.gitdmProvider(time.Hour)
	enr.gitdmProvider(time.Hour)
	enr.gitdmProvider(2 * time.Hour)
	if built != 2 {
		t.Fatalf("built %d providers, want 2", built)
	}
}

// fakeDevstats records every query and answers a fixed activity list.
type fakeDevstats struct {
	queries []resolver.Query
	answer  []resolver.Activity
	err     error
}

func (f *fakeDevstats) Lookup(_ context.Context, q resolver.Query) (resolver.Result, error) {
	f.queries = append(f.queries, q)
	return resolver.Result{Activity: f.answer}, f.err
}

func devstatsHarness(t *testing.T, limit int) (*enrichmentHarness, *fakeDevstats) {
	t.Helper()
	for _, env := range []string{config.EnvCustomersEnrichmentDevstatsEnabled, config.EnvCustomersEnrichmentDevstatsHourlyLimit} {
		t.Setenv(env, "")
	}
	h := newEnrichmentHarness(t, true)
	off, on := false, true
	h.cfg.Customers.Enrichment.Gitdm.Enabled = &off
	h.cfg.Customers.Enrichment.Devstats.Enabled = &on
	h.cfg.Customers.Enrichment.Devstats.HourlyLimit = limit
	ds := &fakeDevstats{answer: []resolver.Activity{{Project: "All CNCF", Contributions: 42, Period: "all time", URL: "https://all.devstats.cncf.io/d/9", Source: "devstats"}}}
	h.enr.newDevstats = func() devstatsLookup { return ds }
	return h, ds
}

func TestCustomersEnrichmentDevstatsLoginOnly(t *testing.T) {
	h, ds := devstatsHarness(t, 0)
	ada, err := h.svc.svc.Add(ownerViewer, "Ada", []customers.Identity{{Kind: customers.KindGitHub, Value: "ada"}, {Kind: customers.KindEmail, Value: "ada@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.svc.Add(ownerViewer, "Mail only", []customers.Identity{{Kind: customers.KindEmail, Value: "m@example.com"}}); err != nil {
		t.Fatal(err)
	}
	res := h.pass()
	if res.Devstats != 1 || len(ds.queries) != 1 {
		t.Fatalf("pass = %+v, queries %v", res, ds.queries)
	}
	if q := ds.queries[0]; q.GitHubLogin != "ada" || len(q.Emails) != 0 {
		t.Fatalf("devstats query = %+v, want the login and no email", q)
	}
	if len(h.audit) != 1 || h.audit[0] != ada.ID+"|devstats" {
		t.Fatalf("audit = %v", h.audit)
	}
	got, err := h.svc.svc.Get(ownerViewer, ada.ID)
	if err != nil || len(got.PublicContributions) != 1 || got.PublicContributions[0].Contributions != 42 || got.PublicContributions[0].Source != "devstats" {
		t.Fatalf("public contributions = %+v, %v", got.PublicContributions, err)
	}
	// Fresh activity is not looked up again inside the refresh window.
	if res := h.pass(); res.Devstats != 0 {
		t.Fatalf("second pass = %+v", res)
	}
	h.now = h.now.Add(customersDevstatsRefresh)
	if res := h.pass(); res.Devstats != 1 || len(h.audit) != 1 {
		t.Fatalf("refresh pass = %+v, audit %v (unchanged answer must not audit)", res, h.audit)
	}
	// Switching devstats off hides and then deletes the activity.
	off := false
	h.cfg.Customers.Enrichment.Devstats.Enabled = &off
	if got, _ := h.svc.svc.Get(ownerViewer, ada.ID); len(got.PublicContributions) != 0 {
		t.Fatal("devstats off still shows activity")
	}
	if res := h.pass(); res.Dropped != 1 || res.Devstats != 0 {
		t.Fatalf("off pass = %+v", res)
	}
}

func TestCustomersEnrichmentDevstatsHourlyLimit(t *testing.T) {
	h, ds := devstatsHarness(t, 2)
	for _, login := range []string{"a1", "a2", "a3"} {
		if _, err := h.svc.svc.Add(ownerViewer, login, []customers.Identity{{Kind: customers.KindGitHub, Value: login}}); err != nil {
			t.Fatal(err)
		}
	}
	if res := h.pass(); res.Devstats != 2 || res.Budgeted != 1 {
		t.Fatalf("first pass = %+v, want 2 lookups and 1 left for later", res)
	}
	if res := h.pass(); res.Devstats != 0 || res.Budgeted != 1 {
		t.Fatalf("same hour = %+v, want the budget spent", res)
	}
	h.now = h.now.Add(customersDevstatsWindow)
	if res := h.pass(); res.Devstats != 1 || len(ds.queries) != 3 {
		t.Fatalf("next hour = %+v, queries %d", res, len(ds.queries))
	}
}

func TestCustomersEnrichmentDevstatsErrorStoresNothing(t *testing.T) {
	h, ds := devstatsHarness(t, 0)
	ds.err = errors.New("devstats down")
	ada, err := h.svc.svc.Add(ownerViewer, "Ada", []customers.Identity{{Kind: customers.KindGitHub, Value: "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	if res := h.pass(); res.Errors != 1 {
		t.Fatalf("pass = %+v", res)
	}
	if _, ok := h.svc.svc.Enrichment().Get(ada.ID); ok {
		t.Fatal("a failed lookup stored activity")
	}
}
