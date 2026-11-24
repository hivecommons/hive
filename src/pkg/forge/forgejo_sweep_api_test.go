package forge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func newSweepAPIServer(t *testing.T, routes map[string]string) (SweepAPI, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token fj-token" {
			t.Errorf("%s %s Authorization = %q", r.Method, r.URL.Path, got)
		}
		key := r.Method + " " + r.URL.Path
		if r.Method == http.MethodPatch {
			b, _ := io.ReadAll(r.Body)
			key += " " + strings.TrimSpace(string(b))
		}
		calls = append(calls, key+"?"+r.URL.RawQuery)
		body, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
			return
		}
		if body == "500" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	api, err := NewForgejoSweepAPI("fj-token", Options{BaseURL: srv.URL, Org: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if api.Kind() != KindForgejo {
		t.Fatalf("Kind() = %q", api.Kind())
	}
	return api, &calls
}

const repoAPI = "/api/v1/repos/acme/widgets"

func TestForgejoSweepAPIReads(t *testing.T) {
	merged := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	mergedJSON, _ := json.Marshal(merged)
	api, calls := newSweepAPIServer(t, map[string]string{
		"GET " + repoAPI + "/pulls/7": `{"number":7,"state":"open","draft":false,"merged":false,"mergeable":true,
			"user":{"login":"hive-bot"},"labels":[{"name":"lgtm"},{"name":""}],"head":{"ref":"feat","sha":"abc"},"base":{"ref":"main"}}`,
		"GET " + repoAPI + "/commits/abc/status": `{"state":"success","total_count":2}`,
		"GET " + repoAPI + "/issues/7/timeline": `[
			{"type":"label","body":"1","user":{"login":"alice"},"label":{"name":"lgtm"}},
			{"type":"comment","body":"hi","user":{"login":"bob"}},
			{"type":"pull_push","body":"{\"is_force_push\":false,\"commit_ids\":[\"abc\"]}","user":{"login":"hive-bot"}},
			{"type":"label","body":"1","user":{"login":"carol"},"label":{"name":"other"}},
			{"type":"label","body":"","user":{"login":"dave"},"label":{"name":"LGTM"}},
			{"type":"label","body":"1","user":{"login":"erin"},"label":{"name":"lgtm"}},
			{"type":"reopen","user":{"login":"erin"}}]`,
		"GET " + repoAPI + "/issues/9/timeline": `[
			{"type":"label","body":"1","user":{"login":"erin"},"label":{"name":"lgtm"}},
			{"type":"comment","body":"thanks","user":{"login":"hive-bot"}},
			{"type":"pull_push","body":"{\"is_force_push\":true,\"commit_ids\":[\"abc\",\"def\"]}","user":{"login":"hive-bot"}}]`,
		"GET " + repoAPI + "/collaborators/erin/permission": `{"permission":"Write"}`,
		"GET " + repoAPI + "/pulls": `[
			{"number":3,"title":"t","body":"Fixes #1","merged":true,"merged_at":` + string(mergedJSON) + `,"user":{"login":"hive-bot"},"base":{"ref":"release"}},
			{"number":4,"merged":false},
			{"number":5,"merged":true}]`,
		"GET " + repoAPI + "/issues/1":          `{"number":1,"state":"open","user":{"login":"reporter"},"labels":[{"name":"bug"}],"created_at":"2026-11-01T00:00:00Z"}`,
		"GET " + repoAPI + "/issues/2":          `{"number":2,"state":"open","pull_request":{"merged":true}}`,
		"GET " + repoAPI + "/issues/1/comments": `[{"body":"a"},{"body":"b"}]`,
		"GET /api/v1/user":                      `{"login":"hive-bot"}`,
		"PATCH " + repoAPI + "/issues/1":        `{}`,
	})
	ctx := context.Background()

	pr, err := api.PullFacts(ctx, "widgets", 7)
	if err != nil {
		t.Fatal(err)
	}
	want := PullFacts{Number: 7, State: "open", Mergeable: true, Author: "hive-bot", Labels: []string{"lgtm"}, HeadSHA: "abc", Base: "main"}
	if !reflect.DeepEqual(pr, want) {
		t.Errorf("PullFacts = %+v, want %+v", pr, want)
	}
	state, total, err := api.CombinedStatus(ctx, "widgets", "abc")
	if err != nil || state != "success" || total != 2 {
		t.Errorf("CombinedStatus = %q %d %v", state, total, err)
	}
	if _, _, err := api.CombinedStatus(ctx, "widgets", " "); err == nil {
		t.Error("CombinedStatus with no sha: want an error")
	}
	add, ok, err := api.LabelLastAdded(ctx, "widgets", 7, "lgtm")
	if err != nil || !ok || add != (LabelAdd{Login: "erin"}) {
		t.Errorf("LabelLastAdded = %+v %v %v, want erin with no push after", add, ok, err)
	}
	add, ok, err = api.LabelLastAdded(ctx, "widgets", 9, "lgtm")
	if err != nil || !ok || add != (LabelAdd{Login: "erin", PushedAfter: true}) {
		t.Errorf("LabelLastAdded(pushed after) = %+v %v %v, want erin with a push after", add, ok, err)
	}
	if _, ok, err := api.LabelLastAdded(ctx, "widgets", 7, "missing"); ok || err != nil {
		t.Errorf("LabelLastAdded(missing) ok=%v err=%v", ok, err)
	}
	reopened, err := api.IssueReopened(ctx, "widgets", 7)
	if err != nil || !reopened {
		t.Errorf("IssueReopened = %v %v", reopened, err)
	}
	perm, err := api.Permission(ctx, "widgets", "erin")
	if err != nil || perm != "write" {
		t.Errorf("Permission = %q %v", perm, err)
	}
	pulls, err := api.RecentlyMergedPulls(ctx, "widgets", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pulls) != 1 || pulls[0].Number != 3 || pulls[0].Author != "hive-bot" || pulls[0].Title != "t" ||
		pulls[0].Body != "Fixes #1" || pulls[0].Base != "release" || !pulls[0].MergedAt.Equal(merged) {
		t.Errorf("RecentlyMergedPulls = %+v", pulls)
	}
	is, err := api.IssueFacts(ctx, "widgets", 1)
	if err != nil || is.State != "open" || is.IsPull || is.Author != "reporter" || !reflect.DeepEqual(is.Labels, []string{"bug"}) || is.CreatedAt.IsZero() {
		t.Errorf("IssueFacts(1) = %+v %v", is, err)
	}
	if is, err := api.IssueFacts(ctx, "widgets", 2); err != nil || !is.IsPull {
		t.Errorf("IssueFacts(2) = %+v %v", is, err)
	}
	bodies, err := api.IssueCommentBodies(ctx, "widgets", 1)
	if err != nil || !reflect.DeepEqual(bodies, []string{"a", "b"}) {
		t.Errorf("IssueCommentBodies = %v %v", bodies, err)
	}
	if self, err := api.Self(ctx); err != nil || self != "hive-bot" {
		t.Errorf("Self = %q %v", self, err)
	}
	if err := api.CloseIssue(ctx, "widgets", 1); err != nil {
		t.Errorf("CloseIssue: %v", err)
	}
	var sawClose, sawLimit bool
	for _, c := range *calls {
		if strings.HasPrefix(c, "PATCH "+repoAPI+`/issues/1 {"state":"closed"}`) {
			sawClose = true
		}
		if strings.HasPrefix(c, "GET "+repoAPI+"/pulls?") && strings.Contains(c, "state=closed") && strings.Contains(c, "limit=50") {
			sawLimit = true
		}
	}
	if !sawClose || !sawLimit {
		t.Errorf("calls = %v; want the close PATCH and a closed-pulls list with limit 50", *calls)
	}
}

func TestForgejoSweepAPIErrors(t *testing.T) {
	api, _ := newSweepAPIServer(t, map[string]string{
		"GET " + repoAPI + "/issues/7/timeline": `{not json`,
		"GET " + repoAPI + "/issues/8/timeline": `500`,
		"GET " + repoAPI + "/issues/7/comments": `{not json`,
		"GET /api/v1/user":                      `{"login":""}`,
	})
	ctx := context.Background()
	if _, err := api.PullFacts(ctx, "widgets", 1); err == nil {
		t.Error("PullFacts: want an error")
	}
	if _, _, err := api.CombinedStatus(ctx, "widgets", "abc"); err == nil {
		t.Error("CombinedStatus: want an error")
	}
	if _, _, err := api.LabelLastAdded(ctx, "widgets", 7, "lgtm"); err == nil {
		t.Error("LabelLastAdded bad json: want an error")
	}
	if _, _, err := api.LabelLastAdded(ctx, "widgets", 8, "lgtm"); err == nil {
		t.Error("LabelLastAdded 500: want an error")
	}
	if _, err := api.IssueReopened(ctx, "widgets", 7); err == nil {
		t.Error("IssueReopened bad json: want an error")
	}
	if _, err := api.IssueReopened(ctx, "widgets", 8); err == nil {
		t.Error("IssueReopened 500: want an error")
	}
	if _, err := api.Permission(ctx, "widgets", "x"); err == nil {
		t.Error("Permission: want an error")
	}
	if _, err := api.RecentlyMergedPulls(ctx, "widgets", 10); err == nil {
		t.Error("RecentlyMergedPulls: want an error")
	}
	if _, err := api.IssueFacts(ctx, "widgets", 1); err == nil {
		t.Error("IssueFacts: want an error")
	}
	if _, err := api.IssueCommentBodies(ctx, "widgets", 7); err == nil {
		t.Error("IssueCommentBodies bad json: want an error")
	}
	if _, err := api.IssueCommentBodies(ctx, "widgets", 9); err == nil {
		t.Error("IssueCommentBodies 404: want an error")
	}
	if err := api.CloseIssue(ctx, "widgets", 1); err == nil {
		t.Error("CloseIssue: want an error")
	}
	if _, err := api.Self(ctx); err == nil {
		t.Error("Self with no login: want an error")
	}
	if _, err := NewForgejoSweepAPI("t", Options{}); err == nil {
		t.Error("NewForgejoSweepAPI without BaseURL: want an error")
	}
}

func TestForgejoSweepAPISelfError(t *testing.T) {
	api, _ := newSweepAPIServer(t, map[string]string{})
	if _, err := api.Self(context.Background()); err == nil {
		t.Error("Self 404: want an error")
	}
}
