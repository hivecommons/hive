package convergence_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/audit"
	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/convergence/outcome"
	"github.com/hivecommons/hive/pkg/convergence/proof"
	"github.com/hivecommons/hive/pkg/convergence/publish"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
)

const (
	publishOwner = "maintainer"
	privateRepo  = "hivecommons/security-intake"
)

// fakeGitHub is an httptest GitHub that stores issues per repository. With
// failWrites set, any write is a test failure: that is how the disabled and
// shadow tests prove zero GitHub writes.
type fakeGitHub struct {
	t          *testing.T
	mu         sync.Mutex
	failWrites bool
	requests   int
	writes     int
	issues     map[string][]map[string]any
	next       int
}

func newFakeGitHub(t *testing.T, failWrites bool) (*fakeGitHub, *httptest.Server) {
	t.Helper()
	gh := &fakeGitHub{t: t, failWrites: failWrites, issues: map[string][]map[string]any{}, next: 500}
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	return gh, srv
}

func (g *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests++
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 || parts[0] != "repos" || parts[3] != "issues" {
		http.NotFound(w, r)
		return
	}
	repo := parts[1] + "/" + parts[2]
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(g.issues[repo])
	case http.MethodPost:
		g.writes++
		if g.failWrites {
			g.t.Errorf("GitHub write attempted while publication must be inert: %s %s", r.Method, r.URL.Path)
			http.Error(w, "writes forbidden", http.StatusForbidden)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		g.next++
		issue := map[string]any{
			"number":   g.next,
			"title":    req["title"],
			"body":     req["body"],
			"html_url": fmt.Sprintf("https://github.test/%s/issues/%d", repo, g.next),
			"state":    "open",
		}
		g.issues[repo] = append(g.issues[repo], issue)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(issue)
	default:
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
	}
}

func (g *fakeGitHub) count(repo string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.issues[repo])
}

func (g *fakeGitHub) totalRequests() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requests
}

func issueSeam(t *testing.T, srv *httptest.Server) forge.IssueSeam {
	t.Helper()
	client := github.NewClientForTest(srv.URL, "hivecommons", nil, slog.Default())
	seam := forge.NewGitHubIssueSeam(client, "hivecommons")
	if seam == nil {
		t.Fatal("issue seam not built")
	}
	return seam
}

type recordingAudit struct {
	mu      sync.Mutex
	actions []string
}

func (a *recordingAudit) Record(actor, action, agentName string, fields map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.actions = append(a.actions, action+":"+fmt.Sprint(fields["outcome"]))
}

func (a *recordingAudit) has(entry string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, got := range a.actions {
		if got == entry {
			return true
		}
	}
	return false
}

func publisherFor(t *testing.T, st auditFixtureStores, seam forge.IssueSeam, pol publish.Policy, private publish.PrivateChannel) (*publish.Publisher, *recordingAudit) {
	t.Helper()
	rec := &recordingAudit{}
	return &publish.Publisher{
		Executor:   mutation.Executor{Ledger: st.ledger, Journal: st.journal, Mode: pol.Mode, Now: func() time.Time { return st.now }},
		Issues:     seam,
		Private:    private,
		Proofs:     st.proofs,
		PolicyFunc: func() publish.Policy { return pol },
		Actor:      publishOwner,
		Audit:      rec,
		Now:        func() time.Time { return st.now },
	}, rec
}

type auditFixtureStores struct {
	store    *beads.Store
	ledger   *mutation.Ledger
	journal  *mutation.Journal
	proofs   *proof.Store
	outcomes *outcome.Ledger
	now      time.Time
}

