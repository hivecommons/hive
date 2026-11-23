package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/customers"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
)

func sourceNames(src []customers.ProfileLookup) []string {
	var out []string
	for _, s := range src {
		out = append(out, s.Source())
	}
	return out
}

func TestCustomersProfileSourcesFollowTheForge(t *testing.T) {
	gh := github.NewClient("t", "acme", nil, discardLogger(), "")
	cfg := &config.Config{}
	deps := customersProfileFillDeps{
		cfg:    func() *config.Config { return cfg },
		client: func() *github.Client { return gh },
		orgs:   newCustomersOrgNames(),
	}
	if got := sourceNames(customersProfileSources(deps)); len(got) != 1 || got[0] != customers.ProfileSourceGitHub {
		t.Fatalf("github hive sources = %v", got)
	}
	cfg.GitHub.BaseURL = "https://github.example.com"
	if got := sourceNames(customersProfileSources(deps)); len(got) != 1 || got[0] != customers.ProfileSourceGHE {
		t.Fatalf("GHE hive sources = %v", got)
	}
	cfg.GitHub.BaseURL = ""
	cfg.Project.Forge = config.ForgeGitLab
	cfg.GitLab.URL = "https://gitlab.example.com"
	if got := sourceNames(customersProfileSources(deps)); len(got) != 1 || got[0] != customers.ProfileSourceGitLab {
		t.Fatalf("gitlab hive sources = %v", got)
	}
	cfg.Project.Forge = config.ForgeGitea
	cfg.Gitea.URL = "https://codeberg.example.org"
	if got := sourceNames(customersProfileSources(deps)); len(got) != 1 || got[0] != customers.ProfileSourceGitea {
		t.Fatalf("gitea hive sources = %v", got)
	}
	// A Gitea forge with no URL cannot be built: no forge source, no panic.
	cfg.Gitea.URL = ""
	if got := customersProfileSources(deps); len(got) != 0 {
		t.Fatalf("unbuildable forge sources = %v", sourceNames(got))
	}
	// A connected Linear workspace adds the member-name source last.
	cfg.Project.Forge = ""
	deps.linearToken = func() string { return "lin" }
	if got := sourceNames(customersProfileSources(deps)); len(got) != 2 || got[1] != customers.ProfileSourceLinear {
		t.Fatalf("with Linear = %v", got)
	}
	// No GitHub client on a GitHub hive: only Linear.
	deps.client = func() *github.Client { return nil }
	if got := sourceNames(customersProfileSources(deps)); len(got) != 1 || got[0] != customers.ProfileSourceLinear {
		t.Fatalf("no client = %v", got)
	}
}

func TestGitHubProfileLookupMapsOrgHandleOnceADay(t *testing.T) {
	var orgCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/users/ada", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"login":"ada","name":"Ada Lovelace","company":"@acme","location":"London, UK"}`))
	})
	mux.HandleFunc("/users/bob", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"login":"bob","company":"@someuser"}`))
	})
	mux.HandleFunc("/orgs/acme", func(w http.ResponseWriter, r *http.Request) {
		orgCalls.Add(1)
		_, _ = w.Write([]byte(`{"login":"acme","name":"Acme Corporation"}`))
	})
	mux.HandleFunc("/orgs/someuser", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	mux.HandleFunc("/users/ghost", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	gh := github.NewClientForTest(srv.URL, "acme", nil, discardLogger())
	l := githubProfileLookup{client: func() *github.Client { return gh }, source: customers.ProfileSourceGitHub, orgs: newCustomersOrgNames()}
	p, err := l.Lookup(context.Background(), "ada")
	if err != nil || p.Company != "Acme Corporation" || p.Requests != 2 || p.Location != "London, UK" {
		t.Fatalf("first lookup = %+v, %v", p, err)
	}
	p, err = l.Lookup(context.Background(), "ada")
	if err != nil || p.Company != "Acme Corporation" || p.Requests != 1 || orgCalls.Load() != 1 {
		t.Fatalf("cached lookup = %+v, %v (org calls %d)", p, err, orgCalls.Load())
	}
	// A user handle in the company field is kept as written.
	if p, err := l.Lookup(context.Background(), "bob"); err != nil || p.Company != "@someuser" {
		t.Fatalf("user handle = %+v, %v", p, err)
	}
	if _, err := l.Lookup(context.Background(), "ghost"); !errors.Is(err, customers.ErrProfileNotFound) {
		t.Fatalf("unknown login err = %v", err)
	}
}

func TestCustomersOrgHandle(t *testing.T) {
	for in, want := range map[string]string{"@acme": "acme", " @Red-Hat ": "Red-Hat", "Acme": "", "@acme, @globex": "", "@": "", "@a/b": ""} {
		if got := customersOrgHandle(in); got != want {
			t.Errorf("customersOrgHandle(%q) = %q, want %q", in, got, want)
		}
	}
}

// End to end over a GitLab hive: "Refresh from profiles" on one customer
// fills the empty fields with gitlab_profile provenance through the GitLab
// adapter, and the switch being off refuses with 503-class unavailability.
func TestCustomersRefreshProfileOverGitLab(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/api/v4/users":
			_, _ = w.Write([]byte(`[{"id": 7, "username": "ada"}]`))
		case "/api/v4/users/7":
			_, _ = w.Write([]byte(`{"username":"ada","name":"Ada Lovelace","organization":"Analytical Engines","location":"Berlin, Germany"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := wireTestCustomers(t)
	on := false
	cfg := &config.Config{}
	cfg.Project.Forge = config.ForgeGitLab
	cfg.GitLab.URL = srv.URL
	cfg.Customers.ProfileFill.Enabled = &on
	t.Setenv(config.EnvCustomersProfileFillEnabled, "")
	c.profileFill = newCustomersProfileFill(c.svc.Store(), customersProfileFillDeps{
		cfg:  func() *config.Config { return cfg },
		orgs: newCustomersOrgNames(),
	})
	ada, err := c.svc.Store().Add("ada", []customers.Identity{{Kind: customers.KindGitHub, Value: "ada"}}, "olivia")
	if err != nil {
		t.Fatal(err)
	}
	owner := dashboard.CustomersViewer{User: "olivia", Full: true, Owner: true}
	if _, _, err := c.RefreshProfile(context.Background(), owner, ada.ID); !errors.Is(err, dashboard.ErrCustomerUnavailable) || calls.Load() != 0 {
		t.Fatalf("off: err = %v, requests %d", err, calls.Load())
	}
	on = true
	if _, _, err := c.RefreshProfile(context.Background(), owner, ada.ID); err != nil {
		t.Fatal(err)
	}
	p, err := c.svc.Store().Get(ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Profile.Organization.Value != "Analytical Engines" || p.Profile.Organization.Rule != customers.ProfileSourceGitLab || p.Profile.Organization.Login != "ada" {
		t.Fatalf("organization = %+v", p.Profile.Organization)
	}
	if p.Profile.Country.Value != "DE" {
		t.Fatalf("country = %+v", p.Profile.Country)
	}
	if calls.Load() != 2 {
		t.Fatalf("requests = %d, want 2", calls.Load())
	}
}
