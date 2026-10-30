package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/writeback"
)

// a1Fixture replays the 2026-10-28 a1-vllm-spyre incident: two project
// repos, torch-spyre (human maintainers, never opted in, 50 pre-existing
// PRs touching .github/workflows) and spyre-inference (hive users, PRs
// labelled hive-review). Every non-GET request is recorded so the test can
// assert on the exact GitHub write surface.
type a1Fixture struct {
	mu     sync.Mutex
	prs    map[string][]map[string]any // repo → PRs
	writes []string                    // "METHOD path"
}

func (f *a1Fixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := r.URL.Path
		if r.Method != http.MethodGet {
			f.writes = append(f.writes, r.Method+" "+p)
			if strings.HasSuffix(p, "/labels") {
				_ = json.NewEncoder(w).Encode([]map[string]any{{"name": "sentinel-alert"}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
			return
		}
		for repo, prs := range f.prs {
			base := "/repos/" + repo
			switch {
			case p == base+"/pulls":
				_ = json.NewEncoder(w).Encode(prs)
				return
			case strings.HasPrefix(p, base+"/pulls/") && strings.HasSuffix(p, "/files"):
				_ = json.NewEncoder(w).Encode([]map[string]any{{
					"filename": ".github/workflows/ci.yml", "status": "modified", "additions": 1, "deletions": 0,
					"patch": "@@ -1,2 +1,3 @@\n on: push\n+permissions: write-all\n",
				}})
				return
			case strings.HasPrefix(p, base+"/labels/"):
				_ = json.NewEncoder(w).Encode(map[string]any{"name": "sentinel-alert"})
				return
			case strings.HasPrefix(p, base+"/"):
				_ = json.NewEncoder(w).Encode([]map[string]any{})
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

func a1PR(num int, author, head string, created time.Time, labels ...string) map[string]any {
	pr := sentinelPR(num, author, head, labels...)
	pr["created_at"] = created.UTC().Format(time.RFC3339)
	return pr
}

func a1Gate(t *testing.T, cfg writeback.Config, levels map[string]int, logs *bytes.Buffer, now time.Time) *writeback.Gate {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return writeback.New(cfg, writeback.Deps{
		LevelFor:    func(repo string) int { return levels[repo] },
		OptInLabels: func(string) []string { return []string{"hive-review"} },
		RepoListed:  func(string) bool { return true },
		Logger:      logger,
		Now:         func() time.Time { return now },
	}, filepath.Join(t.TempDir(), "writeback.json"))
}

// TestWritebackA1ReplayTorchSpyreStaysSilent is the dry-run proof for the
// incident: at ACMM level 4 with torch-spyre not opted in, the sentinel
// sweep over 50 pre-existing workflow-touching PRs performs zero GitHub
// writes, records all 50 findings internally, and logs exactly one
// backfill summary line.
func TestWritebackA1ReplayTorchSpyreStaysSilent(t *testing.T) {
	const repo = "torch-spyre/torch-spyre"
	now := time.Date(2026, 10, 28, 14, 0, 0, 0, time.UTC)
	f := &a1Fixture{prs: map[string][]map[string]any{repo: {}}}
	for i := 1; i <= 50; i++ {
		f.prs[repo] = append(f.prs[repo], a1PR(i, fmt.Sprintf("maintainer%d", i), fmt.Sprintf("sha%03d", i), now.Add(-time.Duration(i)*24*time.Hour)))
	}
	srv := f.server(t)
	defer srv.Close()

	var logs bytes.Buffer
	gate := a1Gate(t, writeback.Config{}, map[string]int{repo: 4}, &logs, now)
	gate.ObserveRepo(repo) // the App install moment: first sight of the repo

	c := newTestClient(t, srv, "torch-spyre", []string{repo})
	c.SetWritebackGate(gate)

	var audited []SentinelSweepEvent
	res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(func(e SentinelSweepEvent) { audited = append(audited, e) }))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("expected zero GitHub writes, got %d: %v", len(f.writes), f.writes)
	}
	if res.Seen != 50 || len(res.Suppressed) != 50 || len(res.Flagged) != 0 || len(res.Notified) != 0 {
		t.Fatalf("seen=%d suppressed=%d flagged=%d notified=%d errors=%v", res.Seen, len(res.Suppressed), len(res.Flagged), len(res.Notified), res.Errors)
	}
	if len(audited) != 50 {
		t.Fatalf("expected 50 internal findings, got %d", len(audited))
	}
	for _, ev := range audited {
		if !ev.Suppressed || ev.SuppressedReason == "" || len(ev.Rules()) == 0 {
			t.Fatalf("finding should be suppressed with a reason and rules: %+v", ev)
		}
	}
	summary := "writeback: skipped 50 pre-existing items on " + repo + " (backfill off)"
	if got := strings.Count(logs.String(), summary); got != 1 {
		t.Fatalf("want exactly one summary line %q, got %d in:\n%s", summary, got, logs.String())
	}
	st := gate.Snapshot()
	if st.Repos[repo].PreExisting != 50 || len(st.Banners) != 1 || !strings.Contains(st.Banners[0].Message, repo) {
		t.Fatalf("status=%+v", st)
	}
	if got := len(c.WritebackSuppressedSince(now.Add(-time.Hour))); got != 50 {
		t.Fatalf("suppressed ring=%d", got)
	}

	// Second sweep: still silent, and the summary line is not repeated.
	if _, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(nil)); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if len(f.writes) != 0 || strings.Count(logs.String(), summary) != 1 {
		t.Fatalf("second sweep wrote %v / summary lines=%d", f.writes, strings.Count(logs.String(), summary))
	}
}

// TestWritebackA1ReplayOptedInRepoStillGetsSentinel is the other half of
// the proof: a spyre-inference PR carrying hive-review at level 5 still
// receives the sentinel comment and label, while an unlabelled PR in the
// same repo at the same level does not (L6 floor without the label).
func TestWritebackA1ReplayOptedInRepoStillGetsSentinel(t *testing.T) {
	const repo = "spyre/spyre-inference"
	now := time.Date(2026, 10, 28, 14, 0, 0, 0, time.UTC)
	f := &a1Fixture{prs: map[string][]map[string]any{repo: {
		a1PR(10, "hiveuser", "aaa", now.Add(time.Minute), "hive-review"),
		a1PR(11, "visitor", "bbb", now.Add(time.Minute)),
	}}}
	srv := f.server(t)
	defer srv.Close()

	var logs bytes.Buffer
	gate := a1Gate(t, writeback.Config{}, map[string]int{repo: 5}, &logs, now)
	gate.ObserveRepo(repo)
	c := newTestClient(t, srv, "spyre", []string{repo})
	c.SetWritebackGate(gate)

	res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Flagged) != 1 || res.Flagged[0].Number != 10 || len(res.Suppressed) != 1 || res.Suppressed[0].Number != 11 {
		t.Fatalf("flagged=%+v suppressed=%+v skipped=%d notified=%+v errors=%v writes=%v", res.Flagged, res.Suppressed, res.Skipped, res.Notified, res.Errors, f.writes)
	}
	var comment, label bool
	for _, w := range f.writes {
		if strings.Contains(w, "/issues/11/") {
			t.Fatalf("unlabelled PR 11 must not be written to: %v", f.writes)
		}
		comment = comment || w == "POST /repos/"+repo+"/issues/10/comments"
		label = label || w == "POST /repos/"+repo+"/issues/10/labels"
	}
	if !comment || !label {
		t.Fatalf("opted-in PR 10 should get comment+label, writes=%v", f.writes)
	}

	// The same PRs at level 6 with the per-kind override lowered to L5 for
	// unlabelled items: both get written.
	f.writes = nil
	five := 5
	cfg := writeback.Config{Kinds: map[string]writeback.KindPolicyOverride{
		string(writeback.KindSentinel): {UnlabeledMinLevel: &five},
	}}
	gate2 := a1Gate(t, cfg, map[string]int{repo: 5}, &logs, now)
	gate2.ObserveRepo(repo)
	c2 := newTestClient(t, srv, "spyre", []string{repo})
	c2.SetWritebackGate(gate2)
	res, err = c2.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Flagged) != 2 || len(res.Suppressed) != 0 {
		t.Fatalf("override: flagged=%d suppressed=%d", len(res.Flagged), len(res.Suppressed))
	}
}

// TestWritebackPerRepoIssuesOnlyBlocksSentinel covers the operator's
// torch-spyre override: mode issues-only keeps the deps agent's regression
// issues flowing while every PR write-back stays off, at any level.
func TestWritebackPerRepoIssuesOnlyBlocksSentinel(t *testing.T) {
	const repo = "torch-spyre/torch-spyre"
	now := time.Now()
	f := &a1Fixture{prs: map[string][]map[string]any{repo: {a1PR(1, "m", "aaa", now.Add(time.Minute), "hive-review")}}}
	srv := f.server(t)
	defer srv.Close()
	cfg := writeback.Config{PerRepo: map[string]writeback.RepoOverride{repo: {Mode: writeback.RepoModeIssuesOnly}}}
	gate := a1Gate(t, cfg, map[string]int{repo: 6}, &bytes.Buffer{}, now)
	c := newTestClient(t, srv, "torch-spyre", []string{repo})
	c.SetWritebackGate(gate)
	res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(f.writes) != 0 || len(res.Suppressed) != 1 {
		t.Fatalf("writes=%v suppressed=%d", f.writes, len(res.Suppressed))
	}
	d := gate.Allowed(repo, writeback.Item{Number: 0}, writeback.KindIssueFile)
	if !d.Allowed {
		t.Fatalf("issues-only must still allow issue filing: %+v", d)
	}
}

// TestWritebackGatedPathsMatrix is the table-driven check over every
// gated path's kind: what each sweep may write on a foreign PR without the
// opt-in label, on a foreign PR carrying it, and on a hive-authored PR,
// across ACMM levels 4, 5 and 6 with default config.
func TestWritebackGatedPathsMatrix(t *testing.T) {
	now := time.Now()
	type want struct{ unlabeled, labeled, own [3]bool } // index: L4, L5, L6
	cases := []struct {
		path string
		kind writeback.Kind
		want want
	}{
		{"sentinel sweep (comment + sentinel-alert label)", writeback.KindSentinel, want{[3]bool{false, false, true}, [3]bool{false, true, true}, [3]bool{true, true, true}}},
		{"hold-gate mergeability / drift notice", writeback.KindHoldGateNotice, want{[3]bool{false, false, false}, [3]bool{false, false, false}, [3]bool{true, true, true}}},
		{"stage/* labels on PRs", writeback.KindStageLabel, want{[3]bool{false, false, false}, [3]bool{false, true, true}, [3]bool{true, true, true}}},
		{"hive:stage-needs-decision comment", writeback.KindNeedsDecisionComment, want{[3]bool{false, false, false}, [3]bool{false, true, true}, [3]bool{true, true, true}}},
		{"supersession notice", writeback.KindSupersessionNotice, want{[3]bool{false, false, false}, [3]bool{false, true, true}, [3]bool{true, true, true}}},
		{"supersession auto-close", writeback.KindSupersessionClose, want{[3]bool{false, false, false}, [3]bool{false, false, true}, [3]bool{true, true, true}}},
		{"duplicate-fix notice", writeback.KindDuplicateFixNotice, want{[3]bool{false, false, false}, [3]bool{false, true, true}, [3]bool{true, true, true}}},
		{"reporter-trust hold", writeback.KindReporterTrustHold, want{[3]bool{false, false, false}, [3]bool{false, true, true}, [3]bool{true, true, true}}},
		{"covered-by-PR label", writeback.KindCoveredByPRLabel, want{[3]bool{false, false, false}, [3]bool{false, true, true}, [3]bool{true, true, true}}},
		{"contributor adopt", writeback.KindContributorAdopt, want{[3]bool{false, false, false}, [3]bool{false, false, true}, [3]bool{true, true, true}}},
		{"welcome / outreach comment", writeback.KindWelcomeComment, want{[3]bool{false, false, true}, [3]bool{false, true, true}, [3]bool{true, true, true}}},
	}
	const repo = "o/r"
	for _, tc := range cases {
		for i, level := range []int{4, 5, 6} {
			gate := a1Gate(t, writeback.Config{}, map[string]int{repo: level}, &bytes.Buffer{}, now)
			c := newTestClient(t, httptest.NewUnstartedServer(nil), "o", []string{repo})
			c.SetWritebackGate(gate)
			created := now.Add(time.Minute)
			unlabeled := c.WritebackItemForPR(PullRequest{Number: 1, Author: "visitor", CreatedAt: created})
			labeled := c.WritebackItemForPR(PullRequest{Number: 2, Author: "visitor", Labels: []string{"hive-review"}, CreatedAt: created})
			own := c.WritebackItemForPR(PullRequest{Number: 3, Author: "hive[bot]", AppAuthored: true, CreatedAt: created})
			for name, pair := range map[string]struct {
				item writeback.Item
				want bool
			}{"unlabeled": {unlabeled, tc.want.unlabeled[i]}, "labeled": {labeled, tc.want.labeled[i]}, "own": {own, tc.want.own[i]}} {
				d := c.WritebackAllowed(repo, pair.item, tc.kind, "")
				if d.Allowed != pair.want {
					t.Errorf("%s L%d %s: allowed=%v (%s) want %v", tc.path, level, name, d.Allowed, d.Reason, pair.want)
				}
			}
		}
	}
	// Level 0 (unknown) fails closed for every kind on foreign items; the
	// pre-existing guard also wins over the opt-in label.
	gate := a1Gate(t, writeback.Config{}, map[string]int{}, &bytes.Buffer{}, now)
	if d := gate.Allowed(repo, writeback.Item{Number: 1, IsPR: true, Labels: []string{"hive-review"}, CreatedAt: now.Add(time.Minute)}, writeback.KindSentinel); d.Allowed {
		t.Fatalf("unknown level must fail closed: %+v", d)
	}
	gate6 := a1Gate(t, writeback.Config{}, map[string]int{repo: 6}, &bytes.Buffer{}, now)
	gate6.ObserveRepo(repo)
	if d := gate6.Allowed(repo, writeback.Item{Number: 1, IsPR: true, CreatedAt: now.Add(-time.Hour)}, writeback.KindSentinel); d.Allowed || d.Reason != writeback.ReasonBackfill {
		t.Fatalf("pre-existing unlabeled item must be skipped at L6: %+v", d)
	}
}
