package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// coverageServer serves GET /installation/repositories with the given repo
// full-names, paginating at perPage so the walk is exercised for real.
func coverageServer(t *testing.T, full []string, perPage int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		page := 1
		fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
		if page < 1 {
			page = 1
		}
		start := (page - 1) * perPage
		if start > len(full) {
			start = len(full)
		}
		end := start + perPage
		if end > len(full) {
			end = len(full)
		}
		repos := make([]map[string]any, 0, end-start)
		for _, f := range full[start:end] {
			repos = append(repos, map[string]any{"full_name": f})
		}
		if end < len(full) {
			w.Header().Set("Link", fmt.Sprintf(`<%s?page=%d>; rel="next"`, r.URL.Path, page+1))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total_count":  len(full),
			"repositories": repos,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func coverageAuth(t *testing.T, apiURL string) *AppAuth {
	t.Helper()
	a := newTestAppAuth(t, filepath.Join(t.TempDir(), "token-cache.json"))
	a.apiURL = apiURL
	// A cached token short-circuits minting, so these tests exercise the
	// listing rather than the JWT path.
	a.cachedToken = "test-installation-token"
	a.tokenExpiry = time.Now().Add(time.Hour)
	return a
}

// The live case from #4360: org AI-native-Systems-Research, one installation
// covering two storage repos, and the hive configured for a third repo that is
// not ticked. The comparison must yield exactly that repo.
func TestInstallationCoverage_LiveHeadwatersCase(t *testing.T) {
	const org = "AI-native-Systems-Research"
	srv := coverageServer(t, []string{
		org + "/ai-native-storage-certus",
		org + "/ai-native-storage-certus-workbench",
	}, 100)

	cov, err := coverageAuth(t, srv.URL+"/").InstallationCoverage(context.Background())
	if err != nil {
		t.Fatalf("InstallationCoverage: %v", err)
	}

	missing := cov.Missing(org, []string{"headwaters"})
	want := []string{"ai-native-systems-research/headwaters"}
	if !reflect.DeepEqual(missing, want) {
		t.Fatalf("missing = %v, want %v", missing, want)
	}

	// The covered ones must not be accused.
	if got := cov.Missing(org, []string{"ai-native-storage-certus"}); len(got) != 0 {
		t.Fatalf("a covered repo was reported missing: %v", got)
	}
}

func TestInstallationCoverage_PaginatesAndComparesCaseInsensitively(t *testing.T) {
	srv := coverageServer(t, []string{
		"acme/one", "acme/two", "acme/Three", "acme/four", "acme/five",
	}, 2)

	cov, err := coverageAuth(t, srv.URL+"/").InstallationCoverage(context.Background())
	if err != nil {
		t.Fatalf("InstallationCoverage: %v", err)
	}
	if cov.Truncated {
		t.Fatal("five repos over three pages must not be truncated")
	}
	if len(cov.Repos) != 5 {
		t.Fatalf("got %d repos across pages, want 5", len(cov.Repos))
	}
	// GitHub repo names are case-insensitive; a config that spells one
	// differently must not be accused.
	if got := cov.Missing("acme", []string{"THREE", "acme/One"}); len(got) != 0 {
		t.Fatalf("case difference reported as missing: %v", got)
	}
}

// A truncated listing must accuse nobody. Absence from a partial set is not
// evidence of absence, and a false accusation here sends an operator to change
// a setting that was already correct.
func TestInstallationCoverage_TruncatedAccusesNobody(t *testing.T) {
	full := make([]string, 0, coveragePageSize*(coverageMaxPages+2))
	for i := 0; i < cap(full); i++ {
		full = append(full, fmt.Sprintf("acme/repo-%d", i))
	}
	srv := coverageServer(t, full, coveragePageSize)

	cov, err := coverageAuth(t, srv.URL+"/").InstallationCoverage(context.Background())
	if err != nil {
		t.Fatalf("InstallationCoverage: %v", err)
	}
	if !cov.Truncated {
		t.Fatal("a listing beyond the page cap must report Truncated")
	}
	if got := cov.Missing("acme", []string{"definitely-not-listed"}); got != nil {
		t.Fatalf("truncated coverage reported %v missing; it must report nothing", got)
	}
}

// "We could not ask" must never become "it covers nothing", which would accuse
// every configured repo at once.
func TestInstallationCoverage_ErrorIsNotAnEmptySet(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/installation/repositories", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := coverageAuth(t, srv.URL+"/").InstallationCoverage(context.Background())
	if err == nil {
		t.Fatal("a failed listing must return an error, not an empty coverage set")
	}

	var nilAuth *AppAuth
	if _, err := nilAuth.InstallationCoverage(context.Background()); err == nil {
		t.Fatal("a nil AppAuth must error rather than report empty coverage")
	}
}

func TestNormalizeRepoRef(t *testing.T) {
	for _, tc := range []struct{ owner, ref, want string }{
		{"Acme", "widget", "acme/widget"},
		{"Acme", "Other/Widget", "other/widget"},
		{"", "widget", "widget"},
		{"Acme", "  spaced  ", "acme/spaced"},
		{"Acme", "", ""},
	} {
		if got := NormalizeRepoRef(tc.owner, tc.ref); got != tc.want {
			t.Errorf("NormalizeRepoRef(%q, %q) = %q, want %q", tc.owner, tc.ref, got, tc.want)
		}
	}
}

func TestRepoNotCoveredMessage(t *testing.T) {
	d := AppAuthDiagnosis{
		State:           AppStateRepoNotCovered,
		ExpectedAccount: "AI-native-Systems-Research",
		InstallationID:  140943481,
		APIURL:          "https://api.github.com/",
		Repos:           []string{"ai-native-systems-research/headwaters"},
	}
	msg := d.Message()

	for _, want := range []string{
		"ai-native-systems-research/headwaters",
		"https://github.com/organizations/AI-native-Systems-Research/settings/installations/140943481",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
	// The whole point is not repeating the misdiagnosis this replaces: the
	// live report blamed an undelivered private key and told the operator to
	// re-upload it, which could not possibly have helped.
	for _, never := range []string{
		"has not reached this spoke",
		"re-upload",
		"not installed on",
		"Issues: Read & Write required",
		"contact your hub administrator",
	} {
		if strings.Contains(msg, never) {
			t.Errorf("message wrongly blames %q:\n%s", never, msg)
		}
	}
	// It should say plainly what is NOT at fault, because that is the part the
	// operator got wrong last time.
	if !strings.Contains(msg, "Nothing is wrong with the App, the organization, or the private key") {
		t.Errorf("message does not exonerate the credentials:\n%s", msg)
	}

	// GitHub Enterprise must not be linked to github.com.
	d.APIURL = "https://ghe.example.com/api/v3/"
	if got := d.InstallationSettingsURL(); got != "https://ghe.example.com/organizations/AI-native-Systems-Research/settings/installations/140943481" {
		t.Errorf("enterprise settings URL = %q", got)
	}

	// Not enough information for a correct link means no link at all.
	d.InstallationID = 0
	if got := d.InstallationSettingsURL(); got != "" {
		t.Errorf("expected no link without an installation id, got %q", got)
	}
}

func TestRepoNotCoveredStateWiring(t *testing.T) {
	if got := AppStateRepoNotCovered.String(); got != "repo-not-covered" {
		t.Fatalf("wire token = %q", got)
	}
	if got := ParseAppAuthState("repo-not-covered"); got != AppStateRepoNotCovered {
		t.Fatalf("round trip = %v", got)
	}
	if !AppStateRepoNotCovered.UserActionable() {
		t.Error("an org owner can tick the repo, so this must be user-actionable")
	}
	if AppStateRepoNotCovered.OperatorActionable() {
		t.Error("the hub operator cannot fix repository access; this must not be operator-actionable")
	}
}
