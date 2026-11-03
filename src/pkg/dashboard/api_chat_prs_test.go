package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

func TestParseChatPRFilter(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		name   string
		query  string
		viewer string
		want   chatPRFilter
	}{
		{name: "bare command default arg", query: "prs: status", want: chatPRFilter{}},
		{name: "slash prefix", query: "/prs", want: chatPRFilter{}},
		{name: "natural language", query: "what pull requests are open?", want: chatPRFilter{}},
		{name: "waiting on review", query: "prs: waiting on review", want: chatPRFilter{NeedsReview: true}},
		{name: "needs review", query: "prs: needs review", want: chatPRFilter{NeedsReview: true}},
		{name: "unreviewed", query: "prs unreviewed", want: chatPRFilter{NeedsReview: true}},
		{name: "needs decision", query: "prs: needs decision", want: chatPRFilter{NeedsDecision: true}},
		{name: "decision stage label", query: "prs stage/085-needs-decision", want: chatPRFilter{NeedsDecision: true}},
		{name: "hold", query: "prs: on hold", want: chatPRFilter{Hold: true}},
		{name: "held", query: "prs held", want: chatPRFilter{Hold: true}},
		{name: "failing ci", query: "prs: failing ci", want: chatPRFilter{FailingCI: true}},
		{name: "red checks", query: "prs with red checks", want: chatPRFilter{FailingCI: true}},
		{name: "mine resolves viewer", query: "prs: mine", viewer: "Alice", want: chatPRFilter{Author: "alice"}},
		{name: "mine without viewer", query: "prs mine", want: chatPRFilter{Mine: true}},
		{name: "by login", query: "prs: by bob", want: chatPRFilter{Author: "bob"}},
		{name: "by at-login", query: "prs by @bob", want: chatPRFilter{Author: "bob"}},
		{name: "opened by", query: "prs opened by bob", want: chatPRFilter{Author: "bob"}},
		{name: "at-login alone", query: "prs @carol", want: chatPRFilter{Author: "carol"}},
		{name: "repo keyword", query: "prs: repo hivecommons/docs", want: chatPRFilter{Repo: "hivecommons/docs"}},
		{name: "bare owner/repo", query: "prs hivecommons/hive", want: chatPRFilter{Repo: "hivecommons/hive"}},
		{name: "in repo short name", query: "prs in repo docs", want: chatPRFilter{Repo: "docs"}},
		{name: "stale default", query: "prs: stale", want: chatPRFilter{OlderThan: chatPRStaleAge}},
		{name: "older than days", query: "prs older than 3d", want: chatPRFilter{OlderThan: 3 * day}},
		{name: "older than weeks", query: "prs older than 2w", want: chatPRFilter{OlderThan: 14 * day}},
		{name: "bare duration", query: "prs 12h", want: chatPRFilter{OlderThan: 12 * time.Hour}},
		{name: "explicit age wins over stale", query: "prs stale older than 30d", want: chatPRFilter{OlderThan: 30 * day}},
		{name: "combined", query: "prs: waiting on review by bob in hivecommons/hive failing ci", want: chatPRFilter{NeedsReview: true, FailingCI: true, Author: "bob", Repo: "hivecommons/hive"}},
		{name: "unknown words collected", query: "prs: purple elephants", want: chatPRFilter{Unknown: []string{"purple", "elephants"}}},
		{name: "needs without object", query: "prs needs", want: chatPRFilter{Unknown: []string{"needs"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseChatPRFilter(tc.query, tc.viewer)
			if got.NeedsReview != tc.want.NeedsReview || got.NeedsDecision != tc.want.NeedsDecision ||
				got.Hold != tc.want.Hold || got.FailingCI != tc.want.FailingCI ||
				got.Author != tc.want.Author || got.Mine != tc.want.Mine || got.Repo != tc.want.Repo ||
				got.OlderThan != tc.want.OlderThan || strings.Join(got.Unknown, " ") != strings.Join(tc.want.Unknown, " ") {
				t.Fatalf("parseChatPRFilter(%q) = %+v, want %+v", tc.query, got, tc.want)
			}
		})
	}
}

