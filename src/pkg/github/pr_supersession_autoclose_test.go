package github

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDecideSupersessionAutoClose(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	eligible := func() SupersessionAutoCloseFacts {
		return SupersessionAutoCloseFacts{
			FilesSubset:  true,
			SizeLabel:    "size/S",
			ChangedLines: -1,
			NoticeAt:     now.Add(-25 * time.Hour),
			Now:          now,
			GracePeriod:  24 * time.Hour,
		}
	}
	cases := []struct {
		name   string
		mutate func(*SupersessionAutoCloseFacts)
		want   SupersessionAutoCloseVerdict
	}{
		{"past grace closes", func(f *SupersessionAutoCloseFacts) {}, SupersessionClose},
		{"open claim keeps", func(f *SupersessionAutoCloseFacts) { f.OpenClaimRemaining = true }, SupersessionKeep},
		{"extra files keep", func(f *SupersessionAutoCloseFacts) { f.FilesSubset = false }, SupersessionKeep},
		{"size/L keeps", func(f *SupersessionAutoCloseFacts) { f.SizeLabel = "size/L" }, SupersessionKeep},
		{"size/XXL keeps", func(f *SupersessionAutoCloseFacts) { f.SizeLabel = "size/XXL" }, SupersessionKeep},
		{"lowercase size/xs closes", func(f *SupersessionAutoCloseFacts) { f.SizeLabel = "size/xs" }, SupersessionClose},
		{"size/M closes", func(f *SupersessionAutoCloseFacts) { f.SizeLabel = "size/M" }, SupersessionClose},
		{"unknown size keeps", func(f *SupersessionAutoCloseFacts) { f.SizeLabel = "" }, SupersessionKeep},
		{"small unlabeled closes", func(f *SupersessionAutoCloseFacts) { f.SizeLabel = ""; f.ChangedLines = 20 }, SupersessionClose},
		{"large unlabeled keeps", func(f *SupersessionAutoCloseFacts) { f.SizeLabel = ""; f.ChangedLines = 150 }, SupersessionKeep},
		{"threads unknown keeps", func(f *SupersessionAutoCloseFacts) { f.ThreadsUnknown = true }, SupersessionKeep},
		{"unresolved author threads keep", func(f *SupersessionAutoCloseFacts) { f.UnresolvedAuthorThreads = true }, SupersessionKeep},
		{"author replied keeps", func(f *SupersessionAutoCloseFacts) { f.HumanActivityAfterNotice = true }, SupersessionKeep},
		{"no notice yet waits", func(f *SupersessionAutoCloseFacts) { f.NoticeAt = time.Time{} }, SupersessionWaitGrace},
		{"within grace waits", func(f *SupersessionAutoCloseFacts) { f.NoticeAt = now.Add(-time.Hour) }, SupersessionWaitGrace},
		{"default grace applies", func(f *SupersessionAutoCloseFacts) {
			f.GracePeriod = 0
			f.NoticeAt = now.Add(-23 * time.Hour)
		}, SupersessionWaitGrace},
		{"hive-authored closes without grace", func(f *SupersessionAutoCloseFacts) {
			f.HiveAuthored = true
			f.NoticeAt = time.Time{}
			f.HumanActivityAfterNotice = true
		}, SupersessionClose},
		{"hive-authored still needs subset", func(f *SupersessionAutoCloseFacts) {
			f.HiveAuthored = true
			f.FilesSubset = false
		}, SupersessionKeep},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := eligible()
			tc.mutate(&f)
			got := DecideSupersessionAutoClose(f)
			if got.Verdict != tc.want {
				t.Fatalf("verdict = %s (%s), want %s", got.Verdict, got.Reason, tc.want)
			}
			if got.Reason == "" {
				t.Fatal("decision has no reason")
			}
		})
	}
}

