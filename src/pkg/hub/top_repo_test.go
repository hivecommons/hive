package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTopRepoEventsAggregateAffiliationByOwner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/alice/events/public" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("page") != "1" {
			_ = json.NewEncoder(w).Encode([]any{})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"type":"PushEvent","created_at":"2026-09-24T10:00:00Z","repo":{"name":"kubestellar/console"},"payload":{"commits":[{},{}]}},
			{"type":"PullRequestEvent","created_at":"2026-09-24T11:00:00Z","repo":{"name":"kubestellar/docs"},"payload":{}},
			{"type":"IssueCommentEvent","created_at":"2026-09-24T12:00:00Z","repo":{"name":"alice/personal"},"payload":{}},
			{"type":"WatchEvent","created_at":"2026-09-24T13:00:00Z","repo":{"name":"noise/noise"},"payload":{}}
		]`))
	}))
	defer srv.Close()

	got, err := (topRepoResolver{client: srv.Client()}).resolveEvents(context.Background(), topRepoProfile{
		Login:       "alice",
		APIBase:     srv.URL,
		WebBase:     "https://github.example",
		AllowEvents: true,
	})
	if err != nil {
		t.Fatalf("resolveEvents: %v", err)
	}
	if got.TopRepo != "kubestellar/console" || got.TopRepoURL != "https://github.example/kubestellar/console" {
		t.Fatalf("top repo = %+v, want kubestellar/console on github.example", got)
	}
	if org := got.TopOrgExcludingLogin("alice"); org != "kubestellar" {
		t.Fatalf("TopOrgExcludingLogin = %q, want kubestellar", org)
	}
}

func TestTopRepoResolvePrefersCompanyThenGraphQLRepoDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/alice":
			_ = json.NewEncoder(w).Encode(map[string]string{"company": "@ibm", "bio": "Works on Kubernetes", "location": "Austin, TX"})
		case "/users/alice/orgs":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"login": "fallback-org"}})
		case "/graphql":
			if got := r.Header.Get("Authorization"); got != "Bearer token123" {
				t.Fatalf("Authorization = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"user":{"contributionsCollection":{"commitContributionsByRepository":[{"repository":{"nameWithOwner":"kubestellar/console","url":"https://github.example/kubestellar/console"},"contributions":{"totalCount":7,"nodes":[{"occurredAt":"2026-09-24T00:00:00Z"}]}}]}}}}`))
		case "/users/alice/events/public":
			if r.URL.Query().Get("page") == "1" {
				_, _ = w.Write([]byte(`[
					{"type":"WatchEvent","created_at":"2026-09-20T00:00:00Z","repo":{"name":"noise/one"},"payload":{}},
					{"type":"WatchEvent","created_at":"2026-09-21T00:00:00Z","repo":{"name":"noise/two"},"payload":{}},
					{"type":"WatchEvent","created_at":"2026-09-22T00:00:00Z","repo":{"name":"noise/three"},"payload":{}},
					{"type":"WatchEvent","created_at":"2026-09-23T00:00:00Z","repo":{"name":"noise/four"},"payload":{}},
					{"type":"WatchEvent","created_at":"2026-09-24T00:00:00Z","repo":{"name":"noise/five"},"payload":{}}
				]`))
				return
			}
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	got, err := (topRepoResolver{
		client: srv.Client(),
		clock: func() time.Time {
			return time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
		},
	}).resolve(context.Background(), topRepoProfile{
		Login:       "alice",
		APIBase:     srv.URL,
		WebBase:     "https://github.example",
		GraphQLURL:  srv.URL + "/graphql",
		Token:       "token123",
		AllowEvents: true,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Affiliation != "ibm" || got.AffiliationSource != affiliationSourceCompany {
		t.Fatalf("affiliation = %q/%q, want ibm/company_field", got.Affiliation, got.AffiliationSource)
	}
	if got.TopRepo != "kubestellar/console" || got.TopRepoURL != "https://github.example/kubestellar/console" {
		t.Fatalf("top repo detail = %+v", got)
	}
	if got.ProfileBio != "Works on Kubernetes" || got.ProfileLocation != "Austin, TX" || got.PublicActivity != publicActivityActive {
		t.Fatalf("profile context/activity = %+v, want bio/location/active", got)
	}
}

func TestTopRepoResolveFallsBackToContributionOrgThenMembership(t *testing.T) {
	tests := []struct {
		name       string
		eventsJSON string
		orgsJSON   string
		wantAff    string
		wantSource string
	}{
		{
			name:       "contribution owner beats org membership",
			eventsJSON: `[{"type":"PullRequestEvent","created_at":"2026-09-24T11:00:00Z","repo":{"name":"kubestellar/console"},"payload":{}}]`,
			orgsJSON:   `[{"login":"fallback-org"}]`,
			wantAff:    "kubestellar",
			wantSource: affiliationSourceContributions,
		},
		{
			name:       "org membership when only personal contributions",
			eventsJSON: `[{"type":"PullRequestEvent","created_at":"2026-09-24T11:00:00Z","repo":{"name":"alice/personal"},"payload":{}}]`,
			orgsJSON:   `[{"login":"fallback-org"}]`,
			wantAff:    "fallback-org",
			wantSource: affiliationSourceOrgMembership,
		},
		{
			name:       "personal owner last resort",
			eventsJSON: `[{"type":"PullRequestEvent","created_at":"2026-09-24T11:00:00Z","repo":{"name":"alice/personal"},"payload":{}}]`,
			orgsJSON:   `[]`,
			wantAff:    "alice",
			wantSource: affiliationSourceContributions,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/users/alice":
					_ = json.NewEncoder(w).Encode(map[string]string{"company": ""})
				case "/users/alice/orgs":
					_, _ = w.Write([]byte(tc.orgsJSON))
				case "/users/alice/events/public":
					if r.URL.Query().Get("page") == "1" {
						_, _ = w.Write([]byte(tc.eventsJSON))
						return
					}
					_ = json.NewEncoder(w).Encode([]any{})
				default:
					t.Fatalf("unexpected path %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			got, err := (topRepoResolver{client: srv.Client()}).resolve(context.Background(), topRepoProfile{Login: "alice", APIBase: srv.URL, WebBase: "https://github.example", AllowEvents: true})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.Affiliation != tc.wantAff || got.AffiliationSource != tc.wantSource {
				t.Fatalf("affiliation = %q/%q, want %q/%q", got.Affiliation, got.AffiliationSource, tc.wantAff, tc.wantSource)
			}
		})
	}
}