func chatPRTestRepos(now time.Time) []FrontendRepo {
	approved := &github.ProtectionFacts{ReviewDecision: github.ReviewDecisionApproved, ApprovalsGiven: 1}
	changes := &github.ProtectionFacts{ReviewDecision: github.ReviewDecisionChangesRequested}
	hive := FrontendRepo{Name: "hive", Full: "hivecommons/hive", OpenPrs: []any{
		FrontendPR{PullRequest: github.PullRequest{Repo: "hivecommons/hive", Number: 101, Title: "Fix the thing", Author: "alice",
			Labels: []string{"stage/060-review", "kind/bug"}, CIStatus: "success", UpdatedAt: now.Add(-2 * time.Hour), URL: "https://github.com/hivecommons/hive/pull/101"},
			MergeVerdict: &github.MergeVerdict{State: github.MergeVerdictEligible}},
		FrontendPR{PullRequest: github.PullRequest{Repo: "hivecommons/hive", Number: 102, Title: "Approved already", Author: "bob",
			CIStatus: "success", Protection: approved, UpdatedAt: now.Add(-30 * time.Hour)}},
		FrontendPR{PullRequest: github.PullRequest{Repo: "hivecommons/hive", Number: 103, Title: "Draft work", Author: "alice", Draft: true,
			CIStatus: "pending", UpdatedAt: now.Add(-10 * 24 * time.Hour)}},
		&github.PullRequest{Repo: "hivecommons/hive", Number: 104, Title: "Needs a call", Author: "carol",
			Labels: []string{github.PRNeedsDecisionStageLabel}, CIStatus: "failure", Protection: changes, UpdatedAt: now.Add(-3 * 24 * time.Hour)},
	}, HeldPrs: []any{
		github.PullRequest{Repo: "hivecommons/hive", Number: 105, Title: "Parked", Author: "bob", Labels: []string{"hold"}, CIStatus: "success", UpdatedAt: now.Add(-20 * 24 * time.Hour)},
	}}
	docs := FrontendRepo{Name: "docs", Full: "hivecommons/docs", OpenPrs: []any{
		FrontendPR{PullRequest: github.PullRequest{Repo: "hivecommons/docs", Number: 7, Title: "Enterprise host", Author: "dave",
			CIStatus: "failure", UpdatedAt: now.Add(-5 * time.Minute), URL: "https://ghe.example.com/hivecommons/docs/pull/7"}},
	}}
	return []FrontendRepo{hive, docs}
}

func TestRenderChatPRList(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	repos := chatPRTestRepos(now)
	cases := []struct {
		name    string
		query   string
		viewer  string
		want    []string
		absent  []string
		wantRaw string
	}{
		{
			name:  "unfiltered lists every PR per repo with header",
			query: "prs: status",
			want: []string{
				"There are 6 open pull request(s):",
				"**hivecommons/docs** (1):",
				"- hivecommons/docs#7 — Enterprise host (@dave · 5m · ❌ CI) https://ghe.example.com/hivecommons/docs/pull/7",
				"**hivecommons/hive** (5):",
				"- hivecommons/hive#101 — Fix the thing (@alice · stage/060-review · 2h · ✅ CI · mergeable)",
				"- hivecommons/hive#102 — Approved already (@bob · 30h · ✅ CI · approved)",
				"- hivecommons/hive#103 — Draft work (@alice · draft · 10d · ⏳ CI)",
				"- hivecommons/hive#104 — Needs a call (@carol · stage/085-needs-decision · 3d · ❌ CI · changes requested · needs decision)",
				"- hivecommons/hive#105 — Parked (@bob · 20d · ✅ CI · hold)",
			},
			absent: []string{"github.com/hivecommons/hive/pull/101", "Ignored", "… and"},
		},
		{
			name:   "waiting on review drops approved, drafts and changes requested",
			query:  "prs: waiting on review",
			want:   []string{"3 of 6 open pull request(s) match waiting on review:", "#101", "#105", "docs#7"},
			absent: []string{"#102", "#103", "#104"},
		},
		{
			name:   "needs decision",
			query:  "prs needs decision",
			want:   []string{"1 of 6 open pull request(s) match needs decision:", "#104"},
			absent: []string{"#101"},
		},
		{
			name:   "hold reads the held set",
			query:  "prs on hold",
			want:   []string{"1 of 6 open pull request(s) match hold:", "#105"},
			absent: []string{"#101"},
		},
		{
			name:   "failing ci",
			query:  "prs failing ci",
			want:   []string{"2 of 6 open pull request(s) match failing ci:", "#104", "docs#7"},
			absent: []string{"#101"},
		},
		{
			name:   "mine via viewer",
			query:  "prs mine",
			viewer: "alice",
			want:   []string{"2 of 6 open pull request(s) match by alice:", "#101", "#103"},
			absent: []string{"#102", "needs a signed-in viewer"},
		},
		{
			name:   "mine without viewer explains",
			query:  "prs mine",
			want:   []string{"`mine` needs a signed-in viewer; showing every author.", "There are 6 open pull request(s):"},
			absent: nil,
		},
		{
			name:   "repo filter by short name",
			query:  "prs repo docs",
			want:   []string{"1 of 6 open pull request(s) match repo docs:", "docs#7"},
			absent: []string{"hive#"},
		},
		{
			name:   "stale",
			query:  "prs stale",
			want:   []string{"2 of 6 open pull request(s) match older than 7d:", "#103", "#105"},
			absent: []string{"#101", "#104"},
		},
		{
			name:   "older than 2d",
			query:  "prs older than 2d",
			want:   []string{"3 of 6 open pull request(s) match older than 2d:", "#103", "#104", "#105"},
			absent: []string{"#102"},
		},
		{
			name:    "no match",
			query:   "prs by nobody",
			wantRaw: "0 of 6 open pull request(s) match by nobody; nothing to list.",
		},
		{
			name:  "unknown words still list with hint",
			query: "prs purple elephants",
			want:  []string{"Ignored `purple elephants`; filters are " + chatPRFilterHint + ".", "There are 6 open pull request(s):", "#101"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderChatPRList(repos, parseChatPRFilter(tc.query, tc.viewer), now)
			if tc.wantRaw != "" && got != tc.wantRaw {
				t.Fatalf("got %q, want %q", got, tc.wantRaw)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(got, a) {
					t.Errorf("unexpected %q in:\n%s", a, got)
				}
			}
		})
	}
}

