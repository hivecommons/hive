package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/dupsweep"
)

// dupWirePR is the minimum of GitHub's PR shape this sweep reads.
type dupWirePR struct {
	Number       int              `json:"number"`
	Title        string           `json:"title"`
	User         map[string]any   `json:"user"`
	Draft        bool             `json:"draft"`
	CreatedAt    string           `json:"created_at"`
	ChangedFiles int              `json:"changed_files"`
	Head         map[string]any   `json:"head"`
	HTMLURL      string           `json:"html_url"`
	Files        []dupWireFile    `json:"-"`
	Comments     []dupWireComment `json:"-"`
}

type dupWireFile struct {
	Filename string `json:"filename"`
	Patch    string `json:"patch"`
}

type dupWireComment struct {
	ID   int64          `json:"id"`
	Body string         `json:"body"`
	User map[string]any `json:"user"`
}

// dupSweepServer serves one repo's open PRs, their files, and their comment
// lists, recording every write the sweep performs.
type dupSweepServer struct {
	mu sync.Mutex
	// posted maps PR number to the bodies POSTed on it.
	posted map[int][]string
	// patched maps comment ID to the body it was edited to.
	patched map[int64]string
	// writes counts every non-GET request, including ones to endpoints the
	// sweep must never touch.
	writes []string
}

func newDupSweepServer(t *testing.T, owner, repo string, prs []dupWirePR) (*httptest.Server, *dupSweepServer) {
	t.Helper()
	rec := &dupSweepServer{posted: map[int][]string{}, patched: map[int64]string{}}
	byNum := map[int]dupWirePR{}
	for _, pr := range prs {
		byNum[pr.Number] = pr
	}

	mux := http.NewServeMux()
	base := fmt.Sprintf("/repos/%s/%s/", owner, repo)

	mux.HandleFunc(base, func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, base)
		rec.mu.Lock()
		if r.Method != http.MethodGet {
			rec.writes = append(rec.writes, r.Method+" "+path)
		}
		rec.mu.Unlock()

		switch {
		case path == "pulls" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(prs)

		case strings.HasPrefix(path, "pulls/") && strings.HasSuffix(path, "/files"):
			var n int
			fmt.Sscanf(path, "pulls/%d/files", &n)
			_ = json.NewEncoder(w).Encode(byNum[n].Files)

		case strings.HasPrefix(path, "issues/") && strings.HasSuffix(path, "/comments"):
			var n int
			fmt.Sscanf(path, "issues/%d/comments", &n)
			if r.Method == http.MethodPost {
				var body map[string]string
				_ = json.NewDecoder(r.Body).Decode(&body)
				rec.mu.Lock()
				rec.posted[n] = append(rec.posted[n], body["body"])
				rec.mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 1000 + n})
				return
			}
			_ = json.NewEncoder(w).Encode(byNum[n].Comments)

		case strings.HasPrefix(path, "issues/comments/"):
			var id int64
			fmt.Sscanf(path, "issues/comments/%d", &id)
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			rec.mu.Lock()
			rec.patched[id] = body["body"]
			rec.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})

		default:
			http.Error(w, "unexpected path: "+path, http.StatusNotFound)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, rec
}

func dupPR(number int, author, title, sha string, created string, files ...dupWireFile) dupWirePR {
	return dupWirePR{
		Number:       number,
		Title:        title,
		User:         map[string]any{"login": author},
		CreatedAt:    created,
		ChangedFiles: len(files),
		Head:         map[string]any{"sha": sha},
		HTMLURL:      fmt.Sprintf("https://github.com/o/r/pull/%d", number),
		Files:        files,
	}
}

func resetDupCache() {
	dupSweepCacheMu.Lock()
	dupSweepCache = map[string]prFingerprint{}
	dupSweepCacheMu.Unlock()
}

const dupWorkflow = ".github/workflows/import-rawhide-package.yml"

// dupMarkerForWorkflowCluster reproduces the marker the sweep will render for
// the dupThreePRs cluster, so a test can plant a stale comment the sweep is
// obliged to recognise as its own.
func dupMarkerForWorkflowCluster() string {
	c := dupsweep.Find([]dupsweep.PR{
		{Repo: "ublue-os/utah-packages", Number: 97, Files: []string{dupWorkflow}},
		{Repo: "ublue-os/utah-packages", Number: 114, Files: []string{dupWorkflow}},
	}, dupsweep.Options{})[0]
	return dupsweep.MarkerFor(c)
}

