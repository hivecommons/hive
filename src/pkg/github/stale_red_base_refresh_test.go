package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/writeback"
)

func TestStaleRedBaseVerdictFor(t *testing.T) {
	base := StaleRedBaseState{
		SHA:       "base1",
		Completed: map[string]bool{"pytest": true, "lint": true, "build": true},
		Red:       map[string]bool{"build": true},
	}
	cases := []struct {
		name      string
		failing   []string
		want      StaleRedBaseVerdict
		wantCheck string
	}{
		{"failing check green on the base tip", []string{"pytest"}, StaleRedBaseClears, ""},
		{"every failing check green", []string{"lint", "pytest"}, StaleRedBaseClears, ""},
		{"failing check red on the base too", []string{"pytest", "build"}, StaleRedBaseRed, "build"},
		{"PR-only check the base never ran", []string{"pytest", "Check consumer validation evidence"}, StaleRedBaseUnknown, "Check consumer validation evidence"},
		{"nothing failing", nil, StaleRedBaseUnknown, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, check := StaleRedBaseVerdictFor(tc.failing, base)
			if got != tc.want || check != tc.wantCheck {
				t.Fatalf("StaleRedBaseVerdictFor(%v) = (%s, %q), want (%s, %q)", tc.failing, got, check, tc.want, tc.wantCheck)
			}
		})
	}
}

// The base state is the branch tip and the newest completed run per check:
// a re-run that went green replaces the earlier failure, a run still in
// progress is not "completed", and meta checks are ignored.
func TestStaleRedBaseStateFor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/git/ref/heads/main":
			_, _ = io.WriteString(w, `{"ref":"refs/heads/main","object":{"sha":"base1","type":"commit"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/commits/base1/check-runs":
			_, _ = io.WriteString(w, `{"total_count":5,"check_runs":[
				{"id":1,"name":"pytest","head_sha":"base1","status":"completed","conclusion":"failure","app":{"id":15368}},
				{"id":2,"name":"pytest","head_sha":"base1","status":"completed","conclusion":"success","app":{"id":15368}},
				{"id":3,"name":"lint","head_sha":"base1","status":"in_progress","app":{"id":15368}},
				{"id":4,"name":"build","head_sha":"base1","status":"completed","conclusion":"failure","app":{"id":15368}},
				{"id":5,"name":"tide","head_sha":"base1","status":"completed","conclusion":"failure","app":{"id":99}}
			]}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widgets"})

	st, exists, err := c.StaleRedBaseStateFor(context.Background(), "acme/widgets", "main")
	if err != nil || !exists {
		t.Fatalf("StaleRedBaseStateFor = (%v, %v), want the tip", exists, err)
	}
	if st.SHA != "base1" {
		t.Fatalf("SHA = %q, want base1", st.SHA)
	}
	if !st.Completed["pytest"] || st.Red["pytest"] {
		t.Fatalf("pytest: completed=%v red=%v, want the newer green run to win", st.Completed["pytest"], st.Red["pytest"])
	}
	if st.Completed["lint"] {
		t.Fatal("lint is still running on the base tip; it must not read as completed")
	}
	if !st.Red["build"] {
		t.Fatal("build is red on the base tip")
	}
	if st.Completed["tide"] || st.Red["tide"] {
		t.Fatal("meta checks (tide) must be ignored")
	}
}

type staleRedRefreshFake struct {
	mu          sync.Mutex
	behindBy    int
	updateCode  int
	updateMsg   string
	compares    []string
	updates     []string
	expectHeads []string
}

