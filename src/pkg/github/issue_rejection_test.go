package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// #6463: a finding a maintainer closed as not-planned must not be re-filed,
// however the producer rewords it. These tests pin the identity key (the
// file-reference set), every fail-toward-filing branch of the gate, and the
// watcher's terminal handling of a refused create.

func TestIssueFileRefSet(t *testing.T) {
	cases := []struct {
		name, text string
		want       []string
	}{
		{
			"paths with drifting line numbers normalize equal",
			"except syntax at __init__.py:35, client.py:290 and client.py:343",
			[]string{"__init__.py", "client.py"},
		},
		{
			"relative paths and column suffixes",
			"see pkg/advisory/evidence.go:53:12 and src/docs/install.md",
			[]string{"pkg/advisory/evidence.go", "src/docs/install.md"},
		},
		{
			"URLs are stripped before extraction",
			"per https://peps.python.org/pep-0758/ the file client.py is valid",
			[]string{"client.py"},
		},
		{
			"bare domains are not files",
			"hosted on github.com and dibs.hivecommons.dev, fix hub.go",
			[]string{"hub.go"},
		},
		{
			"version strings are not files",
			"since v4.23.3 and Python 3.14 nothing changed",
			nil,
		},
		{
			"no refs at all",
			"the roadmap should mention token metering",
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := issueFileRefSet(tc.text)
			if len(got) != len(tc.want) {
				t.Fatalf("issueFileRefSet(%q) = %v, want %v", tc.text, got, tc.want)
			}
			for _, w := range tc.want {
				if !got[w] {
					t.Errorf("issueFileRefSet(%q) missing %q (got %v)", tc.text, w, got)
				}
			}
		})
	}
}

func TestEqualFileRefSets(t *testing.T) {
	set := func(ks ...string) map[string]bool {
		m := make(map[string]bool)
		for _, k := range ks {
			m[k] = true
		}
		return m
	}
	if equalFileRefSets(set(), set()) {
		t.Error("two empty sets must NOT match: no evidence, no gate")
	}
	if !equalFileRefSets(set("a.go", "b.go"), set("b.go", "a.go")) {
		t.Error("equal sets should match regardless of order")
	}
	if equalFileRefSets(set("a.go"), set("a.go", "b.go")) {
		t.Error("subset is not equality — a new defect touching one more file must file")
	}
}

