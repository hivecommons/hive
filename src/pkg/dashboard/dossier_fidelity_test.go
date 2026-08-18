package dashboard

// Tests for the dossier design-fidelity restoration: the generative emblem
// field / hero identity plate / rank metals in the landing HTML, the public
// GitHub enrichment (service years + renown) on the profile response, and the
// real registration-order founding mark.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resetGitHubUserCache clears the enrichment cache between tests.
func resetGitHubUserCache() {
	githubUserCacheMu.Lock()
	githubUserCache = map[string]githubUserCacheEntry{}
	githubUserCacheMu.Unlock()
}

// The profile response carries service_years + renown from the (stubbed)
// public GitHub API, cached server-side.
func TestDossier_GitHubEnrichment(t *testing.T) {
	dir := meSeedDir(t)
	meWriteProfile(t, dir, &ContributorProfile{
		GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "trusted",
		TasksCompleted: 5, RegisteredAt: "2026-01-01T00:00:00Z",
	})

	created := time.Now().AddDate(-9, -1, 0).UTC().Format(time.RFC3339)
	calls := 0
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/users/alice" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"created_at": created, "followers": 214})
	}))
	defer stub.Close()
	origBase := githubUserAPIBase
	githubUserAPIBase = stub.URL
	resetGitHubUserCache()
	defer func() { githubUserAPIBase = origBase; resetGitHubUserCache() }()

	s := covContribServer(t)
	resp := s.BuildContributorProfile("alice")
	if resp.ServiceYears == nil || *resp.ServiceYears != 9 {
		t.Fatalf("service_years: got %v, want 9", resp.ServiceYears)
	}
	if resp.Renown == nil || *resp.Renown != 214 {
		t.Fatalf("renown: got %v, want 214", resp.Renown)
	}
	// Second build must hit the 6h cache, not the stub again.
	s.BuildContributorProfile("alice")
	if calls != 1 {
		t.Fatalf("github fetch calls: got %d, want 1 (cached)", calls)
	}
}

// A failing GitHub fetch omits the fields entirely — never zeros, never fakes.
func TestDossier_GitHubEnrichmentOmittedOnFailure(t *testing.T) {
	dir := meSeedDir(t)
	meWriteProfile(t, dir, &ContributorProfile{
		GitHubUsername: "bob", ContributorID: "c-bob", TrustTier: "newcomer",
		RegisteredAt: "2026-01-02T00:00:00Z",
	})
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer stub.Close()
	origBase := githubUserAPIBase
	githubUserAPIBase = stub.URL
	resetGitHubUserCache()
	defer func() { githubUserAPIBase = origBase; resetGitHubUserCache() }()

	s := covContribServer(t)
	resp := s.BuildContributorProfile("bob")
	if resp.ServiceYears != nil || resp.Renown != nil {
		t.Fatalf("enrichment must be omitted on fetch failure, got %v/%v", resp.ServiceYears, resp.Renown)
	}
	data, _ := json.Marshal(resp)
	if strings.Contains(string(data), "service_years") || strings.Contains(string(data), "renown") {
		t.Fatalf("failed enrichment fields must not serialize: %s", data)
	}
}

// The founding mark reflects REAL registration order (first twenty), and is
// absent when the order cannot be established.
func TestDossier_FoundingPosition(t *testing.T) {
	dir := meSeedDir(t)
	meWriteProfile(t, dir, &ContributorProfile{
		GitHubUsername: "first", ContributorID: "c-1", TrustTier: "trusted",
		RegisteredAt: "2026-01-01T00:00:00Z",
	})
	meWriteProfile(t, dir, &ContributorProfile{
		GitHubUsername: "second", ContributorID: "c-2", TrustTier: "newcomer",
		RegisteredAt: "2026-02-01T00:00:00Z",
	})
	// No parsable RegisteredAt → no founding mark, never faked.
	meWriteProfile(t, dir, &ContributorProfile{
		GitHubUsername: "undated", ContributorID: "c-3", TrustTier: "newcomer",
	})

	origBase := githubUserAPIBase
	githubUserAPIBase = "http://127.0.0.1:0" // unreachable: enrichment degrades
	resetGitHubUserCache()
	defer func() { githubUserAPIBase = origBase; resetGitHubUserCache() }()

	s := covContribServer(t)
	if got := s.BuildContributorProfile("first").FoundingPosition; got != 1 {
		t.Fatalf("first: founding_position=%d, want 1", got)
	}
	if got := s.BuildContributorProfile("second").FoundingPosition; got != 2 {
		t.Fatalf("second: founding_position=%d, want 2", got)
	}
	if got := s.BuildContributorProfile("undated").FoundingPosition; got != 0 {
		t.Fatalf("undated: founding_position=%d, want 0 (skip, do not fake)", got)
	}
}

