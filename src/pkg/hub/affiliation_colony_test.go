package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/colony/devstats"
	"github.com/hivecommons/colony/gitdm"
	"github.com/hivecommons/colony/resolver"
)

// fakeColonyProvider records every query and answers a fixed result.
type fakeColonyProvider struct {
	mu      sync.Mutex
	res     resolver.Result
	err     error
	queries []resolver.Query
}

func (f *fakeColonyProvider) Lookup(_ context.Context, q resolver.Query) (resolver.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)
	return f.res, f.err
}

func (f *fakeColonyProvider) calls() []resolver.Query {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]resolver.Query(nil), f.queries...)
}

func testColonyAffiliator(g, d *fakeColonyProvider) *colonyAffiliator {
	return &colonyAffiliator{
		cacheDir:    "unused",
		newGitdm:    func(string, time.Duration) colonyLookup { return g },
		newDevstats: func() colonyLookup { return d },
	}
}

func clearColonyEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{colonyGitdmEnv, colonyGitdmRefreshEnv, colonyDevstatsEnv, colonyDevstatsHourlyLimitEnv, colonyTimeoutEnv} {
		t.Setenv(name, "")
	}
}

var colonyTestNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func TestColonyAffiliationConfigDefaults(t *testing.T) {
	clearColonyEnv(t)
	cfg := colonyAffiliationConfigFromEnv()
	if !cfg.Gitdm {
		t.Fatal("gitdm must be on by default")
	}
	if cfg.Devstats {
		t.Fatal("devstats must be off by default: it sends a login to a third party")
	}
	if cfg.GitdmRefresh != colonyGitdmRefreshDefault || cfg.DevstatsHourlyLimit != colonyDevstatsHourlyLimitDefault || cfg.Timeout != colonyTimeoutDefault {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestColonyAffiliationConfigOverridesAndBounds(t *testing.T) {
	clearColonyEnv(t)
	t.Setenv(colonyGitdmEnv, "off")
	t.Setenv(colonyDevstatsEnv, "yes")
	t.Setenv(colonyGitdmRefreshEnv, "10m") // below the 1h floor
	t.Setenv(colonyDevstatsHourlyLimitEnv, "100000")
	t.Setenv(colonyTimeoutEnv, "1h")
	cfg := colonyAffiliationConfigFromEnv()
	if cfg.Gitdm || !cfg.Devstats {
		t.Fatalf("switches not read: %+v", cfg)
	}
	if cfg.GitdmRefresh != colonyGitdmRefreshDefault {
		t.Fatalf("refresh below the floor must fall back to the default, got %v", cfg.GitdmRefresh)
	}
	if cfg.DevstatsHourlyLimit != colonyDevstatsHourlyLimitMax {
		t.Fatalf("hourly limit not capped: %d", cfg.DevstatsHourlyLimit)
	}
	if cfg.Timeout != colonyTimeoutMax {
		t.Fatalf("timeout not capped: %v", cfg.Timeout)
	}
	t.Setenv(colonyGitdmEnv, "maybe")
	if !colonyAffiliationConfigFromEnv().Gitdm {
		t.Fatal("an unrecognised value must keep the default (on)")
	}
}

func TestColonyLoginOnlyProvenPublicGitHub(t *testing.T) {
	gh := &SaaSUser{GitHubUsername: "octo-dev"}
	public := topRepoProfile{Login: "octo-dev", APIBase: githubActivityDefaultAPIURL}
	if got := colonyLogin(gh, public); got != "octo-dev" {
		t.Fatalf("public github login = %q", got)
	}
	if got := colonyLogin(gh, topRepoProfile{Login: "octo-dev", APIBase: "https://ghe.example.com/api/v3"}); got != "" {
		t.Fatalf("a GHE login must never be sent to colony, got %q", got)
	}
	if got := colonyLogin(gh, topRepoProfile{IDPFallback: "Example Corp"}); got != "" {
		t.Fatalf("no login, got %q", got)
	}
	linked := &SaaSUser{GitHubUsername: "octo-dev", LinkedGitHubLogin: "someone-else"}
	if got := colonyLogin(linked, topRepoProfile{Login: "someone-else", APIBase: githubActivityDefaultAPIURL}); got != "octo-dev" {
		t.Fatalf("an unproven LinkedGitHubLogin must not be sent, got %q", got)
	}
	oidc := &SaaSUser{GitHubUsername: "google:1234", CanonicalID: "google:1234", LinkedGitHubLogin: "octo-dev"}
	if got := colonyLogin(oidc, public); got != "" {
		t.Fatalf("a non-GitHub primary has no proven login, got %q", got)
	}
}

func TestColonyGitdmFailureBacksOff(t *testing.T) {
	g := &fakeColonyProvider{err: errors.New("unreachable")}
	c := testColonyAffiliator(g, &fakeColonyProvider{})
	cfg := colonyAffiliationConfig{Gitdm: true}
	if _, err := c.lookup(context.Background(), "octo-dev", colonyTestNow, cfg); err == nil {
		t.Fatal("want the gitdm error")
	}
	if _, err := c.lookup(context.Background(), "octo-dev", colonyTestNow.Add(time.Minute), cfg); err != nil {
		t.Fatalf("inside the backoff gitdm must be skipped, got %v", err)
	}
	if n := len(g.calls()); n != 1 {
		t.Fatalf("gitdm calls = %d, want 1 during the backoff", n)
	}
	_, _ = c.lookup(context.Background(), "octo-dev", colonyTestNow.Add(colonyGitdmFailureBackoff+time.Minute), cfg)
	if n := len(g.calls()); n != 2 {
		t.Fatalf("gitdm calls = %d, want a retry after the backoff", n)
	}
}

func TestColonyLookupSendsLoginOnlyAndPicksCurrentGitdm(t *testing.T) {
	g := &fakeColonyProvider{res: resolver.Result{Affiliations: []resolver.Affiliation{
		{Org: "Old Org", Until: colonyTestNow.AddDate(-1, 0, 0), Source: gitdm.SourceName, Confidence: resolver.ConfidenceHigh},
		{Org: "Example Org", From: colonyTestNow.AddDate(-1, 0, 0), Source: gitdm.SourceName, Confidence: resolver.ConfidenceHigh},
	}}}
	d := &fakeColonyProvider{}
	c := testColonyAffiliator(g, d)
	got, err := c.lookup(context.Background(), "octo-dev", colonyTestNow, colonyAffiliationConfig{Gitdm: true, GitdmRefresh: time.Hour, DevstatsHourlyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got.Org != "Example Org" || got.Source != affiliationSourceColonyGitdm || got.Confidence != string(resolver.ConfidenceHigh) {
		t.Fatalf("lookup = %+v, want the current gitdm period", got)
	}
	calls := g.calls()
	if len(calls) != 1 || calls[0].GitHubLogin != "octo-dev" || len(calls[0].Emails) != 0 {
		t.Fatalf("gitdm query = %+v, want the login and no email", calls)
	}
	if len(d.calls()) != 0 {
		t.Fatal("devstats was asked while switched off")
	}
}

func TestColonyLookupDevstatsBudgetAndContributions(t *testing.T) {
	g := &fakeColonyProvider{}
	d := &fakeColonyProvider{res: resolver.Result{Activity: []resolver.Activity{
		{Project: devstats.AllCNCF, Period: devstats.AllTime, Contributions: 42, Source: devstats.SourceName},
		{Project: "Some Project", Period: "Last year", Contributions: 7, Source: devstats.SourceName},
	}}}
	c := testColonyAffiliator(g, d)
	cfg := colonyAffiliationConfig{Devstats: true, DevstatsHourlyLimit: 1}
	got, err := c.lookup(context.Background(), "octo-dev", colonyTestNow, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.CNCFContributions != 42 || got.Org != "" {
		t.Fatalf("devstats lookup = %+v", got)
	}
	if len(g.calls()) != 0 {
		t.Fatal("gitdm was asked while switched off")
	}
	if _, err := c.lookup(context.Background(), "other-dev", colonyTestNow.Add(time.Minute), cfg); err != nil {
		t.Fatal(err)
	}
	if n := len(d.calls()); n != 1 {
		t.Fatalf("devstats calls = %d, want the hourly budget of 1 to hold", n)
	}
	if over, _ := c.lookup(context.Background(), "third-dev", colonyTestNow.Add(2*time.Minute), cfg); !over.KeepCNCF || over.CNCFContributions != 0 {
		t.Fatalf("over budget = %+v, want KeepCNCF so the stored count survives", over)
	}
	if _, err := c.lookup(context.Background(), "other-dev", colonyTestNow.Add(colonyDevstatsWindow+time.Minute), cfg); err != nil {
		t.Fatal(err)
	}
	if n := len(d.calls()); n != 2 {
		t.Fatalf("devstats calls = %d, want the budget to refill after the window", n)
	}
	for _, q := range d.calls() {
		if len(q.Emails) != 0 {
			t.Fatalf("devstats query carried an email: %+v", q)
		}
	}
}

func TestColonyLookupDevstatsAffiliationLabelled(t *testing.T) {
	d := &fakeColonyProvider{res: resolver.Result{Affiliations: []resolver.Affiliation{
		{Org: "Example Org", Source: devstats.SourceName, Confidence: resolver.ConfidenceMedium},
	}}}
	c := testColonyAffiliator(&fakeColonyProvider{}, d)
	got, err := c.lookup(context.Background(), "octo-dev", colonyTestNow, colonyAffiliationConfig{Gitdm: true, Devstats: true, DevstatsHourlyLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got.Org != "Example Org" || got.Source != affiliationSourceColonyDevstats || got.Confidence != string(resolver.ConfidenceMedium) {
		t.Fatalf("lookup = %+v, want a colony:devstats affiliation", got)
	}
}

func TestColonyLookupProviderErrorIsReported(t *testing.T) {
	g := &fakeColonyProvider{err: errors.New("snapshot unavailable")}
	c := testColonyAffiliator(g, &fakeColonyProvider{})
	got, err := c.lookup(context.Background(), "octo-dev", colonyTestNow, colonyAffiliationConfig{Gitdm: true})
	if err == nil || got.Org != "" {
		t.Fatalf("lookup = %+v, %v; want an error and no facts", got, err)
	}
}

func TestMergeColonyAffiliationPrecedence(t *testing.T) {
	col := colonyAffiliation{Org: "Example Org", Source: affiliationSourceColonyGitdm, Confidence: "high", CNCFContributions: 3}

	kept, err := mergeColonyAffiliation(topRepoAssociation{Affiliation: "Profile Co", AffiliationSource: affiliationSourceCompany}, nil, col, 0)
	if err != nil || kept.Affiliation != "Profile Co" || kept.AffiliationSource != affiliationSourceCompany || kept.AffiliationConfidence != "" {
		t.Fatalf("profile company must outrank colony: %+v, %v", kept, err)
	}
	if kept.CNCFContributions != 3 {
		t.Fatalf("contributions not carried: %+v", kept)
	}

	for _, source := range []string{affiliationSourceContributions, affiliationSourceOrgMembership, affiliationSourceGHE, affiliationSourceIDP} {
		got, err := mergeColonyAffiliation(topRepoAssociation{Affiliation: "heuristic", AffiliationSource: source, TopRepo: "example/repo"}, nil, col, 0)
		if err != nil || got.Affiliation != "Example Org" || got.AffiliationSource != affiliationSourceColonyGitdm || got.AffiliationConfidence != "high" {
			t.Fatalf("colony must outrank %s: %+v, %v", source, got, err)
		}
		if got.TopRepo != "example/repo" {
			t.Fatalf("footprint detail lost: %+v", got)
		}
	}

	got, err := mergeColonyAffiliation(topRepoAssociation{}, errTopRepoNoData, col, 0)
	if err != nil || got.Affiliation != "Example Org" {
		t.Fatalf("a colony match must make a no-data footprint saveable: %+v, %v", got, err)
	}

	transient := topRepoHTTPError{status: http.StatusTooManyRequests}
	if _, err := mergeColonyAffiliation(topRepoAssociation{}, transient, col, 0); err == nil {
		t.Fatal("a transient footprint error must still be reported so the old cache is kept")
	}

	if _, err := mergeColonyAffiliation(topRepoAssociation{}, errTopRepoNoData, colonyAffiliation{}, 0); !errors.Is(err, errTopRepoNoData) {
		t.Fatalf("no data and no colony match must stay no data, got %v", err)
	}

	kept, err = mergeColonyAffiliation(topRepoAssociation{Affiliation: "x", AffiliationSource: affiliationSourceCompany}, nil, colonyAffiliation{KeepCNCF: true}, 9)
	if err != nil || kept.CNCFContributions != 9 {
		t.Fatalf("a skipped devstats lookup must keep the stored count: %+v, %v", kept, err)
	}
	cleared, _ := mergeColonyAffiliation(topRepoAssociation{Affiliation: "x", AffiliationSource: affiliationSourceCompany}, nil, colonyAffiliation{}, 9)
	if cleared.CNCFContributions != 0 {
		t.Fatalf("with devstats off the count must clear: %+v", cleared)
	}
}

func TestHubSetCompanyOutranksColonyAffiliation(t *testing.T) {
	stored := SaaSUser{GitHubUsername: "octo-dev", Affiliation: "Example Org", AffiliationSource: affiliationSourceColonyGitdm, AffiliationConfidence: "high"}

	u := stored
	u.Company, u.CompanySource = "User Co", countrySourceUser
	if got := userTopRepoAssociation(&u, nil); got.Affiliation != "User Co" || got.AffiliationSource != affiliationSourceUser || got.AffiliationConfidence != "" {
		t.Fatalf("user-set company must win: %+v", got)
	}

	u = stored
	u.Company, u.CompanySource = "Admin Co", countrySourceAdmin
	if got := userTopRepoAssociation(&u, nil); got.Affiliation != "Admin Co" || got.AffiliationSource != affiliationSourceAdmin {
		t.Fatalf("admin-set company must win: %+v", got)
	}

	u = stored
	u.Company = "Legacy Co" // no recorded source
	if got := userTopRepoAssociation(&u, nil); got.Affiliation != "Example Org" || got.AffiliationSource != affiliationSourceColonyGitdm || got.AffiliationConfidence != "high" {
		t.Fatalf("an unrecorded company is not a set value: %+v", got)
	}
}

func TestSaveUserTopRepoCacheDoesNotResurrectDeletedUser(t *testing.T) {
	useTempUserDir(t)
	gone := &SaaSUser{GitHubUsername: "gone-dev", Hives: map[string]string{"own": "viewer"}}
	if err := saveUserTopRepoCache(gone, topRepoAssociation{Affiliation: "Example Org"}, colonyTestNow); err != nil {
		t.Fatal(err)
	}
	if loadSaaSUser("gone-dev") != nil {
		t.Fatal("a refresh wrote a deleted user back")
	}
}

func TestSaveUserTopRepoCachePersistsColonyFields(t *testing.T) {
	useTempUserDir(t)
	u := &SaaSUser{GitHubUsername: "octo-dev"}
	if err := saveSaaSUser(u); err != nil {
		t.Fatal(err)
	}
	assoc := topRepoAssociation{Affiliation: "Example Org", AffiliationSource: affiliationSourceColonyGitdm, AffiliationConfidence: "high", CNCFContributions: 12}
	if err := saveUserTopRepoCache(u, assoc, colonyTestNow); err != nil {
		t.Fatal(err)
	}
	got := loadSaaSUser("octo-dev")
	if got == nil || got.AffiliationSource != affiliationSourceColonyGitdm || got.AffiliationConfidence != "high" || got.CNCFContributions != 12 {
		t.Fatalf("colony fields not persisted: %+v", got)
	}

	s := sessionTestHub()
	rec := httptest.NewRecorder()
	s.handleAdminUsers(rec, httptest.NewRequest("GET", "/api/saas/admin/users", nil))
	var resp struct {
		Users []struct {
			GitHubUsername        string `json:"github_username"`
			AffiliationSource     string `json:"affiliation_source"`
			AffiliationConfidence string `json:"affiliation_confidence"`
			CNCFContributions     int    `json:"cncf_contributions"`
		} `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, row := range resp.Users {
		if row.GitHubUsername == "octo-dev" {
			if row.AffiliationSource != affiliationSourceColonyGitdm || row.AffiliationConfidence != "high" || row.CNCFContributions != 12 {
				t.Fatalf("admin row = %+v", row)
			}
			return
		}
	}
	t.Fatal("octo-dev missing from admin users payload")
}

// TestColonyGitdmMatchesLocallyByLogin runs the real colony gitdm provider
// against a local fixture server: the hub downloads files and matches the
// login itself, and the only requests made are for the public files.
func TestColonyGitdmMatchesLocallyByLogin(t *testing.T) {
	const file = "developers_affiliations1.txt"
	body := "octo-dev: octo!example.com\n\tPrevious Org until 2020-01-01\n\tExample Org from 2020-01-01\n"
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.String())
		mu.Unlock()
		if r.URL.Path != "/"+file {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := &colonyAffiliator{
		cacheDir: t.TempDir(),
		newGitdm: func(cacheDir string, refresh time.Duration) colonyLookup {
			return gitdm.New(gitdm.Options{BaseURL: srv.URL, AffiliationFiles: []string{file}, AliasFiles: []string{}, CacheDir: cacheDir, Refresh: refresh})
		},
		newDevstats: func() colonyLookup { t.Fatal("devstats must not be built"); return nil },
	}
	got, err := c.lookup(context.Background(), "OCTO-DEV", colonyTestNow, colonyAffiliationConfig{Gitdm: true, GitdmRefresh: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if got.Org != "Example Org" || got.Source != affiliationSourceColonyGitdm || got.Confidence != string(resolver.ConfidenceHigh) {
		t.Fatalf("gitdm lookup = %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range paths {
		if strings.Contains(strings.ToLower(p), "octo") {
			t.Fatalf("a request carried the login: %s", p)
		}
	}
}

func TestAdminUsersAffiliationTooltipShowsColonyConfidence(t *testing.T) {
	html := dashScript(t)
	for _, want := range []string{"u.affiliation_confidence", "' confidence)'", "u.cncf_contributions", "CNCF contributions (devstats): "} {
		if !strings.Contains(html, want) {
			t.Fatalf("affiliation cell missing %q", want)
		}
	}
}
