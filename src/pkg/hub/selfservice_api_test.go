package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	selfServiceTestOrigin   = "https://hive.hivecommons.dev"
	selfServiceTestUser     = "alice"
	selfServiceTestOther    = "bob"
	selfServiceTestAdmin    = "root-admin"
	selfServiceTestToken    = "ghp_selfservice_test_token_do_not_leak"
	selfServiceTestAdminEnv = "HIVE_HUB_ADMINS"
)

// selfServiceTestEnv is a HubServer wired for the registration API with a
// stubbed GitHub, a capturing logger, temp stores and the flag on.
type selfServiceTestEnv struct {
	s      *HubServer
	logs   *lockedBuffer
	github *httptest.Server
	// adminRepos answers permissions.admin=true; any other repo under the
	// stub owner answers admin=false; unknown repos answer 404.
	adminRepos map[string]bool
	now        *time.Time
	// repoCheckGate, when set, runs inside the GitHub stub before it answers,
	// so a test can hold concurrent registrations inside the ownership proof.
	repoCheckGate func()
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newSelfServiceTestEnv(t *testing.T) *selfServiceTestEnv {
	t.Helper()
	t.Setenv(selfServiceRegistrationEnv, "true")
	t.Setenv(selfServiceMaxPerUserEnv, "")
	t.Setenv(selfServiceTestAdminEnv, selfServiceTestAdmin)
	withTempSaaSDirs(t)

	dir := t.TempDir()
	origHMAC := hmacKeyPath
	hmacKeyPath = filepath.Join(dir, "hmac.key")
	t.Cleanup(func() { hmacKeyPath = origHMAC })

	origResolver := privateURLResolver
	privateURLResolver = func(ctx context.Context, host string) ([]string, error) {
		return []string{"203.0.113.10"}, nil
	}
	t.Cleanup(func() { privateURLResolver = origResolver })

	env := &selfServiceTestEnv{logs: &lockedBuffer{}, adminRepos: map[string]bool{"alice/lab": true, "alice/tools": true, "alice/readonly": false}}
	env.github = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+selfServiceTestToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if env.repoCheckGate != nil {
			env.repoCheckGate()
		}
		repo := strings.TrimPrefix(r.URL.Path, "/repos/")
		admin, known := env.adminRepos[repo]
		if !known {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"full_name": repo, "permissions": map[string]bool{"admin": admin, "push": true}})
	}))
	t.Cleanup(env.github.Close)

	logger := slog.New(slog.NewTextHandler(env.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	prevDefault := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	env.now = &now
	store := newSelfServiceStore(filepath.Join(dir, "hub-selfservice.json"))
	store.now = func() time.Time { return now }

	env.s = &HubServer{
		logger:               logger,
		hubSecret:            grantableTestSecret,
		registryPath:         filepath.Join(dir, "hub-registry.json"),
		selfService:          store,
		selfServiceOwnership: &selfServiceOwnershipChecker{apiBase: env.github.URL, client: env.github.Client()},
		registry:             Registry{Hives: []RegistryEntry{}},
	}

	encrypted, err := encryptToken(selfServiceTestToken)
	if err != nil {
		t.Fatalf("encryptToken: %v", err)
	}
	for _, u := range []*SaaSUser{
		{GitHubUsername: selfServiceTestUser, EncryptedToken: encrypted},
		{GitHubUsername: selfServiceTestOther, EncryptedToken: encrypted},
		{GitHubUsername: selfServiceTestAdmin, EncryptedToken: encrypted},
	} {
		if err := saveSaaSUser(u); err != nil {
			t.Fatalf("saveSaaSUser(%s): %v", u.GitHubUsername, err)
		}
	}
	return env
}

// request builds a request through requireAuth for the given handler, signed
// in as user (empty user = anonymous). The Origin header satisfies the CSRF
// check for writes, as a browser on the hub origin would.
func (env *selfServiceTestEnv) request(t *testing.T, user, method, path, body string, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Origin", selfServiceTestOrigin)
	if user != "" {
		req.AddCookie(&http.Cookie{
			Name:  "hive_hub_user",
			Value: mintHubUserCookieValueV2(deriveDomainKey(grantableTestSecret, infoSessionEd25519Seed), user),
		})
	}
	// Path values are set by the mux in production; the tests call the
	// handler directly, so they are filled in from the path here.
	if rest, ok := strings.CutPrefix(path, "/api/hub/selfservice/hives/"); ok {
		id, _, _ := strings.Cut(rest, "/")
		req.SetPathValue("id", id)
	}
	rec := httptest.NewRecorder()
	env.s.requireAuth(handler)(rec, req)
	return rec
}

func (env *selfServiceTestEnv) register(t *testing.T, user, body string) *httptest.ResponseRecorder {
	t.Helper()
	return env.request(t, user, http.MethodPost, "/api/hub/selfservice/hives", body, env.s.handleSelfServiceRegister)
}

func (env *selfServiceTestEnv) list(t *testing.T, user string) *httptest.ResponseRecorder {
	t.Helper()
	return env.request(t, user, http.MethodGet, "/api/hub/selfservice/hives", "", env.s.handleSelfServiceList)
}

func (env *selfServiceTestEnv) rotate(t *testing.T, user, id string) *httptest.ResponseRecorder {
	t.Helper()
	return env.request(t, user, http.MethodPost, "/api/hub/selfservice/hives/"+id+"/rotate", "", env.s.handleSelfServiceRotate)
}

func (env *selfServiceTestEnv) remove(t *testing.T, user, id string) *httptest.ResponseRecorder {
	t.Helper()
	return env.request(t, user, http.MethodDelete, "/api/hub/selfservice/hives/"+id, "", env.s.handleSelfServiceDelete)
}

func (env *selfServiceTestEnv) status(t *testing.T, user string) *httptest.ResponseRecorder {
	t.Helper()
	return env.request(t, user, http.MethodGet, "/api/hub/selfservice/status", "", env.s.handleSelfServiceStatus)
}

const selfServiceHappyBody = `{"name":"Lab","org":"alice","repos":["alice/lab","alice/tools"],"dashboard_url":"https://lab.example.org/"}`

// registerHappy registers alice's lab hive and returns the ID and raw key.
func (env *selfServiceTestEnv) registerHappy(t *testing.T) (string, string) {
	t.Helper()
	rec := env.register(t, selfServiceTestUser, selfServiceHappyBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: status %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Hive selfServiceHiveView `json:"hive"`
		Key  string              `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	if resp.Key == "" || resp.Hive.ID == "" {
		t.Fatalf("register response missing id or key: %s", rec.Body.String())
	}
	return resp.Hive.ID, resp.Key
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("error body is not JSON: %q", rec.Body.String())
	}
	return m
}

func TestSelfServiceAPIUnauthenticated(t *testing.T) {
	env := newSelfServiceTestEnv(t)
	cases := map[string]*httptest.ResponseRecorder{
		"status":   env.status(t, ""),
		"list":     env.list(t, ""),
		"register": env.register(t, "", selfServiceHappyBody),
		"rotate":   env.rotate(t, "", "byo-alice--lab"),
		"delete":   env.remove(t, "", "byo-alice--lab"),
	}
	for name, rec := range cases {
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: anonymous status = %d, want 401 (body %s)", name, rec.Code, rec.Body.String())
		}
	}
	if len(env.s.selfService.list()) != 0 {
		t.Fatal("anonymous request stored something")
	}
}

func TestSelfServiceAPIFlagOffIsNotFound(t *testing.T) {
	env := newSelfServiceTestEnv(t)
	t.Setenv(selfServiceRegistrationEnv, "")
	cases := map[string]*httptest.ResponseRecorder{
		"list":     env.list(t, selfServiceTestUser),
		"register": env.register(t, selfServiceTestUser, selfServiceHappyBody),
		"rotate":   env.rotate(t, selfServiceTestUser, "byo-alice--lab"),
		"delete":   env.remove(t, selfServiceTestUser, "byo-alice--lab"),
	}
	for name, rec := range cases {
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s with flag off: status = %d, want 404", name, rec.Code)
		}
	}
	if len(env.s.selfService.list()) != 0 {
		t.Fatal("flag-off register stored something")
	}
	rec := env.status(t, selfServiceTestUser)
	if rec.Code != http.StatusOK {
		t.Fatalf("status with flag off: %d", rec.Code)
	}
	var st struct {
		Enabled    bool `json:"enabled"`
		MaxPerUser int  `json:"max_per_user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Enabled || st.MaxPerUser != selfServiceMaxPerUserDefault {
		t.Fatalf("status = %+v, want enabled=false max=%d", st, selfServiceMaxPerUserDefault)
	}
}

func TestSelfServiceAPIRegisterHappyPath(t *testing.T) {
	env := newSelfServiceTestEnv(t)
	rec := env.register(t, selfServiceTestUser, selfServiceHappyBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Hive     selfServiceHiveView    `json:"hive"`
		Key      string                 `json:"key"`
		HubURL   string                 `json:"hub_url"`
		Snippets selfServiceKeySnippets `json:"snippets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Hive.ID != "byo-alice--lab" {
		t.Fatalf("id = %q, want byo-alice--lab", resp.Hive.ID)
	}
	if resp.Hive.Owner != selfServiceTestUser || resp.Hive.Org != "alice" || len(resp.Hive.Repos) != 2 || resp.Hive.DashboardURL != "https://lab.example.org/" {
		t.Fatalf("view = %+v", resp.Hive)
	}
	if resp.Hive.OwnershipCheckedAt.IsZero() {
		t.Fatal("OwnershipCheckedAt not stamped")
	}
	for name, snippet := range map[string]string{"compose": resp.Snippets.DockerCompose, "podman": resp.Snippets.Podman, "k8s": resp.Snippets.Kubernetes} {
		for _, want := range []string{"HIVE_HUB_URL", "HIVE_ID", "HIVE_HEARTBEAT_KEY", resp.Key, resp.Hive.ID} {
			if !strings.Contains(snippet, want) {
				t.Errorf("%s snippet lacks %q", name, want)
			}
		}
	}

	// The store holds only the hash, and the hub accepts the raw key as this
	// hive's heartbeat bearer and no other hive's.
	stored, ok := env.s.selfService.get("byo-alice--lab")
	if !ok {
		t.Fatal("hive not stored")
	}
	if stored.KeyHash != hashSelfServiceKey(resp.Key) || stored.KeyHash == resp.Key {
		t.Fatal("store does not hold exactly the hash of the returned key")
	}
	if gen, ok := env.s.verifyHeartbeatBearerGeneration(resp.Key, "byo-alice--lab"); !ok || gen != selfServiceBearerGeneration {
		t.Fatalf("raw key does not authenticate the hive: gen=%d ok=%v", gen, ok)
	}
	if _, ok := env.s.verifyHeartbeatBearerGeneration(resp.Key, "byo-alice--other"); ok {
		t.Fatal("raw key authenticated a different hive")
	}
	if !strings.Contains(env.logs.String(), "audit: selfservice_hive_registered") {
		t.Fatal("no registration audit line")
	}

	// The GitHub stub saw the user's own token, not the hub master.
	listRec := env.list(t, selfServiceTestUser)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list: %d", listRec.Code)
	}
	var listed struct {
		Hives []selfServiceHiveView `json:"hives"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Hives) != 1 || listed.Hives[0].ID != "byo-alice--lab" {
		t.Fatalf("list = %+v", listed.Hives)
	}
	for _, forbidden := range []string{resp.Key, stored.KeyHash, "key_hash", grantableTestSecret} {
		if strings.Contains(listRec.Body.String(), forbidden) {
			t.Fatalf("list body leaks %q", forbidden)
		}
	}
	// The other user sees nothing.
	otherRec := env.list(t, selfServiceTestOther)
	if !strings.Contains(otherRec.Body.String(), `"hives":[]`) {
		t.Fatalf("other user's list = %s", otherRec.Body.String())
	}
}

func TestSelfServiceAPIRegisterRefusals(t *testing.T) {
	env := newSelfServiceTestEnv(t)
	// Pre-existing registry entry for the duplicate case.
	env.s.registry.Hives = []RegistryEntry{{ID: "byo-alice--taken"}}
	cases := []struct {
		name   string
		body   string
		status int
		reason string
	}{
		{"reserved hosted prefix", `{"name":"hosted-prod","repos":["alice/lab"]}`, http.StatusBadRequest, selfServiceRefusedReservedPrefix},
		{"reserved saas prefix", `{"name":"saas-prod","repos":["alice/lab"]}`, http.StatusBadRequest, selfServiceRefusedReservedPrefix},
		{"reserved byo prefix", `{"name":"byo-x","repos":["alice/lab"]}`, http.StatusBadRequest, selfServiceRefusedReservedPrefix},
		{"slash in name", `{"name":"a/b","repos":["alice/lab"]}`, http.StatusBadRequest, selfServiceRefusedInvalidName},
		{"invalid characters", `{"name":"lab!","repos":["alice/lab"]}`, http.StatusBadRequest, selfServiceRefusedInvalidName},
		{"empty name", `{"name":"","repos":["alice/lab"]}`, http.StatusBadRequest, selfServiceRefusedInvalidName},
		{"too long", fmt.Sprintf(`{"name":%q,"repos":["alice/lab"]}`, strings.Repeat("x", 120)), http.StatusBadRequest, selfServiceRefusedInvalidName},
		{"duplicate registry id", `{"name":"taken","repos":["alice/lab"]}`, http.StatusConflict, selfServiceRefusedDuplicateID},
		{"no repos", `{"name":"lab","repos":[]}`, http.StatusBadRequest, selfServiceRefusedInvalidRepos},
		{"bare org repo", `{"name":"lab","repos":["alice"]}`, http.StatusBadRequest, selfServiceRefusedInvalidRepos},
		{"dot-dot repo segment", `{"name":"lab","repos":["alice/.."]}`, http.StatusBadRequest, selfServiceRefusedInvalidRepos},
		{"dot repo segment", `{"name":"lab","repos":["alice/."]}`, http.StatusBadRequest, selfServiceRefusedInvalidRepos},
		{"dot-dot owner segment", `{"name":"lab","repos":["../lab"]}`, http.StatusBadRequest, selfServiceRefusedInvalidRepos},
		{"leading hyphen name", `{"name":"-lab","repos":["alice/lab"]}`, http.StatusBadRequest, selfServiceRefusedInvalidName},
		{"http dashboard", `{"name":"lab","repos":["alice/lab"],"dashboard_url":"http://lab.example.org"}`, http.StatusBadRequest, selfServiceRefusedInvalidURL},
		{"private dashboard", `{"name":"lab","repos":["alice/lab"],"dashboard_url":"https://10.0.0.5/"}`, http.StatusBadRequest, selfServiceRefusedInvalidURL},
		{"bad json", `{`, http.StatusBadRequest, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.register(t, selfServiceTestUser, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.status, rec.Body.String())
			}
			if tc.reason != "" && !strings.Contains(env.logs.String(), "reason="+tc.reason) {
				t.Fatalf("no refused audit line with reason %s", tc.reason)
			}
		})
	}
	if n := len(env.s.selfService.list()); n != 0 {
		t.Fatalf("refused registrations stored %d hives", n)
	}
	// The duplicate check also covers the self-service store itself.
	env.registerHappy(t)
	rec := env.register(t, selfServiceTestUser, selfServiceHappyBody)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second identical registration: %d", rec.Code)
	}
}

