package hub

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func (env *selfServiceTestEnv) syncRepos(id, key, etag, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPatch, hubV1BasePath+"/registrations/"+id, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("If-Match", etag)
	rec := httptest.NewRecorder()
	env.s.mux.ServeHTTP(rec, req)
	return rec
}

func TestHubV1SyncReposAppliesOnlyProvenRepos(t *testing.T) {
	env := newHubV1TestEnv(t)
	env.s.mux = http.NewServeMux()
	env.s.registerHubV1Routes()
	t.Setenv(hubV1SyncReposEnv, "true")
	id, key := env.registerHappy(t)
	before, _ := env.s.selfService.get(id)
	calls := env.countProofs()
	*env.now = env.now.Add(time.Hour)

	body := `{"op":"sync-repos","repos":["ALICE/LAB","alice/bulk-01","ALICE/BULK-01"]}`
	rec := env.syncRepos(id, key, "*", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
	}
	view := decodeV1Registration(t, rec)
	if !slices.Equal(view.Repos, []string{"alice/lab"}) || !slices.Equal(view.PendingProof, []string{"alice/bulk-01"}) {
		t.Fatalf("repos=%v pending=%v", view.Repos, view.PendingProof)
	}
	if len(view.RepoProofs) != 1 || view.RepoProofs[0].Repo != "alice/lab" || !view.RepoProofs[0].CheckedAt.Equal(before.OwnershipCheckedAt) {
		t.Fatalf("proofs = %+v", view.RepoProofs)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" || etag == selfServiceETag(before) || view.ETag != etag {
		t.Fatalf("sync did not publish a new ETag: %q", etag)
	}
	if view.Owner != before.Owner || view.Org != before.Org || view.DashboardURL != before.DashboardURL || !env.s.selfService.verify(id, key) {
		t.Fatal("sync changed registration identity, dashboard or key")
	}
	if calls.Load() != 0 {
		t.Fatal("sync attempted a GitHub ownership check")
	}
	if retry := env.syncRepos(id, key, etag, body); retry.Code != http.StatusOK || retry.Header().Get("ETag") != etag {
		t.Fatalf("identical sync changed state: %d %s", retry.Code, retry.Body.String())
	}

	reloaded := newSelfServiceStore(env.s.selfService.path)
	if err := reloaded.load(); err != nil {
		t.Fatal(err)
	}
	stored, ok := reloaded.get(id)
	if !ok || !slices.Equal(stored.PendingProof, view.PendingProof) || !slices.Equal(stored.Repos, view.Repos) {
		t.Fatalf("sync not persisted: repos=%v pending=%v", stored.Repos, stored.PendingProof)
	}

	approval := env.v1Patch(t, selfServiceTestUser, id, etag, `{"repos":{"add":["alice/bulk-01"]}}`)
	if approval.Code != http.StatusOK {
		t.Fatalf("owner proof: %d %s", approval.Code, approval.Body.String())
	}
	approved := decodeV1Registration(t, approval)
	if !slices.Equal(approved.Repos, []string{"alice/lab", "alice/bulk-01"}) || len(approved.PendingProof) != 0 || calls.Load() != 1 {
		t.Fatalf("owner proof: repos=%v pending=%v checks=%d", approved.Repos, approved.PendingProof, calls.Load())
	}
}

func TestHubV1SyncReposRefusesOutOfScopeRequests(t *testing.T) {
	env := newHubV1TestEnv(t)
	env.s.mux = http.NewServeMux()
	env.s.registerHubV1Routes()
	id, key := env.registerHappy(t)
	foreignID, _ := env.registerHappy2(t)
	etag, foreignETag := env.etagOf(t, id), env.etagOf(t, foreignID)
	body := `{"op":"sync-repos","repos":["alice/readonly"]}`

	t.Setenv(hubV1SyncReposEnv, "")
	if rec := env.syncRepos(id, key, etag, body); rec.Code != http.StatusNotFound {
		t.Fatalf("flag off: %d %s", rec.Code, rec.Body.String())
	}
	t.Setenv(hubV1SyncReposEnv, "on")
	for _, target := range []string{foreignID, "byo-alice--missing"} {
		if rec := env.syncRepos(target, key, etag, body); rec.Code != http.StatusNotFound || rec.Header().Get("ETag") != "" {
			t.Fatalf("foreign or missing ID %q: %d %s", target, rec.Code, rec.Body.String())
		}
	}
	for _, field := range []string{"org", "owner", "dashboard_url", "id", "pending_proof", "repo_proofs"} {
		withField := strings.TrimSuffix(body, "}") + `,"` + field + `":null}`
		if rec := env.syncRepos(id, key, etag, withField); rec.Code != http.StatusBadRequest {
			t.Fatalf("forbidden field %q: %d %s", field, rec.Code, rec.Body.String())
		}
	}
	if rec := env.syncRepos(id, key, etag, `{"repos":["alice/lab"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("ordinary PATCH with key: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.syncRepos(id, key, "", body); rec.Code != http.StatusPreconditionRequired || rec.Header().Get("ETag") != etag {
		t.Fatalf("missing If-Match: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.syncRepos(id, key, foreignETag, body); rec.Code != http.StatusPreconditionFailed || rec.Header().Get("ETag") != etag {
		t.Fatalf("stale If-Match: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.syncRepos(id, key, etag, body); rec.Code != http.StatusBadRequest {
		t.Fatalf("all-new repo list: %d %s", rec.Code, rec.Body.String())
	}
	if env.etagOf(t, id) != etag || env.etagOf(t, foreignID) != foreignETag {
		t.Fatal("refused requests changed a registration")
	}
}