func publishStores(t *testing.T, now time.Time) auditFixtureStores {
	t.Helper()
	store, ledger, journal, proofs := auditStores(t)
	outcomes, err := outcome.Open(filepath.Join(t.TempDir(), "outcomes.json"), outcome.Options{Principals: []string{publishOwner}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return auditFixtureStores{store: store, ledger: ledger, journal: journal, proofs: proofs, outcomes: outcomes, now: now}
}

func runAudit(t *testing.T, st auditFixtureStores, pub audit.FindingPublisher, mode string, now time.Time) (audit.Result, error) {
	t.Helper()
	return audit.Run(audit.Options{
		CampaignKey: "fixture", ScopeDir: "testdata/audit-scope", Store: st.store, Ledger: st.ledger, Journal: st.journal,
		ProofStore: st.proofs, Mode: mode, Generation: 5, Holder: "tester", Now: func() time.Time { return now },
		Publisher: pub, Outcomes: st.outcomes, RunKey: "run-fixture", RunURL: "https://hive.test/runs/run-fixture",
	})
}

func enforcePublishPolicy() publish.Policy {
	return publish.Policy{Enabled: true, ACMMLevel: config.PublicationMinACMMLevel, Mode: config.ConvergenceModeEnforce}
}

func TestAuditPublishDisabledAndShadowPerformNoGitHubWrites(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	for name, pol := range map[string]publish.Policy{
		"disabled": {Enabled: false, ACMMLevel: 6, Mode: config.ConvergenceModeEnforce},
		"shadow":   {Enabled: true, ACMMLevel: 6, Mode: config.ConvergenceModeShadow},
	} {
		t.Run(name, func(t *testing.T) {
			gh, srv := newFakeGitHub(t, true)
			st := publishStores(t, time.Date(2026, 9, 22, 21, 0, 0, 0, time.UTC))
			pub, _ := publisherFor(t, st, issueSeam(t, srv), pol, nil)
			res, err := runAudit(t, st, pub, pol.Mode, st.now)
			if err != nil {
				t.Fatal(err)
			}
			if gh.totalRequests() != 0 {
				t.Fatalf("%s: publisher touched GitHub %d times", name, gh.totalRequests())
			}
			if res.Publication == nil || res.Publication.Accepted {
				t.Fatalf("%s: outcome must stay proposed: %+v", name, res.Publication)
			}
			for hash, p := range res.Publication.Publications {
				if p.State == publish.StatePublished || p.State == publish.StatePrivate {
					t.Fatalf("%s: finding %s was published: %+v", name, hash, p)
				}
			}
			assertPublicationBead(t, st.store, "none=1 withheld=2")
		})
	}
}

func TestAuditPublishFilesEachValidatedFindingOnceAcrossReruns(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	gh, srv := newFakeGitHub(t, false)
	st := publishStores(t, time.Date(2026, 9, 22, 21, 30, 0, 0, time.UTC))
	seam := issueSeam(t, srv)
	pub, rec := publisherFor(t, st, seam, enforcePublishPolicy(), publish.RepoChannel{Repo: privateRepo, Issues: seam})

	res, err := runAudit(t, st, pub, config.ConvergenceModeEnforce, st.now)
	if err != nil {
		t.Fatal(err)
	}
	if gh.count("hivecommons/hive") != 1 || gh.count(privateRepo) != 1 {
		t.Fatalf("public=%d private=%d issues, want 1/1", gh.count("hivecommons/hive"), gh.count(privateRepo))
	}
	if !res.Publication.Accepted || res.Publication.Outcome.State != outcome.StateAccepted {
		t.Fatalf("satisfied campaign must accept its outcome generation: %+v", res.Publication.Outcome)
	}
	if !rec.has(publish.AuditFindingPublished+":"+publish.StatePublished) || !rec.has(publish.AuditFindingDisclosed+":"+publish.StatePrivate) {
		t.Fatalf("audit trail = %v", rec.actions)
	}
	body, _ := gh.issues["hivecommons/hive"][0]["body"].(string)
	if !strings.Contains(body, publish.RunTrailer+": run-fixture") || !strings.Contains(body, publish.MarkerPrefix) || !strings.Contains(body, "https://hive.test/runs/run-fixture") {
		t.Fatalf("published body lacks trailer, marker, or run link:\n%s", body)
	}
	if title, _ := gh.issues["hivecommons/hive"][0]["title"].(string); strings.Contains(strings.ToLower(title), "auth") {
		t.Fatalf("security-labelled finding reached the public repo: %q", title)
	}
	if got := len(st.proofs.List()); got != 8 {
		t.Fatalf("proofs = %d, want 6 inspections + 2 publications", got)
	}
	assertPublicationBead(t, st.store, "none=1 private=1 published=1")

	// Rerun after the campaign claim expired: nothing filed, the existing
	// issues are recorded, the outcome stays accepted.
	res, err = runAudit(t, st, pub, config.ConvergenceModeEnforce, st.now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if gh.count("hivecommons/hive") != 1 || gh.count(privateRepo) != 1 {
		t.Fatalf("rerun filed again: public=%d private=%d", gh.count("hivecommons/hive"), gh.count(privateRepo))
	}
	if res.Publication.Accepted || res.Publication.Outcome.State != outcome.StateAccepted || res.Publication.Outcome.Generation != 1 {
		t.Fatalf("rerun outcome = %+v", res.Publication.Outcome)
	}
	assertPublicationBead(t, st.store, "existing=1 none=1 private=1")
}

func TestAuditPublishSensitiveWithoutChannelRefusesAndKeepsPublicRepoClean(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	gh, srv := newFakeGitHub(t, false)
	st := publishStores(t, time.Date(2026, 9, 22, 22, 0, 0, 0, time.UTC))
	pub, rec := publisherFor(t, st, issueSeam(t, srv), enforcePublishPolicy(), nil)
	res, err := runAudit(t, st, pub, config.ConvergenceModeEnforce, st.now)
	if !errors.Is(err, publish.ErrNoPrivateChannel) {
		t.Fatalf("campaign must surface the refusal, got %v", err)
	}
	if gh.count("hivecommons/hive") != 1 {
		t.Fatalf("public issues = %d, want only the non-sensitive finding", gh.count("hivecommons/hive"))
	}
	for _, issue := range gh.issues["hivecommons/hive"] {
		if title, _ := issue["title"].(string); strings.Contains(strings.ToLower(title), "auth") {
			t.Fatalf("sensitive finding filed publicly: %q", title)
		}
	}
	if !rec.has(publish.AuditFindingRefused + ":" + publish.ReasonNoPrivateChannel) {
		t.Fatalf("refusal not audited: %v", rec.actions)
	}
	if res.Publication == nil || res.Publication.Accepted {
		t.Fatalf("refused finding must leave the outcome unaccepted: %+v", res.Publication)
	}
	assertPublicationBead(t, st.store, "none=1 published=1 refused=1")
}

func TestAuditPublishBelowLevelRefusesEverything(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	gh, srv := newFakeGitHub(t, true)
	st := publishStores(t, time.Date(2026, 9, 22, 22, 30, 0, 0, time.UTC))
	pol := enforcePublishPolicy()
	pol.ACMMLevel = config.PublicationMinACMMLevel - 1
	pub, rec := publisherFor(t, st, issueSeam(t, srv), pol, nil)
	_, err := runAudit(t, st, pub, config.ConvergenceModeEnforce, st.now)
	if !errors.Is(err, publish.ErrLevelBelowFloor) {
		t.Fatalf("level below floor must refuse, got %v", err)
	}
	if gh.totalRequests() != 0 {
		t.Fatalf("refused publisher touched GitHub %d times", gh.totalRequests())
	}
	if !rec.has(publish.AuditFindingRefused + ":" + publish.ReasonLevel) {
		t.Fatalf("refusal not audited: %v", rec.actions)
	}
	assertPublicationBead(t, st.store, "none=1 refused=2")
}

func TestAuditWithoutPublisherKeepsPublicationNone(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "")
	st := publishStores(t, time.Date(2026, 9, 22, 23, 0, 0, 0, time.UTC))
	res, err := runAudit(t, st, nil, config.ConvergenceModeShadow, st.now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Publication != nil {
		t.Fatalf("no publisher must mean no publication result: %+v", res.Publication)
	}
	assertPublicationNone(t, st.store)
}

func assertPublicationBead(t *testing.T, store *beads.Store, want string) {
	t.Helper()
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Meta("audit_kind") == "publication" {
			if got := b.Meta("publication_state"); got != want {
				t.Fatalf("publication bead = %q, want %q", got, want)
			}
			return
		}
	}
	t.Fatal("publication bead not found")
}
