package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/linearagent"
)

// TestOAuthPublicOriginPrecedence pins the order in which the callback origin
// is chosen — dashboard.public_url, then hub.dashboard_url, then the
// forwarded/request host — and that configured values are trimmed so the
// appended callback path never yields a double slash.
func TestOAuthPublicOriginPrecedence(t *testing.T) {
	fwd := httptest.NewRequest(http.MethodPost, "http://hive.internal:8080/api/linear/agent/install", nil)
	fwd.Header.Set("X-Forwarded-Proto", "https")
	fwd.Header.Set("X-Forwarded-Host", "hive.forwarded.example")
	plain := httptest.NewRequest(http.MethodPost, "http://hive.internal:8080/api/linear/agent/install", nil)

	cases := []struct {
		name      string
		publicURL string
		hubURL    string
		req       *http.Request
		want      string
	}{
		{name: "public_url wins over hub and headers", publicURL: "https://public.example", hubURL: "https://hub.example", req: fwd, want: "https://public.example"},
		{name: "public_url trailing slash trimmed", publicURL: "https://public.example/", hubURL: "", req: fwd, want: "https://public.example"},
		{name: "public_url surrounding whitespace trimmed", publicURL: "  https://public.example/  ", hubURL: "", req: fwd, want: "https://public.example"},
		{name: "hub.dashboard_url when public_url unset", publicURL: "", hubURL: "https://hub.example/", req: fwd, want: "https://hub.example"},
		{name: "forwarded headers when nothing configured", publicURL: "", hubURL: "", req: fwd, want: "https://hive.forwarded.example"},
		{name: "request host when no headers", publicURL: "", hubURL: "", req: plain, want: "http://hive.internal:8080"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Dashboard.PublicURL = tc.publicURL
			cfg.Hub.DashboardURL = tc.hubURL
			if got := oauthPublicOrigin(cfg, tc.req); got != tc.want {
				t.Fatalf("oauthPublicOrigin = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("nil config falls back to request", func(t *testing.T) {
		if got := oauthPublicOrigin(nil, fwd); got != "https://hive.forwarded.example" {
			t.Fatalf("oauthPublicOrigin(nil) = %q", got)
		}
	})

	// Both builders share the origin and append their own path exactly once.
	s, deps := apiServer(t)
	deps.Config.Dashboard.PublicURL = "https://public.example/"
	if got := s.linearAgentCallbackURL(fwd); got != "https://public.example/linear/callback" {
		t.Errorf("linearAgentCallbackURL = %q", got)
	}
	if got := s.openRouterCallbackURL(fwd); got != "https://public.example/openrouter/callback" {
		t.Errorf("openRouterCallbackURL = %q", got)
	}
}

// TestLinearAgentInstallAndCallbackAgreeOnRedirectURI reproduces the
// standalone-behind-an-ingress failure: the owner starts the install from the
// private dashboard hostname, but Linear returns to the published callback
// hostname, and the ingress rewrites X-Forwarded-Host on the way in. Without
// dashboard.public_url the redirect_uri sent with the code exchange differs
// from the one in the authorize URL and Linear rejects it ("redirect_uri is
// invalid"). With it, both legs carry the same value — and the install
// response reports it.
func TestLinearAgentInstallAndCallbackAgreeOnRedirectURI(t *testing.T) {
	t.Setenv(linearagent.StoreEnvVar, filepath.Join(t.TempDir(), "linear-agent.json"))
	t.Setenv("LINEAR_CLIENT_ID", "client-id")
	t.Setenv("LINEAR_CLIENT_SECRET", "client-secret")
	t.Setenv("LINEAR_WEBHOOK_SECRET", "hook-secret")

	var exchangedRedirectURI string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			exchangedRedirectURI = r.PostForm.Get("redirect_uri")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"access_token": "at-1", "refresh_token": "rt-1", "expires_in": 86399,
				"scope": "read,write,app:assignable,app:mentionable",
			})
		case "/graphql":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"viewer":       map[string]string{"id": "viewer-1"},
					"organization": map[string]string{"id": "org-1", "name": "Acme", "urlKey": "acme"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fake.Close)

	s, deps := apiServer(t)
	s.linearAgentSvc = s.newLinearAgentService(fake.URL+"/oauth/token", fake.URL+"/graphql")
	deps.Config.Hub.Enabled = false
	deps.Config.Hub.DashboardURL = ""
	deps.Config.Dashboard.PublicURL = "https://hive-public.example/"

	// Leg 1: install, arriving on the PRIVATE hostname.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://hive.private.internal/api/linear/agent/install", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "hive.private.internal")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("install = %d — %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		AuthorizeURL string `json:"authorize_url"`
		RedirectURI  string `json:"redirect_uri"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	const want = "https://hive-public.example/linear/callback"
	if resp.RedirectURI != want {
		t.Fatalf("install redirect_uri = %q, want %q", resp.RedirectURI, want)
	}
	if !strings.Contains(resp.AuthorizeURL, "redirect_uri=https%3A%2F%2Fhive-public.example%2Flinear%2Fcallback") {
		t.Fatalf("authorize_url does not carry the public redirect_uri: %s", resp.AuthorizeURL)
	}

	// Leg 2: the callback, arriving through the PUBLIC ingress with a different
	// X-Forwarded-Host than the install leg saw. Deriving the origin from the
	// request here would produce a redirect_uri that disagrees with leg 1.
	state, err := s.linearAgent().states.Create()
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "http://hive.private.internal/linear/callback?code=abc&state="+state, nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "hive-public.example")
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "linear=connected") {
		t.Fatalf("callback: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if exchangedRedirectURI != resp.RedirectURI {
		t.Fatalf("code exchange redirect_uri = %q, install redirect_uri = %q — Linear would reject the exchange", exchangedRedirectURI, resp.RedirectURI)
	}
}
