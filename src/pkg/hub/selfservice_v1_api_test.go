package hub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// Tests for the hub registration protocol v1 discovery and the configurable
// repo cap, rollout step 1 (#18642). They reuse the legacy self-service test
// env (selfservice_api_test.go): stubbed GitHub, temp stores, the flag on.

const hubV1TestID = "byo-alice--lab"

func newHubV1TestEnv(t *testing.T) *selfServiceTestEnv {
	t.Helper()
	t.Setenv(selfServiceMaxReposEnv, "")
	return newSelfServiceTestEnv(t)
}

// v1 sends a request through requireAuth to a v1 handler, filling the {id}
// path value the mux would set in production.
func (env *selfServiceTestEnv) v1(t *testing.T, user, method, path, body string, headers map[string]string, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Origin", selfServiceTestOrigin)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if user != "" {
		req.AddCookie(&http.Cookie{
			Name:  "hive_hub_user",
			Value: mintHubUserCookieValueV2(deriveDomainKey(grantableTestSecret, infoSessionEd25519Seed), user),
		})
	}
	if rest, ok := strings.CutPrefix(path, hubV1BasePath+"/registrations/"); ok {
		id, _, _ := strings.Cut(rest, "/")
		req.SetPathValue("id", id)
	}
	rec := httptest.NewRecorder()
	env.s.requireAuth(handler)(rec, req)
	return rec
}

func (env *selfServiceTestEnv) v1Get(t *testing.T, user, id string) *httptest.ResponseRecorder {
	t.Helper()
	return env.v1(t, user, http.MethodGet, hubV1BasePath+"/registrations/"+id, "", nil, env.s.handleHubV1Get)
}

func (env *selfServiceTestEnv) v1Patch(t *testing.T, user, id, ifMatch, body string) *httptest.ResponseRecorder {
	t.Helper()
	headers := map[string]string{}
	if ifMatch != "" {
		headers["If-Match"] = ifMatch
	}
	return env.v1(t, user, http.MethodPatch, hubV1BasePath+"/registrations/"+id, body, headers, env.s.handleHubV1Patch)
}

// etagOf reads the current ETag through GET, as a client would.
func (env *selfServiceTestEnv) etagOf(t *testing.T, id string) string {
	t.Helper()
	rec := env.v1Get(t, selfServiceTestUser, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", id, rec.Code, rec.Body.String())
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("GET returned no ETag")
	}
	return etag
}