func TestSelfServiceAPIOwnershipProof(t *testing.T) {
	env := newSelfServiceTestEnv(t)
	t.Run("admin false names the repo", func(t *testing.T) {
		rec := env.register(t, selfServiceTestUser, `{"name":"lab","repos":["alice/lab","alice/readonly"]}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
		}
		body := decodeError(t, rec)
		if body["repo"] != "alice/readonly" || !strings.Contains(body["error"], "alice/readonly") {
			t.Fatalf("refusal does not name the repo: %v", body)
		}
		if !strings.Contains(env.logs.String(), "reason="+selfServiceRefusedNotAdmin) {
			t.Fatal("no refused audit line")
		}
	})
	t.Run("unknown repo refused", func(t *testing.T) {
		rec := env.register(t, selfServiceTestUser, `{"name":"lab","repos":["alice/missing"]}`)
		if rec.Code != http.StatusForbidden || decodeError(t, rec)["repo"] != "alice/missing" {
			t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("no stored token", func(t *testing.T) {
		if err := saveSaaSUser(&SaaSUser{GitHubUsername: selfServiceTestOther}); err != nil {
			t.Fatal(err)
		}
		rec := env.register(t, selfServiceTestOther, `{"name":"lab","repos":["alice/lab"]}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(env.logs.String(), "reason="+selfServiceRefusedNoToken) {
			t.Fatal("no refused audit line")
		}
	})
	if n := len(env.s.selfService.list()); n != 0 {
		t.Fatalf("ownership failures stored %d hives", n)
	}
	// Admin on every repo passes.
	env.registerHappy(t)
}

func TestSelfServiceAPIPerUserCap(t *testing.T) {
	env := newSelfServiceTestEnv(t)
	t.Setenv(selfServiceMaxPerUserEnv, "2")
	for _, name := range []string{"one", "two"} {
		rec := env.register(t, selfServiceTestUser, fmt.Sprintf(`{"name":%q,"repos":["alice/lab"]}`, name))
		if rec.Code != http.StatusCreated {
			t.Fatalf("register %s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	rec := env.register(t, selfServiceTestUser, `{"name":"three","repos":["alice/lab"]}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over cap: status = %d, want 429", rec.Code)
	}
	if n := len(env.s.selfService.list()); n != 2 {
		t.Fatalf("store has %d hives after cap, want 2", n)
	}
	if !strings.Contains(env.logs.String(), "reason="+selfServiceRefusedCap) {
		t.Fatal("no cap audit line")
	}
	// The cap is per user: bob can still register.
	rec = env.register(t, selfServiceTestOther, `{"name":"mine","repos":["alice/lab"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("other user under cap: %d %s", rec.Code, rec.Body.String())
	}
	var st struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(env.status(t, selfServiceTestUser).Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Count != 2 {
		t.Fatalf("status count = %d, want 2", st.Count)
	}
}

// TestSelfServiceAPIPerUserCapConcurrent pins #18166: the handler's cap count
// runs before the GitHub ownership calls, so two POSTs from one user at cap-1
// can both pass it. The stub holds both requests inside the ownership proof
// until each has passed the handler's count; the store's own check under its
// lock must still let exactly one through.
func TestSelfServiceAPIPerUserCapConcurrent(t *testing.T) {
	env := newSelfServiceTestEnv(t)
	t.Setenv(selfServiceMaxPerUserEnv, "2")
	if rec := env.register(t, selfServiceTestUser, `{"name":"one","repos":["alice/lab"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("register one: %d %s", rec.Code, rec.Body.String())
	}

	var arrived sync.WaitGroup
	arrived.Add(2)
	env.repoCheckGate = func() {
		arrived.Done()
		all := make(chan struct{})
		go func() { arrived.Wait(); close(all) }()
		select {
		case <-all:
		case <-time.After(10 * time.Second):
			// A request never reached the stub; the counts below report it.
		}
	}
	codes := make([]int, 2)
	var done sync.WaitGroup
	for i, name := range []string{"two", "three"} {
		done.Add(1)
		go func(i int, name string) {
			defer done.Done()
			codes[i] = env.register(t, selfServiceTestUser, fmt.Sprintf(`{"name":%q,"repos":["alice/lab"]}`, name)).Code
		}(i, name)
	}
	done.Wait()

	created, capped := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusTooManyRequests:
			capped++
		}
	}
	if created != 1 || capped != 1 {
		t.Fatalf("concurrent registers at cap-1: codes %v, want exactly one 201 and one 429", codes)
	}
	if n := env.s.selfServiceCount(selfServiceTestUser); n != 2 {
		t.Fatalf("store holds %d of alice's hives, want 2 (the cap)", n)
	}
	if !strings.Contains(env.logs.String(), "reason="+selfServiceRefusedCap) {
		t.Fatal("no cap audit line for the store-level refusal")
	}
}

// TestSelfServiceAPIHyphenLoginsDoNotCollide pins #18166: with a single '-'
// separator, login a-b + name c and login a + name b-c both produced
// byo-a-b-c, so one user could squat the other's ID. The "--" separator keeps
// them apart.
func TestSelfServiceAPIHyphenLoginsDoNotCollide(t *testing.T) {
	env := newSelfServiceTestEnv(t)
	encrypted, err := encryptToken(selfServiceTestToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, login := range []string{"a", "a-b"} {
		if err := saveSaaSUser(&SaaSUser{GitHubUsername: login, EncryptedToken: encrypted}); err != nil {
			t.Fatal(err)
		}
	}
	ids := map[string]string{}
	for _, tc := range []struct{ login, name, want string }{
		{"a", "b-c", "byo-a--b-c"},
		{"a-b", "c", "byo-a-b--c"},
	} {
		rec := env.register(t, tc.login, fmt.Sprintf(`{"name":%q,"repos":["alice/lab"]}`, tc.name))
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s/%s: status %d body %s", tc.login, tc.name, rec.Code, rec.Body.String())
		}
		var resp struct {
			Hive selfServiceHiveView `json:"hive"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Hive.ID != tc.want {
			t.Fatalf("%s/%s: id = %q, want %q", tc.login, tc.name, resp.Hive.ID, tc.want)
		}
		ids[tc.login] = resp.Hive.ID
	}
	for login, id := range ids {
		if h, ok := env.s.selfService.get(id); !ok || !selfServiceOwns(h, login) {
			t.Fatalf("%s does not own %s: %+v", login, id, h)
		}
	}
}

func TestSelfServiceBuildIDRefusesAmbiguousInput(t *testing.T) {
	for _, tc := range []struct {
		login, name, reason string
	}{
		{"a--b", "lab", selfServiceRefusedInvalidLogin},
		{"a-", "lab", selfServiceRefusedInvalidLogin},
		{"-a", "lab", selfServiceRefusedInvalidLogin},
		{"a/b", "lab", selfServiceRefusedInvalidLogin},
		{"alice", "-lab", selfServiceRefusedInvalidName},
	} {
		if id, reason, err := selfServiceBuildID(tc.login, tc.name); err == nil || reason != tc.reason {
			t.Errorf("selfServiceBuildID(%q, %q) = %q, %q, %v; want refusal %s", tc.login, tc.name, id, reason, err, tc.reason)
		}
	}
	if id, _, err := selfServiceBuildID("Alice-Smith", "Lab--Two"); err != nil || id != "byo-alice-smith--lab--two" {
		t.Fatalf("selfServiceBuildID(Alice-Smith, Lab--Two) = %q, %v", id, err)
	}
}

func TestIsValidRepoRefRefusesDotSegments(t *testing.T) {
	for _, ref := range []string{".", "..", "owner/.", "owner/..", "./repo", "../repo"} {
		if isValidRepoRef(ref) {
			t.Errorf("isValidRepoRef(%q) = true, want false", ref)
		}
	}
	for _, ref := range []string{"owner/.github", "owner/a.b", "owner/...", ".github"} {
		if !isValidRepoRef(ref) {
			t.Errorf("isValidRepoRef(%q) = false, want true", ref)
		}
	}
}

func TestSelfServiceAPIRotateInvalidatesOldKey(t *testing.T) {
	env := newSelfServiceTestEnv(t)
	id, oldKey := env.registerHappy(t)

	rec := env.rotate(t, selfServiceTestUser, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != id || resp.Key == "" || resp.Key == oldKey {
		t.Fatalf("rotate response = %+v", resp)
	}
	if _, ok := env.s.verifyHeartbeatBearerGeneration(resp.Key, id); !ok {
		t.Fatal("new key does not authenticate")
	}
	// Inside the overlap the old key still works so a redeploy is not an
	// outage; after it, the old key is dead.
	if _, ok := env.s.verifyHeartbeatBearerGeneration(oldKey, id); !ok {
		t.Fatal("old key refused inside the rotation overlap")
	}
	*env.now = env.now.Add(selfServiceKeyRotationOverlap + time.Second)
	if _, ok := env.s.verifyHeartbeatBearerGeneration(oldKey, id); ok {
		t.Fatal("old key still authenticates after the overlap")
	}
	if _, ok := env.s.verifyHeartbeatBearerGeneration(resp.Key, id); !ok {
		t.Fatal("new key stopped authenticating")
	}
	if !strings.Contains(env.logs.String(), "audit: selfservice_hive_rotated") {
		t.Fatal("no rotation audit line")
	}
}

func TestSelfServiceAPIDeleteRemoves(t *testing.T) {
	env := newSelfServiceTestEnv(t)
	id, key := env.registerHappy(t)
	// A heartbeat created a registry entry; delete must take it too.
	env.s.registry.Hives = append(env.s.registry.Hives, RegistryEntry{ID: id, Online: true})

	rec := env.remove(t, selfServiceTestUser, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := env.s.selfService.get(id); ok {
		t.Fatal("hive still in the self-service store")
	}
	if _, ok := env.s.verifyHeartbeatBearerGeneration(key, id); ok {
		t.Fatal("revoked key still authenticates")
	}
	if env.s.selfServiceRegistryHas(id) {
		t.Fatal("registry entry survived the delete")
	}
	if !strings.Contains(env.logs.String(), "audit: selfservice_hive_revoked") {
		t.Fatal("no revocation audit line")
	}
	// Gone means gone: a second delete is 404.
	if rec := env.remove(t, selfServiceTestUser, id); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete: %d", rec.Code)
	}
}

func TestSelfServiceAPIAnotherUsersHiveIsNotFound(t *testing.T) {
	env := newSelfServiceTestEnv(t)
	id, key := env.registerHappy(t)

	if rec := env.rotate(t, selfServiceTestOther, id); rec.Code != http.StatusNotFound {
		t.Fatalf("rotate by other user: %d, want 404", rec.Code)
	}
	if rec := env.remove(t, selfServiceTestOther, id); rec.Code != http.StatusNotFound {
		t.Fatalf("delete by other user: %d, want 404", rec.Code)
	}
	if rec := env.rotate(t, selfServiceTestUser, "byo-alice--nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("rotate of missing hive: %d, want 404", rec.Code)
	}
	if _, ok := env.s.verifyHeartbeatBearerGeneration(key, id); !ok {
		t.Fatal("other user's attempts disturbed the owner's key")
	}
	// A hub admin may revoke but not rotate (rotation hands out the key).
	if rec := env.rotate(t, selfServiceTestAdmin, id); rec.Code != http.StatusNotFound {
		t.Fatalf("rotate by admin: %d, want 404", rec.Code)
	}
	if rec := env.remove(t, selfServiceTestAdmin, id); rec.Code != http.StatusOK {
		t.Fatalf("delete by admin: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := env.s.selfService.get(id); ok {
		t.Fatal("admin delete did not remove the hive")
	}
	if !strings.Contains(env.logs.String(), "by_admin=true") {
		t.Fatal("admin revocation not attributed in the audit line")
	}
}

func TestSelfServiceAPINeverLeaksKeyMaterial(t *testing.T) {
	env := newSelfServiceTestEnv(t)
	id, key := env.registerHappy(t)
	rec := env.rotate(t, selfServiceTestUser, id)
	var rotated struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rotated); err != nil {
		t.Fatal(err)
	}
	env.list(t, selfServiceTestUser)
	env.status(t, selfServiceTestUser)
	env.remove(t, selfServiceTestOther, id)

	logs := env.logs.String()
	for _, secret := range []string{key, rotated.Key, grantableTestSecret, selfServiceTestToken} {
		if secret == "" {
			t.Fatal("empty secret under test")
		}
		if strings.Contains(logs, secret) {
			t.Fatalf("log output contains secret material: %s", logs)
		}
	}
	data, err := os.ReadFile(env.s.selfService.path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	disk := string(data)
	for _, secret := range []string{key, rotated.Key, grantableTestSecret, selfServiceTestToken} {
		if strings.Contains(disk, secret) {
			t.Fatal("store file contains raw secret material")
		}
	}
	if !strings.Contains(disk, hashSelfServiceKey(rotated.Key)) || !strings.Contains(disk, hashSelfServiceKey(key)) {
		t.Fatal("store file lacks the expected hashes")
	}
	// The GitHub client sent the user's token, never the hub master or a key:
	// the stub answered 401 to anything else, and registration succeeded.
}

func TestSelfServiceRegistrationEnabledParsing(t *testing.T) {
	for raw, want := range map[string]bool{"": false, "0": false, "false": false, "no": false, "1": true, "true": true, "YES": true, "On": true, "maybe": false} {
		t.Setenv(selfServiceRegistrationEnv, raw)
		if got := selfServiceRegistrationEnabled(); got != want {
			t.Errorf("%q: enabled = %v, want %v", raw, got, want)
		}
	}
	t.Setenv(selfServiceMaxPerUserEnv, "")
	if selfServiceMaxPerUser() != selfServiceMaxPerUserDefault {
		t.Fatal("default cap not applied")
	}
	t.Setenv(selfServiceExpiryEnv, "")
	if selfServiceExpiry() != selfServiceExpiryDefault {
		t.Fatal("default expiry not applied")
	}
	t.Setenv(selfServiceExpiryEnv, "48h")
	if selfServiceExpiry() != 2*selfServiceExpiryDefault {
		t.Fatal("expiry env override ignored")
	}
}
