package github

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func legacySweepIssue(number int, title, body, login, userType, stateReason string) map[string]any {
	return map[string]any{
		"number": number, "title": title, "body": body, "state": "open", "state_reason": stateReason,
		"html_url": "https://github.com/hivecommons/hive/issues/" + strconv.Itoa(number),
		"user":     map[string]any{"login": login, "type": userType},
	}
}

// The governor sweep closes bot-filed standalone "Fleet report:" issues
// (retired by #13470) with a pointer at the self-report tracker, and leaves
// alone anything a person wrote or reopened (hivecommons/hive#13567).
func TestSweepLegacyFleetReportIssuesClosesBotStandaloneOnly(t *testing.T) {
	var mu sync.Mutex
	var comments, closes []string
	var searchQ string
	issues := map[int]map[string]any{
		1: legacySweepIssue(1, "Fleet report: agent-runtime blocks Green-CI streak [3669d5d395e864a651596cae]", "<!-- hive-fleet-fingerprint:3669d5d395e864a651596cae -->\nbody", "hivecommons-hive[bot]", "Bot", ""),
		2: legacySweepIssue(2, "Fleet report: merge hive-code defect [abc]", "<!-- hive-fleet-fingerprint:abc -->\nbody", "hivecommons-hive[bot]", "Bot", "reopened"),
		3: legacySweepIssue(3, "Fleet report: something a person wrote", "no marker here", "alice", "User", ""),
		4: legacySweepIssue(4, "Fleet report: human with marker pasted", "<!-- hive-fleet-fingerprint:abc -->", "alice", "User", ""),
		5: legacySweepIssue(5, "[selfreport] agent_stall: scanner frozen", "<!-- hive-fleet-fingerprint:def -->", "hivecommons-hive[bot]", "Bot", ""),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/search/issues":
			searchQ = r.URL.Query().Get("q")
			items := []map[string]any{}
			for i := 1; i <= 5; i++ {
				items = append(items, issues[i])
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(items), "items": items})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			mu.Lock()
			comments = append(comments, r.URL.Path+" "+payload.Body)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/hivecommons/hive/issues/"):
			n := int(r.URL.Path[len(r.URL.Path)-1] - '0')
			_ = json.NewEncoder(w).Encode(issues[n])
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/hivecommons/hive/issues/"):
			var payload struct {
				State       string `json:"state"`
				StateReason string `json:"state_reason"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			mu.Lock()
			closes = append(closes, r.URL.Path+" "+payload.State+"/"+payload.StateReason)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 1, "state": "closed"})
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/labels/"):
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/collaborators/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"permission": "read"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "hivecommons", nil, slog.Default())
	var events []LegacyFleetReportSweepEvent
	res, err := c.SweepLegacyFleetReportIssues(context.Background(), LegacyFleetReportSweepOptions{Audit: func(e LegacyFleetReportSweepEvent) { events = append(events, e) }})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(searchQ, "repo:hivecommons/hive") || !strings.Contains(searchQ, `"Fleet report:"`) || !strings.Contains(searchQ, "is:open") {
		t.Fatalf("search query = %q", searchQ)
	}
	if res.Scanned != 5 || res.Skipped != 4 || len(res.Closed) != 1 || len(res.Errors) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if len(closes) != 1 || closes[0] != "/repos/hivecommons/hive/issues/1 closed/not_planned" {
		t.Fatalf("closes = %v", closes)
	}
	if len(comments) != 1 || !strings.HasPrefix(comments[0], "/repos/hivecommons/hive/issues/1/comments "+legacyFleetReportCloseMarker) || !strings.Contains(comments[0], "#13470") || !strings.Contains(comments[0], "[selfreport]") {
		t.Fatalf("comments = %v", comments)
	}
	if len(events) != 1 || events[0].Number != 1 || events[0].Author != "hivecommons-hive[bot]" || events[0].Repo != fleetReportRepo {
		t.Fatalf("events = %+v", events)
	}
}

func TestSweepLegacyFleetReportIssuesCapsClosesPerPass(t *testing.T) {
	var mu sync.Mutex
	var closes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/search/issues":
			items := []map[string]any{}
			for i := 1; i <= 3; i++ {
				items = append(items, legacySweepIssue(i, "Fleet report: x blocks Y [fp]", "<!-- hive-fleet-fingerprint:fp -->", "hive[bot]", "Bot", ""))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/hivecommons/hive/issues/"):
			_ = json.NewEncoder(w).Encode(legacySweepIssue(1, "Fleet report: x blocks Y [fp]", "<!-- hive-fleet-fingerprint:fp -->", "hive[bot]", "Bot", ""))
		case r.Method == http.MethodPatch:
			mu.Lock()
			closes++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 1, "state": "closed"})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNotFound)
		default:
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "hivecommons", nil, slog.Default())
	res, err := c.SweepLegacyFleetReportIssues(context.Background(), LegacyFleetReportSweepOptions{MaxCloses: 2})
	if err != nil {
		t.Fatal(err)
	}
	if closes != 2 || len(res.Closed) != 2 || res.Skipped != 1 || res.Scanned != 3 {
		t.Fatalf("closes=%d result=%+v", closes, res)
	}
	if _, err := (*Client)(nil).SweepLegacyFleetReportIssues(context.Background(), LegacyFleetReportSweepOptions{}); err == nil {
		t.Fatal("nil client must error")
	}
}

func TestLegacyFleetReportIssueEligible(t *testing.T) {
	mk := func(title, body, login, typ, reason string) *gh.Issue {
		return &gh.Issue{Title: gh.Ptr(title), Body: gh.Ptr(body), StateReason: gh.Ptr(reason), User: &gh.User{Login: gh.Ptr(login), Type: gh.Ptr(typ)}}
	}
	cases := []struct {
		name  string
		issue *gh.Issue
		want  bool
	}{
		{"bot standalone", mk("Fleet report: a blocks B [fp]", "<!-- hive-fleet-fingerprint:fp -->", "hive[bot]", "Bot", ""), true},
		{"bot login suffix without type", mk("fleet report: a hive-code defect [fp]", "<!-- hive-fleet-fingerprint:fp -->", "x[bot]", "", ""), true},
		{"reopened", mk("Fleet report: a blocks B [fp]", "<!-- hive-fleet-fingerprint:fp -->", "hive[bot]", "Bot", "reopened"), false},
		{"human", mk("Fleet report: a blocks B [fp]", "<!-- hive-fleet-fingerprint:fp -->", "alice", "User", ""), false},
		{"no marker", mk("Fleet report: a blocks B [fp]", "body", "hive[bot]", "Bot", ""), false},
		{"tracker title", mk("[selfreport] acmm-shortfall: a blocks B", "<!-- hive-fleet-fingerprint:fp -->", "hive[bot]", "Bot", ""), false},
		{"already swept", mk("Fleet report: a blocks B [fp]", "<!-- hive-fleet-fingerprint:fp -->"+legacyFleetReportCloseMarker, "hive[bot]", "Bot", ""), false},
		{"pull request", &gh.Issue{Title: gh.Ptr("Fleet report: x"), PullRequestLinks: &gh.PullRequestLinks{}}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if _, got := legacyFleetReportIssueEligible(tc.issue); got != tc.want {
			t.Errorf("%s: eligible = %v, want %v", tc.name, got, tc.want)
		}
	}
}