func decodeV1Registration(t *testing.T, rec *httptest.ResponseRecorder) hubV1RegistrationView {
	t.Helper()
	var resp struct {
		Registration hubV1RegistrationView `json:"registration"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode registration: %v (%s)", err, rec.Body.String())
	}
	return resp.Registration
}

func sortedLower(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	sort.Strings(out)
	return out
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
			got := map[string]hubV1Operation{}
			for _, op := range d.Operations {
				got[op.Name] = op
			}
			want := map[string]string{
				"discovery":  "GET /api/hub/v1",
				"create":     "POST /api/hub/v1/registrations",
				"list":       "GET /api/hub/v1/registrations",
				"get":        "GET /api/hub/v1/registrations/{id}",
				"update":     "PATCH /api/hub/v1/registrations/{id}",
				"rotate_key": "POST /api/hub/v1/registrations/{id}/keys",
				"delete":     "DELETE /api/hub/v1/registrations/{id}",
			}
			if len(got) != len(want) {
				t.Fatalf("operations = %+v", d.Operations)
			}
			for name, route := range want {
				op, ok := got[name]
				if !ok || op.Method+" "+op.Path != route {
					t.Errorf("operation %s = %+v, want %s", name, op, route)
				}
			}
			if !reflect.DeepEqual(d.PatchFields, []string{"repos", "dashboard_url"}) {
				t.Fatalf("patch_fields = %v", d.PatchFields)
			}
			if got["update"].IfMatch != "required" {
				t.Errorf("update if_match = %q, want required", got["update"].IfMatch)
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

func TestHubV1CreateGetList(t *testing.T) {
	env := newHubV1TestEnv(t)
	rec := env.v1(t, selfServiceTestUser, http.MethodPost, hubV1BasePath+"/registrations", selfServiceHappyBody, nil, env.s.handleHubV1Create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != hubV1BasePath+"/registrations/"+hubV1TestID {
		t.Fatalf("Location = %q", loc)
	}
	etag := rec.Header().Get("ETag")
	var created struct {
		Registration hubV1RegistrationView `json:"registration"`
		Key          string                `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if etag == "" || created.Registration.ETag != etag || created.Key == "" {
		t.Fatalf("create: etag header %q, body etag %q, key set %v", etag, created.Registration.ETag, created.Key != "")
	}
	if _, ok := env.s.verifyHeartbeatBearerGeneration(created.Key, hubV1TestID); !ok {
		t.Fatal("v1-created key does not authenticate")
	}

	if got := env.etagOf(t, hubV1TestID); got != etag {
		t.Fatalf("GET ETag %q != create ETag %q", got, etag)
	}
	notModified := env.v1(t, selfServiceTestUser, http.MethodGet, hubV1BasePath+"/registrations/"+hubV1TestID, "", map[string]string{"If-None-Match": etag}, env.s.handleHubV1Get)
	if notModified.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match: %d", notModified.Code)
	}

	list := env.v1(t, selfServiceTestUser, http.MethodGet, hubV1BasePath+"/registrations", "", nil, env.s.handleHubV1List)
	var listed struct {
		Registrations []hubV1RegistrationView `json:"registrations"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Registrations) != 1 || listed.Registrations[0].ETag != etag {
		t.Fatalf("list = %s", list.Body.String())
	}
	if strings.Contains(list.Body.String(), created.Key) || strings.Contains(list.Body.String(), "key_hash") {
		t.Fatal("list leaked key material")
	}
	other := env.v1(t, selfServiceTestOther, http.MethodGet, hubV1BasePath+"/registrations", "", nil, env.s.handleHubV1List)
	if strings.Contains(other.Body.String(), hubV1TestID) {
		t.Fatal("another user's list shows alice's registration")
	}
}

func TestHubV1RotateAndDelete(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		suffix     string
		user       string
		ifMatch    string // "current", "stale" or ""
		body       string
		wantStatus int
	}{
		{name: "rotate without precondition", method: http.MethodPost, suffix: "/keys", user: selfServiceTestUser, wantStatus: http.StatusCreated},
		{name: "rotate with current etag", method: http.MethodPost, suffix: "/keys", user: selfServiceTestUser, ifMatch: "current", wantStatus: http.StatusCreated},
		{name: "rotate with stale etag", method: http.MethodPost, suffix: "/keys", user: selfServiceTestUser, ifMatch: "stale", wantStatus: http.StatusPreconditionFailed},
		{name: "rotate grace is not offered yet", method: http.MethodPost, suffix: "/keys", user: selfServiceTestUser, body: `{"grace":"1h"}`, wantStatus: http.StatusBadRequest},
		{name: "rotate someone else's", method: http.MethodPost, suffix: "/keys", user: selfServiceTestOther, wantStatus: http.StatusNotFound},
		{name: "delete with stale etag", method: http.MethodDelete, user: selfServiceTestUser, ifMatch: "stale", wantStatus: http.StatusPreconditionFailed},
		{name: "delete with current etag", method: http.MethodDelete, user: selfServiceTestUser, ifMatch: "current", wantStatus: http.StatusOK},
		{name: "delete by hub admin", method: http.MethodDelete, user: selfServiceTestAdmin, wantStatus: http.StatusOK},
		{name: "delete someone else's", method: http.MethodDelete, user: selfServiceTestOther, wantStatus: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newHubV1TestEnv(t)
			id, key := env.registerHappy(t)
			ifMatch := ""
			switch tc.ifMatch {
			case "current":
				ifMatch = env.etagOf(t, id)
			case "stale":
				ifMatch = env.etagOf(t, id)
				// Another operator changes the record first.
				if _, err := env.s.selfService.update(id, ifMatch, func(*SelfServiceHive) error { return nil }); err != nil {
					t.Fatalf("setup update: %v", err)
				}
			}
			headers := map[string]string{}
			if ifMatch != "" {
				headers["If-Match"] = ifMatch
			}
			handler := env.s.handleHubV1Delete
			if tc.suffix == "/keys" {
				handler = env.s.handleHubV1RotateKey
			}
			rec := env.v1(t, tc.user, tc.method, hubV1BasePath+"/registrations/"+id+tc.suffix, tc.body, headers, handler)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			_, stillThere := env.s.selfService.get(id)
			_, keyOK := env.s.verifyHeartbeatBearerGeneration(key, id)
			switch {
			case tc.method == http.MethodDelete && tc.wantStatus == http.StatusOK:
				if stillThere || keyOK {
					t.Fatal("delete left the record or the key alive")
				}
			case tc.suffix == "/keys" && tc.wantStatus == http.StatusCreated:
				var resp struct {
					Key string `json:"key"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &resp)
				if _, ok := env.s.verifyHeartbeatBearerGeneration(resp.Key, id); !ok || resp.Key == key {
					t.Fatal("rotation did not mint a working new key")
				}
				if !keyOK {
					t.Fatal("old key refused inside the rotation overlap")
				}
				if stored, _ := env.s.selfService.get(id); rec.Header().Get("ETag") != selfServiceETag(stored) {
					t.Fatal("rotation response ETag is not the record's")
				}
			default:
				if !stillThere || !keyOK {
					t.Fatal("refused request removed the record or broke the key")
				}
			}
		})
	}
}