func dupThreePRs() []dupWirePR {
	return []dupWirePR{
		dupPR(97, "alice", "bind dispatch inputs at job scope", "sha97", "2026-09-01T00:00:00Z",
			dupWireFile{Filename: dupWorkflow, Patch: "@@ job scope @@"}),
		dupPR(114, "bob", "bind dispatch inputs through env", "sha114", "2026-09-05T00:00:00Z",
			dupWireFile{Filename: dupWorkflow, Patch: "@@ step scope @@"}),
		dupPR(121, "carol", "fix unbound workflow inputs", "sha121", "2026-09-07T00:00:00Z",
			dupWireFile{Filename: dupWorkflow, Patch: "@@ step scope @@"}),
	}
}

// Report-only is the default shape of the feature: `duplicate_sweep.enabled`
// without `post_comments` must find the cluster and write NOTHING. An operator
// has to be able to read what it would say before it says it.
func TestSweepDuplicatePRs_ReportOnlyWritesNothing(t *testing.T) {
	resetDupCache()
	srv, rec := newDupSweepServer(t, "ublue-os", "utah-packages", dupThreePRs())
	c := newTestClient(t, srv, "ublue-os", []string{"ublue-os/utah-packages"})

	result, err := c.SweepDuplicatePRs(context.Background(), DuplicateSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Clusters) != 1 {
		t.Fatalf("want 1 cluster, got %d", len(result.Clusters))
	}
	if result.Clusters[0].Survivor != 97 {
		t.Errorf("survivor = #%d, want #97", result.Clusters[0].Survivor)
	}
	if result.Commented != 0 {
		t.Errorf("report-only sweep wrote %d comments", result.Commented)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.writes) != 0 {
		t.Errorf("report-only sweep performed writes: %v", rec.writes)
	}
}

func TestSweepDuplicatePRs_PostsOnLaterPRsOnly(t *testing.T) {
	resetDupCache()
	srv, rec := newDupSweepServer(t, "ublue-os", "utah-packages", dupThreePRs())
	c := newTestClient(t, srv, "ublue-os", []string{"ublue-os/utah-packages"})

	result, err := c.SweepDuplicatePRs(context.Background(), DuplicateSweepOptions{PostComments: true})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Commented != 2 {
		t.Errorf("commented = %d, want 2", result.Commented)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.posted[97]) != 0 {
		t.Errorf("the survivor #97 was commented on: %v", rec.posted[97])
	}
	for _, n := range []int{114, 121} {
		if len(rec.posted[n]) != 1 {
			t.Fatalf("#%d got %d comments, want 1", n, len(rec.posted[n]))
		}
		if !strings.Contains(rec.posted[n][0], "#97") {
			t.Errorf("#%d comment does not name the survivor", n)
		}
	}
}

// The only GitHub mutation this sweep is permitted to make is a comment. A
// close, a label, a review, or a merge would convert a candidate generator
// with known false positives into an automated action.
func TestSweepDuplicatePRs_OnlyEverComments(t *testing.T) {
	resetDupCache()
	srv, rec := newDupSweepServer(t, "ublue-os", "utah-packages", dupThreePRs())
	c := newTestClient(t, srv, "ublue-os", []string{"ublue-os/utah-packages"})

	if _, err := c.SweepDuplicatePRs(context.Background(), DuplicateSweepOptions{PostComments: true}); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.writes) == 0 {
		t.Fatal("no writes at all; the test would pass vacuously")
	}
	for _, wr := range rec.writes {
		if !strings.HasSuffix(wr, "/comments") || !strings.HasPrefix(wr, "POST issues/") {
			t.Errorf("sweep performed a non-comment write: %q", wr)
		}
		for _, forbidden := range []string{"/merge", "/labels", "/reviews", "PATCH pulls/"} {
			if strings.Contains(wr, forbidden) {
				t.Errorf("sweep touched a forbidden endpoint: %q", wr)
			}
		}
	}
}

