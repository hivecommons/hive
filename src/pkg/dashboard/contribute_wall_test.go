package dashboard

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func wallTestDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(".test-work", strings.NewReplacer("/", "_", " ", "_", ":", "_").Replace(t.Name()))
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func newWallTestServer(t *testing.T, enabled bool) (*Server, string) {
	t.Helper()
	dir := wallTestDir(t)
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.SetContributorsDir(dir)
	s.deps = &Dependencies{Config: &config.Config{Hub: config.HubConfig{ContributeWallEnabled: enabled}}}
	s.contributeHub = NewContributeWSHub(slog.New(slog.NewTextHandler(io.Discard, nil)), s)
	return s, dir
}

func writeWallProfile(t *testing.T, dir, user, tier string, prs int) {
	t.Helper()
	p := ContributorProfile{GitHubUsername: user, ContributorID: "c-" + strings.ToLower(user), TrustTier: tier, RegisteredAt: time.Now().UTC().Format(time.RFC3339), TasksWithPR: prs, AvatarURL: "https://example.invalid/" + user + ".png"}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, user+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func wallReq(method, path, body, user string) *http.Request {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		req.Header.Set("X-Hive-User", user)
	}
	return req
}

func decodeWallResp(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("json %s: %v", rr.Body.String(), err)
	}
	return out
}

func TestContributeWallAuthMatrixDisabledEscapingAndRateLimit(t *testing.T) {
	s, dir := newWallTestServer(t, false)
	writeWallProfile(t, dir, "alice", "contributor", 3)
	writeWallProfile(t, dir, "revoked", "revoked", 0)

	rr := httptest.NewRecorder()
	s.handleContributeWallCreate(rr, wallReq("POST", "/api/contribute/wall", `{"text":"x"}`, ""))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("disabled POST = %d, want 403", rr.Code)
	}

	s.deps.Config.Hub.ContributeWallEnabled = true
	rr = httptest.NewRecorder()
	s.handleContributeWallCreate(rr, wallReq("POST", "/api/contribute/wall", `{"text":"x"}`, ""))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous POST = %d, want 401", rr.Code)
	}
	rr = httptest.NewRecorder()
	s.handleContributeWallCreate(rr, wallReq("POST", "/api/contribute/wall", `{"text":"x"}`, "revoked"))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("revoked POST = %d, want 403", rr.Code)
	}

	for i := 0; i < contributeWallDefaultPostsPerHour; i++ {
		rr = httptest.NewRecorder()
		s.handleContributeWallCreate(rr, wallReq("POST", "/api/contribute/wall", `{"text":"<script>alert(1)</script>","tags":{"model":"m1","backend":"b","repo":"r"}}`, "alice"))
		if rr.Code != http.StatusOK {
			t.Fatalf("post %d = %d: %s", i, rr.Code, rr.Body.String())
		}
	}
	rr = httptest.NewRecorder()
	s.handleContributeWallCreate(rr, wallReq("POST", "/api/contribute/wall", `{"text":"one more"}`, "alice"))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limited POST = %d, want 429", rr.Code)
	}

	rr = httptest.NewRecorder()
	s.handleContributeWallList(rr, wallReq("GET", "/api/contribute/wall", "", ""))
	if rr.Code != http.StatusOK {
		t.Fatal(rr.Code)
	}
	if strings.Contains(rr.Body.String(), "<script>") || !strings.Contains(rr.Body.String(), "\\u003cscript\\u003e") {
		t.Fatalf("wall response did not HTML-escape JSON text: %s", rr.Body.String())
	}
}

