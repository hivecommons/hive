package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Tests for PATCH /api/hub/v1/registrations/{id} (#18642, #19056). They reuse
// the v1 test env: stubbed GitHub, temp stores, the flag on. The GitHub stub
// answers admin for alice/lab, alice/tools and alice/bulk-*, not-admin for
// alice/readonly and 404 for anything else.

func (env *selfServiceTestEnv) v1Patch(t *testing.T, user, id, ifMatch, body string) *httptest.ResponseRecorder {
	t.Helper()
	var headers map[string]string
	if ifMatch != "" {
		headers = map[string]string{"If-Match": ifMatch}
	}
	return env.v1(t, user, http.MethodPatch, hubV1BasePath+"/registrations/"+id, body, headers, env.s.handleHubV1Patch)
}

// countProofs counts ownership calls the stub receives from now on.
func (env *selfServiceTestEnv) countProofs() *atomic.Int64 {
	var n atomic.Int64
	env.repoCheckGate = func() { n.Add(1) }
	return &n
}

func repoNames(v hubV1RegistrationView) []string {
	return v.Repos
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestHubV1PatchAddRepoKeepsKeyAndProvesOnlyTheAddedRepo(t *testing.T) {
	env := newHubV1TestEnv(t)
	id, key := env.registerHappy(t)
	before, _ := env.s.selfService.get(id)
	etag := env.etagOf(t, id)
	calls := env.countProofs()
	*env.now = env.now.Add(time.Hour)

	rec := env.v1Patch(t, selfServiceTestUser, id, etag, `{"repos":{"add":["alice/bulk-01"]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("ownership calls = %d, want 1 (only the added repo is proven)", got)
	}
	view := decodeV1Registration(t, rec)
	if want := []string{"alice/lab", "alice/tools", "alice/bulk-01"}; !sameStrings(repoNames(view), want) {
		t.Fatalf("repos = %v, want %v", view.Repos, want)
	}
	newETag := rec.Header().Get("ETag")
	if newETag == "" || newETag == etag || view.ETag != newETag {
		t.Fatalf("etag header %q, body %q, old %q", newETag, view.ETag, etag)
	}
	if _, ok := env.s.verifyHeartbeatBearerGeneration(key, id); !ok {
		t.Fatal("the registration key stopped authenticating after PATCH")
	}
	after, _ := env.s.selfService.get(id)
	if after.KeyHash != before.KeyHash || after.PrevKeyHash != before.PrevKeyHash || after.Owner != before.Owner || after.Org != before.Org {
		t.Fatal("PATCH changed the key, owner or org")
	}
	if after.Revision != before.Revision+1 {
		t.Fatalf("revision = %d, want %d", after.Revision, before.Revision+1)
	}
	proofs := map[string]bool{}
	for _, p := range view.RepoProofs {
		proofs[p.Repo] = p.CheckedAt.Equal(before.OwnershipCheckedAt)
		if p.Repo == "alice/bulk-01" && !p.CheckedAt.Equal(*env.now) {
			t.Fatalf("added repo proof time = %v, want %v", p.CheckedAt, *env.now)
		}
	}
	if !proofs["alice/lab"] || !proofs["alice/tools"] {
		t.Fatalf("existing repos lost their proof time: %v", view.RepoProofs)
	}
	if strings.Contains(rec.Body.String(), key) || strings.Contains(rec.Body.String(), "key_hash") {
		t.Fatal("PATCH response leaked key material")
	}
	logs := env.logs.String()
	if !strings.Contains(logs, "audit: selfservice_hive_updated") || strings.Contains(logs, key) || strings.Contains(logs, after.KeyHash) {
		t.Fatalf("audit line missing or leaking: %s", logs)
	}
	// The stale ETag from before the edit no longer applies.
	if stale := env.v1Patch(t, selfServiceTestUser, id, etag, `{"repos":{"add":["alice/bulk-02"]}}`); stale.Code != http.StatusPreconditionFailed || stale.Header().Get("ETag") != newETag {
		t.Fatalf("stale PATCH: %d etag %q", stale.Code, stale.Header().Get("ETag"))
	}
}

func TestHubV1PatchReplaceAndRemoveNeedNoProof(t *testing.T) {
	env := newHubV1TestEnv(t)
	id, key := env.registerHappy(t)
	calls := env.countProofs()

	rec := env.v1Patch(t, selfServiceTestUser, id, env.etagOf(t, id), `{"repos":["alice/lab"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("replace: %d %s", rec.Code, rec.Body.String())
	}
	view := decodeV1Registration(t, rec)
	if !sameStrings(view.Repos, []string{"alice/lab"}) || len(view.RepoProofs) != 1 {
		t.Fatalf("after replace: repos %v proofs %v", view.Repos, view.RepoProofs)
	}
	rec = env.v1Patch(t, selfServiceTestUser, id, rec.Header().Get("ETag"), `{"repos":{"add":["alice/tools"],"remove":["ALICE/LAB"]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("swap: %d %s", rec.Code, rec.Body.String())
	}
	if got := decodeV1Registration(t, rec).Repos; !sameStrings(got, []string{"alice/tools"}) {
		t.Fatalf("after swap repos = %v", got)
	}
	// Replace proved nothing; the swap proved only alice/tools.
	if got := calls.Load(); got != 1 {
		t.Fatalf("ownership calls = %d, want 1", got)
	}
	stored, _ := env.s.selfService.get(id)
	if _, ok := stored.RepoProofs["alice/lab"]; ok || len(stored.RepoProofs) != 1 {
		t.Fatalf("removed repo kept a proof: %v", stored.RepoProofs)
	}
	if _, ok := env.s.verifyHeartbeatBearerGeneration(key, id); !ok {
		t.Fatal("key stopped authenticating")
	}
}

func TestHubV1PatchDashboardURL(t *testing.T) {
	env := newHubV1TestEnv(t)
	id, _ := env.registerHappy(t)
	calls := env.countProofs()

	rec := env.v1Patch(t, selfServiceTestUser, id, env.etagOf(t, id), `{"dashboard_url":"https://new.example.org/dash"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set: %d %s", rec.Code, rec.Body.String())
	}
	if got := decodeV1Registration(t, rec).DashboardURL; got != "https://new.example.org/dash" {
		t.Fatalf("dashboard_url = %q", got)
	}
	if calls.Load() != 0 {
		t.Fatal("a dashboard_url edit made a GitHub call")
	}
	rec = env.v1Patch(t, selfServiceTestUser, id, rec.Header().Get("ETag"), `{"dashboard_url":""}`)
	if rec.Code != http.StatusOK || decodeV1Registration(t, rec).DashboardURL != "" {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}
	for name, body := range map[string]string{
		"http scheme": `{"dashboard_url":"http://new.example.org/"}`,
		"credentials": `{"dashboard_url":"https://user:pw@new.example.org/"}`,
		"not a url":   `{"dashboard_url":"lab"}`,
		"null":        `{"dashboard_url":null}`,
		"number":      `{"dashboard_url":7}`,
	} {
		t.Run(name, func(t *testing.T) {
			bad := env.v1Patch(t, selfServiceTestUser, id, env.etagOf(t, id), body)
			if bad.Code != http.StatusBadRequest {
				t.Fatalf("%s: %d %s", name, bad.Code, bad.Body.String())
			}
		})
	}
}

func TestHubV1PatchRequiresAFreshIfMatch(t *testing.T) {
	env := newHubV1TestEnv(t)
	id, _ := env.registerHappy(t)
	etag := env.etagOf(t, id)
	calls := env.countProofs()
	body := `{"repos":{"add":["alice/bulk-01"]}}`

	missing := env.v1Patch(t, selfServiceTestUser, id, "", body)
	if missing.Code != http.StatusPreconditionRequired || missing.Header().Get("ETag") != etag {
		t.Fatalf("missing If-Match: %d etag %q", missing.Code, missing.Header().Get("ETag"))
	}
	for name, header := range map[string]string{
		"wrong tag": `"0000000000000000000000000"`,
		"weak tag":  "W/" + etag,
	} {
		rec := env.v1Patch(t, selfServiceTestUser, id, header, body)
		if rec.Code != http.StatusPreconditionFailed || rec.Header().Get("ETag") != etag {
			t.Fatalf("%s: %d etag %q", name, rec.Code, rec.Header().Get("ETag"))
		}
	}
	if calls.Load() != 0 {
		t.Fatal("a refused precondition still reached GitHub")
	}
	if after, _ := env.s.selfService.get(id); len(after.Repos) != 2 {
		t.Fatalf("a refused PATCH changed the repos: %v", after.Repos)
	}
	// A list of tags is accepted when one matches.
	if rec := env.v1Patch(t, selfServiceTestUser, id, `"abc", `+etag, body); rec.Code != http.StatusOK {
		t.Fatalf("tag list: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHubV1PatchTwoEditorsOneWins(t *testing.T) {
	env := newHubV1TestEnv(t)
	id, _ := env.registerHappy(t)
	etag := env.etagOf(t, id)
	first := env.v1Patch(t, selfServiceTestUser, id, etag, `{"dashboard_url":"https://one.example.org/"}`)
	second := env.v1Patch(t, selfServiceTestUser, id, etag, `{"dashboard_url":"https://two.example.org/"}`)
	if first.Code != http.StatusOK || second.Code != http.StatusPreconditionFailed {
		t.Fatalf("first %d, second %d", first.Code, second.Code)
	}
	if stored, _ := env.s.selfService.get(id); stored.DashboardURL != "https://one.example.org/" {
		t.Fatalf("dashboard_url = %q: the second editor overwrote the first", stored.DashboardURL)
	}
}

func TestHubV1PatchWildcardStillRefusesAConcurrentRepoChange(t *testing.T) {
	env := newHubV1TestEnv(t)
	id, _ := env.registerHappy(t)
	// While the ownership proof is in flight, someone else edits the repos.
	env.repoCheckGate = func() {
		env.repoCheckGate = nil
		_, err := env.s.selfService.update(id, "", func(h *SelfServiceHive) error {
			h.Repos = []string{"alice/lab"}
			return nil
		})
		if err != nil {
			t.Errorf("concurrent edit: %v", err)
		}
	}
	rec := env.v1Patch(t, selfServiceTestUser, id, "*", `{"repos":{"add":["alice/bulk-01"]}}`)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("PATCH after a concurrent repo change: %d %s", rec.Code, rec.Body.String())
	}
	if stored, _ := env.s.selfService.get(id); !sameStrings(stored.Repos, []string{"alice/lab"}) {
		t.Fatalf("repos = %v: the stale plan was applied", stored.Repos)
	}
}

func TestHubV1PatchNoChangeKeepsTheETag(t *testing.T) {
	env := newHubV1TestEnv(t)
	id, _ := env.registerHappy(t)
	etag := env.etagOf(t, id)
	calls := env.countProofs()
	for name, body := range map[string]string{
		"same dashboard":          `{"dashboard_url":"https://lab.example.org/"}`,
		"re-add existing":         `{"repos":{"add":["alice/lab"]}}`,
		"re-add with other case":  `{"repos":{"add":["Alice/Lab"]}}`,
		"remove an unlisted repo": `{"repos":{"remove":["alice/never"]}}`,
		"same list":               `{"repos":["alice/lab","alice/tools"]}`,
		"empty delta":             `{"repos":{}}`,
	} {
		rec := env.v1Patch(t, selfServiceTestUser, id, etag, body)
		if rec.Code != http.StatusOK || rec.Header().Get("ETag") != etag {
			t.Fatalf("%s: %d etag %q (%s)", name, rec.Code, rec.Header().Get("ETag"), rec.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatal("a no-op PATCH made a GitHub call")
	}
	if strings.Contains(env.logs.String(), "selfservice_hive_updated") {
		t.Fatal("a no-op PATCH wrote an update audit line")
	}
}

func TestHubV1PatchRefusals(t *testing.T) {
	env := newHubV1TestEnv(t)
	id, _ := env.registerHappy(t)
	cases := []struct {
		name   string
		body   string
		status int
		want   string
	}{
		{"org is not editable", `{"org":"someone-else"}`, http.StatusBadRequest, "org cannot be changed"},
		{"org with a valid field", `{"org":"x","dashboard_url":""}`, http.StatusBadRequest, "org cannot be changed"},
		{"owner", `{"owner":"bob"}`, http.StatusBadRequest, `"owner" cannot be changed`},
		{"key", `{"key":"anything"}`, http.StatusBadRequest, `"key" cannot be changed`},
		{"empty object", `{}`, http.StatusBadRequest, "send"},
		{"not an object", `[]`, http.StatusBadRequest, "JSON object"},
		{"not json", `nope`, http.StatusBadRequest, "JSON object"},
		{"null body", `null`, http.StatusBadRequest, "JSON object"},
		{"repos null", `{"repos":null}`, http.StatusBadRequest, "must be a list"},
		{"repos string", `{"repos":"alice/lab"}`, http.StatusBadRequest, "must be a list"},
		{"repos list of numbers", `{"repos":[1]}`, http.StatusBadRequest, "must be a list"},
		{"delta unknown key", `{"repos":{"drop":["alice/lab"]}}`, http.StatusBadRequest, "add and remove"},
		{"add and remove the same repo", `{"repos":{"add":["alice/tools"],"remove":["Alice/Tools"]}}`, http.StatusBadRequest, "both add and remove"},
		{"remove every repo", `{"repos":{"remove":["alice/lab","alice/tools"]}}`, http.StatusBadRequest, "at least one"},
		{"replace with nothing", `{"repos":[]}`, http.StatusBadRequest, "at least one"},
		{"bare org repo", `{"repos":{"add":["alice"]}}`, http.StatusBadRequest, "owner/name"},
		{"path escape repo", `{"repos":{"add":["alice/.."]}}`, http.StatusBadRequest, "owner/name"},
	}
	calls := env.countProofs()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.v1Patch(t, selfServiceTestUser, id, env.etagOf(t, id), tc.body)
			if rec.Code != tc.status || !strings.Contains(decodeError(t, rec)["error"], tc.want) {
				t.Fatalf("%s: %d %s, want %d containing %q", tc.name, rec.Code, rec.Body.String(), tc.status, tc.want)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("a refused body reached GitHub")
	}
	stored, _ := env.s.selfService.get(id)
	if stored.Revision != 1 || !sameStrings(stored.Repos, []string{"alice/lab", "alice/tools"}) || stored.Org != "alice" {
		t.Fatalf("a refused PATCH changed the record: %+v", stored)
	}
	if !strings.Contains(env.logs.String(), "reason=invalid_update") {
		t.Fatal("refusals are not audited as invalid_update")
	}
}

func TestHubV1PatchOwnershipProofFailures(t *testing.T) {
	env := newHubV1TestEnv(t)
	id, _ := env.registerHappy(t)
	etag := env.etagOf(t, id)

	rec := env.v1Patch(t, selfServiceTestUser, id, etag, `{"repos":{"add":["alice/readonly"]}}`)
	if rec.Code != http.StatusForbidden || decodeError(t, rec)["repo"] != "alice/readonly" {
		t.Fatalf("not admin: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.v1Patch(t, selfServiceTestUser, id, etag, `{"repos":{"add":["alice/bulk-01","alice/unknown"]},"dashboard_url":"https://moved.example.org/"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("one unproven repo in a batch: %d %s", rec.Code, rec.Body.String())
	}
	stored, _ := env.s.selfService.get(id)
	if stored.Revision != 1 || len(stored.Repos) != 2 || stored.DashboardURL != "https://lab.example.org/" {
		t.Fatalf("a failed proof still applied part of the edit: %+v", stored)
	}
	if got := env.etagOf(t, id); got != etag {
		t.Fatal("a failed proof changed the ETag")
	}
	if !strings.Contains(env.logs.String(), "reason=not_repo_admin") {
		t.Fatal("proof failure not audited")
	}
}

func TestHubV1PatchOverTheRepoCap(t *testing.T) {
	env := newHubV1TestEnv(t)
	id, _ := env.registerHappy(t)
	t.Setenv(selfServiceMaxReposEnv, "2")
	calls := env.countProofs()
	etag := env.etagOf(t, id)

	for name, body := range map[string]string{
		"delta":       `{"repos":{"add":["alice/bulk-01"]}}`,
		"replacement": `{"repos":["alice/lab","alice/tools","alice/bulk-01"]}`,
	} {
		rec := env.v1Patch(t, selfServiceTestUser, id, etag, body)
		var resp struct {
			Limit    string `json:"limit"`
			MaxRepos int    `json:"max_repos"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusRequestEntityTooLarge || resp.Limit != "max_repos" || resp.MaxRepos != 2 {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatal("an over-cap PATCH reached GitHub")
	}
	// Staying at the cap is fine: swap one repo for another.
	rec := env.v1Patch(t, selfServiceTestUser, id, etag, `{"repos":{"add":["alice/bulk-01"],"remove":["alice/tools"]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("swap at the cap: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHubV1PatchIsOwnerSessionOnly(t *testing.T) {
	env := newHubV1TestEnv(t)
	id, _ := env.registerHappy(t)
	etag := env.etagOf(t, id)
	body := `{"dashboard_url":"https://other.example.org/"}`

	for name, user := range map[string]string{"another user": selfServiceTestOther, "hub admin who is not the owner": selfServiceTestAdmin} {
		if rec := env.v1Patch(t, user, id, etag, body); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: %d, want 404", name, rec.Code)
		}
	}
	if rec := env.v1Patch(t, "", id, etag, body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d, want 401", rec.Code)
	}
	if rec := env.v1Patch(t, selfServiceTestUser, "byo-alice--missing", etag, body); rec.Code != http.StatusNotFound {
		t.Fatalf("missing hive: %d, want 404", rec.Code)
	}
	// The registration key is not a credential for PATCH.
	_, key := env.registerHappy2(t)
	req := httptest.NewRequest(http.MethodPatch, hubV1BasePath+"/registrations/"+id, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("If-Match", etag)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	env.s.requireAuth(env.s.handleHubV1Patch)(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("a registration key edited a registration: %d", rec.Code)
	}
	if stored, _ := env.s.selfService.get(id); stored.DashboardURL != "https://lab.example.org/" {
		t.Fatal("an unauthorized PATCH changed the record")
	}
}

// registerHappy2 registers a second hive for alice and returns its ID and key.
func (env *selfServiceTestEnv) registerHappy2(t *testing.T) (string, string) {
	t.Helper()
	rec := env.v1(t, selfServiceTestUser, http.MethodPost, hubV1BasePath+"/registrations", `{"name":"second","repos":["alice/lab"]}`, nil, env.s.handleHubV1Create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("second registration: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Registration hubV1RegistrationView `json:"registration"`
		Key          string                `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Registration.ID, resp.Key
}

// A PATCH shows up through the v6 list route unchanged in shape: the legacy
// view carries the new repos and dashboard_url and no v1-only fields.
func TestHubV1PatchIsVisibleThroughTheLegacyList(t *testing.T) {
	env := newHubV1TestEnv(t)
	id, _ := env.registerHappy(t)
	rec := env.v1Patch(t, selfServiceTestUser, id, env.etagOf(t, id), `{"repos":{"add":["alice/bulk-01"]},"dashboard_url":"https://moved.example.org/"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	list := env.list(t, selfServiceTestUser)
	var resp struct {
		Hives []map[string]any `json:"hives"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &resp); err != nil || len(resp.Hives) != 1 {
		t.Fatalf("legacy list: %v %s", err, list.Body.String())
	}
	hive := resp.Hives[0]
	repos, ok := hive["repos"].([]any)
	if hive["dashboard_url"] != "https://moved.example.org/" || !ok || len(repos) != 3 {
		t.Fatalf("legacy view = %v", hive)
	}
	for _, v1Only := range []string{"etag", "repo_proofs"} {
		if _, ok := hive[v1Only]; ok {
			t.Fatalf("legacy view gained %q", v1Only)
		}
	}
}