// A sweep runs on a cadence. Without in-place edits it would stack a fresh
// suggestion on every contributor's PR every hour, which is worse than the
// queue it is trying to reduce.
func TestSweepDuplicatePRs_IsIdempotent(t *testing.T) {
	resetDupCache()
	prs := dupThreePRs()
	srv, rec := newDupSweepServer(t, "ublue-os", "utah-packages", prs)
	c := newTestClient(t, srv, "ublue-os", []string{"ublue-os/utah-packages"})

	if _, err := c.SweepDuplicatePRs(context.Background(), DuplicateSweepOptions{PostComments: true}); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	rec.mu.Lock()
	first := rec.posted[121][0]
	rec.mu.Unlock()

	// Feed the first pass's comment back as existing history, exactly as
	// GitHub would on the next pass, and sweep again.
	for i := range prs {
		if prs[i].Number == 121 {
			prs[i].Comments = []dupWireComment{{ID: 55, Body: first, User: map[string]any{"login": "hive[bot]"}}}
		}
	}
	resetDupCache()
	srv2, rec2 := newDupSweepServer(t, "ublue-os", "utah-packages", prs)
	c2 := newTestClient(t, srv2, "ublue-os", []string{"ublue-os/utah-packages"})
	if _, err := c2.SweepDuplicatePRs(context.Background(), DuplicateSweepOptions{PostComments: true}); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	rec2.mu.Lock()
	defer rec2.mu.Unlock()
	if len(rec2.posted[121]) != 0 {
		t.Errorf("second sweep re-POSTed on #121 instead of recognising its own comment: %v", rec2.posted[121])
	}
	if _, edited := rec2.patched[55]; edited {
		t.Error("second sweep edited an unchanged comment; the no-op write must be elided")
	}
}

// An existing suggestion whose content has gone stale must be EDITED, not
// left standing: a comment naming a survivor that has since been closed is
// worse than none.
func TestSweepDuplicatePRs_EditsStaleSuggestionInPlace(t *testing.T) {
	resetDupCache()
	prs := dupThreePRs()
	for i := range prs {
		if prs[i].Number == 121 {
			prs[i].Comments = []dupWireComment{{
				ID:   77,
				Body: dupMarkerForWorkflowCluster() + "\nstale text",
				User: map[string]any{"login": "hive[bot]"},
			}}
		}
	}
	srv, rec := newDupSweepServer(t, "ublue-os", "utah-packages", prs)
	c := newTestClient(t, srv, "ublue-os", []string{"ublue-os/utah-packages"})
	if _, err := c.SweepDuplicatePRs(context.Background(), DuplicateSweepOptions{PostComments: true}); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.posted[121]) != 0 {
		t.Errorf("stale comment was duplicated rather than edited: %v", rec.posted[121])
	}
	body, edited := rec.patched[77]
	if !edited {
		t.Fatal("stale suggestion was not edited")
	}
	if !strings.Contains(body, "#97") {
		t.Error("edited body does not name the survivor")
	}
}

// A PR whose file list GitHub truncated must be EXCLUDED, not fingerprinted
// from what came back. A short list fingerprints as a different, smaller set
// that can coincide with another truncated list — inventing a duplicate that
// does not exist and sending a human to close real work.
func TestSweepDuplicatePRs_ExcludesTruncatedFileLists(t *testing.T) {
	resetDupCache()
	prs := dupThreePRs()
	// GitHub reports more changed files than the files endpoint returns.
	prs[1].ChangedFiles = 9
	srv, _ := newDupSweepServer(t, "ublue-os", "utah-packages", prs)
	c := newTestClient(t, srv, "ublue-os", []string{"ublue-os/utah-packages"})

	result, err := c.SweepDuplicatePRs(context.Background(), DuplicateSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Skipped != 1 {
		t.Errorf("skipped = %d, want 1 (the truncated PR)", result.Skipped)
	}
	if len(result.Clusters) != 1 {
		t.Fatalf("want 1 cluster from the two intact PRs, got %d", len(result.Clusters))
	}
	for _, n := range result.Clusters[0].Superseded {
		if n == 114 {
			t.Error("the truncated PR #114 was clustered anyway")
		}
	}
}

func TestSweepDuplicatePRs_SkipsDrafts(t *testing.T) {
	resetDupCache()
	prs := dupThreePRs()
	prs[2].Draft = true
	srv, rec := newDupSweepServer(t, "ublue-os", "utah-packages", prs)
	c := newTestClient(t, srv, "ublue-os", []string{"ublue-os/utah-packages"})

	result, err := c.SweepDuplicatePRs(context.Background(), DuplicateSweepOptions{PostComments: true})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.posted[121]) != 0 {
		t.Error("a draft PR was commented on")
	}
	if result.Commented != 1 {
		t.Errorf("commented = %d, want 1 (#114 only)", result.Commented)
	}
}

