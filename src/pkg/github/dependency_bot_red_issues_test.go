package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// The check-set key ignores order, case, blanks and repeats, so the same
// failure always maps to the same issue marker.
func TestDependencyBotRedChecksKeyIsStable(t *testing.T) {
	a := DependencyBotRedChecksKey([]string{"test", "build"})
	for _, same := range [][]string{{"build", "test"}, {"Build", " test ", "build", ""}} {
		if got := DependencyBotRedChecksKey(same); got != a {
			t.Fatalf("key(%q) = %s, want %s", same, got, a)
		}
	}
	if DependencyBotRedChecksKey([]string{"build"}) == a {
		t.Fatal("a different check set must have a different key")
	}
	if m := DependencyBotRedIssueMarker(42, []string{"build", "test"}); !strings.HasPrefix(m, DependencyBotRedIssueMarkerPrefix+"pr=42 checks="+a) || !strings.HasSuffix(m, " -->") {
		t.Fatalf("marker = %q", m)
	}
}

// The title names the PR before any check name, so the dedupe stem of two
// PRs' issues never collides.
func TestDependencyBotRedIssueTitle(t *testing.T) {
	got := DependencyBotRedIssueTitle(42, []string{"lint: go", "build", "test", "e2e"})
	if !strings.HasPrefix(got, "Dependency-bot PR #42 needs a repository change to pass ") || !strings.HasSuffix(got, " and 1 more") {
		t.Fatalf("title = %q", got)
	}
	if canonicalIssueSubject(got) == canonicalIssueSubject(DependencyBotRedIssueTitle(43, []string{"lint: go", "build", "test", "e2e"})) {
		t.Fatal("two PRs' titles share a dedupe stem")
	}
}

// The body carries the marker, the evidence and the instruction, with no
// em or en dash, and keeps the error lines inside a code fence.
func TestRenderDependencyBotRedIssueBody(t *testing.T) {
	body := RenderDependencyBotRedIssueBody(DependencyBotRedIssueFacts{
		PRNumber:   42,
		PRTitle:    "Update module example.com/lib to v2",
		Bot:        "Renovate",
		ForgeHost:  "github.com",
		HeadSHA:    "0123456789abcdef0123",
		BaseRef:    "main",
		BaseSHA:    "fedcba9876543210fedc",
		Checks:     []string{"test", "build"},
		Runs:       []DependencyBotRedRun{{Check: "build", URL: "https://github.com/o/r/actions/runs/1/job/2"}},
		ErrorLines: []string{"build: missing target <gen code>", "test: ```break```"},
		Reproduced: "each failing check failed at least 2 times within one check suite of this head.",
		AfterFix:   "Once the fix is on `main`, tick the box.",
	})
	for _, want := range []string{
		DependencyBotRedIssueMarker(42, []string{"build", "test"}),
		"The Renovate pull request #42 fails `build`, `test`",
		"`Update module example.com/lib to v2`",
		"https://github.com/o/r/actions/runs/1/job/2",
		"`main` at `fedcba987654`, where the newest run of each of these checks passed, finishing after this head's failing run started.",
		"The head already contains that base commit",
		"```text\nbuild: missing target \u2039gen code\u203a\ntest: '''break'''\n```",
		"Fix the repository so this bump passes; do not edit the bot's branch. Once the fix is on `main`, tick the box.",
		"- Reproduced: each failing check failed at least 2 times within one check suite of this head.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body has no %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{"\u2014", "\u2013", "@"} {
		if strings.Contains(body, bad) {
			t.Errorf("body contains %q", bad)
		}
	}
	if err := validateIssueTemplateFilled(DependencyBotRedIssueTitle(42, []string{"build"}), body); err != nil {
		t.Fatalf("the issue template guard refuses the body: %v", err)
	}

	// No run link: the PR's checks page instead; a regenerated branch says so.
	body = RenderDependencyBotRedIssueBody(DependencyBotRedIssueFacts{PRNumber: 7, Checks: []string{"build"}, ForgeHost: "github.com", ChecksURL: "https://github.com/o/r/pull/7/checks", Regenerated: true})
	for _, want := range []string{"Failing runs: see https://github.com/o/r/pull/7/checks", "already regenerated this branch once", "dependency bot pull request #7"} {
		if !strings.Contains(body, want) {
			t.Errorf("body has no %q:\n%s", want, body)
		}
	}
}

