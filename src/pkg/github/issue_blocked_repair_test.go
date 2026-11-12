package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/writeback"
)

// The blocked sweep turns a `blocked` the hive applied, naming nothing it
// waits on, into a needs-human ask. These tests run that repair through a
// real write-back gate. Asked as gate_comment, the gate refused it on every
// sweep for an issue older than the hive's write access (backfill off) and
// for one without the opt-in label, so the issue stayed blocked, in no inbox,
// for days. Asked as blocked_repair with the fact that the label is the
// hive's, neither check applies; a pause, the repo mode and the level still do.

const (
	blockedRepairTestRepo = "acme/widgets"
	blockedRepairIssue    = 80
	blockedRepairHeldLine = "needs-human ask held by writeback gate"
	// blockedRepairPerson is a maintainer's login: the issue's author, and
	// in some tests the one who applied `blocked`.
	blockedRepairPerson = "maintainer"
)

// blockedRepairFirstSeen is when the gate first saw acme/widgets: an issue
// created before it is one the backfill guard keeps the hive off.
var blockedRepairFirstSeen = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

const (
	blockedRepairOldIssue = "2026-01-15T09:00:00Z" // before blockedRepairFirstSeen
	blockedRepairNewIssue = "2026-07-01T09:00:00Z" // after it
)

// blockedRepairServer is acme/widgets with one open issue, #80. Its fields
// are the issue's state on GitHub and may be changed between sweeps.
type blockedRepairServer struct {
	mu    sync.Mutex
	calls []string
	// blocked: #80 carries `blocked` and is open (so the sweep lists it).
	blocked bool
	// actor applied `blocked`, per the issue's timeline.
	actor string
	// createdAt is the issue's created_at.
	createdAt string
	// marker: a comment on #80 names the open pull request #7 as its blocker.
	marker bool
	// blockedBy: GitHub reports an open native "blocked by" link.
	blockedBy bool
}