// NEGATIVE CONTROL for the whole sweep: a repo whose open PRs share no file
// set must produce no clusters and no writes. Without this the suite could
// pass by always reporting a duplicate.
func TestSweepDuplicatePRs_NoDuplicatesNoOutput(t *testing.T) {
	resetDupCache()
	prs := []dupWirePR{
		dupPR(1, "alice", "fix a", "sha1", "2026-09-01T00:00:00Z", dupWireFile{Filename: "a.go", Patch: "@@a@@"}),
		dupPR(2, "bob", "fix b", "sha2", "2026-09-02T00:00:00Z", dupWireFile{Filename: "b.go", Patch: "@@b@@"}),
		dupPR(3, "carol", "fix c", "sha3", "2026-09-03T00:00:00Z", dupWireFile{Filename: "c.go", Patch: "@@c@@"}),
	}
	srv, rec := newDupSweepServer(t, "ublue-os", "utah-packages", prs)
	c := newTestClient(t, srv, "ublue-os", []string{"ublue-os/utah-packages"})

	result, err := c.SweepDuplicatePRs(context.Background(), DuplicateSweepOptions{PostComments: true})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Scanned != 3 {
		t.Errorf("scanned = %d, want 3", result.Scanned)
	}
	if len(result.Clusters) != 0 {
		t.Errorf("want no clusters, got %+v", result.Clusters)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.writes) != 0 {
		t.Errorf("sweep wrote on a queue with no duplicates: %v", rec.writes)
	}
}

func TestSweepDuplicatePRs_RespectsMaxComments(t *testing.T) {
	resetDupCache()
	srv, rec := newDupSweepServer(t, "ublue-os", "utah-packages", dupThreePRs())
	c := newTestClient(t, srv, "ublue-os", []string{"ublue-os/utah-packages"})

	result, err := c.SweepDuplicatePRs(context.Background(), DuplicateSweepOptions{PostComments: true, MaxComments: 1})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Commented != 1 {
		t.Errorf("commented = %d, want 1 under the cap", result.Commented)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if total := len(rec.posted[114]) + len(rec.posted[121]); total != 1 {
		t.Errorf("wrote %d comments despite MaxComments=1", total)
	}
}

// A title carrying an @mention must not survive to the wire. The comment is
// rewritten on a cadence, so a live mention re-notifies that person forever.
func TestSweepDuplicatePRs_PostedBodyHasNoLiveMention(t *testing.T) {
	resetDupCache()
	prs := []dupWirePR{
		dupPR(1, "alice", "revert what @octocat asked for", "sha1", "2026-09-01T00:00:00Z", dupWireFile{Filename: "a.go", Patch: "@@a@@"}),
		dupPR(2, "bob", "revert per @octocat", "sha2", "2026-09-02T00:00:00Z", dupWireFile{Filename: "a.go", Patch: "@@a@@"}),
	}
	srv, rec := newDupSweepServer(t, "ublue-os", "utah-packages", prs)
	c := newTestClient(t, srv, "ublue-os", []string{"ublue-os/utah-packages"})
	if _, err := c.SweepDuplicatePRs(context.Background(), DuplicateSweepOptions{PostComments: true}); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.posted[2]) != 1 {
		t.Fatalf("want 1 comment on #2, got %d", len(rec.posted[2]))
	}
	if strings.Contains(rec.posted[2][0], "@octocat") {
		t.Errorf("posted body carries a live @mention:\n%s", rec.posted[2][0])
	}
}
