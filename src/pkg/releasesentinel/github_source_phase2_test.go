package releasesentinel

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestGitHubSource_DefaultBranch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"name":"widgets","default_branch":"v5"}`)
	})
	if b, err := newTestSource(t, mux).DefaultBranch(context.Background()); err != nil || b != "v5" {
		t.Fatalf("DefaultBranch = %q, %v", b, err)
	}
	bad := http.NewServeMux()
	bad.HandleFunc("/repos/acme/widgets", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "nope", http.StatusInternalServerError) })
	if _, err := newTestSource(t, bad).DefaultBranch(context.Background()); err == nil {
		t.Fatal("500 did not error")
	}
}

// fixPRsJSON is a closed-PR listing with one PR per disqualifier and two
// qualifying ones (#7 older, #8 newer).
const fixPRsJSON = `[
	{"number":1,"body":"Release-Sentinel: v1.2.3","merge_commit_sha":"` + shaA + `","base":{"ref":"main"}},
	{"number":2,"body":"Release-Sentinel: v1.2.3","merged_at":"2026-09-29T12:30:00Z","merge_commit_sha":"` + shaA + `","base":{"ref":"other"}},
	{"number":3,"body":"Release-Sentinel: v1.2.3","merged_at":"2026-09-29T11:00:00Z","merge_commit_sha":"` + shaA + `","base":{"ref":"main"}},
	{"number":4,"body":"no marker","merged_at":"2026-09-29T12:30:00Z","merge_commit_sha":"` + shaA + `","base":{"ref":"main"}},
	{"number":5,"body":"Release-Sentinel: v1.2.3","merged_at":"2026-09-29T12:30:00Z","base":{"ref":"main"}},
	{"number":7,"body":"fix\n\nRelease-Sentinel: v1.2.3\n","merged_at":"2026-09-29T12:10:00Z","merge_commit_sha":"` + shaB + `","base":{"ref":"main"}},
	{"number":8,"body":"Release-Sentinel: v1.2.3","merged_at":"2026-09-29T12:40:00Z","merge_commit_sha":"` + shaC + `","html_url":"https://example.test/pull/8","base":{"ref":"main"}}
]`

func TestGitHubSource_MergedFixPR(t *testing.T) {
	since := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/pulls", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != "closed" || q.Get("base") != "main" || q.Get("sort") != "updated" || q.Get("direction") != "desc" {
			t.Errorf("pulls query = %s", r.URL.RawQuery)
		}
		writeJSON(w, fixPRsJSON)
	})
	mux.HandleFunc("/repos/acme/widgets/pulls/8/commits", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `[{"sha":"`+shaA+`"},{"sha":""},{"sha":"`+shaB+`"}]`)
	})
	fix, ok, err := newTestSource(t, mux).MergedFixPR(context.Background(), "main", "v1.2.3", since)
	if err != nil || !ok {
		t.Fatalf("MergedFixPR: ok=%v err=%v", ok, err)
	}
	// #1 unmerged, #2 wrong base, #3 merged before the round, #4 no marker,
	// #5 no merge commit; #8 is newer than #7.
	if fix.Number != 8 || fix.MergeSHA != shaC || fix.URL != "https://example.test/pull/8" || !fix.MergedAt.Equal(since.Add(40*time.Minute)) {
		t.Fatalf("fix = %+v", fix)
	}
	if len(fix.Commits) != 2 || fix.Commits[0] != shaA || fix.Commits[1] != shaB {
		t.Fatalf("commits = %v", fix.Commits)
	}

	// Nothing qualifies after a later round start.
	if _, ok, err := newTestSource(t, mux).MergedFixPR(context.Background(), "main", "v1.2.3", since.Add(time.Hour)); ok || err != nil {
		t.Fatalf("later since: ok=%v err=%v", ok, err)
	}
}

func TestGitHubSource_MergedFixPRErrors(t *testing.T) {
	since := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	bad := http.NewServeMux()
	bad.HandleFunc("/repos/acme/widgets/pulls", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "nope", http.StatusInternalServerError) })
	if _, _, err := newTestSource(t, bad).MergedFixPR(context.Background(), "main", "v1.2.3", since); err == nil {
		t.Fatal("list 500 did not error")
	}
	commitsDown := http.NewServeMux()
	commitsDown.HandleFunc("/repos/acme/widgets/pulls", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, fixPRsJSON) })
	commitsDown.HandleFunc("/repos/acme/widgets/pulls/8/commits", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	})
	if _, ok, err := newTestSource(t, commitsDown).MergedFixPR(context.Background(), "main", "v1.2.3", since); err == nil || ok {
		t.Fatalf("commits 500: ok=%v err=%v", ok, err)
	}
}

func TestGitHubSource_LatestReleaseWorkflowRuns(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			writeJSON(w, `{"total_count":3,"workflows":[{"id":3,"name":"Release Gate","path":".github/workflows/release-gate.yml"}]}`)
			return
		}
		w.Header().Set("Link", `<`+"http://"+r.Host+`/repos/acme/widgets/actions/workflows?page=2>; rel="next"`)
		writeJSON(w, `{"total_count":3,"workflows":[
			{"id":1,"name":"Tagged Release","path":".github/workflows/tagged-release.yml"},
			{"id":2,"name":"CI","path":".github/workflows/ci.yml"}]}`)
	})
	mux.HandleFunc("/repos/acme/widgets/actions/workflows/1/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("status") != "completed" || r.URL.Query().Get("per_page") != "1" {
			t.Errorf("runs query = %s", r.URL.RawQuery)
		}
		writeJSON(w, `{"total_count":1,"workflow_runs":[{"id":50,"name":"ignored run name","head_sha":"`+shaB+`","status":"completed","conclusion":"failure","html_url":"https://x/50"}]}`)
	})
	mux.HandleFunc("/repos/acme/widgets/actions/workflows/3/runs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"total_count":0,"workflow_runs":[]}`)
	})
	mux.HandleFunc("/repos/acme/widgets/actions/workflows/2/runs", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a workflow that was not asked for was queried")
	})
	src := newTestSource(t, mux)
	runs, err := src.LatestReleaseWorkflowRuns(context.Background(), []string{" tagged release ", "RELEASE-GATE.yml", "missing", ""})
	if err != nil {
		t.Fatal(err)
	}
	// Release Gate has no completed run yet, so only Tagged Release reports.
	if len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
	r := runs[0]
	if r.ID != 50 || r.Name != "Tagged Release" || r.HeadSHA != shaB || r.Conclusion != "failure" || r.URL != "https://x/50" || r.Status != "completed" {
		t.Fatalf("run = %+v", r)
	}

	if runs, err := src.LatestReleaseWorkflowRuns(context.Background(), []string{" ", ""}); err != nil || runs != nil {
		t.Fatalf("no names: runs=%v err=%v", runs, err)
	}
}

func TestGitHubSource_LatestReleaseWorkflowRunsErrors(t *testing.T) {
	bad := http.NewServeMux()
	bad.HandleFunc("/repos/acme/widgets/actions/workflows", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	})
	if _, err := newTestSource(t, bad).LatestReleaseWorkflowRuns(context.Background(), []string{"Tagged Release"}); err == nil {
		t.Fatal("workflows 403 did not error")
	}
	runsDown := http.NewServeMux()
	runsDown.HandleFunc("/repos/acme/widgets/actions/workflows", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"total_count":1,"workflows":[{"id":1,"name":"Tagged Release","path":".github/workflows/tagged-release.yml"}]}`)
	})
	runsDown.HandleFunc("/repos/acme/widgets/actions/workflows/1/runs", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	})
	if _, err := newTestSource(t, runsDown).LatestReleaseWorkflowRuns(context.Background(), []string{"Tagged Release"}); err == nil {
		t.Fatal("runs 500 did not error")
	}
}
