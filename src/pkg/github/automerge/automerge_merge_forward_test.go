package automerge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

const mergeForwardHead = "sha-human"

func mergeForwardPR(fork bool, labels ...string) *gh.PullRequest {
	headRepo := "acme/widget"
	if fork {
		headRepo = "someone/widget"
	}
	pr := &gh.PullRequest{
		Number: gh.Ptr(7),
		User:   &gh.User{Login: gh.Ptr("alice")},
		Head:   &gh.PullRequestBranch{Ref: gh.Ptr("feature"), SHA: gh.Ptr(mergeForwardHead), Repo: &gh.Repository{FullName: gh.Ptr(headRepo)}},
		Base:   &gh.PullRequestBranch{Ref: gh.Ptr("main"), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
	}
	for _, l := range labels {
		pr.Labels = append(pr.Labels, &gh.Label{Name: gh.Ptr(l)})
	}
	return pr
}

type mergeForwardAPI struct {
	mu            sync.Mutex
	mergeStatus   int
	compareStatus int
	merges        int
	comments      []string
	mergeBody     map[string]string
}

func newMergeForwardServer(t *testing.T, a *mergeForwardAPI) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
			pr := mergeForwardPR(false, "author/human")
			pr.State = gh.Ptr("open")
			pr.MergeableState = gh.Ptr("dirty")
			pr.Base.SHA = gh.Ptr("base1")
			_ = json.NewEncoder(w).Encode(pr)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/merges":
			a.merges++
			_ = json.NewDecoder(r.Body).Decode(&a.mergeBody)
			w.WriteHeader(a.mergeStatus)
			switch a.mergeStatus {
			case http.StatusCreated:
				_, _ = w.Write([]byte(`{"sha":"sha-merge"}`))
			case http.StatusNoContent:
			default:
				_, _ = w.Write([]byte(`{"message":"merge refused"}`))
			}
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/compare/main..."+mergeForwardHead:
			if a.compareStatus != 0 {
				w.WriteHeader(a.compareStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"merge_base_commit": map[string]string{"sha": "sha-base"},
				"files":             []map[string]string{{"filename": "b.go"}, {"filename": "a.go"}, {"filename": "only-head.go"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/compare/sha-base...main":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]string{{"filename": "a.go"}, {"filename": "b.go"}, {"filename": "only-base.go"}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/comments":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			a.comments = append(a.comments, body["body"])
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestMergeForwardOrConflict(t *testing.T) {
	cases := []struct {
		name          string
		optIn         bool
		pr            *gh.PullRequest
		mergeStatus   int
		compareStatus int
		wantReason    string
		wantErr       bool
		wantUpdated   bool
		wantMerges    int
		wantComment   bool
		wantFiles     bool
	}{
		{name: "off by default", pr: mergeForwardPR(false, "author/human"), wantReason: "conflicting"},
		{name: "fork head is never written", optIn: true, pr: mergeForwardPR(true, "author/human"), wantReason: "conflicting"},
		{name: "clanker author keeps the hold", optIn: true, pr: mergeForwardPR(false, "author/clanker"), wantReason: "conflicting"},
		{name: "unknown author keeps the hold", optIn: true, pr: mergeForwardPR(false), wantReason: "conflicting"},
		{name: "nil pr", optIn: true, wantReason: "conflicting"},
		{name: "clean merge pushes a merge commit", optIn: true, pr: mergeForwardPR(false, "author/human"), mergeStatus: http.StatusCreated, wantReason: "check-pending", wantUpdated: true, wantMerges: 1},
		{name: "nothing to merge waits for github", optIn: true, pr: mergeForwardPR(false, "author/human"), mergeStatus: http.StatusNoContent, wantReason: "check-pending", wantMerges: 1},
		{name: "conflict posts the files note", optIn: true, pr: mergeForwardPR(false, "author/human"), mergeStatus: http.StatusConflict, wantReason: "conflicting", wantMerges: 1, wantComment: true, wantFiles: true},
		{name: "conflict note without files when compare fails", optIn: true, pr: mergeForwardPR(false, "author/human"), mergeStatus: http.StatusConflict, compareStatus: http.StatusInternalServerError, wantReason: "conflicting", wantMerges: 1, wantComment: true},
		{name: "forbidden keeps the hold", optIn: true, pr: mergeForwardPR(false, "author/human"), mergeStatus: http.StatusForbidden, wantReason: "conflicting", wantMerges: 1},
		{name: "other failure surfaces", optIn: true, pr: mergeForwardPR(false, "author/human"), mergeStatus: http.StatusUnprocessableEntity, wantReason: "merge-forward", wantErr: true, wantMerges: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &mergeForwardAPI{mergeStatus: tc.mergeStatus, compareStatus: tc.compareStatus}
			srv := newMergeForwardServer(t, api)
			defer srv.Close()
			c := newAutoMergeSweepClient(srv.URL)
			c.mergeForwardTrustedHuman = func(repo string) bool { return tc.optIn && strings.EqualFold(repo, "acme/widget") }

			event, reason, err := c.mergeForwardOrConflict(context.Background(), "acme/widget", "acme", "widget", tc.pr, 7)
			if reason != tc.wantReason || (err != nil) != tc.wantErr {
				t.Fatalf("reason, err = %q, %v; want %q, wantErr %v", reason, err, tc.wantReason, tc.wantErr)
			}
			if event.BranchUpdated != tc.wantUpdated {
				t.Fatalf("BranchUpdated = %v, want %v", event.BranchUpdated, tc.wantUpdated)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if api.merges != tc.wantMerges {
				t.Fatalf("merges = %d, want %d", api.merges, tc.wantMerges)
			}
			if tc.wantMerges > 0 {
				if api.mergeBody["base"] != "feature" || api.mergeBody["head"] != "main" {
					t.Fatalf("merge request = %v, want base=feature head=main", api.mergeBody)
				}
				if !strings.Contains(api.mergeBody["commit_message"], "Signed-off-by: "+testHiveAppBotLogin) {
					t.Fatalf("merge commit message lacks the App sign-off: %q", api.mergeBody["commit_message"])
				}
			}
			if got := len(api.comments) == 1; got != tc.wantComment {
				t.Fatalf("comments = %d, want comment %v", len(api.comments), tc.wantComment)
			}
			if tc.wantComment {
				body := api.comments[0]
				if !strings.Contains(body, MergeForwardConflictMarkerPrefix+" head="+mergeForwardHead) || !strings.Contains(body, "@alice") {
					t.Fatalf("conflict note = %q", body)
				}
				hasFiles := strings.Contains(body, "- `a.go`\n- `b.go`\n")
				if hasFiles != tc.wantFiles || strings.Contains(body, "only-head.go") || strings.Contains(body, "only-base.go") {
					t.Fatalf("conflict note files (want %v) = %q", tc.wantFiles, body)
				}
			}
		})
	}
}

func TestMergeForwardOrConflictOncePerHead(t *testing.T) {
	api := &mergeForwardAPI{mergeStatus: http.StatusConflict}
	srv := newMergeForwardServer(t, api)
	defer srv.Close()
	c := newAutoMergeSweepClient(srv.URL)
	c.mergeForwardTrustedHuman = func(string) bool { return true }
	pr := mergeForwardPR(false, "author/human")
	for i := 0; i < 3; i++ {
		if _, reason, err := c.mergeForwardOrConflict(context.Background(), "acme/widget", "acme", "widget", pr, 7); reason != "conflicting" || err != nil {
			t.Fatalf("tick %d: reason, err = %q, %v", i, reason, err)
		}

	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.merges != 1 || len(api.comments) != 1 {
		t.Fatalf("merges, comments = %d, %d; want 1, 1", api.merges, len(api.comments))
	}
}

func TestMergeForwardRepairSharesPreparationWithoutDisablingRepair(t *testing.T) {
	api := &mergeForwardAPI{mergeStatus: http.StatusCreated}
	srv := newMergeForwardServer(t, api)
	defer srv.Close()
	c := newAutoMergeSweepClient(srv.URL)
	c.requireCurrentBase = func(string) bool { return true }
	c.mergeForwardTrustedHuman = func(string) bool { return true }
	for tick := range 2 {
		ev, reason, err := c.mergeForwardOrConflict(context.Background(), "acme/widget", "acme", "widget", mergeForwardPR(false, "author/human"), 7)
		if err != nil {
			t.Fatal(err)
		}
		if tick == 0 && !ev.BranchUpdated {
			t.Fatalf("explicit repair was disabled: %+v %q", ev, reason)
		}
		if tick == 1 && (ev.BranchUpdated || reason != basePreparationReady) {
			t.Fatalf("repair refresh repeated: %+v %q", ev, reason)
		}
	}
	if api.merges != 1 {
		t.Fatalf("repair mutations=%d, want 1", api.merges)
	}
}

func TestMergeForwardOrConflictNeedsRefsAndAppLogin(t *testing.T) {
	srv := newMergeForwardServer(t, &mergeForwardAPI{})
	defer srv.Close()
	c := newAutoMergeSweepClient(srv.URL)
	c.mergeForwardTrustedHuman = func(string) bool { return true }
	pr := mergeForwardPR(false, "author/human")
	pr.Head.Ref = gh.Ptr("")
	if _, reason, _ := c.mergeForwardOrConflict(context.Background(), "acme/widget", "acme", "widget", pr, 7); reason != "conflicting" {
		t.Fatalf("missing head ref reason = %q, want conflicting", reason)
	}

	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, srv.URL)
	noLogin := New(client, Options{MergeForwardTrustedHuman: func(string) bool { return true }})
	if _, reason, _ := noLogin.mergeForwardOrConflict(context.Background(), "acme/widget", "acme", "widget", mergeForwardPR(false, "author/human"), 7); reason != "conflicting" {
		t.Fatalf("unknown app login reason = %q, want conflicting", reason)
	}
}

func TestMergeForwardTrustedHumanForNilEngine(t *testing.T) {
	var c *Engine
	if c.mergeForwardTrustedHumanFor("acme/widget") {
		t.Fatal("nil engine must not merge forward")
	}
}

func TestMergeForwardConflictCommentTruncates(t *testing.T) {
	var files []string
	for i := 0; i < mergeForwardConflictFilesMax+3; i++ {
		files = append(files, fmt.Sprintf("f%02d.go", i))
	}
	body := MergeForwardConflictComment("alice", "sha1", "main", files)
	if !strings.Contains(body, "and 3 more") || strings.Contains(body, fmt.Sprintf("f%02d.go", mergeForwardConflictFilesMax)) {
		t.Fatalf("truncation missing: %q", body)
	}
	if empty := MergeForwardConflictComment("alice", "sha1", "main", nil); strings.Contains(empty, "Files changed") {
		t.Fatalf("empty file list rendered a header: %q", empty)
	}
}

func TestMergeForwardConflictNoteSkipsWithoutAuthor(t *testing.T) {
	api := &mergeForwardAPI{}
	srv := newMergeForwardServer(t, api)
	defer srv.Close()
	c := newAutoMergeSweepClient(srv.URL)
	c.postMergeForwardConflictNote(context.Background(), "acme/widget", "acme", "widget", 7, "", "sha1", "main", nil)
	if len(api.comments) != 0 {
		t.Fatalf("posted %d comments without an author", len(api.comments))
	}
}
