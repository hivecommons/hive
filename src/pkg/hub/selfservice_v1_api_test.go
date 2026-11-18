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
				t.Fatalf("patch_fields = %v, want repos and dashboard_url (org is not editable)", d.PatchFields)
			}
			if got["update"].IfMatch != "required" {
				t.Fatalf("update if_match = %q, want required", got["update"].IfMatch)
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
		{"update", http.MethodPatch, path + "/" + id, env.s.handleHubV1Patch},
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
		{"update", http.MethodPatch, path + "/" + id, env.s.handleHubV1Patch},
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
// repo with the same instant as OwnershipCheckedAt, and a record from before
// the field reports OwnershipCheckedAt for every repo.
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
	for _, legacy := range []bool{false, true} {
		name := "create stamps every repo"
		if legacy {
			name = "legacy record reads OwnershipCheckedAt"
		}
		t.Run(name, func(t *testing.T) {
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
			if legacy {
				if _, err := env.s.selfService.update(id, "", func(h *SelfServiceHive) error { h.RepoProofs = nil; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			got := proofsOf(t, env)
			if len(got) != 2 {
				t.Fatalf("repo_proofs = %v", got)
			}
			for _, repo := range []string{"alice/lab", "alice/tools"} {
				if !got[repo].Equal(created.OwnershipCheckedAt) || got[repo].IsZero() {
					t.Errorf("%s proof = %v, want %v", repo, got[repo], created.OwnershipCheckedAt)
				}
			}
		})
	}
}
