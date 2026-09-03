package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// advisoryPRHub builds a hub whose GitHub client resolves one PR with the given
// merged state and title, and whose deps carry a single bead store holding one
// open advisory finding.
func advisoryPRHub(t *testing.T, merged bool, prTitle, findingTitle string, prAutoClose *bool) (*ContributeWSHub, *beads.Store, string) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number":   7,
			"html_url": "https://github.com/myorg/repo1/pull/7",
			"title":    prTitle,
			"merged":   merged,
			"user":     map[string]any{"login": "alice"},
			"base": map[string]any{
				"repo": map[string]any{
					"name":      "repo1",
					"full_name": "myorg/repo1",
					"owner":     map[string]any{"login": "myorg"},
				},
			},
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating bead store: %v", err)
	}
	bead, err := store.Create(findingTitle, beads.TypeAdvisory, beads.PriorityHigh, "ci-maintainer", "")
	if err != nil {
		t.Fatalf("creating advisory bead: %v", err)
	}

	s := NewServer(0, logger)
	deps := testDeps(t)
	deps.GHClient = ghpkg.NewClientForTest(ts.URL, "myorg", []string{"repo1"}, logger)
	deps.BeadStores = map[string]*beads.Store{"ci-maintainer": store}
	if deps.Config == nil {
		deps.Config = &config.Config{}
	}
	deps.Config.Governor.Advisory.PRAutoClose = prAutoClose
	s.RegisterAPI(deps)
	return NewContributeWSHub(logger, s), store, bead.ID
}

const advisoryFindingTitle = "pr-verifier workflow fails on every pull request"
const advisoryFixPRTitle = "fix the pr-verifier workflow so it stops failing on every pull request"

func advisoryBeadStatus(t *testing.T, store *beads.Store, id string) beads.Status {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("reading bead %s: %v", id, err)
	}
	return b.Status
}

// TestCloseAdvisoryForMergedPR_ClosesMatchingFinding is the happy path: a merged
// PR whose title restates the finding retires it.
func TestCloseAdvisoryForMergedPR_ClosesMatchingFinding(t *testing.T) {
	hub, store, id := advisoryPRHub(t, true, advisoryFixPRTitle, advisoryFindingTitle, nil)

	hub.closeAdvisoryForMergedPR(advisoryFixPRTitle)

	if got := advisoryBeadStatus(t, store, id); got != beads.StatusClosed {
		t.Errorf("bead status = %q, want %q", got, beads.StatusClosed)
	}
}

// TestCloseAdvisoryForMergedPR_RespectsConfigGate confirms an operator who turns
// pr_autoclose off keeps every finding, however similar the PR title is.
func TestCloseAdvisoryForMergedPR_RespectsConfigGate(t *testing.T) {
	off := false
	hub, store, id := advisoryPRHub(t, true, advisoryFixPRTitle, advisoryFindingTitle, &off)

	hub.closeAdvisoryForMergedPR(advisoryFixPRTitle)

	if got := advisoryBeadStatus(t, store, id); got != beads.StatusOpen {
		t.Errorf("bead status = %q, want %q — pr_autoclose: false must close nothing", got, beads.StatusOpen)
	}
}

// TestVerifyReportedPRDetail_ReportsMergedAndTitle pins the two fields the
// advisory close depends on. An UNMERGED PR must report Merged=false even though
// it verifies: closing findings on a fix still in review would retire them
// before anything landed.
func TestVerifyReportedPRDetail_ReportsMergedAndTitle(t *testing.T) {
	for _, merged := range []bool{true, false} {
		hub, _, _ := advisoryPRHub(t, merged, advisoryFixPRTitle, advisoryFindingTitle, nil)
		got := hub.verifyReportedPRDetail("repo1", "https://github.com/myorg/repo1/pull/7", "alice")
		if !got.Verified {
			t.Fatalf("merged=%v: Verified = false, want true (repo and author match)", merged)
		}
		if got.Merged != merged {
			t.Errorf("merged=%v: Merged = %v", merged, got.Merged)
		}
		if got.Title != advisoryFixPRTitle {
			t.Errorf("merged=%v: Title = %q, want the PR's own title %q", merged, got.Title, advisoryFixPRTitle)
		}
	}
}