func TestChatPRWaitingOnReview(t *testing.T) {
	cases := []struct {
		name string
		pr   github.PullRequest
		want bool
	}{
		{name: "no review yet", pr: github.PullRequest{}, want: true},
		{name: "review required", pr: github.PullRequest{Protection: &github.ProtectionFacts{ReviewDecision: github.ReviewDecisionReviewRequired}}, want: true},
		{name: "draft", pr: github.PullRequest{Draft: true}, want: false},
		{name: "approved", pr: github.PullRequest{Protection: &github.ProtectionFacts{ReviewDecision: github.ReviewDecisionApproved}}, want: false},
		{name: "changes requested waits on the author", pr: github.PullRequest{Protection: &github.ProtectionFacts{ReviewDecision: github.ReviewDecisionChangesRequested}}, want: false},
		{name: "changes requested with an approval", pr: github.PullRequest{Protection: &github.ProtectionFacts{ReviewDecision: github.ReviewDecisionChangesRequested, ApprovalsGiven: 1}}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chatPRWaitingOnReview(tc.pr); got != tc.want {
				t.Fatalf("chatPRWaitingOnReview = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRenderChatPRListCapsAndFooter(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	repo := FrontendRepo{Name: "hive", Full: "hivecommons/hive"}
	for i := 1; i <= chatPRListCap+8; i++ {
		repo.OpenPrs = append(repo.OpenPrs, FrontendPR{PullRequest: github.PullRequest{Repo: "hivecommons/hive", Number: i, Title: "PR", Author: "a", UpdatedAt: now}})
	}
	got := renderChatPRList([]FrontendRepo{repo}, chatPRFilter{}, now)
	if n := strings.Count(got, "\n- "); n != chatPRListCap {
		t.Fatalf("listed %d rows, want %d:\n%s", n, chatPRListCap, got)
	}
	if !strings.Contains(got, "… and 8 more — narrow with a filter: "+chatPRFilterHint+".") {
		t.Fatalf("missing footer in:\n%s", got)
	}
}

func TestRenderChatPRListReadsJSONShapedRows(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	repo := FrontendRepo{Name: "repo", Full: "myorg/repo", OpenPrs: []any{
		map[string]any{"number": 7, "title": "From JSON", "author": "erin", "ci_status": "failure", "labels": []any{"stage/085-needs-decision"}},
	}}
	got := renderChatPRList([]FrontendRepo{repo}, chatPRFilter{}, now)
	want := "- myorg/repo#7 — From JSON (@erin · stage/085-needs-decision · ❌ CI · needs decision)"
	if !strings.Contains(got, want) {
		t.Fatalf("missing %q in:\n%s", want, got)
	}
}

func TestRenderChatPRListEmpty(t *testing.T) {
	got := renderChatPRList([]FrontendRepo{{Name: "hive"}}, chatPRFilter{}, time.Now())
	if got != "No open pull requests are currently reported." {
		t.Fatalf("got %q", got)
	}
}

func TestChatScopedLocalAnswerRoutesPRs(t *testing.T) {
	now := time.Now()
	s := &Server{status: &StatusPayload{Repos: chatPRTestRepos(now)}}
	cases := []struct {
		name      string
		query     string
		viewer    string
		wantLocal bool
		want      string
	}{
		{name: "bare prefix", query: "prs: status", wantLocal: true, want: "There are 6 open pull request(s):"},
		{name: "slash filters", query: "/prs waiting on review", wantLocal: true, want: "match waiting on review"},
		{name: "mine uses viewer", query: "prs: mine", viewer: "alice", wantLocal: true, want: "match by alice"},
		{name: "free text with known filters", query: "what PRs are failing ci", wantLocal: true, want: "match failing ci"},
		{name: "unknown words go to grounding", query: "prs: which should merge first", wantLocal: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			answer, ok := s.chatScopedLocalAnswer(chatParseRoute(tc.query), tc.viewer)
			if ok != tc.wantLocal {
				t.Fatalf("local = %v, want %v (answer %q)", ok, tc.wantLocal, answer)
			}
			if ok && !strings.Contains(answer, tc.want) {
				t.Fatalf("answer = %q, want to contain %q", answer, tc.want)
			}
		})
	}
	if !strings.Contains(chatHelpAnswer, "waiting on review") {
		t.Fatalf("help should list the prs filters, got %q", chatHelpAnswer)
	}
}