func (f *blockedRepairServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	const issues = "/repos/" + blockedRepairTestRepo + "/issues"
	const issue = issues + "/80"
	switch {
	case r.Method == "GET" && r.URL.Path == issues && strings.Contains(r.URL.RawQuery, "labels=blocked"):
		items := []map[string]any{}
		if f.blocked {
			items = append(items, f.issueLocked())
		}
		_ = json.NewEncoder(w).Encode(items)
	case r.Method == "GET" && r.URL.Path == issue:
		_ = json.NewEncoder(w).Encode(f.issueLocked())
	case r.Method == "GET" && r.URL.Path == issue+"/comments":
		if f.marker {
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 3, "body": BlockedOnMarker(blockedRepairTestRepo, 7) + "\nblocked on the shared fix"}})
			return
		}
		_, _ = io.WriteString(w, `[]`)
	case r.Method == "GET" && r.URL.Path == "/repos/"+blockedRepairTestRepo+"/pulls/7":
		_, _ = io.WriteString(w, `{"number":7,"state":"open"}`)
	case r.Method == "GET" && r.URL.Path == issue+"/timeline":
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"event": "labeled", "created_at": "2026-10-29T04:17:25Z",
			"label": map[string]any{"name": "blocked"}, "actor": map[string]any{"login": f.actor},
		}})
	case r.Method == "POST" && r.URL.Path == issue+"/labels":
		_, _ = io.WriteString(w, `[{"name":"needs-human"}]`)
	case r.Method == "DELETE" && r.URL.Path == issue+"/labels/blocked":
		f.blocked = false
		_, _ = io.WriteString(w, `[]`)
	case r.Method == "POST" && r.URL.Path == issue+"/comments":
		_, _ = io.WriteString(w, `{"id":9}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
	}
}

// issueLocked is #80 as GitHub returns it: opened by a person, with no
// review opt-in label.
func (f *blockedRepairServer) issueLocked() map[string]any {
	is := map[string]any{
		"number": blockedRepairIssue, "state": "open",
		"user":       map[string]any{"login": blockedRepairPerson},
		"created_at": f.createdAt,
		"labels":     []any{map[string]any{"name": "blocked"}},
		"body":       "Upgrade the client once the upstream API ships.",
	}
	if f.blockedBy {
		is["issue_dependencies_summary"] = map[string]any{"blocked_by": 1, "total_blocked_by": 1}
	}
	return is
}

// set changes the issue's state between sweeps.
func (f *blockedRepairServer) set(change func(*blockedRepairServer)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

// wrote reports the calls that change the issue.
func (f *blockedRepairServer) wrote() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "GET ") {
			out = append(out, c)
		}
	}
	return out
}

// blockedRepairPolicy is what the gate reads on each decision; a test
// changes it between sweeps (never during one).
type blockedRepairPolicy struct {
	level  int
	paused bool
	cfg    writeback.Config
}

// blockedRepairRig is a client over srv with a gate that first saw
// acme/widgets at blockedRepairFirstSeen, and the client's log.
func blockedRepairRig(t *testing.T, f *blockedRepairServer, p *blockedRepairPolicy) (*Client, *writeback.Gate, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	var logs bytes.Buffer
	c := NewClientForTest(srv.URL, "acme", []string{"widgets"}, slog.New(slog.NewTextHandler(&logs, nil)))
	c.appBotLogin = testBot
	gate := writeback.New(writeback.Config{}, writeback.Deps{
		LevelFor: func(string) int { return p.level },
		RepoPause: func(string) (writeback.RepoPauseInfo, bool) {
			return writeback.RepoPauseInfo{By: "owner", Reason: "freeze"}, p.paused
		},
		Config: func() writeback.Config { return p.cfg },
		Now:    func() time.Time { return blockedRepairFirstSeen },
	}, "")
	gate.ObserveRepo(blockedRepairTestRepo)
	c.SetWritebackGate(gate)
	// A sweep that reads a blocked-on comment remembers it process-wide.
	t.Cleanup(func() { forgetBlockedOnSeen(blockedRepairTestRepo, blockedRepairIssue) })
	return c, gate, &logs
}

func blockedRepairSweep(t *testing.T, c *Client) BlockedIssueResult {
	t.Helper()
	res, err := c.SweepBlockedIssues(context.Background(), BlockedSweepOptions{Repos: []string{blockedRepairTestRepo}})
	if err != nil {
		t.Fatalf("SweepBlockedIssues: %v", err)
	}
	if res.Errors != 0 {
		t.Fatalf("res = %+v, want no errors", res)
	}
	return res
}

// A hive-applied, marker-less `blocked` is repaired on an issue the gate
// would not let gate_comment touch: one older than the hive's write access
// with backfill off, and one without the opt-in label at a level where
// gate_comment needs it.
func TestBlockedRepairConvertsHiveBlockTheGateUsedToHold(t *testing.T) {
	cases := []struct {
		name      string
		createdAt string
		// gateComment is the reason the same issue is refused as gate_comment.
		gateComment string
	}{
		{"issue older than the hive's write access, backfill off", blockedRepairOldIssue, writeback.ReasonBackfill},
		{"issue without the opt-in label", blockedRepairNewIssue, writeback.ReasonNeedsLabel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &blockedRepairServer{blocked: true, actor: testBot, createdAt: tc.createdAt}
			c, gate, logs := blockedRepairRig(t, f, &blockedRepairPolicy{level: 6})

			created, err := time.Parse(time.RFC3339, tc.createdAt)
			if err != nil {
				t.Fatal(err)
			}
			item := writeback.Item{Number: blockedRepairIssue, Author: blockedRepairPerson, CreatedAt: created}
			if d := gate.Decide(blockedRepairTestRepo, item, writeback.KindGateComment); d.Allowed || d.Reason != tc.gateComment {
				t.Fatalf("gate_comment on this issue = %+v, want refused with %s (the hold this test is about)", d, tc.gateComment)
			}

			res := blockedRepairSweep(t, c)
			if res.AskedHuman != 1 {
				t.Fatalf("res = %+v, want the block turned into a needs-human ask\nlog:\n%s", res, logs.String())
			}
			joined := strings.Join(f.wrote(), "\n")
			want := []string{
				"POST /repos/acme/widgets/issues/80/labels",
				"DELETE /repos/acme/widgets/issues/80/labels/blocked",
				"POST /repos/acme/widgets/issues/80/comments",
			}
			if joined != strings.Join(want, "\n") {
				t.Fatalf("writes =\n%s\nwant needs-human added, then blocked removed, then the note:\n%s", joined, strings.Join(want, "\n"))
			}
			if held := gate.Recent(); len(held) != 0 {
				t.Fatalf("gate suppressed %+v, want nothing held", held)
			}
			if strings.Contains(logs.String(), blockedRepairHeldLine) {
				t.Fatalf("a repair that went through logged a hold:\n%s", logs.String())
			}
			// The issue is no longer blocked, so the next sweep has nothing to do.
			if res := blockedRepairSweep(t, c); res.Items != 0 || res.AskedHuman != 0 {
				t.Fatalf("second sweep res = %+v, want nothing left", res)
			}
		})
	}
}

// A person's marker-less `blocked` on the same old issue is theirs: nothing
// is written and the gate is not even asked.
func TestBlockedRepairLeavesAPersonsBlockAlone(t *testing.T) {
	f := &blockedRepairServer{blocked: true, actor: blockedRepairPerson, createdAt: blockedRepairOldIssue}
	c, gate, logs := blockedRepairRig(t, f, &blockedRepairPolicy{level: 6})
	for pass := 1; pass <= 2; pass++ {
		if res := blockedRepairSweep(t, c); res.Items != 1 || res.AskedHuman != 0 || res.Unblocked != 0 {
			t.Fatalf("pass %d: res = %+v, want the issue seen and left alone", pass, res)
		}
	}
	if w := f.wrote(); len(w) != 0 {
		t.Fatalf("writes on a person's block: %v", w)
	}
	if held := gate.Recent(); len(held) != 0 {
		t.Fatalf("the gate was asked about a person's block: %+v", held)
	}
	if strings.Contains(logs.String(), blockedRepairHeldLine) {
		t.Fatalf("a person's block logged a gate hold:\n%s", logs.String())
	}
}

// What the fact does not lift still refuses the repair: a paused repo, repo
// mode off, a level below the kind's floor and an unset level.
func TestBlockedRepairStillRefusedByPauseModeAndLevel(t *testing.T) {
	off := writeback.Config{PerRepo: map[string]writeback.RepoOverride{blockedRepairTestRepo: {Mode: writeback.RepoModeOff}}}
	three := 3
	raised := writeback.Config{Kinds: map[string]writeback.KindPolicyOverride{string(writeback.KindBlockedRepair): {MinLevel: &three}}}
	cases := []struct {
		name   string
		policy blockedRepairPolicy
		reason string
	}{
		{"paused repo", blockedRepairPolicy{level: 6, paused: true}, writeback.ReasonRepoPaused},
		{"repo mode off", blockedRepairPolicy{level: 6, cfg: off}, writeback.ReasonRepoOff},
		{"below the kind's minimum level", blockedRepairPolicy{level: 2, cfg: raised}, writeback.ReasonBelowLevel},
		{"level unset", blockedRepairPolicy{level: 0}, writeback.ReasonLevelUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &blockedRepairServer{blocked: true, actor: testBot, createdAt: blockedRepairOldIssue}
			policy := tc.policy
			c, gate, logs := blockedRepairRig(t, f, &policy)
			if res := blockedRepairSweep(t, c); res.AskedHuman != 0 || res.Unblocked != 0 {
				t.Fatalf("res = %+v, want nothing converted while the gate refuses", res)
			}
			if w := f.wrote(); len(w) != 0 {
				t.Fatalf("writes while the gate refuses: %v", w)
			}
			held := gate.Recent()
			if len(held) != 1 || held[0].Kind != writeback.KindBlockedRepair || held[0].Reason != tc.reason || held[0].Number != blockedRepairIssue {
				t.Fatalf("gate suppressed %+v, want one blocked_repair refusal with reason %s", held, tc.reason)
			}
			if out := logs.String(); !strings.Contains(out, blockedRepairHeldLine) || !strings.Contains(out, "issue=80") || !strings.Contains(out, "reason="+tc.reason) {
				t.Fatalf("hold not logged with reason %s:\n%s", tc.reason, out)
			}
		})
	}
}

// The sweep runs every few minutes. A hold is logged once per issue and
// reason, again when the reason changes, and again when the issue is held
// anew after it stopped being one the repair converts: it was repaired, it
// left the listing, it named a blocker, or a person took the label over.
func TestBlockedRepairHoldLoggedOncePerIssueAndReason(t *testing.T) {
	f := &blockedRepairServer{blocked: true, actor: testBot, createdAt: blockedRepairOldIssue}
	p := &blockedRepairPolicy{level: 6, paused: true}
	c, _, logs := blockedRepairRig(t, f, p)
	off := writeback.Config{PerRepo: map[string]writeback.RepoOverride{blockedRepairTestRepo: {Mode: writeback.RepoModeOff}}}

	sweeps := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			blockedRepairSweep(t, c)
		}
	}
	wantHeld := func(step string, lines int, lastReason string) {
		t.Helper()
		out := logs.String()
		if got := strings.Count(out, blockedRepairHeldLine); got != lines {
			t.Fatalf("%s: %d hold line(s), want %d:\n%s", step, got, lines, out)
		}
		if i := strings.LastIndex(out, blockedRepairHeldLine); i < 0 || !strings.Contains(out[i:], "reason="+lastReason) {
			t.Fatalf("%s: newest hold line does not say reason=%s:\n%s", step, lastReason, out)
		}
	}

	sweeps(3)
	wantHeld("same reason on three sweeps", 1, writeback.ReasonRepoPaused)

	p.paused, p.cfg = false, off
	sweeps(2)
	wantHeld("reason changed to repo mode off", 2, writeback.ReasonRepoOff)

	p.paused = true
	sweeps(2)
	wantHeld("reason changed back to the pause", 3, writeback.ReasonRepoPaused)

	// The repair goes through; a relay later blocks the issue again and the
	// repo is paused again.
	p.paused, p.cfg = false, writeback.Config{}
	if res := blockedRepairSweep(t, c); res.AskedHuman != 1 {
		t.Fatalf("res = %+v, want the repair to go through once the gate allows it\nlog:\n%s", res, logs.String())
	}
	wantHeld("repair went through", 3, writeback.ReasonRepoPaused)
	f.set(func(f *blockedRepairServer) { f.blocked = true })
	p.paused = true
	sweeps(2)
	wantHeld("held anew after a repair", 4, writeback.ReasonRepoPaused)

	// The issue leaves the listing (closed, or the label removed) and comes
	// back held for the same reason.
	f.set(func(f *blockedRepairServer) { f.blocked = false })
	sweeps(1)
	f.set(func(f *blockedRepairServer) { f.blocked = true })
	sweeps(2)
	wantHeld("held anew after leaving the listing", 5, writeback.ReasonRepoPaused)

	// A comment names a blocker, then is gone again.
	f.set(func(f *blockedRepairServer) { f.marker = true })
	sweeps(1)
	f.set(func(f *blockedRepairServer) { f.marker = false })
	sweeps(2)
	wantHeld("held anew after naming a blocker", 6, writeback.ReasonRepoPaused)

	// GitHub reports an open "blocked by" link, then it is gone again.
	f.set(func(f *blockedRepairServer) { f.blockedBy = true })
	sweeps(1)
	f.set(func(f *blockedRepairServer) { f.blockedBy = false })
	sweeps(2)
	wantHeld("held anew after a native blocked-by link", 7, writeback.ReasonRepoPaused)

	// A person re-applies the label, then the hive's bot does.
	f.set(func(f *blockedRepairServer) { f.actor = blockedRepairPerson })
	sweeps(1)
	f.set(func(f *blockedRepairServer) { f.actor = testBot })
	sweeps(2)
	wantHeld("held anew after a person's label", 8, writeback.ReasonRepoPaused)

	// Only the one repair wrote anything.
	if w := f.wrote(); len(w) != 3 {
		t.Fatalf("writes = %v, want the single repair's three", w)
	}
}

// A sweep of other repos does not forget this repo's hold, so the next sweep
// of this one does not log it again.
func TestBlockedRepairHoldKeptAcrossSweepOfOtherRepos(t *testing.T) {
	f := &blockedRepairServer{blocked: true, actor: testBot, createdAt: blockedRepairOldIssue}
	c, _, logs := blockedRepairRig(t, f, &blockedRepairPolicy{level: 6, paused: true})
	blockedRepairSweep(t, c)
	// acme/gadgets has no blocked issue; the server answers its listing 404,
	// which the sweep reports as an error, so nothing is forgotten.
	if _, err := c.SweepBlockedIssues(context.Background(), BlockedSweepOptions{Repos: []string{"acme/gadgets"}}); err == nil {
		t.Fatal("sweep of an unlistable repo returned no error")
	}
	// Nor does a clean listing of that other repo, with no blocked issue.
	c.retainBlockedRepairHeld([]string{"acme/gadgets"}, nil)
	blockedRepairSweep(t, c)
	if got := strings.Count(logs.String(), blockedRepairHeldLine); got != 1 {
		t.Fatalf("%d hold line(s) after a sweep of another repo, want 1:\n%s", got, logs.String())
	}
	// With no gate installed there is nothing to keep.
	c.SetWritebackGate(nil)
	c.retainBlockedRepairHeld([]string{blockedRepairTestRepo}, nil)
}
