package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/stageengine/stageenginetest"
)

// scannerVerdictServer fakes one issue (state, labels, PR-ness) and its
// comments for the scanner verdict relay.
type scannerVerdictServer struct {
	mu       sync.Mutex
	number   int
	state    string
	isPR     bool
	labels   []string
	comments []map[string]any
	creates  int
}

var (
	scannerVerdictIssueRE = regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues/(\d+)$`)
	scannerVerdictLabelRE = regexp.MustCompile(`^/repos/[^/]+/[^/]+/issues/\d+/labels/(.+)$`)
)

func (s *scannerVerdictServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.EscapedPath()
		switch {
		case r.Method == http.MethodGet && scannerVerdictIssueRE.MatchString(path):
			if scannerVerdictIssueRE.FindStringSubmatch(path)[1] != jsonNumber(s.number) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			labels := []any{}
			for _, l := range s.labels {
				labels = append(labels, map[string]any{"name": l})
			}
			issue := map[string]any{"number": s.number, "state": s.state, "labels": labels, "updated_at": "2026-11-17T10:22:00Z",
				"user": map[string]any{"login": "alice", "type": "User"}}
			if s.isPR {
				issue["pull_request"] = map[string]any{"url": "https://api.github.com/repos/o/r/pulls/1"}
			}
			_ = json.NewEncoder(w).Encode(issue)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/comments"):
			_ = json.NewEncoder(w).Encode(s.comments)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/comments"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.creates++
			cm := map[string]any{"id": 100 + s.creates, "body": body["body"], "user": map[string]any{"login": "hive-app[bot]", "type": "Bot"}}
			s.comments = append(s.comments, cm)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(cm)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/labels"):
			var add []string
			_ = json.NewDecoder(r.Body).Decode(&add)
			for _, l := range add {
				if !labelPresent(s.labels, l) {
					s.labels = append(s.labels, l)
				}
			}
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodDelete && scannerVerdictLabelRE.MatchString(path):
			name, _ := url.PathUnescape(scannerVerdictLabelRE.FindStringSubmatch(path)[1])
			kept := s.labels[:0]
			for _, l := range s.labels {
				if !strings.EqualFold(l, name) {
					kept = append(kept, l)
				}
			}
			s.labels = kept
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *scannerVerdictServer) currentLabels() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.labels...)
}

func (s *scannerVerdictServer) addHumanComment(body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.comments = append(s.comments, map[string]any{"id": 900 + len(s.comments), "body": body, "user": map[string]any{"login": "alice", "type": "User"}})
}

func scannerVerdictClient(t *testing.T, s *scannerVerdictServer) (*Client, *stageenginetest.Recorder) {
	t.Helper()
	c := testClient(t, s.start(t).URL)
	c.SetAppBotLogin("hive-app[bot]")
	sink := &stageenginetest.Recorder{}
	c.SetStageFactSink(sink)
	return c, sink
}

const scannerNotStartable = "**Scanner:** not started / not startable by an agent. Reproducing this needs real subscription logins that no agent pane has."

// The #18421 / #18659 shape: a "not startable" verdict moves the issue out
// of stage/010-actionable (needs-human, a Parked fact, help wanted off), and
// the next scanner pass skips it: the enumeration hard-suppresses it, the
// stage derivation holds it on a person, and the claim gate refuses it.
func TestScannerIssueVerdictParksIssue(t *testing.T) {
	s := &scannerVerdictServer{number: 18421, state: "open", labels: []string{"stage/010-actionable", helpWantedLabel, "kind/bug"}}
	c, sink := scannerVerdictClient(t, s)
	ctx := context.Background()

	var resp IssueResponse
	handled, err := c.relayScannerIssueVerdict(ctx, IssueRequest{Kind: "comment", Repo: "o/r", Number: 18421, Agent: "scanner"}, scannerNotStartable+testScannerFooter, &resp)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v, want handled", handled, err)
	}
	if resp.AlreadyExisted || s.creates != 1 {
		t.Fatalf("already_existed=%v creates=%d, want one new verdict comment", resp.AlreadyExisted, s.creates)
	}
	if body, _ := s.comments[0]["body"].(string); !strings.HasPrefix(body, ScannerVerdictMarker+"\n") || !strings.Contains(body, "not startable by an agent") {
		t.Fatalf("verdict comment = %q, want the marker and the scanner's text", body)
	}
	labels := s.currentLabels()
	if !hasIssueNeedsHumanLabel(labels) || labelPresent(labels, helpWantedLabel) {
		t.Fatalf("labels = %v, want needs-human on and help wanted off", labels)
	}
	if len(sink.Facts) != 1 || sink.Facts[0].Method != "Parked" {
		t.Fatalf("facts = %+v, want one Parked", sink.Facts)
	}

	// The next pass skips it.
	if got := hardSuppressIssueLabel(labels); got != issueNeedsHumanLabel {
		t.Fatalf("enumeration hard-suppress = %q, want %q", got, issueNeedsHumanLabel)
	}
	if !issueHeldByNeedsHuman(IssueStageInput{Labels: labels}) {
		t.Fatal("stage derivation does not hold the parked issue on a person")
	}
	if reason, err := c.IssueClaimBlockReason(ctx, "o/r", 18421); err != nil || reason != issueNeedsHumanLabel {
		t.Fatalf("claim gate = %q, %v; want %q", reason, err, issueNeedsHumanLabel)
	}
}

// The #18636 shape: the scanner triages the same issue pass after pass with
// the same "not a code defect" verdict. Through the relay the repeat posts
// nothing and completes with already_existed; a human reply re-arms it.
func TestScannerIssueVerdictRepeatSuppressed(t *testing.T) {
	s := &scannerVerdictServer{number: 18636, state: "open", labels: []string{"stage/010-actionable", helpWantedLabel}}
	c, _ := scannerVerdictClient(t, s)
	c.issueAuthz = func(agent string, uid int, kind string) error { return nil }
	dir := withIssueDir(t)
	verdict := "Triage (scanner): a single-hive configuration condition, not a code defect. Remedy: set governor.repos on that hive." + testScannerFooter

	var last string
	relay := func() {
		t.Helper()
		path, err := WriteIssueRequest(dir, IssueRequest{Kind: "comment", Repo: "o/r", Number: 18636, Body: verdict, Agent: "scanner"})
		if err != nil {
			t.Fatal(err)
		}
		c.ProcessIssueRequestsOnce(context.Background())
		last = path
	}

	relay()
	relay()
	if s.creates != 1 {
		t.Fatalf("creates=%d after a repeated identical triage, want 1", s.creates)
	}
	if resp := readIssueResultFile(t, last); !resp.OK || !resp.AlreadyExisted {
		t.Fatalf("repeat result = %+v, want ok with already_existed", resp)
	}

	s.addHumanComment("I changed the config; can you look again?")
	relay()
	if s.creates != 2 {
		t.Fatalf("creates=%d after a human reply, want the verdict posted again", s.creates)
	}
}

func TestScannerIssueVerdictPassThrough(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		number int
		state  string
		isPR   bool
		agent  string
		body   string
		block  int
	}{
		{"other lane", 7, "open", false, "reviewer", scannerNotStartable, 0},
		{"not a verdict", 7, "open", false, "scanner", "Repro confirmed on v5; the bug is in the claim gate.", 0},
		{"starts now", 7, "open", false, "scanner", "Not a defect in the gate itself, but starting on this now.", 0},
		{"pull request", 7, "open", true, "scanner", scannerNotStartable, 0},
		{"closed issue", 7, "closed", false, "scanner", scannerNotStartable, 0},
		{"missing issue", 8, "open", false, "scanner", scannerNotStartable, 0},
		{"blocked-on-PR park", 7, "open", false, "scanner", scannerNotStartable, 5},
		{"dedicated hive marker", 7, "open", false, "scanner", "needs-human: a secret\n<!-- hive:blocker kind=secret -->", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &scannerVerdictServer{number: tc.number, state: tc.state, isPR: tc.isPR}
			c, _ := scannerVerdictClient(t, s)
			var resp IssueResponse
			handled, err := c.relayScannerIssueVerdict(ctx, IssueRequest{Kind: "comment", Repo: "o/r", Number: 7, Agent: tc.agent, BlockedOnPR: tc.block}, tc.body+testScannerFooter, &resp)
			if handled || err != nil {
				t.Fatalf("handled=%v err=%v, want pass-through", handled, err)
			}
			if s.creates != 0 || hasIssueNeedsHumanLabel(s.currentLabels()) {
				t.Fatalf("pass-through wrote: creates=%d labels=%v", s.creates, s.currentLabels())
			}
		})
	}
}

func TestScannerIssueVerdictMatcher(t *testing.T) {
	for body, want := range map[string]bool{
		scannerNotStartable: true,
		"Scanner: not started / not startable by an agent (needs a Mac and a Zoom call).":    true,
		"Triage (scanner): not a code defect; the remedy is a config change.":                true,
		"This is a single-hive configuration condition, not a code defect.":                  true,
		"needs-human: the fix needs repository admin rights.":                                true,
		"This is not agent work: it needs a maintainer's subscription login.":                true,
		"Taking this out of the agent work queue until the vendor answers.":                  true,
		"Verified still open on v5.298.1; root cause is in claims.go.":                       false,
		"needs-decision (scanner): which of the two layouts do we want?":                     false,
		"Deferred this cycle: picks up once #40 merges.":                                     false,
		"Not a defect in the gate, but starting on this now.":                                false,
		"Quoted log:\n```\nnot startable by an agent\n```\nRoot cause is in the claim gate.": false,
	} {
		if got := scannerIssueVerdict(body + testScannerFooter); got != want {
			t.Errorf("scannerIssueVerdict(%q) = %v, want %v", body, got, want)
		}
	}
}