// The landing HTML carries the restored dossier fidelity: the generative
// emblem field, the hero-name treatment, the rank-metal designations, the
// ceremony ladder, the deeds grid extras, and the theaters rows.
func TestDossier_LandingHTMLFidelity(t *testing.T) {
	meSeedDir(t)
	s := covContribServer(t)
	rec := doGet(s, "/contribute")
	if rec.Code != http.StatusOK {
		t.Fatalf("landing status: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"me-emblem",                     // generative emblem field layer
		"meEmblemProps",                 // deterministic seed → gradient props
		"conic-gradient(from var(--a1)", // the layered emblem background
		"clamp(1.6rem,3.6vw,2.6rem)",    // hero-name treatment (mockup-faithful)
		"dz-heroname",                   // Michroma hero name on the identity plate
		"dz-identity",                   // ZONE A full-width identity plate
		"dz-livebar",                    // ON OPERATION strip
		"dz-grid",                       // the 5fr/7fr sheet grid
		"dz-zone-head",                  // zoned card heads with the metal dot
		"Deeds of Record",
		"Operator Profile",
		"dz-seal",      // Triumphs milestone seals
		"dz-flog",      // Field Log rows
		"dz-footer",    // record footer (HIVE // id + quote)
		"EPIGRAPH",     // configurable epigraph constant
		"FOOTER_QUOTE", // configurable footer quote constant
		"ME_RANK_META", // tier → designation + metal map
		"SPECIALIST",   // a ceremony designation
		"Rank metal",   // style1 renamed from Signal blue
		"--me-metal",   // metal custom prop drives style1
		"me-ladder",    // RECRUIT … PARAGON ladder
		"PARAGON",
		"me-path-reqs",   // req progress bars
		"github service", // deeds grid enrichment blocks
		"renown · followers",
		"Theaters of Operation", // hives zone as theater rows
		"me-founding",           // founding mark (real order only)
		"founding_position",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("landing HTML missing %q", want)
		}
	}
	// Michroma is REQUIRED but must be base64-embedded — never a network font.
	if !strings.Contains(body, "font-family:'Michroma'") || !strings.Contains(body, "data:font/woff2;base64,") {
		t.Fatalf("landing HTML must embed Michroma as a base64 data-URI @font-face")
	}
	if strings.Contains(body, "fonts.googleapis") || strings.Contains(body, "fonts.gstatic") {
		t.Fatalf("landing HTML must not pull network fonts")
	}
}

// The public dossier endpoints reach unauthenticated api.github.com, which
// allows only 60 requests/hour/IP. Without in-flight de-duplication a burst of
// concurrent views of the same contributor fires one outbound request EACH,
// burns the budget, and re-arms every negative-TTL window.
func TestGitHubEnrichmentDeduplicatesConcurrentFetches(t *testing.T) {
	var calls int32
	release := make(chan struct{})
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		<-release // hold the leader open so every racer piles up behind it
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created_at":"2019-01-01T00:00:00Z","followers":7}`))
	}))
	defer stub.Close()

	origBase := githubUserAPIBase
	githubUserAPIBase = stub.URL
	resetGitHubUserCache()
	defer func() { githubUserAPIBase = origBase; resetGitHubUserCache() }()

	const racers = 12
	var wg sync.WaitGroup
	results := make([]bool, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, followers, ok := githubPublicFor("alice")
			results[i] = ok && followers == 7
		}(i)
	}
	time.Sleep(100 * time.Millisecond) // let the racers converge on the key
	close(release)
	wg.Wait()

	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("%d concurrent lookups made %d upstream calls, want exactly 1", racers, n)
	}
	for i, ok := range results {
		if !ok {
			t.Fatalf("racer %d did not receive the leader's result", i)
		}
	}
}
