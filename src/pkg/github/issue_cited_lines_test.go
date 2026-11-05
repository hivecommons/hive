package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
)

const (
	citedOldSHA = "20f3aa8c15968c434ad2428280fac614d907df6d"
	citedFile   = "docs/skills/test-authoring/gnome/SKILL.md"
)

// citedBody is projectbluefin/testsuite#1085's evidence line.
func citedBody(org, repo, path string, line int) string {
	return "Filed from an out-of-scope finding in the Hive review of PR #1081.\n\n" +
		"Evidence: `" + path + ":" + strconv.Itoa(line) + "` (https://github.com/" + org + "/" + repo + "/blob/" + citedOldSHA + "/" + path + "#L" + strconv.Itoa(line) + ")\n" +
		"Finding: SKILL.md still documents a Firefox per-name tree.root.application() fallback that no longer exists\n"
}

func citedIssue(n int, body string) *gh.Issue {
	return &gh.Issue{Number: gh.Ptr(n), Body: gh.Ptr(body)}
}

func TestCitedRefsParsesPermalinks(t *testing.T) {
	body := citedBody("projectbluefin", "testsuite", citedFile, 163) +
		"also https://github.com/projectbluefin/testsuite/blob/" + citedOldSHA + "/scripts/vm_validation_plan.py#L108-L110\n" +
		"elsewhere https://github.com/other/repo/blob/" + citedOldSHA + "/x.go#L1\n" +
		"a branch link https://github.com/projectbluefin/testsuite/blob/main/README.md#L3\n" +
		"a huge range https://github.com/projectbluefin/testsuite/blob/" + citedOldSHA + "/big.txt#L10-L500\n"
	refs := citedRefs(body, "projectbluefin", "testsuite")
	var got []string
	for _, r := range refs {
		got = append(got, r.display())
	}
	want := []string{citedFile + ":163", "scripts/vm_validation_plan.py:108-110", "big.txt:10-29"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("refs = %v, want %v (same repo, pinned SHA, span capped, other repos and branch links ignored)", got, want)
	}
}

// citedRepo serves file contents at a pinned SHA and on the default branch,
// counting reads.
type citedRepo struct {
	mu    sync.Mutex
	files map[string]string // "<ref>:<path>"; ref "" is the default branch
	reads int
	fail  bool
}

func (cr *citedRepo) handle(w http.ResponseWriter, r *http.Request) {
	cr.mu.Lock()
	cr.reads++
	fail := cr.fail
	path := r.URL.Path[strings.Index(r.URL.Path, "/contents/")+len("/contents/"):]
	content, ok := cr.files[r.URL.Query().Get("ref")+":"+path]
	cr.mu.Unlock()
	if fail {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "file", "encoding": "base64", "path": path, "content": base64.StdEncoding.EncodeToString([]byte(content))})
}

func fileWithLine(n int, line string) string {
	lines := make([]string, n+5)
	for i := range lines {
		lines[i] = "filler line number " + strconv.Itoa(i+1) + " of the skill guide"
	}
	lines[n-1] = line
	return strings.Join(lines, "\n")
}

// projectbluefin/testsuite#1085's shape end to end through the enumeration:
// the agent finding the issue filter refuses records the citation main no
// longer has; a finding whose line is still there, a person's issue and an
// admitted issue record nothing.
func TestEnumerationRecordsCitedLinesGone(t *testing.T) {
	const org, repo = "testorg", "testrepo"
	cited := "`_firefox_app()` falls back to per-name tree.root.application() lookups"
	issues := []wireIssue{
		{Number: 1085, Title: "[reviewer] stale doc", User: wireUser{"hive[bot]"}, Labels: []wireLabel{{Name: "agent/reviewer"}}, Body: citedBody(org, repo, citedFile, 163), CreatedAt: hoursAgo(5)},
		{Number: 1086, Title: "[reviewer] still true", User: wireUser{"hive[bot]"}, Labels: []wireLabel{{Name: "agent/reviewer"}}, Body: citedBody(org, repo, "scripts/orca_steps.py", 12), CreatedAt: hoursAgo(4)},
		{Number: 1087, Title: "[reviewer] file deleted", User: wireUser{"hive[bot]"}, Labels: []wireLabel{{Name: "agent/reviewer"}}, Body: citedBody(org, repo, "scripts/gone.py", 3), CreatedAt: hoursAgo(3)},
		{Number: 1088, Title: "a person's", User: wireUser{"erin"}, Body: citedBody(org, repo, citedFile, 163), CreatedAt: hoursAgo(2)},
		{Number: 1089, Title: "admitted finding", User: wireUser{"hive[bot]"}, Labels: []wireLabel{{Name: "agent/reviewer"}, {Name: "triage/accepted"}}, Body: citedBody(org, repo, citedFile, 163), CreatedAt: hoursAgo(1)},
	}
	cr := &citedRepo{files: map[string]string{
		citedOldSHA + ":" + citedFile:          fileWithLine(163, cited),
		":" + citedFile:                        fileWithLine(170, "`_firefox_app()` uses the shared accessible lookup only"),
		citedOldSHA + ":scripts/orca_steps.py": fileWithLine(12, "def press_key_combination(context, combo):"),
		":scripts/orca_steps.py":               fileWithLine(40, "def press_key_combination(context, combo):"),
		citedOldSHA + ":scripts/gone.py":       fileWithLine(3, "ASSERT_PASSED_LANE = ('kde-smoke', GUI)"),
	}}
	mux := buildMux(t, org, repo, issues, nil)
	mux.HandleFunc("/repos/"+org+"/"+repo+"/contents/", cr.handle)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	c.SetIssueFilter(config.IssueFilterConfig{RequireLabels: []string{"triage/accepted"}})

	gone := func() map[int]string {
		result, err := c.EnumerateActionable(context.Background())
		if err != nil {
			t.Fatalf("EnumerateActionable: %v", err)
		}
		out := map[int]string{}
		for _, d := range result.WorkDetailsByRepo[repo].Issues {
			out[d.Number] = d.CitedLinesGone
		}
		return out
	}
	got := gone()
	want := map[int]string{1085: citedFile + ":163", 1086: "", 1087: "scripts/gone.py:3", 1088: ""}
	for n, w := range want {
		if got[n] != w {
			t.Errorf("#%d CitedLinesGone = %q, want %q", n, got[n], w)
		}
	}
	if _, ok := got[1089]; ok {
		t.Errorf("#1089 is admitted and has no work row: %v", got)
	}
	first := cr.reads
	if first == 0 || first > 2*citedMaxChecksPerRepo {
		t.Fatalf("reads = %d, want at most two per checked finding", first)
	}
	// A second pass within the TTL reads nothing.
	if again := gone(); again[1085] != want[1085] || cr.reads != first {
		t.Fatalf("second pass: #1085 = %q, reads %d → %d; want the cached result and no reads", again[1085], first, cr.reads)
	}
}