func (f *staleRedRefreshFake) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/compare/"):
			f.compares = append(f.compares, strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets/compare/"))
			_ = json.NewEncoder(w).Encode(map[string]any{"behind_by": f.behindBy, "ahead_by": 2, "base_commit": map[string]any{"sha": "base1"}})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widgets/pulls/7/update-branch":
			var body struct {
				ExpectedHeadSHA string `json:"expected_head_sha"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.updates = append(f.updates, r.URL.Path)
			f.expectHeads = append(f.expectHeads, body.ExpectedHeadSHA)
			code := f.updateCode
			if code == 0 {
				code = http.StatusAccepted
			}
			w.WriteHeader(code)
			msg := f.updateMsg
			if msg == "" {
				msg = "Updating pull request branch."
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"message": msg})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/7":
			_, _ = io.WriteString(w, `{"number":7,"head":{"sha":"head1"}}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
}

// projectbluefin/actions#544: pytest red on the head, green on the main tip
// after #734, head behind main. The hive merges main in, pinned to the head
// it judged, and posts nothing.
func TestRefreshStaleRedPR(t *testing.T) {
	pr := PullRequest{Repo: "acme/widgets", Number: 7, Author: "contributor", HeadSHA: "head1", BaseRef: "main",
		CIStatus: "failure", FailingChecks: []string{"pytest"}, FromFork: true, MaintainerCanModify: true}
	base := StaleRedBaseState{SHA: "base1", Completed: map[string]bool{"pytest": true}, Red: map[string]bool{}}
	fullGate := func() *writeback.Gate {
		return writeback.New(writeback.Config{PerRepo: map[string]writeback.RepoOverride{"acme/widgets": {Mode: writeback.RepoModeFull}}}, writeback.Deps{}, "")
	}

	t.Run("behind: merges the base in, pinned to the evaluated head", func(t *testing.T) {
		fake := &staleRedRefreshFake{behindBy: 3}
		srv := httptest.NewServer(fake.handler(t))
		defer srv.Close()
		c := newTestClient(t, srv, "acme", []string{"widgets"})
		c.SetWritebackGate(fullGate())
		got, err := c.RefreshStaleRedPR(context.Background(), pr, base)
		if err != nil {
			t.Fatalf("RefreshStaleRedPR: %v", err)
		}
		if !got.Updated || !got.Settled || got.BehindBy != 3 || got.Skipped != "" {
			t.Fatalf("outcome = %+v, want updated and settled, behind by 3", got)
		}
		if len(fake.compares) != 1 || fake.compares[0] != "base1...head1" {
			t.Fatalf("compares = %v, want base1...head1", fake.compares)
		}
		if len(fake.updates) != 1 || fake.expectHeads[0] != "head1" {
			t.Fatalf("update-branch calls = %v (expected heads %v), want one pinned to head1", fake.updates, fake.expectHeads)
		}
	})

	t.Run("current: nothing to merge", func(t *testing.T) {
		fake := &staleRedRefreshFake{behindBy: 0}
		srv := httptest.NewServer(fake.handler(t))
		defer srv.Close()
		c := newTestClient(t, srv, "acme", []string{"widgets"})
		c.SetWritebackGate(fullGate())
		got, err := c.RefreshStaleRedPR(context.Background(), pr, base)
		if err != nil {
			t.Fatalf("RefreshStaleRedPR: %v", err)
		}
		if got.Updated || !got.Settled || got.Skipped != "current" || len(fake.updates) != 0 {
			t.Fatalf("outcome = %+v, updates %v; want settled as current with no update", got, fake.updates)
		}
	})

	t.Run("write-back gate refusal makes no call at all", func(t *testing.T) {
		fake := &staleRedRefreshFake{behindBy: 3}
		srv := httptest.NewServer(fake.handler(t))
		defer srv.Close()
		c := newTestClient(t, srv, "acme", []string{"widgets"})
		c.SetWritebackGate(writeback.New(writeback.Config{PerRepo: map[string]writeback.RepoOverride{
			"acme/widgets": {Mode: writeback.RepoModeOff},
		}}, writeback.Deps{}, ""))
		got, err := c.RefreshStaleRedPR(context.Background(), pr, base)
		if err != nil {
			t.Fatalf("RefreshStaleRedPR: %v", err)
		}
		if got.Updated || !got.Settled || got.Skipped != writeback.ReasonRepoOff {
			t.Fatalf("outcome = %+v, want settled and skipped for %q", got, writeback.ReasonRepoOff)
		}
		if len(fake.compares)+len(fake.updates) != 0 {
			t.Fatalf("gate refused, but called compare %v / update %v", fake.compares, fake.updates)
		}
	})

	t.Run("a foreign PR without the opt-in label is refused by default", func(t *testing.T) {
		fake := &staleRedRefreshFake{behindBy: 3}
		srv := httptest.NewServer(fake.handler(t))
		defer srv.Close()
		c := newTestClient(t, srv, "acme", []string{"widgets"})
		c.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{LevelFor: func(string) int { return 6 }}, ""))
		got, err := c.RefreshStaleRedPR(context.Background(), pr, base)
		if err != nil {
			t.Fatalf("RefreshStaleRedPR: %v", err)
		}
		if got.Updated || got.Skipped != writeback.ReasonNeedsLabel || len(fake.updates) != 0 {
			t.Fatalf("outcome = %+v, updates %v; want refused for the opt-in label", got, fake.updates)
		}
	})

	t.Run("the automerge preparation front owns the repo", func(t *testing.T) {
		fake := &staleRedRefreshFake{behindBy: 3}
		srv := httptest.NewServer(fake.handler(t))
		defer srv.Close()
		c := newTestClient(t, srv, "acme", []string{"widgets"})
		c.SetWritebackGate(fullGate())
		c.SetScheduledBaseRefresh(func(string) bool { return true })
		got, err := c.RefreshStaleRedPR(context.Background(), pr, base)
		if err != nil {
			t.Fatalf("RefreshStaleRedPR: %v", err)
		}
		if got.Updated || got.Skipped != "automerge-owns-base-refresh" || len(fake.compares)+len(fake.updates) != 0 {
			t.Fatalf("outcome = %+v, calls %v/%v; want it left to automerge", got, fake.compares, fake.updates)
		}
	})

	t.Run("a merge conflict settles without retrying", func(t *testing.T) {
		fake := &staleRedRefreshFake{behindBy: 3, updateCode: http.StatusUnprocessableEntity, updateMsg: "merge conflict between base and head"}
		srv := httptest.NewServer(fake.handler(t))
		defer srv.Close()
		c := newTestClient(t, srv, "acme", []string{"widgets"})
		c.SetWritebackGate(fullGate())
		got, err := c.RefreshStaleRedPR(context.Background(), pr, base)
		if err != nil {
			t.Fatalf("RefreshStaleRedPR: %v", err)
		}
		if got.Updated || !got.Settled || got.Skipped != "conflict" {
			t.Fatalf("outcome = %+v, want settled as conflict", got)
		}
	})

	t.Run("a head that moved under the request stays open for the next pass", func(t *testing.T) {
		fake := &staleRedRefreshFake{behindBy: 3, updateCode: http.StatusUnprocessableEntity, updateMsg: "expected head sha didn't match current head ref."}
		srv := httptest.NewServer(fake.handler(t))
		defer srv.Close()
		c := newTestClient(t, srv, "acme", []string{"widgets"})
		c.SetWritebackGate(fullGate())
		got, err := c.RefreshStaleRedPR(context.Background(), pr, base)
		if err != nil {
			t.Fatalf("RefreshStaleRedPR: %v", err)
		}
		if got.Updated || got.Settled || got.Skipped != "head-moved" {
			t.Fatalf("outcome = %+v, want unsettled head-moved", got)
		}
	})
}
