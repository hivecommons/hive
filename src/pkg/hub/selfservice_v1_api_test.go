package hub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// Tests for the hub registration protocol v1 discovery and the configurable
// repo cap, rollout step 1 (#18642). They reuse the legacy self-service test
// env (selfservice_api_test.go): stubbed GitHub, temp stores, the flag on.

func newHubV1TestEnv(t *testing.T) *selfServiceTestEnv {
	t.Helper()
	t.Setenv(selfServiceMaxReposEnv, "")
	return newSelfServiceTestEnv(t)
}

func TestHubV1Discovery(t *testing.T) {
	cases := []struct {
		name         string
		flag         string
		maxReposEnv  string
		wantStatus   int
		wantMaxRepos int
	}{
		{name: "default cap", flag: "true", wantStatus: http.StatusOK, wantMaxRepos: selfServiceMaxReposDefault},
		{name: "configured cap", flag: "true", maxReposEnv: "100", wantStatus: http.StatusOK, wantMaxRepos: 100},
		{name: "cap clamped to ceiling", flag: "true", maxReposEnv: "100000", wantStatus: http.StatusOK, wantMaxRepos: selfServiceMaxReposCeiling},
		{name: "invalid cap falls back", flag: "true", maxReposEnv: "-3", wantStatus: http.StatusOK, wantMaxRepos: selfServiceMaxReposDefault},
		{name: "flag off hides the protocol", flag: "", wantStatus: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newHubV1TestEnv(t)
			t.Setenv(selfServiceRegistrationEnv, tc.flag)
			t.Setenv(selfServiceMaxReposEnv, tc.maxReposEnv)
			// Discovery is public: no session cookie.
			req := httptest.NewRequest(http.MethodGet, hubV1BasePath, nil)
			rec := httptest.NewRecorder()
			env.s.handleHubV1Discovery(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus != http.StatusOK {
				return
			}
			var d struct {
				Protocol    string           `json:"protocol"`
				Version     string           `json:"version"`
				BasePath    string           `json:"base_path"`
				Operations  []hubV1Operation `json:"operations"`
				PatchFields []string         `json:"patch_fields"`
				Limits      struct {
					MaxRepos           int    `json:"max_repos"`
					MaxPerUser         int    `json:"max_per_user"`
					Expiry             string `json:"expiry"`
					KeyRotationOverlap string `json:"key_rotation_overlap"`
				} `json:"limits"`
				ProofMethods []string `json:"proof_methods"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
			if d.Protocol != hubV1Protocol || d.Version != "1" || d.BasePath != "/api/hub/v1" {
				t.Fatalf("identity = %q %q %q", d.Protocol, d.Version, d.BasePath)
			}
			if d.Limits.MaxRepos != tc.wantMaxRepos || d.Limits.MaxPerUser != selfServiceMaxPerUserDefault ||
				d.Limits.Expiry != selfServiceExpiryDefault.String() || d.Limits.KeyRotationOverlap != selfServiceKeyRotationOverlap.String() {
				t.Fatalf("limits = %+v", d.Limits)
			}
			if !reflect.DeepEqual(d.ProofMethods, []string{"hub-session-token"}) {
				t.Fatalf("proof_methods = %v", d.ProofMethods)
			}
			if len(d.Operations) != 1 || d.Operations[0].Name != "discovery" || d.Operations[0].Method != http.MethodGet || d.Operations[0].Path != "/api/hub/v1" {
				t.Fatalf("operations = %+v", d.Operations)
			}
			if len(d.PatchFields) != 0 {
				t.Fatalf("patch_fields = %v, want none until update exists", d.PatchFields)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q", got)
			}
		})
	}
}

// Discovery must list exactly the operations that are routed, so a client
// never reads an operation that answers 404.
func TestHubV1RoutesAreRegistered(t *testing.T) {
	env := newHubV1TestEnv(t)
	env.s.mux = http.NewServeMux()
	env.s.registerHubV1Routes()
	for _, op := range hubV1Operations {
		path := strings.ReplaceAll(op.Path, "{id}", "byo-alice--lab")
		req := httptest.NewRequest(op.Method, path, nil)
		if _, pattern := env.s.mux.Handler(req); pattern != op.Method+" "+op.Path {
			t.Errorf("%s %s routed to %q", op.Method, path, pattern)
		}
	}
}

// The real hub constructor must wire the v1 routes, not only the helper, and
// the flag gate must hold through the real mux.
func TestHubV1RoutesWiredIntoHubServer(t *testing.T) {
	t.Setenv("HUB_DATA_DIR", t.TempDir())
	t.Setenv(selfServiceMaxReposEnv, "")
	srv := NewHubServer(0, slog.Default(), "test", "v5")
	t.Cleanup(srv.StopSaveLoop)

	t.Setenv(selfServiceRegistrationEnv, "true")
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, hubV1BasePath, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), hubV1Protocol) {
		t.Fatalf("GET %s through the hub mux: %d %s", hubV1BasePath, rec.Code, rec.Body.String())
	}

	t.Setenv(selfServiceRegistrationEnv, "")
	rec = httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, hubV1BasePath, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET %s with the flag off: %d, want 404", hubV1BasePath, rec.Code)
	}
}

func TestSelfServiceMaxReposReader(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"", 20},
		{"1", 1},
		{"20", 20},
		{"21", 21},
		{"500", 500},
		{"501", 500},
		{"100000", 500},
		{"0", 20},
		{"-5", 20},
		{"many", 20},
	}
	for _, tc := range cases {
		t.Run("env="+tc.env, func(t *testing.T) {
			t.Setenv(selfServiceMaxReposEnv, tc.env)
			if got := selfServiceMaxRepos(); got != tc.want {
				t.Fatalf("selfServiceMaxRepos() = %d, want %d", got, tc.want)
			}
		})
	}
}

// A larger cap must not be undone by the body limit: the largest legal body at
// the ceiling has to fit, and the default keeps the old floor.
func TestSelfServiceRequestBodyLimitScalesWithCap(t *testing.T) {
	t.Setenv(selfServiceMaxReposEnv, "")
	if got := selfServiceRequestBodyLimit(); got < selfServiceMaxRequestBody {
		t.Fatalf("default body limit %d below the floor %d", got, selfServiceMaxRequestBody)
	}
	t.Setenv(selfServiceMaxReposEnv, "500")
	// owner/name refs at GitHub's longest (39 + 1 + 100) plus JSON quoting.
	const longestRef = 39 + 1 + 100 + 4
	if got := selfServiceRequestBodyLimit(); got < int64(selfServiceMaxReposCeiling)*longestRef {
		t.Fatalf("body limit %d cannot hold %d max-length repos", got, selfServiceMaxReposCeiling)
	}
}

// The legacy register route reads the configurable cap and keeps its 400 body.
func TestSelfServiceRegisterRepoCapIsConfigurable(t *testing.T) {
	repos := func(n int) string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("alice/bulk-%02d", i)
		}
		b, _ := json.Marshal(out)
		return string(b)
	}
	cases := []struct {
		name       string
		env        string
		n          int
		wantStatus int
		wantErr    string
	}{
		{name: "default accepts 20", env: "", n: 20, wantStatus: http.StatusCreated},
		{name: "default refuses 21 with the legacy 400 body", env: "", n: 21, wantStatus: http.StatusBadRequest, wantErr: "at most 20 repos per registration"},
		{name: "raised cap accepts 21", env: "25", n: 21, wantStatus: http.StatusCreated},
		{name: "raised cap refuses past it", env: "25", n: 26, wantStatus: http.StatusBadRequest, wantErr: "at most 25 repos per registration"},
		{name: "ceiling clamps a typo", env: "100000", n: 501, wantStatus: http.StatusBadRequest, wantErr: "at most 500 repos per registration"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newHubV1TestEnv(t)
			t.Setenv(selfServiceMaxReposEnv, tc.env)
			rec := env.register(t, selfServiceTestUser, fmt.Sprintf(`{"name":"big","repos":%s}`, repos(tc.n)))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantErr != "" {
				if got := decodeError(t, rec)["error"]; got != tc.wantErr {
					t.Fatalf("error = %q, want %q", got, tc.wantErr)
				}
				if len(env.s.selfService.list()) != 0 {
					t.Fatal("refused registration stored a record")
				}
			}
		})
	}
}