func TestTopRepoResolveKeepsZeroActivityProfileContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/alice":
			_ = json.NewEncoder(w).Encode(map[string]string{"company": "", "bio": "Student exploring Kubernetes", "location": "Toronto"})
		case "/users/alice/orgs":
			_ = json.NewEncoder(w).Encode([]any{})
		case "/users/alice/events/public":
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	got, err := (topRepoResolver{client: srv.Client(), clock: func() time.Time {
		return time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	}}).resolve(context.Background(), topRepoProfile{Login: "alice", APIBase: srv.URL, WebBase: "https://github.example", AllowEvents: true})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Affiliation != "" || got.TopRepo != "" {
		t.Fatalf("zero-activity profile should not invent affiliation/repo: %+v", got)
	}
	if got.ProfileBio != "Student exploring Kubernetes" || got.ProfileLocation != "Toronto" || got.PublicActivity != publicActivityQuiet {
		t.Fatalf("zero-activity context = %+v, want bio/location/quiet", got)
	}
}

func TestTopRepoResolveKeepsCompanyWhenEventsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/alice":
			_ = json.NewEncoder(w).Encode(map[string]string{"company": "@redhat"})
		case "/users/alice/orgs":
			_ = json.NewEncoder(w).Encode([]any{})
		case "/users/alice/events/public":
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	got, err := (topRepoResolver{client: srv.Client()}).resolve(context.Background(), topRepoProfile{Login: "alice", APIBase: srv.URL, WebBase: "https://github.example", AllowEvents: true})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Affiliation != "redhat" || got.AffiliationSource != affiliationSourceCompany {
		t.Fatalf("affiliation = %q/%q, want redhat/company_field", got.Affiliation, got.AffiliationSource)
	}
	if got.PublicActivity != "" {
		t.Fatalf("activity should be omitted when events are rate-limited, got %+v", got)
	}
}

