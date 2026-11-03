package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/writeback"
)

type dupFixPR struct {
	Number    int
	Body      string
	Author    string
	Human     bool // carries author/human
	CreatedAt time.Time
	UpdatedAt time.Time
	Checks    string // "green", "red", "pending", "" (none)
	Comments  []string
	Labels    []string
}

type dupFixObs struct {
	mu       sync.Mutex
	comments map[int][]string
	labels   map[int][]string
	closed   []int
	pulls    int
}

func dupFixServer(t *testing.T, prs []dupFixPR, issueState string) (*httptest.Server, *dupFixObs) {
	t.Helper()
	obs := &dupFixObs{comments: map[int][]string{}, labels: map[int][]string{}}
	byNum := map[int]dupFixPR{}
	for _, pr := range prs {
		byNum[pr.Number] = pr
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		obs.mu.Lock()
		obs.pulls++
		obs.mu.Unlock()
		out := []map[string]any{}
		for _, pr := range prs {
			labels := []map[string]any{}
			if pr.Human {
				labels = append(labels, map[string]any{"name": duplicateFixHumanLabel})
			}
			for _, l := range pr.Labels {
				labels = append(labels, map[string]any{"name": l})
			}
			userType := "User"
			if strings.HasSuffix(pr.Author, "[bot]") {
				userType = "Bot"
			}
			out = append(out, map[string]any{
				"number": pr.Number, "state": "open", "title": fmt.Sprintf("fix %d", pr.Number), "body": pr.Body,
				"html_url":   fmt.Sprintf("https://github.com/o/r/pull/%d", pr.Number),
				"created_at": pr.CreatedAt.UTC().Format(time.RFC3339),
				"updated_at": pr.UpdatedAt.UTC().Format(time.RFC3339),
				"user":       map[string]any{"login": pr.Author, "type": userType},
				"head":       map[string]any{"sha": fmt.Sprintf("sha%d", pr.Number)},
				"labels":     labels,
			})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/repos/o/r/issues/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/repos/o/r/issues/")
		parts := strings.Split(rest, "/")
		var n int
		fmt.Sscanf(parts[0], "%d", &n)
		switch {
		case len(parts) == 1 && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "state": issueState})
		case len(parts) == 1 && r.Method == http.MethodPatch:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["state"] == "closed" {
				obs.mu.Lock()
				obs.closed = append(obs.closed, n)
				obs.mu.Unlock()
			}
			_, _ = io.WriteString(w, `{}`)
		case parts[1] == "comments" && r.Method == http.MethodGet:
			out := []map[string]any{}
			obs.mu.Lock()
			existing := append([]string{}, byNum[n].Comments...)
			existing = append(existing, obs.comments[n]...)
			obs.mu.Unlock()
			for i, body := range existing {
				out = append(out, map[string]any{"id": i + 1, "body": body})
			}
			_ = json.NewEncoder(w).Encode(out)
		case parts[1] == "comments" && r.Method == http.MethodPost:
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			obs.mu.Lock()
			obs.comments[n] = append(obs.comments[n], body.Body)
			obs.mu.Unlock()
			_, _ = io.WriteString(w, `{"id":99}`)
		case parts[1] == "labels" && r.Method == http.MethodPost:
			var names []string
			_ = json.NewDecoder(r.Body).Decode(&names)
			obs.mu.Lock()
			obs.labels[n] = append(obs.labels[n], names...)
			obs.mu.Unlock()
			_, _ = io.WriteString(w, `[]`)
		default:
			t.Errorf("unexpected issues request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc("/repos/o/r/commits/", func(w http.ResponseWriter, r *http.Request) {
		sha := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/o/r/commits/"), "/check-runs")
		var n int
		fmt.Sscanf(sha, "sha%d", &n)
		runs := []map[string]any{}
		switch byNum[n].Checks {
		case "green":
			runs = append(runs, map[string]any{"id": 1, "name": "build", "status": "completed", "conclusion": "success"})
		case "red":
			runs = append(runs, map[string]any{"id": 1, "name": "build", "status": "completed", "conclusion": "failure"})
		case "pending":
			runs = append(runs, map[string]any{"id": 1, "name": "build", "status": "in_progress"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "check_runs": runs})
	})
	mux.HandleFunc("/repos/o/r/labels/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"name":"x"}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	return httptest.NewServer(mux), obs
}

var dupT0 = time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC)

func dupNow() time.Time { return dupT0.Add(2 * time.Hour) }

func TestDecideDuplicateFix(t *testing.T) {
	for _, tc := range []struct {
		name  string
		facts DuplicateFixFacts
		want  DuplicateFixVerdict
	}{
		{"earlier green, later green", DuplicateFixFacts{EarlierChecks: DuplicateFixChecksGreen, LaterChecks: DuplicateFixChecksGreen}, DuplicateFixVerdictClose},
		{"earlier pending, later green", DuplicateFixFacts{EarlierChecks: DuplicateFixChecksPending, LaterChecks: DuplicateFixChecksGreen}, DuplicateFixVerdictClose},
		{"earlier red, later pending", DuplicateFixFacts{EarlierChecks: DuplicateFixChecksRed, LaterChecks: DuplicateFixChecksPending}, DuplicateFixVerdictClose},
		{"earlier red, later green", DuplicateFixFacts{EarlierChecks: DuplicateFixChecksRed, LaterChecks: DuplicateFixChecksGreen}, DuplicateFixVerdictEscalate},
		{"earlier stale, later green", DuplicateFixFacts{EarlierChecks: DuplicateFixChecksGreen, EarlierStale: true, LaterChecks: DuplicateFixChecksGreen}, DuplicateFixVerdictEscalate},
		{"human later", DuplicateFixFacts{EarlierChecks: DuplicateFixChecksGreen, LaterChecks: DuplicateFixChecksGreen, LaterHuman: true}, DuplicateFixVerdictCommentOnly},
		{"human later, earlier red", DuplicateFixFacts{EarlierChecks: DuplicateFixChecksRed, LaterChecks: DuplicateFixChecksGreen, LaterHuman: true}, DuplicateFixVerdictEscalate},
		{"relay later", DuplicateFixFacts{EarlierChecks: DuplicateFixChecksGreen, LaterChecks: DuplicateFixChecksRed, LaterRelay: true}, DuplicateFixVerdictCommentOnly},
		{"relay later, earlier red", DuplicateFixFacts{EarlierChecks: DuplicateFixChecksRed, LaterChecks: DuplicateFixChecksGreen, LaterRelay: true}, DuplicateFixVerdictEscalate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecideDuplicateFix(tc.facts); got != tc.want {
				t.Fatalf("verdict = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSweepDuplicateFixPRs_EarlierGreenWinsLaterClosedOnce(t *testing.T) {
	srv, obs := dupFixServer(t, []dupFixPR{
		{Number: 14775, Body: "Fixes #14766", Author: "clubanderson", Human: true, CreatedAt: dupT0, UpdatedAt: dupT0, Checks: "green"},
		// A relay contributor's agent run (author/clanker) under a person's login.
		{Number: 14780, Body: "Fixes #14766", Author: "thatguywob", Labels: []string{"author/clanker"}, CreatedAt: dupT0.Add(9 * time.Minute), UpdatedAt: dupT0.Add(9 * time.Minute), Checks: "green"},
		{Number: 14771, Body: "Fixes #14763", Author: "clubanderson", Human: true, CreatedAt: dupT0, UpdatedAt: dupT0, Checks: "green"},
		{Number: 14776, Body: "Fixes #14763", Author: "hive-app[bot]", CreatedAt: dupT0.Add(2 * time.Minute), UpdatedAt: dupT0.Add(2 * time.Minute), Checks: "pending"},
		{Number: 14790, Body: "Fixes #14700", Author: "hive-app[bot]", CreatedAt: dupT0, UpdatedAt: dupT0, Checks: "green"},
	}, "open")
	defer srv.Close()
	c, rec := newSupersessionTestClient(t, srv, "o", "r")

	var audited []DuplicateFixEvent
	res, err := c.SweepDuplicateFixPRs(context.Background(), DuplicateFixSweepOptions{Now: dupNow, Audit: func(e DuplicateFixEvent) { audited = append(audited, e) }})
	if err != nil {
		t.Fatalf("SweepDuplicateFixPRs: %v", err)
	}
	if res.Pairs != 2 || len(res.Closed) != 2 || len(res.Commented) != 0 || len(res.Escalated) != 0 {
		t.Fatalf("result = %+v, want two pairs both closed", res)
	}
	if len(obs.closed) != 2 || obs.closed[0] != 14776 || obs.closed[1] != 14780 {
		t.Fatalf("closed = %v, want [14776 14780]", obs.closed)
	}
	want := "Superseded by #14775 (earlier fix for #14766). Closing as duplicate."
	if cs := obs.comments[14780]; len(cs) != 1 || !strings.Contains(cs[0], want) {
		t.Fatalf("comments on 14780 = %v, want exactly one: %q", cs, want)
	}
	if got := obs.labels[14780]; len(got) != 1 || got[0] != SupersededLabel {
		t.Fatalf("labels on 14780 = %v, want [%s]", got, SupersededLabel)
	}
	if len(obs.comments[14775])+len(obs.comments[14771])+len(obs.comments[14790]) != 0 {
		t.Fatal("keepers and the lone PR must be untouched")
	}
	if len(parkedPRs(rec)) != 0 {
		t.Fatalf("parked = %v, want none", parkedPRs(rec))
	}
	for _, e := range audited {
		if e.Action != DuplicateFixActionClosed || !strings.HasPrefix(e.Reason, "duplicate-of:") || e.KeeperPR == 0 {
			t.Fatalf("audit event %+v lacks the duplicate-of:<keeper> reason", e)
		}
	}
	if audited[1].Reason != "duplicate-of:14775" || audited[1].Issue != 14766 {
		t.Fatalf("audit = %+v", audited[1])
	}
	if res.Actions() != 2 {
		t.Fatalf("Actions = %d", res.Actions())
	}

	// Second sweep: the marker comment is already there, nothing is written.
	res, err = c.SweepDuplicateFixPRs(context.Background(), DuplicateFixSweepOptions{Now: dupNow})
	if err != nil {
		t.Fatal(err)
	}
	if res.Actions() != 0 || res.Skipped != 2 || len(obs.closed) != 2 || len(obs.comments[14780]) != 1 {
		t.Fatalf("second sweep = %+v closed=%v comments=%v, want idempotent", res, obs.closed, obs.comments[14780])
	}
}

func TestSweepDuplicateFixPRs_EarlierRedLaterGreenEscalates(t *testing.T) {
	srv, obs := dupFixServer(t, []dupFixPR{
		{Number: 10, Body: "Fixes #1", Author: "hive-app[bot]", CreatedAt: dupT0, UpdatedAt: dupT0, Checks: "red"},
		{Number: 20, Body: "Closes #1", Author: "hive-app[bot]", CreatedAt: dupT0.Add(time.Hour), UpdatedAt: dupT0.Add(time.Hour), Checks: "green"},
	}, "open")
	defer srv.Close()
	c, rec := newSupersessionTestClient(t, srv, "o", "r")
	res, err := c.SweepDuplicateFixPRs(context.Background(), DuplicateFixSweepOptions{Now: dupNow})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Escalated) != 1 || res.Escalated[0].Number != 20 || res.Escalated[0].Reason != "earlier-red-later-green" {
		t.Fatalf("escalated = %+v", res.Escalated)
	}
	if len(obs.closed) != 0 {
		t.Fatalf("closed = %v, want none when the owner must pick", obs.closed)
	}
	if got := parkedPRs(rec); len(got) != 1 || got[0] != 20 {
		t.Fatalf("parked = %v, want [20]", got)
	}
	if cs := obs.comments[20]; len(cs) != 1 || !strings.Contains(cs[0], "#10 opened first but its checks are red") {
		t.Fatalf("comments on 20 = %v", cs)
	}
}

func TestSweepDuplicateFixPRs_EarlierStaleLaterGreenEscalates(t *testing.T) {
	stale := dupNow().Add(-DuplicateFixStaleAfter - time.Minute)
	srv, obs := dupFixServer(t, []dupFixPR{
		{Number: 10, Body: "Fixes #1", Author: "hive-app[bot]", CreatedAt: stale, UpdatedAt: stale, Checks: "green"},
		{Number: 20, Body: "Fixes #1", Author: "hive-app[bot]", CreatedAt: dupT0, UpdatedAt: dupT0, Checks: "green"},
	}, "open")
	defer srv.Close()
	c, rec := newSupersessionTestClient(t, srv, "o", "r")
	res, err := c.SweepDuplicateFixPRs(context.Background(), DuplicateFixSweepOptions{Now: dupNow})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Escalated) != 1 || res.Escalated[0].Reason != "earlier-stale-later-green" || len(obs.closed) != 0 || len(parkedPRs(rec)) != 1 {
		t.Fatalf("res=%+v closed=%v parked=%v", res, obs.closed, parkedPRs(rec))
	}
	if cs := obs.comments[20]; len(cs) != 1 || !strings.Contains(cs[0], "no update for over 24h") {
		t.Fatalf("comments on 20 = %v", cs)
	}
}

func TestSweepDuplicateFixPRs_HumanLaterPRCommentedNotClosed(t *testing.T) {
	srv, obs := dupFixServer(t, []dupFixPR{
		{Number: 10, Body: "Fixes #1", Author: "hive-app[bot]", CreatedAt: dupT0, UpdatedAt: dupT0, Checks: "green"},
		{Number: 20, Body: "Fixes #1", Author: "alice", Human: true, CreatedAt: dupT0.Add(time.Hour), UpdatedAt: dupT0.Add(time.Hour), Checks: "green"},
		// No author/human label but a person's login: still a person.
		{Number: 30, Body: "Fixes #1", Author: "bob", CreatedAt: dupT0.Add(2 * time.Hour), UpdatedAt: dupT0.Add(2 * time.Hour), Checks: ""},
	}, "open")
	defer srv.Close()
	c, rec := newSupersessionTestClient(t, srv, "o", "r")
	res, err := c.SweepDuplicateFixPRs(context.Background(), DuplicateFixSweepOptions{Now: dupNow})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Commented) != 2 || len(res.Closed) != 0 || len(obs.closed) != 0 || len(parkedPRs(rec)) != 0 {
		t.Fatalf("res=%+v closed=%v parked=%v, want comment-only for both human PRs", res, obs.closed, parkedPRs(rec))
	}
	for _, n := range []int{20, 30} {
		cs := obs.comments[n]
		if len(cs) != 1 || !strings.Contains(cs[0], "Superseded by #10 (earlier fix for #1)") || !strings.Contains(cs[0], "Leaving open") {
			t.Fatalf("comments on %d = %v", n, cs)
		}
		if len(obs.labels[n]) != 0 {
			t.Fatalf("labels on %d = %v, want none (no park, no superseded)", n, obs.labels[n])
		}
	}
	if res.Commented[0].Reason != "duplicate-of:10" {
		t.Fatalf("reason = %q", res.Commented[0].Reason)
	}
}

func TestSweepDuplicateFixPRs_SkipsClosedIssueHeldPRAndRespectsMaxActions(t *testing.T) {
	t.Run("closed issue", func(t *testing.T) {
		srv, obs := dupFixServer(t, []dupFixPR{
			{Number: 10, Body: "Fixes #1", Author: "hive-app[bot]", CreatedAt: dupT0, UpdatedAt: dupT0, Checks: "green"},
			{Number: 20, Body: "Fixes #1", Author: "hive-app[bot]", CreatedAt: dupT0.Add(time.Hour), UpdatedAt: dupT0.Add(time.Hour), Checks: "green"},
		}, "closed")
		defer srv.Close()
		c, _ := newSupersessionTestClient(t, srv, "o", "r")
		res, err := c.SweepDuplicateFixPRs(context.Background(), DuplicateFixSweepOptions{Now: dupNow})
		if err != nil || res.Actions() != 0 || res.Skipped != 1 || len(obs.closed) != 0 {
			t.Fatalf("res=%+v err=%v closed=%v, want the supersession sweep's case left alone", res, err, obs.closed)
		}
	})
	t.Run("held later PR", func(t *testing.T) {
		srv, obs := dupFixServer(t, []dupFixPR{
			{Number: 10, Body: "Fixes #1", Author: "hive-app[bot]", CreatedAt: dupT0, UpdatedAt: dupT0, Checks: "green"},
			{Number: 20, Body: "Fixes #1", Author: "hive-app[bot]", Labels: []string{"hold"}, CreatedAt: dupT0.Add(time.Hour), UpdatedAt: dupT0.Add(time.Hour), Checks: "green"},
		}, "open")
		defer srv.Close()
		c, _ := newSupersessionTestClient(t, srv, "o", "r")
		res, err := c.SweepDuplicateFixPRs(context.Background(), DuplicateFixSweepOptions{Now: dupNow})
		if err != nil || res.Actions() != 0 || len(obs.closed) != 0 || len(obs.comments[20]) != 0 {
			t.Fatalf("res=%+v err=%v, want an operator hold honoured", res, err)
		}
	})
	t.Run("max actions", func(t *testing.T) {
		srv, obs := dupFixServer(t, []dupFixPR{
			{Number: 10, Body: "Fixes #1", Author: "hive-app[bot]", CreatedAt: dupT0, UpdatedAt: dupT0, Checks: "green"},
			{Number: 20, Body: "Fixes #1", Author: "hive-app[bot]", CreatedAt: dupT0.Add(time.Hour), UpdatedAt: dupT0.Add(time.Hour), Checks: "green"},
			{Number: 11, Body: "Fixes #2", Author: "hive-app[bot]", CreatedAt: dupT0, UpdatedAt: dupT0, Checks: "green"},
			{Number: 21, Body: "Fixes #2", Author: "hive-app[bot]", CreatedAt: dupT0.Add(time.Hour), UpdatedAt: dupT0.Add(time.Hour), Checks: "green"},
		}, "open")
		defer srv.Close()
		c, _ := newSupersessionTestClient(t, srv, "o", "r")
		res, err := c.SweepDuplicateFixPRs(context.Background(), DuplicateFixSweepOptions{Now: dupNow, MaxActions: 1})
		if err != nil || res.Actions() != 1 || len(obs.closed) != 1 {
			t.Fatalf("res=%+v err=%v closed=%v, want one action", res, err, obs.closed)
		}
	})
	t.Run("nil client", func(t *testing.T) {
		var c *Client
		if _, err := c.SweepDuplicateFixPRs(context.Background(), DuplicateFixSweepOptions{}); err == nil {
			t.Fatal("nil client must error")
		}
		var r *DuplicateFixSweepResult
		if r.Actions() != 0 {
			t.Fatal("nil result Actions must be 0")
		}
	})
}

func TestSweepDuplicateFixPRs_ListingErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c, _ := newSupersessionTestClient(t, srv, "o", "r")
	if _, err := c.SweepDuplicateFixPRs(context.Background(), DuplicateFixSweepOptions{}); err == nil {
		t.Fatal("want the listing error surfaced")
	}
}

// hivecommons/hive#15469: a person filed #15469, hive dispatched its own
// relay contributor within seconds, and the person's PR (#15470) landed
// 45 s later; the relay's PR (#15473) followed. The relay's login carries
// no author/* label, so the sweep read it as a person's and the write-back
// gate (L4, no opt-in label) refused the notice — both PRs rode the merge
// train. With the relay registry installed the later PR is the hive's own
// for the gate, gets exactly one superseded notice, and is left open
// (parked by the stage engine, never closed by this sweep).
func TestSweepDuplicateFixPRs_RelayLaterPRCommentedThroughGateNotClosed(t *testing.T) {
	srv, obs := dupFixServer(t, []dupFixPR{
		{Number: 15470, Body: "Fixes #15469", Author: "clubanderson", CreatedAt: dupT0, UpdatedAt: dupT0, Checks: "pending"},
		{Number: 15473, Body: "Fixes #15469", Author: "odttlnt", CreatedAt: dupT0.Add(8 * time.Minute), UpdatedAt: dupT0.Add(8 * time.Minute), Checks: "red"},
	}, "open")
	defer srv.Close()
	c, rec := newSupersessionTestClient(t, srv, "o", "r")
	c.SetRelayKnownFunc(func(login string) bool { return login == "odttlnt" })
	c.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{
		LevelFor:   func(string) int { return 4 },
		RepoListed: func(string) bool { return true },
		Now:        dupNow,
	}, filepath.Join(t.TempDir(), "writeback.json")))

	run := func() *DuplicateFixSweepResult {
		t.Helper()
		res, err := c.SweepDuplicateFixPRs(context.Background(), DuplicateFixSweepOptions{Now: dupNow})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := run()
	if len(res.Commented) != 1 || len(res.Closed) != 0 || len(res.Escalated) != 0 || len(obs.closed) != 0 || len(parkedPRs(rec)) != 0 {
		t.Fatalf("res=%+v closed=%v parked=%v, want one comment and nothing else", res, obs.closed, parkedPRs(rec))
	}
	if res.Commented[0].Number != 15473 || res.Commented[0].KeeperPR != 15470 || res.Commented[0].Reason != "duplicate-of:15470" {
		t.Fatalf("event = %+v", res.Commented[0])
	}
	cs := obs.comments[15473]
	if len(cs) != 1 || !strings.Contains(cs[0], duplicateFixMarker) || !strings.Contains(cs[0], "Superseded by #15470 (earlier fix for #15469)") || !strings.Contains(cs[0], "held out of it") {
		t.Fatalf("comments on 15473 = %v", cs)
	}
	if len(obs.comments[15470]) != 0 || len(obs.labels[15473]) != 0 {
		t.Fatalf("keeper comments=%v later labels=%v, want none", obs.comments[15470], obs.labels[15473])
	}
	// A second pass finds the marker and writes nothing more.
	if res := run(); res.Actions() != 0 || len(obs.comments[15473]) != 1 {
		t.Fatalf("second sweep res=%+v comments=%v, want no further writes", res, obs.comments[15473])
	}
}