// A v1 create over the repo cap answers 413 naming the limit; the legacy
// create keeps its 400 (pinned in TestSelfServiceRegisterRepoCapIsConfigurable).
func TestHubV1CreateRepoCap(t *testing.T) {
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
	}{
		{name: "at the default cap", env: "", n: 20, wantStatus: http.StatusCreated},
		{name: "over the default cap", env: "", n: 21, wantStatus: http.StatusRequestEntityTooLarge},
		{name: "raised cap accepts 21", env: "25", n: 21, wantStatus: http.StatusCreated},
		{name: "raised cap refuses past it", env: "25", n: 26, wantStatus: http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newHubV1TestEnv(t)
			t.Setenv(selfServiceMaxReposEnv, tc.env)
			body := fmt.Sprintf(`{"name":"big","repos":%s}`, repos(tc.n))
			rec := env.v1(t, selfServiceTestUser, http.MethodPost, hubV1BasePath+"/registrations", body, nil, env.s.handleHubV1Create)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus == http.StatusCreated {
				return
			}
			var resp struct {
				Error    string `json:"error"`
				Limit    string `json:"limit"`
				MaxRepos int    `json:"max_repos"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Limit != "max_repos" || resp.MaxRepos != selfServiceMaxRepos() || resp.Error == "" {
				t.Fatalf("413 body = %+v", resp)
			}
			if len(env.s.selfService.list()) != 0 {
				t.Fatal("refused create stored a record")
			}
		})
	}
}

// A v1 create refused for any other reason keeps the legacy status codes.
func TestHubV1CreateRefusalsMatchLegacy(t *testing.T) {
	cases := []struct {
		name string
		user string
		body string
		want int
	}{
		{name: "not signed in", user: "", body: selfServiceHappyBody, want: http.StatusUnauthorized},
		{name: "not admin of a repo", user: selfServiceTestUser, body: `{"name":"Lab","repos":["alice/readonly"]}`, want: http.StatusForbidden},
		{name: "no repos", user: selfServiceTestUser, body: `{"name":"Lab","repos":[]}`, want: http.StatusBadRequest},
		{name: "bad dashboard url", user: selfServiceTestUser, body: `{"name":"Lab","repos":["alice/lab"],"dashboard_url":"http://lab.example.org/"}`, want: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newHubV1TestEnv(t)
			legacy := env.register(t, tc.user, tc.body)
			v1 := env.v1(t, tc.user, http.MethodPost, hubV1BasePath+"/registrations", tc.body, nil, env.s.handleHubV1Create)
			if legacy.Code != tc.want || v1.Code != tc.want {
				t.Fatalf("legacy %d, v1 %d, want %d (%s | %s)", legacy.Code, v1.Code, tc.want, legacy.Body.String(), v1.Body.String())
			}
			if len(env.s.selfService.list()) != 0 {
				t.Fatal("a refused create stored a record")
			}
		})
	}
}

func TestHubV1FlagOffHidesEveryRoute(t *testing.T) {
	env := newHubV1TestEnv(t)
	id, _ := env.registerHappy(t)
	t.Setenv(selfServiceRegistrationEnv, "")
	path := hubV1BasePath + "/registrations"
	cases := []struct {
		name    string
		method  string
		path    string
		handler http.HandlerFunc
	}{
		{"create", http.MethodPost, path, env.s.handleHubV1Create},
		{"list", http.MethodGet, path, env.s.handleHubV1List},
		{"get", http.MethodGet, path + "/" + id, env.s.handleHubV1Get},
		{"rotate", http.MethodPost, path + "/" + id + "/keys", env.s.handleHubV1RotateKey},
		{"delete", http.MethodDelete, path + "/" + id, env.s.handleHubV1Delete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.v1(t, selfServiceTestUser, tc.method, tc.path, selfServiceHappyBody, nil, tc.handler)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s with the flag off: %d, want 404 (%s)", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
	if _, ok := env.s.selfService.get(id); !ok {
		t.Fatal("a flag-off request changed the registry")
	}
}

func TestHubV1RoutesRequireSession(t *testing.T) {
	env := newHubV1TestEnv(t)
	id, _ := env.registerHappy(t)
	path := hubV1BasePath + "/registrations"
	cases := []struct {
		name    string
		method  string
		path    string
		handler http.HandlerFunc
	}{
		{"create", http.MethodPost, path, env.s.handleHubV1Create},
		{"list", http.MethodGet, path, env.s.handleHubV1List},
		{"get", http.MethodGet, path + "/" + id, env.s.handleHubV1Get},
		{"rotate", http.MethodPost, path + "/" + id + "/keys", env.s.handleHubV1RotateKey},
		{"delete", http.MethodDelete, path + "/" + id, env.s.handleHubV1Delete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.v1(t, "", tc.method, tc.path, "", nil, tc.handler)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s without a session: %d, want 401", tc.name, rec.Code)
			}
		})
	}
}

// TestHubV1LegacyRoutesUnchanged pins the legacy /api/hub/selfservice/*
// responses so the v1 work cannot drift them: exact top-level keys, no ETag
// header, none of the v1-only fields, and the view bytes equal to what the
// pre-v1 encoder produced for the same record.
func TestHubV1LegacyRoutesUnchanged(t *testing.T) {
	topKeys := func(t *testing.T, rec *httptest.ResponseRecorder) []string {
		t.Helper()
		var m map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("body is not a JSON object: %s", rec.Body.String())
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	}
	encode := func(v any) string {
		var b strings.Builder
		if err := json.NewEncoder(&b).Encode(v); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	env := newHubV1TestEnv(t)

	reg := env.register(t, selfServiceTestUser, selfServiceHappyBody)
	list := env.list(t, selfServiceTestUser)
	status := env.status(t, selfServiceTestUser)
	cases := []struct {
		name     string
		rec      *httptest.ResponseRecorder
		wantCode int
		wantKeys []string
	}{
		{name: "register", rec: reg, wantCode: http.StatusCreated, wantKeys: []string{"hive", "hub_url", "key", "snippets"}},
		{name: "list", rec: list, wantCode: http.StatusOK, wantKeys: []string{"hives"}},
		{name: "status", rec: status, wantCode: http.StatusOK, wantKeys: []string{"count", "enabled", "expiry", "max_per_user"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (%s)", tc.rec.Code, tc.wantCode, tc.rec.Body.String())
			}
			if got := topKeys(t, tc.rec); !reflect.DeepEqual(got, tc.wantKeys) {
				t.Fatalf("keys = %v, want %v", got, tc.wantKeys)
			}
			if tc.rec.Header().Get("ETag") != "" {
				t.Fatal("legacy route started sending an ETag")
			}
			for _, leaked := range []string{`"etag"`, `"revision"`, `"key_hash"`, `"repo_proofs"`} {
				if strings.Contains(tc.rec.Body.String(), leaked) {
					t.Fatalf("legacy body gained %s: %s", leaked, tc.rec.Body.String())
				}
			}
		})
	}

	// Byte-for-byte: the legacy view of the stored record is exactly the
	// pre-v1 selfServiceHiveView encoding, in the register and list bodies.
	stored, ok := env.s.selfService.get(hubV1TestID)
	if !ok {
		t.Fatal("registered hive missing")
	}
	wantView := encode(selfServiceView(stored, false))
	var created struct {
		Hive json.RawMessage `json:"hive"`
	}
	if err := json.Unmarshal(reg.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if got := string(created.Hive) + "\n"; got != wantView {
		t.Fatalf("legacy register view changed:\n got %s want %s", got, wantView)
	}
	wantList := encode(map[string]any{"hives": []selfServiceHiveView{selfServiceView(stored, false)}})
	if list.Body.String() != wantList {
		t.Fatalf("legacy list body changed:\n got %s want %s", list.Body.String(), wantList)
	}
	// The view struct's wire fields are the pre-v1 set and no others.
	viewKeys := map[string]struct{}{}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(created.Hive, &m); err != nil {
		t.Fatal(err)
	}
	for k := range m {
		viewKeys[k] = struct{}{}
	}
	for _, k := range []string{"id", "owner", "org", "repos", "dashboard_url", "created_at", "ownership_checked_at", "online"} {
		if _, ok := viewKeys[k]; !ok {
			t.Errorf("legacy view lost %q", k)
			continue
		}
		delete(viewKeys, k)
	}
	delete(viewKeys, "last_accepted_heartbeat_at")
	if len(viewKeys) != 0 {
		t.Fatalf("legacy view gained fields: %v", viewKeys)
	}

	// The legacy rotate (200) and delete still answer as before, and see a
	// record that v1 has also touched.
	if rec := env.v1(t, selfServiceTestUser, http.MethodPost, hubV1BasePath+"/registrations/"+hubV1TestID+"/keys", "", nil, env.s.handleHubV1RotateKey); rec.Code != http.StatusCreated {
		t.Fatalf("v1 rotate: %d %s", rec.Code, rec.Body.String())
	}
	rot := env.rotate(t, selfServiceTestUser, hubV1TestID)
	if rot.Code != http.StatusOK || !reflect.DeepEqual(topKeys(t, rot), []string{"hub_url", "id", "key", "overlap", "snippets"}) {
		t.Fatalf("legacy rotate: %d %s", rot.Code, rot.Body.String())
	}
	del := env.remove(t, selfServiceTestUser, hubV1TestID)
	if del.Code != http.StatusOK || del.Body.String() != encode(map[string]any{"id": hubV1TestID, "removed": true}) {
		t.Fatalf("legacy delete: %d %s", del.Code, del.Body.String())
	}
}

// TestHubV1RepoProofs checks the per-repo proof times: create stamps every
// repo, a PATCH stamps only the repos it adds, keeps the others' times, and
// drops a removed repo's entry. A record from before the field reports
// OwnershipCheckedAt for every repo.
func TestHubV1RepoProofs(t *testing.T) {
	proofsOf := func(t *testing.T, env *selfServiceTestEnv) map[string]time.Time {
		t.Helper()
		rec := env.v1Get(t, selfServiceTestUser, hubV1TestID)
		out := map[string]time.Time{}
		for _, p := range decodeV1Registration(t, rec).RepoProofs {
			out[p.Repo] = p.CheckedAt
		}
		return out
	}
	cases := []struct {
		name       string
		legacy     bool // strip RepoProofs as a pre-#18642 record would have none
		patch      string
		wantRepos  []string
		wantFresh  []string // proof time moved past the create's
		wantSameAs []string // proof time equals the create's
	}{
		{name: "create stamps every repo", wantRepos: []string{"alice/lab", "alice/tools"}, wantSameAs: []string{"alice/lab", "alice/tools"}},
		{name: "add stamps only the new repo", patch: `{"repos":{"add":["alice/docs"]}}`, wantRepos: []string{"alice/lab", "alice/tools", "alice/docs"}, wantFresh: []string{"alice/docs"}, wantSameAs: []string{"alice/lab", "alice/tools"}},
		{name: "remove keeps the rest", patch: `{"repos":{"remove":["alice/tools"]}}`, wantRepos: []string{"alice/lab"}, wantSameAs: []string{"alice/lab"}},
		{name: "url edit keeps every proof", patch: `{"dashboard_url":""}`, wantRepos: []string{"alice/lab", "alice/tools"}, wantSameAs: []string{"alice/lab", "alice/tools"}},
		{name: "legacy record reads OwnershipCheckedAt", legacy: true, wantRepos: []string{"alice/lab", "alice/tools"}, wantSameAs: []string{"alice/lab", "alice/tools"}},
		{name: "legacy record add keeps the fallback", legacy: true, patch: `{"repos":{"add":["alice/site"]}}`, wantRepos: []string{"alice/lab", "alice/tools", "alice/site"}, wantFresh: []string{"alice/site"}, wantSameAs: []string{"alice/lab", "alice/tools"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newHubV1TestEnv(t)
			id, _ := env.registerHappy(t)
			created, _ := env.s.selfService.get(id)
			if len(created.RepoProofs) != 2 {
				t.Fatalf("create stored proofs %v", created.RepoProofs)
			}
			for repo, at := range created.RepoProofs {
				if !at.Equal(created.OwnershipCheckedAt) {
					t.Fatalf("create proof for %s = %v, want %v", repo, at, created.OwnershipCheckedAt)
				}
			}
			if tc.legacy {
				if _, err := env.s.selfService.update(id, "", func(h *SelfServiceHive) error { h.RepoProofs = nil; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if tc.patch != "" {
				// Make sure a fresh stamp is distinguishable from the create's.
				time.Sleep(2 * time.Millisecond)
				if rec := env.v1Patch(t, selfServiceTestUser, id, env.etagOf(t, id), tc.patch); rec.Code != http.StatusOK {
					t.Fatalf("PATCH: %d %s", rec.Code, rec.Body.String())
				}
			}
			got := proofsOf(t, env)
			if len(got) != len(tc.wantRepos) {
				t.Fatalf("repo_proofs = %v, want repos %v", got, tc.wantRepos)
			}
			for _, repo := range tc.wantRepos {
				if _, ok := got[repo]; !ok {
					t.Fatalf("repo_proofs lacks %s: %v", repo, got)
				}
			}
			for _, repo := range tc.wantSameAs {
				if !got[repo].Equal(created.OwnershipCheckedAt) {
					t.Errorf("%s proof = %v, want the create's %v", repo, got[repo], created.OwnershipCheckedAt)
				}
			}
			for _, repo := range tc.wantFresh {
				if !got[repo].After(created.OwnershipCheckedAt) {
					t.Errorf("%s proof = %v, want after %v", repo, got[repo], created.OwnershipCheckedAt)
				}
			}
			after, _ := env.s.selfService.get(id)
			if !after.OwnershipCheckedAt.Equal(created.OwnershipCheckedAt) {
				t.Error("OwnershipCheckedAt moved; legacy readers rely on it")
			}
		})
	}
}

func TestHubV1PatchKeepsKey(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantRepos []string
		wantURL   string
		wantCalls []string // GitHub repo checks the PATCH may make
	}{
		{
			name:      "add one repo",
			body:      `{"repos":{"add":["alice/docs"]}}`,
			wantRepos: []string{"alice/lab", "alice/tools", "alice/docs"},
			wantURL:   "https://lab.example.org/",
			wantCalls: []string{"alice/docs"},
		},
		{
			name:      "add and remove",
			body:      `{"repos":{"add":["alice/docs","alice/site"],"remove":["alice/tools"]}}`,
			wantRepos: []string{"alice/lab", "alice/docs", "alice/site"},
			wantURL:   "https://lab.example.org/",
			wantCalls: []string{"alice/docs", "alice/site"},
		},
		{
			name:      "remove only needs no proof",
			body:      `{"repos":{"remove":["alice/tools"]}}`,
			wantRepos: []string{"alice/lab"},
			wantURL:   "https://lab.example.org/",
		},
		{
			name:      "full replacement proves only the new repo",
			body:      `{"repos":["alice/lab","alice/docs"]}`,
			wantRepos: []string{"alice/lab", "alice/docs"},
			wantURL:   "https://lab.example.org/",
			wantCalls: []string{"alice/docs"},
		},
		{
			name:      "dashboard url only",
			body:      `{"dashboard_url":"https://dash.example.org/"}`,
			wantRepos: []string{"alice/lab", "alice/tools"},
			wantURL:   "https://dash.example.org/",
		},
		{
			name:      "clear dashboard url",
			body:      `{"dashboard_url":""}`,
			wantRepos: []string{"alice/lab", "alice/tools"},
			wantURL:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newHubV1TestEnv(t)
			id, key := env.registerHappy(t)
			before, _ := env.s.selfService.get(id)
			etag := env.etagOf(t, id)

			env.checkedRepos() // drop the registration's checks

			rec := env.v1Patch(t, selfServiceTestUser, id, etag, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("PATCH: %d %s", rec.Code, rec.Body.String())
			}
			view := decodeV1Registration(t, rec)
			if !reflect.DeepEqual(sortedLower(view.Repos), sortedLower(tc.wantRepos)) || view.DashboardURL != tc.wantURL {
				t.Fatalf("view repos=%v url=%q", view.Repos, view.DashboardURL)
			}
			if newTag := rec.Header().Get("ETag"); newTag == "" || newTag == etag || newTag != view.ETag {
				t.Fatalf("ETag after PATCH = %q (was %q, body %q)", newTag, etag, view.ETag)
			}
			if calls := env.checkedRepos(); !reflect.DeepEqual(sortedLower(calls), sortedLower(tc.wantCalls)) {
				t.Fatalf("GitHub checks = %v, want %v", calls, tc.wantCalls)
			}

			after, _ := env.s.selfService.get(id)
			if after.KeyHash != before.KeyHash || after.PrevKeyHash != before.PrevKeyHash || after.Owner != before.Owner || !after.CreatedAt.Equal(before.CreatedAt) {
				t.Fatal("PATCH changed the credential or identity fields")
			}
			// The whole point of the edit route: the spoke keeps heartbeating
			// with the key it already has.
			if gen, ok := env.s.verifyHeartbeatBearerGeneration(key, id); !ok || gen != selfServiceBearerGeneration {
				t.Fatalf("key no longer authenticates the heartbeat after PATCH: gen=%d ok=%v", gen, ok)
			}
			if !strings.Contains(env.logs.String(), "audit: selfservice_hive_updated") {
				t.Fatal("no update audit line")
			}
		})
	}
}

func TestHubV1PatchRefusals(t *testing.T) {
	cases := []struct {
		name    string
		user    string
		ifMatch string // "current" = read it first, "stale" = from before a change
		body    string
		status  int
		wantErr string // exact error message, when set
	}{
		{name: "stale etag", user: selfServiceTestUser, ifMatch: "stale", body: `{"repos":{"add":["alice/docs"]}}`, status: http.StatusPreconditionFailed},
		{name: "made-up etag", user: selfServiceTestUser, ifMatch: `"nope"`, body: `{"repos":{"add":["alice/docs"]}}`, status: http.StatusPreconditionFailed},
		{name: "weak etag never matches", user: selfServiceTestUser, ifMatch: "weak", body: `{"repos":{"add":["alice/docs"]}}`, status: http.StatusPreconditionFailed},
		{name: "missing if-match", user: selfServiceTestUser, ifMatch: "", body: `{"repos":{"add":["alice/docs"]}}`, status: http.StatusPreconditionRequired},
		{name: "someone else's registration", user: selfServiceTestOther, ifMatch: "current", body: `{"repos":{"add":["alice/docs"]}}`, status: http.StatusNotFound},
		{name: "hub admin cannot edit", user: selfServiceTestAdmin, ifMatch: "current", body: `{"dashboard_url":"https://x.example.org/"}`, status: http.StatusNotFound},
		{name: "not admin of added repo", user: selfServiceTestUser, ifMatch: "current", body: `{"repos":{"add":["alice/readonly"]}}`, status: http.StatusForbidden},
		{name: "added repo invisible to token", user: selfServiceTestUser, ifMatch: "current", body: `{"repos":["alice/lab","alice/secret"]}`, status: http.StatusForbidden},
		{name: "org is not editable", user: selfServiceTestUser, ifMatch: "current", body: `{"org":"evil"}`, status: http.StatusBadRequest, wantErr: "org is not editable; delete and re-register"},
		{name: "org with other fields", user: selfServiceTestUser, ifMatch: "current", body: `{"org":"evil","dashboard_url":"https://x.example.org/"}`, status: http.StatusBadRequest, wantErr: "org is not editable; delete and re-register"},
		{name: "key is not editable", user: selfServiceTestUser, ifMatch: "current", body: `{"key_hash":"00"}`, status: http.StatusBadRequest},
		{name: "empty patch", user: selfServiceTestUser, ifMatch: "current", body: `{}`, status: http.StatusBadRequest},
		{name: "remove every repo", user: selfServiceTestUser, ifMatch: "current", body: `{"repos":{"remove":["alice/lab","alice/tools"]}}`, status: http.StatusBadRequest},
		{name: "add and remove same repo", user: selfServiceTestUser, ifMatch: "current", body: `{"repos":{"add":["alice/docs"],"remove":["alice/docs"]}}`, status: http.StatusBadRequest},
		{name: "bad repo ref", user: selfServiceTestUser, ifMatch: "current", body: `{"repos":{"add":["../x"]}}`, status: http.StatusBadRequest},
		{name: "http dashboard", user: selfServiceTestUser, ifMatch: "current", body: `{"dashboard_url":"http://lab.example.org/"}`, status: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newHubV1TestEnv(t)
			id, key := env.registerHappy(t)
			ifMatch := tc.ifMatch
			switch ifMatch {
			case "current":
				ifMatch = env.etagOf(t, id)
			case "weak":
				ifMatch = "W/" + env.etagOf(t, id)
			case "stale":
				ifMatch = env.etagOf(t, id)
				// Another operator edits first.
				if rec := env.v1Patch(t, selfServiceTestUser, id, ifMatch, `{"dashboard_url":"https://other.example.org/"}`); rec.Code != http.StatusOK {
					t.Fatalf("first PATCH: %d %s", rec.Code, rec.Body.String())
				}
			}
			before, _ := env.s.selfService.get(id)

			rec := env.v1Patch(t, tc.user, id, ifMatch, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
			if tc.wantErr != "" {
				if got := decodeError(t, rec)["error"]; got != tc.wantErr {
					t.Fatalf("error = %q, want %q", got, tc.wantErr)
				}
			}
			after, _ := env.s.selfService.get(id)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("refused PATCH changed the record:\nbefore %+v\nafter  %+v", before, after)
			}
			if _, ok := env.s.verifyHeartbeatBearerGeneration(key, id); !ok {
				t.Fatal("refused PATCH disturbed the key")
			}
			if tc.status == http.StatusForbidden && !strings.Contains(env.logs.String(), "reason="+selfServiceRefusedNotAdmin) {
				t.Fatal("no not_repo_admin audit line")
			}
			if tc.status == http.StatusPreconditionFailed && rec.Header().Get("ETag") != selfServiceETag(after) {
				t.Fatal("412 does not carry the current ETag")
			}
		})
	}
}

func TestHubV1PatchAndCreateRepoCap(t *testing.T) {
	repos := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("alice/bulk-%02d", i)
		}
		return out
	}
	quoted := func(rs []string) string {
		b, _ := json.Marshal(rs)
		return string(b)
	}
	cases := []struct {
		name         string
		env          string
		registerN    int
		v1Create     bool // create through POST /api/hub/v1/registrations
		wantCreate   int
		wantCreateIn string
		patchAddN    int // extra repos a PATCH adds after a successful create
		wantPatch    int
		wantPatchIn  string
	}{
		{name: "default 20 refuses 21 on create", env: "", registerN: 21, wantCreate: http.StatusBadRequest},
		{name: "default 20 accepts 20", env: "", registerN: 20, wantCreate: http.StatusCreated},
		{name: "raised cap accepts 21", env: "25", registerN: 21, wantCreate: http.StatusCreated},
		{name: "v1 create over the cap is 413 naming the limit", env: "", registerN: 21, v1Create: true, wantCreate: http.StatusRequestEntityTooLarge, wantCreateIn: `"limit":"max_repos"`},
		{name: "v1 create at the cap", env: "", registerN: 20, v1Create: true, wantCreate: http.StatusCreated},
		{name: "patch past the cap is 413 naming the limit", env: "22", registerN: 21, wantCreate: http.StatusCreated, patchAddN: 2, wantPatch: http.StatusRequestEntityTooLarge, wantPatchIn: `"limit":"max_repos"`},
		{name: "patch up to the cap", env: "22", registerN: 21, wantCreate: http.StatusCreated, patchAddN: 1, wantPatch: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newHubV1TestEnv(t)
			t.Setenv(selfServiceMaxReposEnv, tc.env)
			all := repos(tc.registerN + tc.patchAddN)
			body := fmt.Sprintf(`{"name":"big","repos":%s}`, quoted(all[:tc.registerN]))
			// The v6 route reads the same cap but keeps its 400.
			var rec *httptest.ResponseRecorder
			if tc.v1Create {
				rec = env.v1(t, selfServiceTestUser, http.MethodPost, hubV1BasePath+"/registrations", body, nil, env.s.handleHubV1Create)
			} else {
				rec = env.register(t, selfServiceTestUser, body)
			}
			if rec.Code != tc.wantCreate {
				t.Fatalf("create: %d, want %d (%s)", rec.Code, tc.wantCreate, rec.Body.String())
			}
			if tc.wantCreateIn != "" && !strings.Contains(rec.Body.String(), tc.wantCreateIn) {
				t.Fatalf("create body %s lacks %s", rec.Body.String(), tc.wantCreateIn)
			}
			if tc.wantCreate != http.StatusCreated && len(env.s.selfService.list()) != 0 {
				t.Fatal("refused create stored a record")
			}
			if tc.patchAddN == 0 {
				return
			}
			id := "byo-alice--big"
			patch := fmt.Sprintf(`{"repos":{"add":%s}}`, quoted(all[tc.registerN:]))
			before, _ := env.s.selfService.get(id)
			rec = env.v1Patch(t, selfServiceTestUser, id, env.etagOf(t, id), patch)
			if rec.Code != tc.wantPatch {
				t.Fatalf("PATCH: %d, want %d (%s)", rec.Code, tc.wantPatch, rec.Body.String())
			}
			if tc.wantPatchIn != "" && !strings.Contains(rec.Body.String(), tc.wantPatchIn) {
				t.Fatalf("PATCH body %s lacks %s", rec.Body.String(), tc.wantPatchIn)
			}
			after, _ := env.s.selfService.get(id)
			if tc.wantPatch != http.StatusOK && !reflect.DeepEqual(before, after) {
				t.Fatal("refused PATCH changed the record")
			}
			if tc.wantPatch == http.StatusOK && len(after.Repos) != tc.registerN+tc.patchAddN {
				t.Fatalf("repos after PATCH = %d", len(after.Repos))
			}
		})
	}
}

// A v1 PATCH is visible through the legacy list, which still shows the legacy
// view of the edited record.
func TestHubV1PatchVisibleThroughLegacyList(t *testing.T) {
	env := newHubV1TestEnv(t)
	id, _ := env.registerHappy(t)
	if rec := env.v1Patch(t, selfServiceTestUser, id, env.etagOf(t, id), `{"repos":{"add":["alice/docs"]}}`); rec.Code != http.StatusOK {
		t.Fatalf("v1 PATCH: %d %s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Hives []selfServiceHiveView `json:"hives"`
	}
	if err := json.Unmarshal(env.list(t, selfServiceTestUser).Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Hives) != 1 || len(listed.Hives[0].Repos) != 3 {
		t.Fatalf("legacy list after v1 PATCH = %+v", listed.Hives)
	}
}

// The credential must survive an edit across a rotation too: after a PATCH, the
// current key and the previous key (inside the overlap) both still pass the
// heartbeat bearer check, and a refused proof leaves the record and the keys
// exactly as they were.
func TestHubV1PatchKeepsBothKeysAcrossRotation(t *testing.T) {
	env := newHubV1TestEnv(t)
	id, oldKey := env.registerHappy(t)
	rot := env.v1(t, selfServiceTestUser, http.MethodPost, hubV1BasePath+"/registrations/"+id+"/keys", "", nil, env.s.handleHubV1RotateKey)
	if rot.Code != http.StatusCreated {
		t.Fatalf("rotate: %d %s", rot.Code, rot.Body.String())
	}
	var rotated struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rot.Body.Bytes(), &rotated); err != nil {
		t.Fatal(err)
	}

	// Refused proof: nothing changes, not even the revision.
	before, _ := env.s.selfService.get(id)
	rec := env.v1Patch(t, selfServiceTestUser, id, env.etagOf(t, id), `{"repos":{"add":["alice/readonly"]}}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PATCH adding a repo the user cannot admin: %d %s", rec.Code, rec.Body.String())
	}
	if after, _ := env.s.selfService.get(id); !reflect.DeepEqual(before, after) {
		t.Fatalf("refused proof changed the record:\nbefore %+v\nafter  %+v", before, after)
	}

	rec = env.v1Patch(t, selfServiceTestUser, id, env.etagOf(t, id), `{"repos":{"add":["alice/docs"]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH: %d %s", rec.Code, rec.Body.String())
	}
	for name, key := range map[string]string{"previous key": oldKey, "current key": rotated.Key} {
		if gen, ok := env.s.verifyHeartbeatBearerGeneration(key, id); !ok || gen != selfServiceBearerGeneration {
			t.Errorf("%s no longer passes the heartbeat bearer check after PATCH: gen=%d ok=%v", name, gen, ok)
		}
	}
}
