package releasesentinel

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// newTestSource serves mux as the GitHub API for acme/widgets.
func newTestSource(t *testing.T, mux *http.ServeMux) *GitHubSource {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := gh.NewClient(srv.Client())
	base, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	client.BaseURL = base
	return NewGitHubSource(client, "acme", "widgets")
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprint(w, body)
}

func TestSplitRepo(t *testing.T) {
	if o, r, ok := SplitRepo(" acme/widgets "); !ok || o != "acme" || r != "widgets" {
		t.Fatalf("SplitRepo = %q %q %v", o, r, ok)
	}
	for _, bad := range []string{"", "widgets", "/widgets", "acme/", "a/b/c"} {
		if _, _, ok := SplitRepo(bad); ok {
			t.Errorf("SplitRepo(%q) accepted", bad)
		}
	}
}

func TestParseReleaseTag(t *testing.T) {
	if v, ok := parseReleaseTag("v5.86.10"); !ok || v != (semver{5, 86, 10}) {
		t.Fatalf("parse = %v %v", v, ok)
	}
	for _, bad := range []string{"5.86.1", "v5.86", "v5.86.1-rc1", "stable", "v99999999999999999999.0.0"} {
		if _, ok := parseReleaseTag(bad); ok {
			t.Errorf("parseReleaseTag(%q) accepted", bad)
		}
	}
	if !(semver{1, 9, 9}).less(semver{1, 10, 0}) || (semver{1, 10, 0}).less(semver{1, 9, 9}) || (semver{1, 2, 3}).less(semver{1, 2, 3}) {
		t.Fatal("semver ordering is lexical, not numeric")
	}
}

func TestGitHubSource_CurrentTagPicksHighestSemverAcrossPages(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/tags", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			writeJSON(w, `[{"name":"v1.10.0","commit":{"sha":"`+shaB+`"}},{"name":"v1.11.0","commit":{}}]`)
			return
		}
		w.Header().Set("Link", `<`+"http://"+r.Host+`/repos/acme/widgets/tags?page=2>; rel="next"`)
		writeJSON(w, `[{"name":"stable","commit":{"sha":"x"}},{"name":"v1.9.9","commit":{"sha":"`+shaA+`"}},{"name":"v1.2.0-rc1","commit":{"sha":"y"}}]`)
	})
	tag, ok, err := newTestSource(t, mux).CurrentTag(context.Background())
	if err != nil || !ok {
		t.Fatalf("CurrentTag: %v %v", ok, err)
	}
	// v1.10.0 beats v1.9.9 numerically; v1.11.0 has no commit SHA and is skipped.
	if tag.Name != "v1.10.0" || tag.SHA != shaB {
		t.Fatalf("tag = %+v", tag)
	}
}

func TestGitHubSource_CurrentTagNoneAndError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/tags", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, `[]`) })
	if _, ok, err := newTestSource(t, mux).CurrentTag(context.Background()); ok || err != nil {
		t.Fatalf("empty: ok=%v err=%v", ok, err)
	}
	bad := http.NewServeMux()
	bad.HandleFunc("/repos/acme/widgets/tags", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "nope", http.StatusInternalServerError) })
	if _, _, err := newTestSource(t, bad).CurrentTag(context.Background()); err == nil {
		t.Fatal("500 did not error")
	}
}

func TestGitHubSource_RunsKeepsLatestPerWorkflow(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("head_sha"); got != shaA {
			t.Errorf("head_sha = %q", got)
		}
		if r.URL.Query().Get("page") == "2" {
			writeJSON(w, `{"total_count":3,"workflow_runs":[
				{"id":30,"name":"CI","workflow_id":1,"event":"push","head_sha":"`+shaA+`","status":"completed","conclusion":"success","html_url":"https://x/30"}]}`)
			return
		}
		w.Header().Set("Link", `<`+"http://"+r.Host+`/repos/acme/widgets/actions/runs?page=2>; rel="next"`)
		writeJSON(w, `{"total_count":3,"workflow_runs":[
			{"id":10,"name":"CI","workflow_id":1,"event":"push","head_sha":"`+shaA+`","status":"completed","conclusion":"failure"},
			{"id":20,"name":"Release","workflow_id":2,"event":"push","head_sha":"`+shaA+`","status":"in_progress"}]}`)
	})
	runs, err := newTestSource(t, mux).Runs(context.Background(), shaA)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %+v", runs)
	}
	// CI's newer run (30, success) replaced the older failure (10).
	if runs[0].ID != 30 || runs[0].Conclusion != "success" || runs[0].URL != "https://x/30" || runs[0].HeadSHA != shaA {
		t.Fatalf("CI run = %+v", runs[0])
	}
	if runs[1].ID != 20 || runs[1].Status != "in_progress" {
		t.Fatalf("Release run = %+v", runs[1])
	}
}