func TestTopRepoTransientProviderErrorDoesNotClassifyAsNoData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := (topRepoResolver{client: srv.Client()}).resolveEvents(context.Background(), topRepoProfile{
		Login:       "alice",
		APIBase:     srv.URL,
		WebBase:     "https://github.example",
		AllowEvents: true,
	})
	if err == nil {
		t.Fatal("expected provider error")
	}
	if errors.Is(err, errTopRepoNoData) {
		t.Fatalf("transient provider error must preserve cached top repo, got no-data error: %v", err)
	}
}

func TestPublicActivityBucket(t *testing.T) {
	tests := []struct {
		events int
		want   string
	}{
		{events: 0, want: publicActivityQuiet},
		{events: topRepoActivityActive - 1, want: publicActivityQuiet},
		{events: topRepoActivityActive, want: publicActivityActive},
		{events: topRepoActivityProlific, want: publicActivityProlific},
	}
	for _, tc := range tests {
		if got := publicActivityBucket(tc.events); got != tc.want {
			t.Fatalf("publicActivityBucket(%d) = %q, want %q", tc.events, got, tc.want)
		}
	}
}

func TestTopRepoProfileResolvesIBMidGHEEndpointAndIDPFallback(t *testing.T) {
	s := &HubServer{
		envGitHubToken: "ghe-token",
		clusters: map[string]ClusterConfig{
			"ibm": {GitHubBaseURL: "https://github.ibm.com", GitHubAPIURL: "https://github.ibm.com/api/v3"},
		},
	}
	profile := s.topRepoProfile(&SaaSUser{
		GitHubUsername:    "ibmid:650001ABCD",
		CanonicalID:       "ibmid:650001ABCD",
		Provider:          "ibmid",
		LinkedGitHubLogin: "jane",
	})
	if profile.Login != "jane" || profile.APIBase != "https://github.ibm.com/api/v3" || profile.WebBase != "https://github.ibm.com" || profile.Token != "ghe-token" || profile.IDPFallback != "IBM" || profile.GHEFallback != "IBM" {
		t.Fatalf("GHE profile = %+v", profile)
	}

	withoutToken := (&HubServer{clusters: s.clusters}).topRepoProfile(&SaaSUser{
		GitHubUsername:    "ibmid:650001ABCD",
		CanonicalID:       "ibmid:650001ABCD",
		Provider:          "ibmid",
		LinkedGitHubLogin: "jane",
	})
	if withoutToken.IDPFallback != "IBM" || withoutToken.GHEFallback != "IBM" || withoutToken.Token != "" {
		t.Fatalf("IBMid without GHE token should keep IBM fallback only: %+v", withoutToken)
	}
	assoc, err := (topRepoResolver{}).resolve(context.Background(), withoutToken)
	if err != nil {
		t.Fatal(err)
	}
	if assoc.Affiliation != "IBM" || assoc.AffiliationSource != affiliationSourceIDP {
		t.Fatalf("fallback association = %+v, want IBM/idp", assoc)
	}
}

func TestLegacyTopRepoOnlyCacheIsAffiliationRefreshDue(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	u := &SaaSUser{TopRepo: "kubestellar/console", TopRepoUpdatedAt: now.Format(time.RFC3339)}
	if !topRepoRefreshDue(u, now) {
		t.Fatal("top-repo-only cache from the previous implementation must refresh immediately for affiliation fields")
	}
	assoc := userTopRepoAssociation(u, nil)
	if assoc.Affiliation != "kubestellar" || assoc.AffiliationSource != affiliationSourceContributions {
		t.Fatalf("legacy association fallback = %+v, want kubestellar/contributions", assoc)
	}
}

func TestTopRepoRefreshDueUsesCachedTimestamp(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	fresh := &SaaSUser{TopRepo: "acme/repo", Affiliation: "acme", AffiliationSource: affiliationSourceContributions, TopRepoUpdatedAt: now.Add(-topRepoRefreshInterval + time.Minute).Format(time.RFC3339)}
	if topRepoRefreshDue(fresh, now) {
		t.Fatal("fresh cached affiliation should not refresh")
	}
	stale := &SaaSUser{TopRepo: "acme/repo", Affiliation: "acme", AffiliationSource: affiliationSourceContributions, TopRepoUpdatedAt: now.Add(-topRepoRefreshInterval - time.Minute).Format(time.RFC3339)}
	if !topRepoRefreshDue(stale, now) {
		t.Fatal("stale cached affiliation should refresh")
	}
	if !topRepoRefreshDue(&SaaSUser{}, now) {
		t.Fatal("missing timestamp should refresh")
	}
}

