package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// The rule: a failing check whose name is also red on the base branch tip
// is the base's, unless the PR touches a CI-owning path.
func TestBaseRedChecksFor(t *testing.T) {
	baseRed := map[string]bool{"docker (hive)": true, "build (linux/amd64, true, false, 60)": true}
	cases := []struct {
		name    string
		failing []string
		files   []string
		want    []string
	}{
		{"same check red on base", []string{"docker (hive)"}, []string{"src/pkg/dashboard/static/app.css"}, []string{"docker (hive)"}},
		{"all failing checks red on base", []string{"docker (hive)", "build (linux/amd64, true, false, 60)"}, []string{"README.md"}, []string{"build (linux/amd64, true, false, 60)", "docker (hive)"}},
		{"mixed keeps only the base's", []string{"docker (hive)", "unit"}, []string{"README.md"}, []string{"docker (hive)"}},
		{"PR-local failure only", []string{"unit"}, []string{"README.md"}, nil},
		{"touching a workflow owns the check", []string{"docker (hive)"}, []string{".github/workflows/docker.yml"}, nil},
		{"touching a Dockerfile owns the check", []string{"docker (hive)"}, []string{"src/Dockerfile.hub"}, nil},
		{"touching go.mod owns the check", []string{"docker (hive)"}, []string{"src/go.mod"}, nil},
		{"no base evidence", []string{"docker (hive)"}, nil, []string{"docker (hive)"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BaseRedChecksFor(tc.failing, baseRed, tc.files); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("BaseRedChecksFor=%v want %v", got, tc.want)
			}
		})
	}
	if got := BaseRedChecksFor([]string{"docker (hive)"}, nil, nil); got != nil {
		t.Fatalf("green base yields %v, want nil", got)
	}
}

func TestTouchesCIOwningPath(t *testing.T) {
	for _, p := range []string{".github/workflows/ci.yml", "Dockerfile", "src/Dockerfile", "src/Dockerfile.hub", "images/hub.Dockerfile", "src/go.mod", "src/go.sum"} {
		if !TouchesCIOwningPath([]string{p}) {
			t.Errorf("%q should own CI", p)
		}
	}
	for _, p := range []string{".github/CODEOWNERS", "src/pkg/github/client.go", "docs/Dockerfile.md", "src/cmd/hive/main.go", "package.json"} {
		if TouchesCIOwningPath([]string{p}) {
			t.Errorf("%q should not own CI", p)
		}
	}
}

func TestPullRequestAllFailuresBaseRed(t *testing.T) {
	pr := PullRequest{FailingChecks: []string{"docker (hive)", "docker (hub)"}, BaseRedChecks: []string{"docker (hive)", "docker (hub)"}}
	if !pr.AllFailuresBaseRed() {
		t.Fatal("every failing check red on base must read as base red")
	}
	pr.FailingChecks = append(pr.FailingChecks, "unit")
	if pr.AllFailuresBaseRed() {
		t.Fatal("a PR-local failure must keep the PR author-facing")
	}
	if (PullRequest{}).AllFailuresBaseRed() || (PullRequest{FailingChecks: []string{"x"}}).AllFailuresBaseRed() {
		t.Fatal("no base evidence is not base red")
	}
}

// Enrichment: one base listing per repo+base, a file listing only for a PR
// whose failures overlap the base's, and a CI-owning diff stays author-facing.
func TestEnrichBaseBranchRed(t *testing.T) {
	var baseListings, fileListings int
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widget/commits/v5/check-runs", func(w http.ResponseWriter, r *http.Request) {
		baseListings++
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 3, "check_runs": []map[string]any{
			{"id": 1, "name": "docker (hive)", "status": "completed", "conclusion": "failure", "started_at": "2026-10-20T20:00:00Z"},
			{"id": 2, "name": "overlayfs-exec-guard", "status": "completed", "conclusion": "failure", "started_at": "2026-10-20T20:00:00Z"},
			{"id": 3, "name": "unit", "status": "completed", "conclusion": "success", "started_at": "2026-10-20T20:00:00Z"},
		}})
	})
	files := map[string][]string{
		"13270": {"src/pkg/dashboard/static/app.css"},
		"13266": {"src/Dockerfile"},
	}
	mux.HandleFunc("/repos/acme/widget/pulls/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/files") {
			http.NotFound(w, r)
			return
		}
		fileListings++
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		number := parts[len(parts)-2]
		var out []map[string]any
		for _, f := range files[number] {
			out = append(out, map[string]any{"filename": f})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widget"})

	prs := []PullRequest{
		{Repo: "acme/widget", Number: 13270, BaseRef: "v5", CIStatus: "failure", FailingChecks: []string{"docker (hive)", "overlayfs-exec-guard"}},
		{Repo: "acme/widget", Number: 13266, BaseRef: "v5", CIStatus: "failure", FailingChecks: []string{"docker (hive)"}},
		{Repo: "acme/widget", Number: 13280, BaseRef: "v5", CIStatus: "failure", FailingChecks: []string{"unit"}},
		{Repo: "acme/widget", Number: 13281, BaseRef: "v5", CIStatus: "success"},
	}
	c.enrichBaseBranchRed(context.Background(), prs)

	if want := []string{"docker (hive)", "overlayfs-exec-guard"}; !reflect.DeepEqual(prs[0].BaseRedChecks, want) || !prs[0].AllFailuresBaseRed() {
		t.Fatalf("CSS-only PR red on the base's checks: BaseRedChecks=%v want %v", prs[0].BaseRedChecks, want)
	}
	if len(prs[1].BaseRedChecks) != 0 {
		t.Fatalf("Dockerfile PR must stay author-facing, got %v", prs[1].BaseRedChecks)
	}
	if len(prs[2].BaseRedChecks) != 0 || len(prs[3].BaseRedChecks) != 0 {
		t.Fatalf("PR-local / green PRs must carry no base-red checks: %v %v", prs[2].BaseRedChecks, prs[3].BaseRedChecks)
	}
	if baseListings != 1 {
		t.Fatalf("base check runs listed %d times, want once per repo+base", baseListings)
	}
	if fileListings != 2 {
		t.Fatalf("file listings = %d, want 2 (only PRs whose failures overlap the base's)", fileListings)
	}
}