// The same pair without the relay registry: the later PR reads as a
// person's and an L4 repo without the opt-in label suppresses the notice —
// the pre-fix behaviour, pinned so the registry wiring stays load-bearing.
func TestSweepDuplicateFixPRs_UnknownLoginSuppressedByGate(t *testing.T) {
	srv, obs := dupFixServer(t, []dupFixPR{
		{Number: 15470, Body: "Fixes #15469", Author: "clubanderson", CreatedAt: dupT0, UpdatedAt: dupT0, Checks: "pending"},
		{Number: 15473, Body: "Fixes #15469", Author: "odttlnt", CreatedAt: dupT0.Add(8 * time.Minute), UpdatedAt: dupT0.Add(8 * time.Minute), Checks: "red"},
	}, "open")
	defer srv.Close()
	c, _ := newSupersessionTestClient(t, srv, "o", "r")
	c.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{
		LevelFor:   func(string) int { return 4 },
		RepoListed: func(string) bool { return true },
		Now:        dupNow,
	}, filepath.Join(t.TempDir(), "writeback.json")))
	var events []DuplicateFixEvent
	res, err := c.SweepDuplicateFixPRs(context.Background(), DuplicateFixSweepOptions{Now: dupNow, Audit: func(e DuplicateFixEvent) { events = append(events, e) }})
	if err != nil {
		t.Fatal(err)
	}
	if res.Actions() != 0 || res.Pairs != 1 || len(obs.comments[15473]) != 0 {
		t.Fatalf("res=%+v comments=%v, want the pair suppressed", res, obs.comments[15473])
	}
	if len(events) != 1 || events[0].Action != DuplicateFixActionSuppressed {
		t.Fatalf("events = %+v, want one suppressed", events)
	}
}