func TestRenderSupersessionCloseComment(t *testing.T) {
	body := renderSupersessionCloseComment(supersessionCloser{Repo: "o/r", Number: 7}, 90*time.Minute)
	for _, want := range []string{supersessionAutoCloseMarker, "o/r#7", "which has merged", "1h30m0s", "Reopen if that's wrong", SupersededLabel} {
		if !strings.Contains(body, want) {
			t.Errorf("close comment missing %q:\n%s", want, body)
		}
	}
	merged := time.Date(2026, 10, 10, 11, 40, 0, 0, time.UTC)
	body = renderSupersessionCloseComment(supersessionCloser{Repo: "o/r", Number: 7, URL: "https://x/7", MergedAt: merged}, 24*time.Hour)
	for _, want := range []string{"[#7](https://x/7)", "merged at 2026-10-10 11:40Z", "24h grace window"} {
		if !strings.Contains(body, want) {
			t.Errorf("close comment missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(renderSupersessionGraceNotice(0), "24h") {
		t.Error("grace notice should fall back to the default window")
	}
}

const (
	autoCloseOrg  = "hivecommons"
	autoCloseRepo = "hive"
	autoCloseBot  = "hive-app[bot]"
)

func autoClosePRs(open supersessionPRFixture) []supersessionPRFixture {
	if open.Number == 0 {
		open.Number = 12
	}
	if open.Title == "" {
		open.Title = "fix thing"
	}
	if open.Body == "" {
		open.Body = "Refs #3"
	}
	if open.Author == "" {
		open.Author = "contributor"
	}
	if open.Files == nil {
		open.Files = []string{"pkg/a.go"}
	}
	if open.Labels == nil && open.Additions == 0 {
		open.Labels = []string{"size/S"}
	}
	return []supersessionPRFixture{
		open,
		{Number: 22, Title: "landed", Body: "Fixes #3", Author: autoCloseBot, Merged: true, Files: []string{"pkg/a.go", "pkg/b.go"}},
	}
}

func runAutoCloseSweep(t *testing.T, open supersessionPRFixture, opts SupersessionSweepOptions) (*SupersessionSweepResult, *supersessionObservations) {
	t.Helper()
	server, obs := supersessionSweepServer(t, autoCloseOrg, autoCloseRepo, autoClosePRs(open),
		[]supersessionIssueFixture{{Number: 3, State: "closed", GraphQLCloserPR: 22}})
	defer server.Close()
	c := newTestClient(t, server, autoCloseOrg, []string{autoCloseRepo})
	c.SetAppBotLogin(autoCloseBot)
	if opts.ACMMLevelForRepo == nil {
		opts.ACMMLevelForRepo = func(string) int { return acmmLevelFullyAutonomous }
	}
	res, err := c.SweepSupersededOpenPRs(context.Background(), opts)
	if err != nil {
		t.Fatalf("SweepSupersededOpenPRs: %v", err)
	}
	return res, obs
}

var autoCloseEnabled = SupersessionSweepOptions{CloseContributorPRs: true}

// graceNoticeBody runs one pass with no prior comments and returns the grace
// notice the sweep posts, so later passes can be seeded with the exact body.
func graceNoticeBody(t *testing.T) string {
	t.Helper()
	res, obs := runAutoCloseSweep(t, supersessionPRFixture{}, autoCloseEnabled)
	if len(res.Commented) != 1 || res.Commented[0].Action != "commented-grace" {
		t.Fatalf("first pass should post the grace notice: %+v", res)
	}
	if len(obs.closed) != 0 {
		t.Fatalf("first pass closed a PR: %v", obs.closed)
	}
	body := obs.comments[12][0]
	if !strings.Contains(body, supersessionSweepMarker) || !strings.Contains(body, "24h") {
		t.Fatalf("grace notice body unexpected: %q", body)
	}
	return body
}

func noticeComment(body string, at time.Time) supersessionCommentFixture {
	return supersessionCommentFixture{ID: 900, Body: body, Author: autoCloseBot, Bot: true, CreatedAt: at, UpdatedAt: at}
}

func TestSupersessionAutoClose_ClosesAfterGrace(t *testing.T) {
	body := graceNoticeBody(t)
	old := time.Now().Add(-25 * time.Hour)
	res, obs := runAutoCloseSweep(t, supersessionPRFixture{
		Comments: []supersessionCommentFixture{
			noticeComment(body, old),
			{ID: 901, Body: "/lgtm", Author: "ci-robot[bot]", CreatedAt: old.Add(time.Hour)},
		},
	}, autoCloseEnabled)
	if len(res.Closed) != 1 || res.Closed[0].Action != "closed-contributor" {
		t.Fatalf("expected contributor close, got %+v", res)
	}
	if len(obs.closed) != 1 || obs.closed[0] != 12 {
		t.Fatalf("closed = %v, want [12]", obs.closed)
	}
	if got := obs.labels[12]; len(got) != 1 || got[0] != SupersededLabel {
		t.Fatalf("labels = %v, want %s", got, SupersededLabel)
	}
	if len(obs.comments[12]) != 1 || !strings.Contains(obs.comments[12][0], supersessionAutoCloseMarker) || !strings.Contains(obs.comments[12][0], "/pull/22") {
		t.Fatalf("close comment = %v", obs.comments[12])
	}
}

func TestSupersessionAutoClose_UnlabeledSmallPRUsesDiffSize(t *testing.T) {
	body := graceNoticeBody(t)
	res, obs := runAutoCloseSweep(t, supersessionPRFixture{
		Labels:    []string{},
		Additions: 20,
		Comments:  []supersessionCommentFixture{noticeComment(body, time.Now().Add(-25*time.Hour))},
	}, autoCloseEnabled)
	if len(res.Closed) != 1 || len(obs.closed) != 1 {
		t.Fatalf("small unlabeled PR should close: %+v", res)
	}
}

func TestSupersessionAutoClose_DoesNotRepostCloseComment(t *testing.T) {
	body := graceNoticeBody(t)
	old := time.Now().Add(-25 * time.Hour)
	_, obs := runAutoCloseSweep(t, supersessionPRFixture{
		Comments: []supersessionCommentFixture{
			noticeComment(body, old),
			{ID: 902, Body: supersessionAutoCloseMarker + "\nClosing", Author: autoCloseBot, Bot: true, CreatedAt: old.Add(25 * time.Hour)},
		},
	}, autoCloseEnabled)
	if len(obs.comments[12]) != 0 {
		t.Fatalf("close comment reposted: %v", obs.comments[12])
	}
	if len(obs.closed) != 1 {
		t.Fatalf("closed = %v, want retry of the close", obs.closed)
	}
}

func TestSupersessionAutoClose_WithinGraceWaits(t *testing.T) {
	body := graceNoticeBody(t)
	res, obs := runAutoCloseSweep(t, supersessionPRFixture{
		Comments: []supersessionCommentFixture{noticeComment(body, time.Now().Add(-time.Hour))},
	}, autoCloseEnabled)
	if len(obs.closed) != 0 || len(obs.comments[12]) != 0 || len(obs.edits) != 0 {
		t.Fatalf("within grace must not write: closed=%v comments=%v edits=%v", obs.closed, obs.comments, obs.edits)
	}
	if len(res.Commented) != 1 || res.Commented[0].Action != "commented-grace" {
		t.Fatalf("result = %+v", res)
	}
}

func TestSupersessionAutoClose_OldNoticeRestartsGrace(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)
	_, obs := runAutoCloseSweep(t, supersessionPRFixture{
		Comments: []supersessionCommentFixture{noticeComment(supersessionSweepMarker+"\nLeaving this contributor PR open.", old)},
	}, autoCloseEnabled)
	if len(obs.closed) != 0 {
		t.Fatalf("a notice that never announced the grace window must not close: %v", obs.closed)
	}
	if got := obs.edits[900]; !strings.Contains(got, "grace") && !strings.Contains(got, "Unless someone comments") {
		t.Fatalf("notice not rewritten to the grace text: %q", got)
	}
}

func TestSupersessionAutoClose_Guards(t *testing.T) {
	body := graceNoticeBody(t)
	old := time.Now().Add(-25 * time.Hour)
	notice := noticeComment(body, old)
	cases := []struct {
		name string
		pr   supersessionPRFixture
		opts SupersessionSweepOptions
	}{
		{"author replied", supersessionPRFixture{Comments: []supersessionCommentFixture{
			notice, {ID: 903, Body: "still needed: has a test", Author: "contributor", CreatedAt: old.Add(time.Hour)},
		}}, autoCloseEnabled},
		{"maintainer replied", supersessionPRFixture{Comments: []supersessionCommentFixture{
			notice, {ID: 904, Body: "keep", Author: "maintainer", CreatedAt: old.Add(time.Hour)},
		}}, autoCloseEnabled},
		{"human review comment after notice", supersessionPRFixture{Comments: []supersessionCommentFixture{notice},
			Threads: []supersessionThreadFixture{{Resolved: true, Comments: []supersessionCommentFixture{{Author: "maintainer", CreatedAt: old.Add(time.Hour)}}}},
		}, autoCloseEnabled},
		{"unresolved author thread", supersessionPRFixture{Comments: []supersessionCommentFixture{notice},
			Threads: []supersessionThreadFixture{{Comments: []supersessionCommentFixture{
				{Author: "reviewer-bot", Bot: true, CreatedAt: old.Add(-time.Hour)},
				{Author: "contributor", CreatedAt: old.Add(-30 * time.Minute)},
			}}},
		}, autoCloseEnabled},
		{"review threads unreadable", supersessionPRFixture{Comments: []supersessionCommentFixture{notice}, ThreadsBroken: true}, autoCloseEnabled},
		{"size/L", supersessionPRFixture{Comments: []supersessionCommentFixture{notice}, Labels: []string{"size/L"}}, autoCloseEnabled},
		{"large unlabeled", supersessionPRFixture{Comments: []supersessionCommentFixture{notice}, Labels: []string{}, Additions: 400}, autoCloseEnabled},
		{"extra files", supersessionPRFixture{Comments: []supersessionCommentFixture{notice}, Files: []string{"pkg/a.go", "pkg/c.go"}}, autoCloseEnabled},
		{"disabled", supersessionPRFixture{Comments: []supersessionCommentFixture{notice}}, SupersessionSweepOptions{}},
		{"below L6", supersessionPRFixture{Comments: []supersessionCommentFixture{notice}},
			SupersessionSweepOptions{CloseContributorPRs: true, ACMMLevelForRepo: func(string) int { return acmmLevelFullyAutonomous - 1 }}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, obs := runAutoCloseSweep(t, tc.pr, tc.opts)
			if len(obs.closed) != 0 || len(res.Closed) != 0 {
				t.Fatalf("guard %q closed the PR: %v", tc.name, obs.closed)
			}
			if len(obs.labels[12]) != 0 {
				t.Fatalf("guard %q labeled the PR: %v", tc.name, obs.labels[12])
			}
			if len(res.Commented) != 1 || res.Commented[0].Action != "commented-contributor" {
				t.Fatalf("guard %q result = %+v", tc.name, res)
			}
			if got := obs.edits[900]; !strings.Contains(got, "Leaving this contributor PR open") {
				t.Fatalf("guard %q did not rewrite the notice to the keep-open text: %q", tc.name, got)
			}
		})
	}
}
