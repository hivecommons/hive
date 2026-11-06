package github

import (
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

const noPRHiveBot = "danathar-atomic-hive[bot]"

func noPRComment(login, kind, body string, at time.Time, id int) map[string]any {
	return map[string]any{
		"id":         id,
		"body":       body,
		"user":       map[string]any{"login": login, "type": kind},
		"created_at": at.UTC().Format(time.RFC3339),
		"updated_at": at.UTC().Format(time.RFC3339),
		"html_url":   "https://github.com/acme/widgets/issues/834#issuecomment-" + strings.Repeat("1", 3) + string(rune('0'+id%10)),
	}
}

const noPRDiagnosis = "**Summary:** The monthly check found gaps in changes that were already merged.\n\n## Why no PR\n\nFixing it means editing merged commits.\n\n## Suggested follow-up for the owner\n\nPrevent new gaps."

// noPRSweep runs the issue stage sweep for #834 at clock with labels.
func noPRSweep(t *testing.T, fake *issueStageServer, c *Client, store *stagelabels.Store, clock time.Time, labels ...string) IssueStageResult {
	t.Helper()
	result := &ActionableResult{}
	result.Issues = IssueResultFromItems([]Issue{{Repo: "acme/widgets", Number: 834, Labels: labels}})
	return syncSeeded(t, fake, c, result, store, clock)
}

func noPRClient(t *testing.T, fake *issueStageServer) *Client {
	t.Helper()
	c := issueStageTestClient(t, fake.srv, true)
	c.SetAppBotLogin(noPRHiveBot)
	return c
}

// Danathar/atomic-image-builder#834 (hivecommons/hive#16947): a bot-opened
// issue the scanner answered with a no-PR diagnosis and did not park sat at
// 010 "waiting for the scanner" for five days. Once it stalls there, the
// sweep parks it for the owner with options the menu offers.
func TestNoPRAnswerOnStalled010ParksForTheOwner(t *testing.T) {
	fake := newIssueStageServer(t)
	c := noPRClient(t, fake)
	now := stageNow()
	store := stagelabels.NewStore("", func() time.Time { return now })

	// Cycle 1: the auto-issue arrives; actionable → 010.
	noPRSweep(t, fake, c, store, now)
	if rec, _ := store.Get(stagelabels.Key("acme/widgets", 834)); rec.Stage != stagelabels.Actionable {
		t.Fatalf("stage after cycle 1 = %v", rec.Stage)
	}
	// An hour later the scanner answers with a diagnosis and no PR.
	fake.mu.Lock()
	fake.commentObjs = map[int][]map[string]any{834: {noPRComment(noPRHiveBot, "Bot", noPRDiagnosis, now.Add(time.Hour), 1)}}
	fake.mu.Unlock()

	// Two hours on: not stalled yet, so the scanner's move stands.
	if res := noPRSweep(t, fake, c, store, now.Add(2*time.Hour), stagelabels.Actionable.Label()); res.NoPRParked != 0 {
		t.Fatalf("parked before the stall: %+v", res)
	}

	// A day and more on, stalled: parked for the owner.
	res := noPRSweep(t, fake, c, store, now.Add(26*time.Hour), stagelabels.Actionable.Label())
	if res.NoPRParked != 1 {
		t.Fatalf("NoPRParked = %d, want 1 (result %+v)", res.NoPRParked, res)
	}
	fake.mu.Lock()
	notes := append([]string(nil), fake.commentBodies[834]...)
	added := append([]string(nil), fake.added[834]...)
	removed := append([]string(nil), fake.removed[834]...)
	fake.mu.Unlock()
	if len(notes) != 1 || !strings.Contains(notes[0], noPRParkMarker) || !strings.Contains(notes[0], "Option A: close it") || !strings.Contains(notes[0], "Option B: keep it open") {
		t.Fatalf("park note = %q", notes)
	}
	if !strings.Contains(notes[0], "issuecomment-") {
		t.Fatalf("the note must link the answer it parks on: %q", notes[0])
	}
	if !labelPresent(added, c.needsDecisionLabel()) || !labelPresent(removed, stagelabels.Actionable.Label()) {
		t.Fatalf("park wrote added=%v removed=%v, want %s on and 010 off", added, removed, c.needsDecisionLabel())
	}

	// The next pass sees the park: 045, waiting on a person, nothing posted again.
	res = noPRSweep(t, fake, c, store, now.Add(28*time.Hour), stagelabels.IssueNeedsDecision.Label())
	if rec, _ := store.Get(stagelabels.Key("acme/widgets", 834)); rec.Stage != stagelabels.IssueNeedsDecision {
		t.Fatalf("stage after the park = %v, want 045", rec.Stage)
	}
	fake.mu.Lock()
	posted := len(fake.commentBodies[834])
	fake.mu.Unlock()
	if posted != 1 || res.NoPRParked != 0 {
		t.Fatalf("second pass posted %d notes, parked %d", posted, res.NoPRParked)
	}

	// The "What to reply" menu built from the park offers the options.
	comments := []*gh.IssueComment{
		{Body: gh.Ptr(noPRDiagnosis), User: &gh.User{Login: gh.Ptr(noPRHiveBot)}},
		{Body: gh.Ptr(notes[0]), User: &gh.User{Login: gh.Ptr(noPRHiveBot)}, HTMLURL: gh.Ptr("https://github.com/acme/widgets/issues/834#issuecomment-2")},
	}
	notice := renderUnparkNotice(c.unparkMenuFor("Scheduled run failing", &gh.User{Login: gh.Ptr("github-actions[bot]")}, comments))
	for _, want := range []string{"`/hive approve` → go with option A: close it", "`/hive decision B` → go with option B: keep it open", "`/fixed`"} {
		if !strings.Contains(notice, want) {
			t.Errorf("What-to-reply menu misses %q:\n%s", want, notice)
		}
	}
}

func TestNoPRParkLeavesTheScannersMoveAlone(t *testing.T) {
	now := stageNow()
	for _, tc := range []struct {
		name     string
		comments func() []map[string]any
		labels   []string
		issue    func(*Issue)
	}{
		{
			name: "a person spoke after the hive",
			comments: func() []map[string]any {
				return []map[string]any{
					noPRComment(noPRHiveBot, "Bot", noPRDiagnosis, now.Add(time.Hour), 1),
					noPRComment("danathar", "User", "I'll look at this tomorrow.", now.Add(2*time.Hour), 2),
				}
			},
		},
		{
			name: "the hive's word predates this stay at 010",
			comments: func() []map[string]any {
				return []map[string]any{noPRComment(noPRHiveBot, "Bot", noPRDiagnosis, now.Add(-time.Hour), 1)}
			},
		},
		{
			name: "only the hive's own bookkeeping comments",
			comments: func() []map[string]any {
				return []map[string]any{noPRComment(noPRHiveBot, "Bot", "<!-- hive:status -->\n**Hive status**", now.Add(time.Hour), 1)}
			},
		},
		{
			name:     "nobody spoke",
			comments: func() []map[string]any { return nil },
		},
		{
			name: "an agent holds a claim",
			comments: func() []map[string]any {
				return []map[string]any{noPRComment(noPRHiveBot, "Bot", noPRDiagnosis, now.Add(time.Hour), 1)}
			},
			issue: func(is *Issue) { is.ClaimKind, is.ClaimedBy = "agent", "scanner" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newIssueStageServer(t)
			c := noPRClient(t, fake)
			store := stagelabels.NewStore("", func() time.Time { return now })
			noPRSweep(t, fake, c, store, now)
			fake.mu.Lock()
			fake.commentObjs = map[int][]map[string]any{834: tc.comments()}
			fake.mu.Unlock()
			is := Issue{Repo: "acme/widgets", Number: 834, Labels: []string{stagelabels.Actionable.Label()}}
			if tc.issue != nil {
				tc.issue(&is)
			}
			result := &ActionableResult{}
			result.Issues = IssueResultFromItems([]Issue{is})
			res := syncSeeded(t, fake, c, result, store, now.Add(26*time.Hour))
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if res.NoPRParked != 0 || len(fake.commentBodies[834]) != 0 || labelPresent(fake.truth[834], c.needsDecisionLabel()) {
				t.Fatalf("parked: result %+v, posted %q, labels %v", res, fake.commentBodies[834], fake.truth[834])
			}
		})
	}
}

func TestHiveMachineryComment(t *testing.T) {
	for body, want := range map[string]bool{
		noPRDiagnosis: false,
		noPRDiagnosis + "\n\n— hive: agent=scanner\n<!-- hive:origin hive=hive-wild-mole at=2026-11-06T05:34:02Z -->": false,
		"<!-- hive:status -->\n**Hive status**":                   true,
		"<!-- hive:origin hive=x -->\n<!-- hive:park-recheck -->": true,
		noPRParkNote("https://example.test/c/1"):                  true,
	} {
		if got := hiveMachineryComment(body); got != want {
			t.Errorf("hiveMachineryComment(%.40q) = %v, want %v", body, got, want)
		}
	}
}