func TestGitHubSource_RunsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	})
	if _, err := newTestSource(t, mux).Runs(context.Background(), shaA); err == nil {
		t.Fatal("403 did not error")
	}
}

func TestGitHubSource_Details(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/actions/runs/77/jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("filter") != "latest" {
			t.Errorf("filter = %q", r.URL.Query().Get("filter"))
		}
		writeJSON(w, `{"total_count":3,"jobs":[
			{"id":1,"name":"build","conclusion":"success"},
			{"id":2,"name":"release","conclusion":"failure","steps":[
				{"name":"checkout","conclusion":"success"},
				{"name":"Open and merge a PR","conclusion":"failure"}]},
			{"id":3,"name":"mirror","conclusion":"timed_out"}]}`)
	})
	mux.HandleFunc("/repos/acme/widgets/check-runs/2/annotations", func(w http.ResponseWriter, _ *http.Request) {
		long := strings.Repeat("x", maxEvidenceRunes+10)
		writeJSON(w, `[{"annotation_level":"failure","message":"gh pr create failed: not permitted to create or approve pull requests"},
			{"annotation_level":"warning","message":"ignored"},
			{"annotation_level":"failure","message":"   "},
			{"annotation_level":"failure","message":"`+long+`"}]`)
	})
	mux.HandleFunc("/repos/acme/widgets/check-runs/3/annotations", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	})
	d, err := newTestSource(t, mux).Details(context.Background(), 77)
	if err != nil {
		t.Fatal(err)
	}
	if d.JobCount != 3 {
		t.Fatalf("JobCount = %d", d.JobCount)
	}
	if len(d.FailedJobs) != 2 || d.FailedJobs[0] != "release / Open and merge a PR" || d.FailedJobs[1] != "mirror" {
		t.Fatalf("FailedJobs = %v", d.FailedJobs)
	}
	if len(d.Evidence) != 2 || !strings.Contains(d.Evidence[0], "not permitted") {
		t.Fatalf("Evidence = %v", d.Evidence)
	}
	if got := []rune(d.Evidence[1]); len(got) != maxEvidenceRunes+1 {
		t.Fatalf("long evidence not truncated: %d runes", len(got))
	}
	// And the evidence is what the classifier needs.
	if class, _ := Classify(BlockingRun{Run: Run{ID: 77}, RunDetails: d}); class != ClassPolicy {
		t.Fatalf("classifier missed the policy failure in fetched evidence")
	}
}

func TestGitHubSource_DetailsZeroJobsAndError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"total_count":0,"jobs":[]}`)
	})
	mux.HandleFunc("/repos/acme/widgets/actions/runs/2/jobs", func(w http.ResponseWriter, _ *http.Request) {
		// total_count missing: fall back to the listed jobs.
		writeJSON(w, `{"jobs":[{"id":5,"name":"a","conclusion":"success"}]}`)
	})
	mux.HandleFunc("/repos/acme/widgets/actions/runs/3/jobs", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	})
	src := newTestSource(t, mux)
	if d, err := src.Details(context.Background(), 1); err != nil || d.JobCount != 0 {
		t.Fatalf("zero jobs: %+v %v", d, err)
	}
	if d, err := src.Details(context.Background(), 2); err != nil || d.JobCount != 1 || len(d.FailedJobs) != 0 {
		t.Fatalf("missing total: %+v %v", d, err)
	}
	if _, err := src.Details(context.Background(), 3); err == nil {
		t.Fatal("500 did not error")
	}
}

func TestGitHubSource_Release(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/releases/tags/v1.0.0", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"tag_name":"v1.0.0","draft":false,"html_url":"https://x/r/1"}`)
	})
	mux.HandleFunc("/repos/acme/widgets/releases/tags/v1.0.1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"tag_name":"v1.0.1","draft":true}`)
	})
	mux.HandleFunc("/repos/acme/widgets/releases/tags/v1.0.2", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	mux.HandleFunc("/repos/acme/widgets/releases/tags/v1.0.3", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	})
	src := newTestSource(t, mux)
	ctx := context.Background()
	if rel, err := src.Release(ctx, "v1.0.0"); err != nil || !rel.Exists || rel.Draft || rel.URL != "https://x/r/1" {
		t.Fatalf("published: %+v %v", rel, err)
	}
	if rel, err := src.Release(ctx, "v1.0.1"); err != nil || !rel.Exists || !rel.Draft {
		t.Fatalf("draft: %+v %v", rel, err)
	}
	if rel, err := src.Release(ctx, "v1.0.2"); err != nil || rel.Exists {
		t.Fatalf("missing: %+v %v", rel, err)
	}
	if _, err := src.Release(ctx, "v1.0.3"); err == nil {
		t.Fatal("502 did not error")
	}
}