func TestSaveUserTopRepoCacheMergesLatestRecord(t *testing.T) {
	useTempUserDir(t)
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "alice", FullName: "new contact value"}); err != nil {
		t.Fatal(err)
	}
	stale := &SaaSUser{GitHubUsername: "alice", FullName: "stale contact value"}
	if err := saveUserTopRepoCache(stale, topRepoAssociation{TopRepo: "acme/repo", TopRepoURL: "https://github.com/acme/repo", Affiliation: "acme", AffiliationSource: affiliationSourceContributions, ProfileBio: "OSS maintainer", ProfileLocation: "Raleigh", PublicActivity: publicActivityProlific}, now); err != nil {
		t.Fatal(err)
	}
	got := loadSaaSUser("alice")
	if got == nil {
		t.Fatal("alice missing")
	}
	if got.FullName != "new contact value" {
		t.Fatalf("FullName was overwritten by stale refresh copy: %q", got.FullName)
	}
	if got.TopRepo != "acme/repo" || got.TopRepoURL != "https://github.com/acme/repo" || got.Affiliation != "acme" || got.AffiliationSource != affiliationSourceContributions || got.TopRepoUpdatedAt != now.Format(time.RFC3339) {
		t.Fatalf("affiliation cache not persisted: %+v", got)
	}
	if got.ProfileBio != "OSS maintainer" || got.ProfileLocation != "Raleigh" || got.PublicActivity != publicActivityProlific {
		t.Fatalf("profile context/activity cache not persisted: %+v", got)
	}
}

func TestAdminUsersCarriesCachedAffiliation(t *testing.T) {
	s := sessionTestHub()
	useTempUserDir(t)
	updated := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "alice", Affiliation: "acme", AffiliationSource: affiliationSourceCompany, ProfileBio: "Kubernetes SIG contributor", ProfileLocation: "Boston", PublicActivity: publicActivityActive, TopRepo: "acme/profile", TopRepoURL: "https://github.com/acme/profile", TopRepoUpdatedAt: updated}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	s.handleAdminUsers(rec, httptest.NewRequest("GET", "/api/saas/admin/users", nil))
	var resp struct {
		Users []struct {
			GitHubUsername    string `json:"github_username"`
			Affiliation       string `json:"affiliation"`
			AffiliationSource string `json:"affiliation_source"`
			ProfileBio        string `json:"profile_bio"`
			ProfileLocation   string `json:"profile_location"`
			PublicActivity    string `json:"public_activity"`
			TopRepo           string `json:"top_repo"`
			TopRepoURL        string `json:"top_repo_url"`
		} `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad admin users payload: %v", err)
	}
	for _, u := range resp.Users {
		if u.GitHubUsername == "alice" {
			if u.Affiliation != "acme" || u.AffiliationSource != affiliationSourceCompany || u.TopRepo != "acme/profile" || u.TopRepoURL != "https://github.com/acme/profile" {
				t.Fatalf("cached affiliation = %+v", u)
			}
			if u.ProfileBio != "Kubernetes SIG contributor" || u.ProfileLocation != "Boston" || u.PublicActivity != publicActivityActive {
				t.Fatalf("cached profile context/activity = %+v", u)
			}
			return
		}
	}
	t.Fatal("alice missing from admin users payload")
}

func TestAdminUsersTableAffiliationColumn(t *testing.T) {
	html := dashScript(t)
	if !strings.Contains(html, `sortUsers(\'affiliation\')`) {
		t.Fatal("Affiliation header is not wired into the existing sortUsers mechanism")
	}
	if !strings.Contains(html, "'<td>' + affiliationCell(u) + '</td>'") {
		t.Fatal("admin users rows do not render affiliationCell")
	}
	if !strings.Contains(html, "AFFILIATION ⇅") {
		t.Fatal("admin users header was not renamed to AFFILIATION")
	}
	for _, want := range []string{"u.public_activity", "u.profile_bio", "u.profile_location", "Public GitHub activity"} {
		if !strings.Contains(html, want) {
			t.Fatalf("affiliation cell missing %q", want)
		}
	}
	if strings.Contains(html, `onclick="sortUsers(\'affiliation\')" style=`) {
		t.Fatal("Affiliation header added a new inline style attribute")
	}
}