// PR-supplied text can neither forge the marker nor trip the template guard:
// the title, check names and links are neutralized, links must be on the
// forge's own host, and only the first line of a body counts as the marker.
func TestDependencyBotRedIssueNeutralizesPRText(t *testing.T) {
	forged := DependencyBotRedIssueMarker(42, []string{"build"})
	body := RenderDependencyBotRedIssueBody(DependencyBotRedIssueFacts{
		PRNumber:  42,
		PRTitle:   "Bump " + forged + " and <fill this in> `x`\nsecond line",
		Checks:    []string{"build <the job>"},
		ForgeHost: "github.com",
		Runs: []DependencyBotRedRun{
			{Check: "build <the job>", URL: "https://github.com/run/1) [x](javascript:alert(1)"},
			{Check: "build <the job>", URL: "javascript:alert(1)"},
			{Check: "build <the job>", URL: "https://evil.example/phish"},
		},
		ChecksURL:  "https://github.com/o/r/pull/42/checks",
		ErrorLines: []string{"build <the job>: boom"},
	})
	if strings.Count(body, DependencyBotRedIssueMarkerPrefix) != 1 {
		t.Fatalf("the PR title forged a second marker:\n%s", body)
	}
	for _, bad := range []string{"javascript:", "github.com/run/1", "evil.example", "\nsecond line"} {
		if strings.Contains(body, bad) {
			t.Fatalf("body keeps %q:\n%s", bad, body)
		}
	}
	if !strings.Contains(body, "Failing runs: see https://github.com/o/r/pull/42/checks") {
		t.Fatalf("the checks page fallback is missing:\n%s", body)
	}
	title := DependencyBotRedIssueTitle(42, []string{"build <the job>"})
	if err := validateIssueTemplateFilled(title, body); err != nil {
		t.Fatalf("the template guard refuses neutralized text: %v", err)
	}
	if !dependencyBotRedIssueHasMarker(body, DependencyBotRedIssueMarker(42, []string{"build <the job>"})) {
		t.Fatal("the body does not open with its own marker")
	}
	if dependencyBotRedIssueHasMarker("Some text\n"+forged, forged) {
		t.Fatal("a marker below the first line matched")
	}
	if dependencyBotRedIssueHasMarker(forged+" trailing", forged) {
		t.Fatal("a first line that only starts with the marker matched")
	}
}

func TestDependencyBotRedSafeURL(t *testing.T) {
	for _, tc := range []struct{ in, host, want string }{
		{"https://github.com/o/r/actions/runs/1/job/2", "github.com", "https://github.com/o/r/actions/runs/1/job/2"},
		{" https://github.com/x ", "github.com", "https://github.com/x"},
		{"https://ghe.example/o/r/runs/3", "ghe.example", "https://ghe.example/o/r/runs/3"},
		{"https://ci.other.example/run/1", "github.com", ""},
		{"https://github.com/x", "", ""},
		{"https://user@github.com/x", "github.com", ""},
		{"ftp://github.com/x", "github.com", ""},
		{"https://github.com/a b", "github.com", ""},
		{"https://github.com/<x>", "github.com", ""},
		{"https://github.com/x)", "github.com", ""},
	} {
		if got := DependencyBotRedSafeURL(tc.in, tc.host); got != tc.want {
			t.Errorf("DependencyBotRedSafeURL(%q, %q) = %q, want %q", tc.in, tc.host, got, tc.want)
		}
	}
	if got := DependencyBotRedURLHost("https://github.com/acme/widgets/pull/42"); got != "github.com" {
		t.Fatalf("host = %q", got)
	}
	if got := DependencyBotRedURLHost(""); got != "" {
		t.Fatalf("empty URL host = %q", got)
	}
}

