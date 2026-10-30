package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHubContributeAppleMLXSetup(t *testing.T) {
	t.Setenv("HIVE_HUB_PUBLIC_URL", "https://hive.hivecommons.dev")
	s := newHubServerForTest(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://hive.hivecommons.dev/contribute", nil)
	s.handleContributeLanding(rec, req)
	for _, want := range []string{
		`id="apple-mlx"`, "Apple Silicon only", "just contribute-mlx-serve",
		"just contribute-mlx-setup", "--verify-tools", "just contribute-setup pi",
		"just contribute-mlx", "#apple-mlx-local-inference",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("hub contributor MLX setup missing %q", want)
		}
	}
}

func TestAppleMLXIsLocalInferenceIntegrationNotSignIn(t *testing.T) {
	page, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	body := string(page)
	start := strings.Index(body, `<p class="stack-group-label">Local AI inference</p>`)
	if start < 0 {
		t.Fatal("public homepage missing local AI inference group")
	}
	group := strings.SplitN(body[start:], `</div>`+"\n        </div>", 2)[0]
	for _, want := range []string{`href="/contribute#apple-mlx"`, "Apple MLX", "Apple Silicon", "Pi coding agent", "not Apple", "just"} {
		if !strings.Contains(strings.ToLower(group), strings.ToLower(want)) {
			t.Errorf("local inference integration missing %q", want)
		}
	}
	signIn := strings.SplitN(strings.SplitN(body, `<p class="stack-group-label">Sign-in</p>`, 2)[1], `</div>`, 2)[0]
	if strings.Contains(signIn, "Apple MLX") {
		t.Error("Apple MLX must not be presented as Apple sign-in")
	}
}

func TestHubContributeLandingListsPublicContributorHives(t *testing.T) {
	t.Setenv("HIVE_HUB_PUBLIC_URL", "https://hive.hivecommons.dev")
	s := newHubServerForTest(t)
	s.registry.Hives = []RegistryEntry{
		{
			ID:                 "alpha",
			Name:               "Alpha Hive",
			Org:                "alpha-org",
			HiveType:           "hosted",
			DashboardURL:       "https://alpha.example.com/dashboard",
			IsPublic:           true,
			Online:             true,
			Owner:              "alice",
			ContributorCount:   12,
			ActiveContributors: 3,
			ActionableIssues:   7,
			ActionablePRs:      2,
		},
		{
			ID:           "private",
			Name:         "Private Hive",
			DashboardURL: "https://private.example.com",
			IsPublic:     false,
			Online:       true,
			Owner:        "bob",
		},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://hive.hivecommons.dev/contribute", nil)
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/contribute status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Contribute compute to public hives",
		"Alpha Hive",
		"https://alpha.example.com/contribute",
		"/get-started#contribute",
		contributorRelayDocsURL,
		"https://hivecommons.dev/discord",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/contribute body missing %q:\n%s", want, body)
		}
	}
	for _, notWant := range []string{"Page Not Found", "Private Hive"} {
		if strings.Contains(body, notWant) {
			t.Fatalf("/contribute body unexpectedly contains %q:\n%s", notWant, body)
		}
	}
}

func TestHubContributeLandingOnlyOnCanonicalHubHost(t *testing.T) {
	t.Setenv("HIVE_HUB_PUBLIC_URL", "https://hive.hivecommons.dev")
	s := newHubServerForTest(t)
	s.registry.Hives = []RegistryEntry{{
		ID:           "alpha",
		Name:         "Alpha Hive",
		HiveType:     "hosted",
		DashboardURL: "https://alpha.example.com",
		IsPublic:     true,
		Online:       true,
		Owner:        "alice",
	}}

	for _, target := range []string{
		"https://alpha.hive.hivecommons.dev/contribute",
		"https://alpha.hive.hivecommons.dev/contribute/operations",
	} {
		t.Run(target, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, target, nil)
			s.mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s status = %d, want 404", target, rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, "Page Not Found") {
				t.Fatalf("%s did not keep the existing not-found page:\n%s", target, body)
			}
			if strings.Contains(body, "Contribute compute to public hives") {
				t.Fatalf("%s rendered the hub contributor landing for a tenant host:\n%s", target, body)
			}
		})
	}
}