func TestContributeWallHideVisibilityFlagDeleteMuteAudit(t *testing.T) {
	s, dir := newWallTestServer(t, true)
	writeWallProfile(t, dir, "alice", "contributor", 4)
	writeWallProfile(t, dir, "bob", "contributor", 1)

	rr := httptest.NewRecorder()
	s.handleContributeWallCreate(rr, wallReq("POST", "/api/contribute/wall", `{"text":"hello"}`, "alice"))
	post := ContributeWallPost{}
	_ = json.Unmarshal(rr.Body.Bytes(), &post)

	flagReq := wallReq("POST", "/api/contribute/wall/"+post.ID+"/flag", "", "bob")
	flagReq.SetPathValue("id", post.ID)
	rr = httptest.NewRecorder()
	s.handleContributeWallFlag(rr, flagReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("flag = %d", rr.Code)
	}

	hideReq := wallReq("POST", "/api/contribute/wall/"+post.ID+"/hide", "", "owner")
	hideReq.Header.Set("X-Hive-Role", config.RoleReadWrite)
	hideReq.SetPathValue("id", post.ID)
	rr = httptest.NewRecorder()
	s.handleContributeWallHide(rr, hideReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("hide = %d %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	s.handleContributeWallList(rr, wallReq("GET", "/api/contribute/wall", "", "bob"))
	if len(decodeWallResp(t, rr)["posts"].([]any)) != 0 {
		t.Fatal("hidden post visible to non-author")
	}
	rr = httptest.NewRecorder()
	s.handleContributeWallList(rr, wallReq("GET", "/api/contribute/wall", "", "alice"))
	if len(decodeWallResp(t, rr)["posts"].([]any)) != 1 {
		t.Fatal("hidden post not visible to author")
	}
	if strings.Contains(rr.Body.String(), `"flags"`) {
		t.Fatalf("public/author wall response leaked flags: %s", rr.Body.String())
	}
	adminList := wallReq("GET", "/api/contribute/wall?flagged=1", "", "owner")
	adminList.Header.Set("X-Hive-Role", config.RoleReadWrite)
	rr = httptest.NewRecorder()
	s.handleContributeWallList(rr, adminList)
	if !strings.Contains(rr.Body.String(), `"flags"`) {
		t.Fatalf("admin flagged view did not include flags: %s", rr.Body.String())
	}

	muteReq := wallReq("POST", "/api/contributors/alice/wall-mute", `{"muted":true}`, "owner")
	muteReq.Header.Set("X-Hive-Role", config.RoleOwner)
	muteReq.Header.Set(ownerRoleVerifiedHeader, "true")
	muteReq.SetPathValue("id", "alice")
	rr = httptest.NewRecorder()
	s.handleContributorWallMute(rr, muteReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("mute = %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	s.handleContributeWallCreate(rr, wallReq("POST", "/api/contribute/wall", `{"text":"muted"}`, "alice"))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("muted post = %d", rr.Code)
	}

	acts := s.audit.Recent(10)
	foundHide, foundMute, foundFlag := false, false, false
	for _, a := range acts {
		if a.Action == "contribute_wall_hide" {
			foundHide = true
		}
		if a.Action == "contribute_wall_mute" {
			foundMute = true
		}
		if a.Action == "contribute_wall_flag" {
			foundFlag = true
		}
	}
	if !foundHide || !foundMute || !foundFlag {
		t.Fatalf("audit missing hide=%v mute=%v flag=%v entries=%v", foundHide, foundMute, foundFlag, acts)
	}
}

func TestContributeWallRetentionAndCorruptTolerance(t *testing.T) {
	s, dir := newWallTestServer(t, true)
	old := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	fresh := time.Now().UTC().Format(time.RFC3339)
	disk := contributeWallDisk{Posts: []ContributeWallPost{{ID: "old", Author: "alice", Text: "old", CreatedAt: old}, {ID: "new", Author: "alice", Text: "new", CreatedAt: fresh}}}
	b, _ := json.Marshal(disk)
	if err := os.WriteFile(filepath.Join(dir, contributeWallFileName), b, 0o600); err != nil {
		t.Fatal(err)
	}
	s.deps.Config.Hub.ContributeWallRetentionDays = 1
	rr := httptest.NewRecorder()
	s.handleContributeWallList(rr, wallReq("GET", "/api/contribute/wall", "", ""))
	posts := decodeWallResp(t, rr)["posts"].([]any)
	if len(posts) != 1 || posts[0].(map[string]any)["id"] != "new" {
		t.Fatalf("retention posts = %#v", posts)
	}

	s2, dir2 := newWallTestServer(t, true)
	wallStores.Delete(dir2)
	if err := os.WriteFile(filepath.Join(dir2, contributeWallFileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	rr = httptest.NewRecorder()
	s2.handleContributeWallList(rr, wallReq("GET", "/api/contribute/wall", "", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("corrupt store = %d", rr.Code)
	}
	if len(decodeWallResp(t, rr)["posts"].([]any)) != 0 {
		t.Fatal("corrupt store should load empty")
	}
}

func TestContributeRunStatsByModel(t *testing.T) {
	s, dir := newWallTestServer(t, true)
	recs := []TaskRunRecord{{TS: time.Now().UTC().Format(time.RFC3339), Model: "m1", Outcome: outcomeCompleted, PRVerified: true}, {TS: time.Now().UTC().Format(time.RFC3339), Model: "m1", Outcome: outcomeFailed}, {TS: time.Now().UTC().Format(time.RFC3339), Model: "m2", Outcome: outcomeCompleted}}
	var buf bytes.Buffer
	for _, r := range recs {
		b, _ := json.Marshal(r)
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, taskRunLogFileName), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	s.handleContributeRunStatsByModel(rr, httptest.NewRequest("GET", "/api/contribute/run-stats/models?days=1", nil))
	body := decodeWallResp(t, rr)
	models := body["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("models len=%d %#v", len(models), models)
	}
	m1 := models[0].(map[string]any)
	if m1["model"] != "m1" || m1["runs"].(float64) != 2 || m1["verified_pr_share"].(float64) != 0.5 || m1["failure_rate"].(float64) != 0.5 {
		t.Fatalf("m1 stats %#v", m1)
	}
}

func TestContributeWallSSEPayload(t *testing.T) {
	s, dir := newWallTestServer(t, true)
	writeWallProfile(t, dir, "alice", "contributor", 0)
	sub := s.contributeHub.sse.subscribe()
	defer s.contributeHub.sse.unsubscribe(sub)
	postReq := wallReq("POST", "/api/contribute/wall", `{"text":"live"}`, "alice")
	prr := httptest.NewRecorder()
	s.handleContributeWallCreate(prr, postReq)
	if prr.Code != http.StatusOK {
		t.Fatalf("post = %d", prr.Code)
	}
	select {
	case ev := <-sub.events:
		if ev.Type != "wall_post" || ev.WallPost == nil || ev.WallPost.Text != "live" {
			t.Fatalf("wall event = %#v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for wall SSE event")
	}
	rec := httptest.NewRecorder()
	ok := writeSSE(rec, sseEvent{Type: "wall_post", WallPost: &ContributeWallPost{ID: "p1", Text: "hello"}})
	if !ok || !strings.Contains(rec.Body.String(), `"type":"wall_post"`) || !strings.Contains(rec.Body.String(), `"wall_post"`) {
		t.Fatalf("wall SSE = %q", rec.Body.String())
	}
	s.broadcastWallPost(ContributeWallPost{ID: "secret", Text: "do not leak", Tags: map[string]string{"model": "m"}}, true)
	select {
	case ev := <-sub.events:
		if ev.Type != "wall_hidden" || ev.WallPost == nil || ev.WallPost.ID != "secret" || ev.WallPost.Text != "" || ev.WallPost.Tags != nil {
			t.Fatalf("redacted hidden event = %#v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for hidden wall SSE event")
	}
}