// The cheap excerpt filter drops exit-code-only lines.
func TestDependencyBotRedExcerptDropsExitCodeLines(t *testing.T) {
	got := DependencyBotRedExcerpt("build: Process completed with exit code 1.\nbuild: undefined: x\ntest: Process completed with exit code 2", []string{"build", "test"})
	if len(got) != 1 || got[0] != "build: undefined: x" {
		t.Fatalf("excerpt = %q, want only the real error", got)
	}
	if got := DependencyBotRedExcerpt("build: Process completed with exit code 1.", []string{"build"}); len(got) != 0 {
		t.Fatalf("excerpt = %q, want nothing", got)
	}
}

// Reproduction counts failures within one check suite only, a passing run
// on the head marks the check flaky, and the newest failing run's
// failure-level annotations are kept without warnings or exit-code lines.
func TestDependencyBotRedRunFacts(t *testing.T) {
	runs := map[string]string{
		// Two attempts of one workflow run (suite 10), the second started
		// after the first finished: reproduced.
		"build": `[
			{"id":1,"name":"build","head_sha":"h1","status":"completed","conclusion":"failure","started_at":"2026-11-25T10:00:00Z","completed_at":"2026-11-25T10:30:00Z","check_suite":{"id":10}},
			{"id":2,"name":"build","head_sha":"h1","status":"completed","conclusion":"failure","started_at":"2026-11-25T11:00:00Z","completed_at":"2026-11-25T11:30:00Z","check_suite":{"id":10}}]`,
		// Two failures in different suites, one after the other: not
		// reproduced.
		"lint": `[
			{"id":3,"name":"lint","head_sha":"h1","status":"completed","conclusion":"failure","started_at":"2026-11-25T10:00:00Z","completed_at":"2026-11-25T10:30:00Z","check_suite":{"id":20}},
			{"id":4,"name":"lint","head_sha":"h1","status":"completed","conclusion":"failure","started_at":"2026-11-25T11:00:00Z","completed_at":"2026-11-25T11:30:00Z","check_suite":{"id":21}}]`,
		// Two same-named failing runs in one suite that overlap in time (a
		// matrix, or duplicate names in one attempt): not reproduced.
		"matrix": `[
			{"id":8,"name":"matrix","head_sha":"h1","status":"completed","conclusion":"failure","started_at":"2026-11-25T10:00:00Z","completed_at":"2026-11-25T10:30:00Z","check_suite":{"id":40}},
			{"id":9,"name":"matrix","head_sha":"h1","status":"completed","conclusion":"failure","started_at":"2026-11-25T10:01:00Z","completed_at":"2026-11-25T10:31:00Z","check_suite":{"id":40}}]`,
		// Fail, pass, fail in one suite: flaky.
		"test": `[
			{"id":5,"name":"test","head_sha":"h1","status":"completed","conclusion":"failure","started_at":"2026-11-25T09:00:00Z","check_suite":{"id":30}},
			{"id":6,"name":"test","head_sha":"h1","status":"completed","conclusion":"success","started_at":"2026-11-25T09:30:00Z","check_suite":{"id":30}},
			{"id":7,"name":"test","head_sha":"h1","status":"completed","conclusion":"failure","started_at":"2026-11-25T10:00:00Z","check_suite":{"id":30}}]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/widgets/commits/h1/check-runs":
			if r.URL.Query().Get("filter") != "all" {
				t.Errorf("filter = %q, want all", r.URL.Query().Get("filter"))
			}
			_, _ = io.WriteString(w, `{"total_count":2,"check_runs":`+runs[r.URL.Query().Get("check_name")]+`}`)
		case r.URL.Path == "/repos/acme/widgets/check-runs/2/annotations":
			_, _ = io.WriteString(w, `[
				{"annotation_level":"failure","message":"undefined: lib.OldName\nmore detail"},
				{"annotation_level":"warning","message":"Node 16 is deprecated"},
				{"annotation_level":"failure","message":"Process completed with exit code 1."}]`)
		case strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/check-runs/"):
			_, _ = io.WriteString(w, `[]`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widgets"})
	got, err := c.DependencyBotRedRunFacts(context.Background(), "acme/widgets", "h1", []string{"build", "lint", "matrix", "test"})
	if err != nil {
		t.Fatal(err)
	}
	if b := got["build"]; !b.Reproduced || b.Passed || !b.FailureStartedAt.Equal(time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC)) ||
		len(b.Errors) != 1 || b.Errors[0] != "undefined: lib.OldName" {
		t.Fatalf("build = %+v, want reproduced in one suite, the newest start, and only the real error", b)
	}
	if l := got["lint"]; l.Reproduced || l.Passed {
		t.Fatalf("lint = %+v, want failures in different suites not to count", l)
	}
	if m := got["matrix"]; m.Reproduced {
		t.Fatalf("matrix = %+v, want overlapping failures in one suite not to count", m)
	}
	if ts := got["test"]; !ts.Passed {
		t.Fatalf("test = %+v, want the passing run to mark it flaky", ts)
	}
}

// The base tip state records a finish time only for a run that concluded
// success: a skipped or neutral run with a newer completed_at is completed
// and not red (the stale-red verdict is unchanged) but proves no pass.
func TestStaleRedBaseStateForSucceededAt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widgets/git/ref/heads/main":
			_, _ = io.WriteString(w, `{"ref":"refs/heads/main","object":{"sha":"base1","type":"commit"}}`)
		case "/repos/acme/widgets/commits/base1/check-runs":
			_, _ = io.WriteString(w, `{"total_count":3,"check_runs":[
				{"id":1,"name":"build","head_sha":"base1","status":"completed","conclusion":"success","completed_at":"2026-11-25T12:00:00Z","app":{"id":15368}},
				{"id":2,"name":"lint","head_sha":"base1","status":"completed","conclusion":"skipped","completed_at":"2026-11-25T13:00:00Z","app":{"id":15368}},
				{"id":3,"name":"docs","head_sha":"base1","status":"completed","conclusion":"neutral","completed_at":"2026-11-25T13:00:00Z","app":{"id":15368}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widgets"})
	st, exists, err := c.StaleRedBaseStateFor(context.Background(), "acme/widgets", "main")
	if err != nil || !exists {
		t.Fatalf("StaleRedBaseStateFor = (%v, %v)", exists, err)
	}
	if want := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC); !st.SucceededAt["build"].Equal(want) {
		t.Fatalf("SucceededAt = %v, want %v", st.SucceededAt["build"], want)
	}
	for _, name := range []string{"lint", "docs"} {
		if _, ok := st.SucceededAt[name]; ok {
			t.Fatalf("%s: a skipped or neutral run recorded a success time", name)
		}
		if !st.Completed[name] || st.Red[name] {
			t.Fatalf("%s: completed=%v red=%v, want the stale-red view unchanged", name, st.Completed[name], st.Red[name])
		}
	}
	if v, _ := StaleRedBaseVerdictFor([]string{"lint"}, st); v != StaleRedBaseClears {
		t.Fatalf("stale-red verdict for a skipped base run = %s, want unchanged (clears)", v)
	}
}

// 404 and 410, bare or wrapped, mean gone; any other error does not.
func TestDependencyBotRedNotFound(t *testing.T) {
	notFound := &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}}
	gone := &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusGone}}
	if !DependencyBotRedNotFound(notFound) || !DependencyBotRedNotFound(fmt.Errorf("reading: %w", gone)) {
		t.Fatal("404 or a wrapped 410 must read as gone")
	}
	if DependencyBotRedNotFound(&gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusBadGateway}}) || DependencyBotRedNotFound(nil) {
		t.Fatal("a 502 or no error must not read as gone")
	}
}