func TestCitedLinesGoneBudgetAndFailures(t *testing.T) {
	const org, repo = "o", "r"
	cr := &citedRepo{files: map[string]string{
		citedOldSHA + ":" + citedFile: fileWithLine(163, "a line long enough to count as evidence here"),
		":" + citedFile:               "rewritten entirely",
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+org+"/"+repo+"/contents/", cr.handle)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	finding := func(n int) *gh.Issue { return citedIssue(n, citedBody(org, repo, citedFile, 163)) }
	agent := []string{"agent/reviewer"}

	budget := 1
	if g := c.citedLinesGone(context.Background(), org, repo, finding(1), agent, &budget); g != citedFile+":163" || budget != 0 {
		t.Fatalf("first = %q budget %d", g, budget)
	}
	reads := cr.reads
	if g := c.citedLinesGone(context.Background(), org, repo, finding(2), agent, &budget); g != "" || cr.reads != reads {
		t.Fatalf("over budget: %q with %d new reads, want nothing read", g, cr.reads-reads)
	}
	budget = 5
	if g := c.citedLinesGone(context.Background(), org, repo, finding(3), nil, &budget); g != "" || budget != 5 {
		t.Fatalf("not an agent finding: %q budget %d, want no check", g, budget)
	}
	// An unreadable main is not "gone", and is not cached as an answer.
	cr.mu.Lock()
	cr.fail = true
	cr.mu.Unlock()
	if g := c.citedLinesGone(context.Background(), org, repo, finding(4), agent, &budget); g != "" {
		t.Fatalf("read failure = %q, want unknown", g)
	}
	cr.mu.Lock()
	cr.fail = false
	cr.mu.Unlock()
	if g := c.citedLinesGone(context.Background(), org, repo, finding(4), agent, &budget); g != citedFile+":163" {
		t.Fatalf("after the failure = %q, want it re-checked", g)
	}
	// Cited lines too short to be evidence decide nothing.
	cr.mu.Lock()
	cr.files[citedOldSHA+":short.go"] = fileWithLine(2, "}")
	cr.mu.Unlock()
	short := citedIssue(5, "https://github.com/o/r/blob/"+citedOldSHA+"/short.go#L2")
	if g := c.citedLinesGone(context.Background(), org, repo, short, agent, &budget); g != "" {
		t.Fatalf("short evidence = %q, want unknown", g)
	}
}

// Two citations: CLOSE only when every one is gone.
func TestCitedLinesGoneNeedsEveryCitationGone(t *testing.T) {
	const org, repo = "o", "r"
	cr := &citedRepo{files: map[string]string{
		citedOldSHA + ":a.md": fileWithLine(5, "the first cited sentence that moved away"),
		":a.md":               "nothing like it",
		citedOldSHA + ":b.md": fileWithLine(7, "the second cited sentence that is still here"),
		":b.md":               fileWithLine(9, "the second cited sentence that is still here"),
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+org+"/"+repo+"/contents/", cr.handle)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	body := "https://github.com/o/r/blob/" + citedOldSHA + "/a.md#L5 and https://github.com/o/r/blob/" + citedOldSHA + "/b.md#L7"
	budget := 3
	if g := c.citedLinesGone(context.Background(), org, repo, citedIssue(1, body), []string{"agent/quality"}, &budget); g != "" {
		t.Fatalf("one citation still on main: %q, want nothing", g)
	}
}