// rejectionMockServer serves: GET /issues?state=open (dedupe) → empty; GET
// /issues?state=closed (rejection gate) → closedJSON if the creator filter was
// applied, recording the scan in closedListed; POST /issues → create, counted.
func rejectionMockServer(t *testing.T, closedJSON string, closedStatus int, created, closedListed *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/labels/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"x"}`))
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/issues"):
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("state") != "closed" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			if closedListed != nil {
				*closedListed++
			}
			if got := r.URL.Query().Get("creator"); got != "hive-app[bot]" {
				t.Errorf("closed-issue scan must filter by App bot creator, got %q", got)
			}
			if closedStatus != 0 && closedStatus != http.StatusOK {
				w.WriteHeader(closedStatus)
				return
			}
			_, _ = w.Write([]byte(closedJSON))
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/issues"):
			if created != nil {
				*created++
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"number":99,"html_url":"https://github.example/o/r/issues/99"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func closedIssueJSON(t *testing.T, number int, title, body, stateReason string, closedAt time.Time) string {
	t.Helper()
	b, err := json.Marshal([]map[string]any{{
		"number":       number,
		"title":        title,
		"body":         body,
		"state":        "closed",
		"state_reason": stateReason,
		"closed_at":    closedAt.UTC().Format(time.RFC3339),
		"updated_at":   closedAt.UTC().Format(time.RFC3339),
		"html_url":     "https://github.example/o/r/issues/79",
		"user":         map[string]any{"login": "hive-app[bot]"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The live loop: five differently-worded filings, same file set, line numbers
// renumbered by unrelated commits. Once one is closed not_planned, the next
// rewording is refused and the response points at the maintainer's closure.
func TestCreateIssue_RefusesRejectedTwin(t *testing.T) {
	created := 0
	closed := closedIssueJSON(t, 79,
		"Python 2 multi-exception syntax in __init__.py and client.py breaks import",
		"except A, B at __init__.py:35, client.py:290 and client.py:343",
		"not_planned", time.Now().Add(-24*time.Hour))
	srv := rejectionMockServer(t, closed, 0, &created, nil)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = "hive-app[bot]"

	res, err := c.CreateIssue(context.Background(),
		"o/r", "SyntaxError: unparenthesized multi-except breaks integration at import",
		"unparenthesized except at __init__.py:45, client.py:321 — see client.py:374", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.RejectedTwin {
		t.Fatalf("expected RejectedTwin, got %+v", res)
	}
	if res.Number != 79 || res.RejectedReason != "not_planned" {
		t.Errorf("rejection should name the closed issue and reason, got %+v", res)
	}
	if created != 0 {
		t.Fatalf("expected 0 issues created, got %d", created)
	}
}

// A "completed" close means fixed, and a re-report may be a real regression —
// it must file.
func TestCreateIssue_FilesWhenClosedCompleted(t *testing.T) {
	created := 0
	closed := closedIssueJSON(t, 79, "bug in client.py", "broken at client.py:290",
		"completed", time.Now().Add(-24*time.Hour))
	srv := rejectionMockServer(t, closed, 0, &created, nil)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = "hive-app[bot]"

	res, err := c.CreateIssue(context.Background(), "o/r", "bug is back in client.py", "regressed at client.py:310", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RejectedTwin || created != 1 {
		t.Fatalf("completed close must not gate: res=%+v created=%d", res, created)
	}
}

// Exact set equality: a finding that names ANY other file is new evidence.
func TestCreateIssue_FilesWhenFileSetDiffers(t *testing.T) {
	created := 0
	closed := closedIssueJSON(t, 79, "bug in client.py", "at client.py:290",
		"not_planned", time.Now().Add(-24*time.Hour))
	srv := rejectionMockServer(t, closed, 0, &created, nil)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = "hive-app[bot]"

	res, err := c.CreateIssue(context.Background(), "o/r", "bug in client.py and server.py", "at client.py:290 and server.py:12", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RejectedTwin || created != 1 {
		t.Fatalf("differing file set must file: res=%+v created=%d", res, created)
	}
}

// A rejection outside the 30-day window has expired.
func TestCreateIssue_FilesWhenRejectionExpired(t *testing.T) {
	created := 0
	closed := closedIssueJSON(t, 79, "bug in client.py", "at client.py:290",
		"not_planned", time.Now().Add(-45*24*time.Hour))
	srv := rejectionMockServer(t, closed, 0, &created, nil)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = "hive-app[bot]"

	res, err := c.CreateIssue(context.Background(), "o/r", "bug in client.py", "at client.py:290", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RejectedTwin || created != 1 {
		t.Fatalf("expired rejection must not gate: res=%+v created=%d", res, created)
	}
}

// Lookup failure files anyway — suppression only on positive evidence.
func TestCreateIssue_FilesWhenRejectionLookupFails(t *testing.T) {
	created := 0
	srv := rejectionMockServer(t, "", http.StatusBadGateway, &created, nil)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = "hive-app[bot]"

	res, err := c.CreateIssue(context.Background(), "o/r", "bug in client.py", "at client.py:290", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RejectedTwin || created != 1 {
		t.Fatalf("failed lookup must file: res=%+v created=%d", res, created)
	}
}

// Without an App-bot identity the gate cannot attribute past filings to the
// hive and must not run at all.
func TestCreateIssue_SkipsGateWithoutBotLogin(t *testing.T) {
	created, closedListed := 0, 0
	srv := rejectionMockServer(t, "[]", 0, &created, &closedListed)
	defer srv.Close()
	c := issueTestClient(t, srv.URL) // appBotLogin left empty

	res, err := c.CreateIssue(context.Background(), "o/r", "bug in client.py", "at client.py:290", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RejectedTwin || created != 1 {
		t.Fatalf("no bot login must file: res=%+v created=%d", res, created)
	}
	if closedListed != 0 {
		t.Errorf("gate must not scan closed issues without a bot identity, scanned %d times", closedListed)
	}
}

// A finding with no file references has nothing to key on — no closed scan.
func TestCreateIssue_SkipsGateWithoutFileRefs(t *testing.T) {
	created, closedListed := 0, 0
	srv := rejectionMockServer(t, "[]", 0, &created, &closedListed)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = "hive-app[bot]"

	res, err := c.CreateIssue(context.Background(), "o/r", "governance process is unclear", "no file names anything", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RejectedTwin || created != 1 {
		t.Fatalf("no file refs must file: res=%+v created=%d", res, created)
	}
	if closedListed != 0 {
		t.Errorf("gate must not scan closed issues without file refs, scanned %d times", closedListed)
	}
}

// End-to-end through the watcher: a refused create is TERMINAL — the request
// is consumed (never retried) and the result names the closed issue.
func TestIssueRequestWatcher_RejectedTwinIsTerminal(t *testing.T) {
	created := 0
	closed := closedIssueJSON(t, 79, "bug in client.py", "at client.py:290",
		"not_planned", time.Now().Add(-24*time.Hour))
	srv := rejectionMockServer(t, closed, 0, &created, nil)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = "hive-app[bot]"
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Repo: "o/r", Title: "[scanner] import broken in client.py", Body: "at client.py:355", Agent: "scanner",
	})
	if err != nil {
		t.Fatal(err)
	}

	c.ProcessIssueRequestsOnce(context.Background())

	if created != 0 {
		t.Fatalf("expected 0 issues created, got %d", created)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Errorf("refused request must be consumed, not retried")
	}
	var res IssueResponse
	b, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("result file missing: %v", err)
	}
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	if res.OK || !res.RejectedDuplicate || res.Number != 79 {
		t.Errorf("expected rejected_duplicate result naming issue 79, got %+v", res)
	}
	if !strings.Contains(res.Error, "do not re-file") {
		t.Errorf("result error should instruct the agent not to re-file, got %q", res.Error)
	}
}
