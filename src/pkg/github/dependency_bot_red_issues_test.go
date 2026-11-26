package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/iotest"
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
	if !strings.HasPrefix(got, "Dependency-bot PR #42 ["+DependencyBotRedChecksKey([]string{"lint: go", "build", "test", "e2e"})+"] needs a repository change to pass ") || !strings.HasSuffix(got, " and 1 more") {
		t.Fatalf("title = %q", got)
	}
	// Two failing sets of one PR that share the listed checks and the count
	// still get different titles and dedupe stems.
	other := DependencyBotRedIssueTitle(42, []string{"lint: go", "build", "test", "unit"})
	if got == other || canonicalIssueSubject(got) == canonicalIssueSubject(other) {
		t.Fatalf("two check sets share a title or dedupe stem: %q", got)
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
		"`main` at `fedcba987654`, where the newest run of each of these checks passed, starting after this head's failing run started.",
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

// The cross-head path says which heads failed and does not claim the head
// contains the base tip; the fixed Reproduced wording names the previous
// head, shortened and neutralized.
func TestRenderDependencyBotRedIssueBodyCrossHead(t *testing.T) {
	reproduced := DependencyBotRedCrossHeadReproduced(" b00c45bcd70a5e114a04 ")
	if want := "the same checks failed on the previous head `b00c45bcd70a` and again on this head after it was brought up to date with the base, and no run of these checks passed on this head."; reproduced != want {
		t.Fatalf("reproduced = %q, want %q", reproduced, want)
	}
	if got := DependencyBotRedCrossHeadReproduced("ab`<c>"); strings.ContainsAny(got[strings.Index(got, "`")+1:strings.LastIndex(got, "`")], "`<>") {
		t.Fatalf("previous head not neutralized: %q", got)
	}
	body := RenderDependencyBotRedIssueBody(DependencyBotRedIssueFacts{
		PRNumber:        1486,
		Bot:             "mergeraptor",
		ForgeHost:       "github.com",
		ChecksURL:       "https://github.com/o/r/pull/1486/checks",
		HeadSHA:         "b1d71454e2671a054efb",
		BaseRef:         "main",
		BaseSHA:         "fedcba9876543210fedc",
		Checks:          []string{"Build Utah"},
		PreviousHeadSHA: "b00c45bcd70a5e114a04",
		MergeBaseSHA:    "0a1b2c3d4e5f60718293",
		Regenerated:     true,
		Reproduced:      reproduced,
	})
	for _, want := range []string{
		"- Reproduced: " + reproduced,
		"- The previous head `b00c45bcd70a` failed the same checks. This head contains base commit `0a1b2c3d4e5f` that the previous head did not, every one of these checks passed on that base commit itself, and it fails them again.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body has no %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{"The head already contains that base commit", "already regenerated this branch once", "\u2014", "\u2013", "@"} {
		if strings.Contains(body, bad) {
			t.Errorf("cross-head body contains %q:\n%s", bad, body)
		}
	}
	if err := validateIssueTemplateFilled(DependencyBotRedIssueTitle(1486, []string{"Build Utah"}), body); err != nil {
		t.Fatalf("the issue template guard refuses the body: %v", err)
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
			{"id":4,"name":"lint","head_sha":"h1","status":"completed","conclusion":"failure","started_at":"2026-11-25T11:00:00Z","completed_at":"2026-11-25T11:30:00Z","check_suite":{"id":21},"app":{"slug":"other-ci"}}]`,
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
				{"annotation_level":"failure","message":"`+strings.Repeat("x", 290)+` ghp_supersecrettokenvalue000000000000 tail"},
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
		len(b.Errors) != 2 || b.Errors[0] != "undefined: lib.OldName" {
		t.Fatalf("build = %+v, want reproduced in one suite, the newest start, and only the real errors", b)
	}
	// Masked before the 300-byte cut: no prefix of the token survives the
	// cut.
	if e := got["build"].Errors[1]; strings.Contains(e, "ghp_") || len(e) > 300 {
		t.Fatalf("annotation not masked before the cut: %q", e)
	}
	if l := got["lint"]; l.Reproduced || l.Passed {
		t.Fatalf("lint = %+v, want failures in different suites not to count", l)
	}
	// The newest failing run's id is its Actions job's, unless another CI
	// app ran it.
	if b, l := got["build"], got["lint"]; b.JobID != 2 || l.JobID != 0 {
		t.Fatalf("job ids: build=%d lint=%d, want 2 and none", b.JobID, l.JobID)
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
				{"id":1,"name":"build","head_sha":"base1","status":"completed","conclusion":"success","started_at":"2026-11-25T11:40:00Z","completed_at":"2026-11-25T12:00:00Z","app":{"id":15368}},
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
	if want := time.Date(2026, 11, 25, 11, 40, 0, 0, time.UTC); !st.SucceededStartedAt["build"].Equal(want) {
		t.Fatalf("SucceededStartedAt = %v, want %v", st.SucceededStartedAt["build"], want)
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

// The compare returns the head's lag and the merge base; the base state at
// a commit reads that commit's runs; a passing run of a check on a head is
// found among every attempt.
func TestDependencyBotRedCompareBaseAtAndPassed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widgets/compare/tip1...h2":
			_, _ = io.WriteString(w, `{"behind_by":3,"ahead_by":2,"merge_base_commit":{"sha":"mb1"}}`)
		case "/repos/acme/widgets/commits/mb1/check-runs":
			_, _ = io.WriteString(w, `{"total_count":2,"check_runs":[
				{"id":1,"name":"build","head_sha":"mb1","status":"completed","conclusion":"success","completed_at":"2026-11-25T12:00:00Z","app":{"id":15368}},
				{"id":2,"name":"test","head_sha":"mb1","status":"completed","conclusion":"failure","completed_at":"2026-11-25T12:00:00Z","app":{"id":15368}}]}`)
		case "/repos/acme/widgets/commits/h1/check-runs":
			if r.URL.Query().Get("filter") != "all" {
				t.Errorf("filter = %q, want all", r.URL.Query().Get("filter"))
			}
			switch r.URL.Query().Get("check_name") {
			case "test":
				_, _ = io.WriteString(w, `{"total_count":2,"check_runs":[
					{"id":5,"name":"test","head_sha":"h1","status":"completed","conclusion":"failure"},
					{"id":6,"name":"test","head_sha":"h1","status":"completed","conclusion":"success"}]}`)
			default:
				_, _ = io.WriteString(w, `{"total_count":1,"check_runs":[
					{"id":7,"name":"build","head_sha":"h1","status":"completed","conclusion":"failure"},
					{"id":8,"name":"build","head_sha":"h1","status":"in_progress"}]}`)
			}
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widgets"})
	cmp, err := c.DependencyBotRedCompare(context.Background(), "acme/widgets", "tip1", "h2")
	if err != nil || cmp.BehindBy != 3 || cmp.MergeBase != "mb1" {
		t.Fatalf("compare = %+v, %v; want behind 3 and merge base mb1", cmp, err)
	}
	st, err := c.DependencyBotRedBaseStateAt(context.Background(), "acme/widgets", "mb1")
	if err != nil || st.SHA != "mb1" || st.SucceededAt["build"].IsZero() || !st.Red["test"] {
		t.Fatalf("base state at mb1 = %+v, %v; want build passed and test red", st, err)
	}
	if got, err := c.DependencyBotRedAnyPassed(context.Background(), "acme/widgets", "h1", []string{"build"}); err != nil || got != "" {
		t.Fatalf("build passed = %q, %v; want none", got, err)
	}
	if got, err := c.DependencyBotRedAnyPassed(context.Background(), "acme/widgets", "h1", []string{"build", "test"}); err != nil || got != "test" {
		t.Fatalf("passed = %q, %v; want test", got, err)
	}
	var nilClient *Client
	if _, err := nilClient.DependencyBotRedCompare(context.Background(), "acme/widgets", "a", "b"); err == nil {
		t.Fatal("a nil client compared")
	}
}

// A lasting 4xx is final; 429, a rate-limited 403, a 5xx or an unknown
// error is not.
func TestDependencyBotRedFinalError(t *testing.T) {
	resp := func(code int, hdr map[string]string, msg string) error {
		r := &http.Response{StatusCode: code, Header: http.Header{}}
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return fmt.Errorf("creating: %w", &gh.ErrorResponse{Response: r, Message: msg})
	}
	for name, tc := range map[string]struct {
		err   error
		final bool
	}{
		"410 issues disabled":    {resp(410, nil, "Issues are disabled for this repo"), true},
		"404":                    {resp(404, nil, "Not Found"), true},
		"422":                    {resp(422, nil, "Validation Failed"), true},
		"403 forbidden":          {resp(403, nil, "Resource not accessible by integration"), true},
		"403 retry-after":        {resp(403, map[string]string{"Retry-After": "60"}, "slow down"), false},
		"403 remaining zero":     {resp(403, map[string]string{"X-RateLimit-Remaining": "0"}, "forbidden"), false},
		"403 rate limit message": {resp(403, nil, "You have exceeded a secondary rate limit"), false},
		"429":                    {resp(429, nil, "Too Many Requests"), false},
		"502":                    {resp(502, nil, "Bad Gateway"), false},
		"go-github rate limit":   {&gh.RateLimitError{Response: &http.Response{StatusCode: 403}}, false},
		"go-github abuse limit":  {&gh.AbuseRateLimitError{Response: &http.Response{StatusCode: 403}}, false},
		"plain error":            {errors.New("connection reset"), false},
		"no error":               {nil, false},
	} {
		if got := DependencyBotRedFinalError(tc.err); got != tc.final {
			t.Errorf("%s: final = %v, want %v", name, got, tc.final)
		}
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

// Without any line to quote the body still reads cleanly: no fence, a
// sentence pointing at the job's own log, the failed steps named
// (neutralized), the missing-secret sentence only for a PR whose runs lack
// the repository's secrets, and the template guard admits it.
func TestRenderDependencyBotRedIssueBodyWithoutExcerpt(t *testing.T) {
	f := DependencyBotRedIssueFacts{
		PRNumber:   1471,
		Bot:        "mergeraptor",
		ForgeHost:  "github.com",
		HeadSHA:    "0123456789abcdef0123",
		BaseRef:    "main",
		BaseSHA:    "fedcba9876543210fedc",
		Checks:     []string{"contract"},
		Runs:       []DependencyBotRedRun{{Check: "contract", URL: "https://github.com/o/r/actions/runs/45432660330/job/1"}},
		FailedJobs: []DependencyBotRedFailedJob{{Check: "contract", Job: "contract", Steps: []string{"Run `pytest` <all>"}}, {Check: "lint", Job: "Lint"}},
		Reproduced: "each failing check failed again within one check suite of this head.",
	}
	body := RenderDependencyBotRedIssueBody(f)
	for _, want := range []string{
		"- Failed steps:\n  - `contract`: `Run 'pytest' \u2039all\u203a`\n",
		"The failing runs left no annotation beyond the exit code; the failing job's own log, linked above, has the details.\n",
		"https://github.com/o/r/actions/runs/45432660330/job/1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body has no %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{"```", "Lint", "Actions secrets", "\u2014", "\u2013", "@"} {
		if strings.Contains(body, bad) {
			t.Errorf("body contains %q:\n%s", bad, body)
		}
	}
	if err := validateIssueTemplateFilled(DependencyBotRedIssueTitle(1471, []string{"contract"}), body); err != nil {
		t.Fatalf("the issue template guard refuses the body: %v", err)
	}
	f.WithoutSecrets = true
	if body := RenderDependencyBotRedIssueBody(f); !strings.Contains(body, "so a secret the workflow needs is a possible cause.") {
		t.Fatalf("no missing-secret sentence for a PR without the repository's secrets:\n%s", body)
	}
	// With annotations to quote, neither sentence.
	f.ErrorLines = []string{"contract: boom"}
	if body := RenderDependencyBotRedIssueBody(f); strings.Contains(body, "own log") || strings.Contains(body, "possible cause") {
		t.Fatalf("no-quote sentences with a quote:\n%s", body)
	}
}

// The job log filter: GitHub's timestamps and color codes go, the step's
// echoed script inside its "Run" group never counts, log commands other
// than ##[error] do not count, and the bare exit code never counts. The
// lines are for comparing only, so they are not masked here.
func TestDependencyBotRedLogErrorLines(t *testing.T) {
	log := strings.Join([]string{
		"\ufeff2026-11-25T18:20:00.0000000Z ##[group]Run pytest -q || echo \"tests failed\"",
		"2026-11-25T18:20:00.0000001Z pytest -q || echo \"tests failed\"",
		"2026-11-25T18:20:00.0000002Z shell: /usr/bin/bash -e {0}",
		"2026-11-25T18:20:00.0000003Z ##[endgroup]",
		"2026-11-25T18:20:01.0000000Z collected 300 items",
		"2026-11-25T18:20:02.0000000Z \x1b[31mFAILED\x1b[0m tests/test_release_trigger.py::test_pin - AssertionError: '9eccc77' != '3ab43283'",
		"2026-11-25T18:20:02.5000000Z E   AssertionError: '9eccc77' != '3ab43283'",
		"2026-11-25T18:20:02.6000000Z E   AssertionError: '9eccc77' != '3ab43283'",
		"2026-11-25T18:20:03.0000000Z ##[warning]Node.js 16 actions are deprecated, error: none",
		"2026-11-25T18:20:03.5000000Z [command]/usr/bin/git fetch error: none",
		"2026-11-25T18:20:04.0000000Z curl: (22) The requested URL returned error: 404\r",
		"2026-11-25T18:20:04.5000000Z ##[error]Bad credentials",
		"2026-11-25T18:20:05.0000000Z ##[error]Process completed with exit code 1.",
	}, "\n")
	got, err := dependencyBotRedLogErrorLines(strings.NewReader(log), false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"FAILED tests/test_release_trigger.py::test_pin - AssertionError: '9eccc77' != '3ab43283'",
		"E   AssertionError: '9eccc77' != '3ab43283'",
		"curl: (22) The requested URL returned error: 404",
		"Bad credentials",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines = %q, want %q", got, want)
	}

	// Only the last lines are kept, an overlong line is cut without losing
	// the lines after it, and each kept line is at most 300 bytes.
	var b strings.Builder
	for i := 0; i < dependencyBotRedJobLogMaxLines+3; i++ {
		fmt.Fprintf(&b, "FAILED test_%d\n", i)
	}
	b.WriteString("FAILED " + strings.Repeat("x", 3*dependencyBotRedJobLogMaxLineBytes) + "\n")
	b.WriteString("FAILED test_last\n")
	got, _ = dependencyBotRedLogErrorLines(strings.NewReader(b.String()), false)
	if len(got) != dependencyBotRedJobLogMaxLines || got[len(got)-1] != "FAILED test_last" || len(got[len(got)-2]) > 300 || got[0] != "FAILED test_5" {
		t.Fatalf("tail = %q", got)
	}
	if got, _ := dependencyBotRedLogErrorLines(strings.NewReader("all good\n##[error]Process completed with exit code 2.\n"), false); len(got) != 0 {
		t.Fatalf("lines = %q, want none", got)
	}
	// A ranged read drops its first line, which may start mid-line.
	if got, _ := dependencyBotRedLogErrorLines(strings.NewReader("ror: half a line FAILED\nFAILED test_real\n"), true); len(got) != 1 || got[0] != "FAILED test_real" {
		t.Fatalf("ranged lines = %q", got)
	}
	// A log that breaks off (a reset, a timeout) is an error, never a
	// complete read with its tail cut; the byte cap's EOF is a clean end.
	broken := io.MultiReader(strings.NewReader("FAILED test_a\n"), iotest.ErrReader(errors.New("connection reset by peer")))
	if got, err := dependencyBotRedLogErrorLines(broken, false); err == nil || got != nil {
		t.Fatalf("broken log = %q, %v, want an error and no lines", got, err)
	}
	longBroken := io.MultiReader(strings.NewReader("FAILED "+strings.Repeat("x", 2*dependencyBotRedJobLogMaxLineBytes)), iotest.ErrReader(errors.New("reset")))
	if _, err := dependencyBotRedLogErrorLines(longBroken, false); err == nil {
		t.Fatal("a log that broke off inside an overlong line read as whole")
	}
	if got, err := dependencyBotRedLogErrorLines(io.LimitReader(strings.NewReader("FAILED test_a\nFAILED test_b\n"), 14), false); err != nil || len(got) != 1 {
		t.Fatalf("capped log = %q, %v, want the first line and no error", got, err)
	}
	res := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(io.MultiReader(strings.NewReader("FAILED x\n"), iotest.ErrReader(errors.New("unexpected EOF"))))}
	if _, done, read, err := dependencyBotRedReadLog(res, false); err == nil || read || !done {
		t.Fatalf("broken download: done=%v read=%v err=%v, want a finished attempt with an error", done, read, err)
	}
	// Normalized for comparing; generic lines never count.
	if n := DependencyBotRedNormalizeLine("E   AssertionError: at line 12,  took 3.5s"); n != "e assertionerror: at line 0, took 0.0s" {
		t.Fatalf("normalized = %q", n)
	}
	// PR-specific ids fold: a digest, a commit, a UUID.
	a := DependencyBotRedNormalizeLine("secret missing for run 3f2a9c1d-0b4e-4c8a-9e7f-12ab34cd56ef at 9eccc77aB0")
	b2 := DependencyBotRedNormalizeLine("secret missing for run 0d1e2f3a-4b5c-4d6e-8f9a-abcdefabcdef at deadbee1234")
	if a != b2 || a != "secret missing for run 0 at 0" {
		t.Fatalf("ids not folded: %q vs %q", a, b2)
	}
	for _, g := range []string{"exit status 1", "The operation was canceled.", "Error: Process completed with exit code 22.", "make: *** [Makefile:12: test] Error 2", "Error: The process '/usr/bin/git' failed with exit code 128"} {
		if !DependencyBotRedGenericLine(DependencyBotRedNormalizeLine(g)) {
			t.Errorf("%q is not generic", g)
		}
	}
	if DependencyBotRedGenericLine(DependencyBotRedNormalizeLine("curl: (22) The requested URL returned error: 404")) {
		t.Error("a real error reads as generic")
	}
}

// The job log read, for comparing only: the tail is asked for with a
// suffix Range (its first, cut line dropped), storage refusing the Range
// is asked again without it, a job with no log (404) is no error, a 5xx on
// the link or the download is temporary, and no error ever carries the
// signed link. The download gets no GitHub token. DependencyBotRedJobSteps
// names the failed steps and leaves them out on any error.
func TestDependencyBotRedJobErrorLines(t *testing.T) {
	const sig = "sig=SIGNED-SECRET"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widgets/actions/jobs/11/logs", "/repos/acme/widgets/actions/jobs/12/logs",
			"/repos/acme/widgets/actions/jobs/15/logs", "/repos/acme/widgets/actions/jobs/16/logs":
			id := strings.Split(r.URL.Path, "/")[6]
			w.Header().Set("Location", srv.URL+"/download/"+id+"?"+sig)
			w.WriteHeader(http.StatusFound)
		case "/download/11":
			if r.Header.Get("Authorization") != "" {
				t.Errorf("the download got an Authorization header")
			}
			if got := r.Header.Get("Range"); got != fmt.Sprintf("bytes=-%d", dependencyBotRedJobLogMaxBytes) {
				t.Errorf("Range = %q, want the last %d bytes", got, dependencyBotRedJobLogMaxBytes)
			}
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "half a line FAILED x\n2026-11-25T18:20:02.0000000Z E   AssertionError: 'a' != 'b' at line 12\n"+
				"2026-11-25T18:20:03.0000000Z make: *** [Makefile:12: test] Error 2\n2026-11-25T18:20:05.0000000Z ##[error]Process completed with exit code 1.\n")
		case "/download/12":
			if r.Header.Get("Range") != "" {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			_, _ = io.WriteString(w, "FAILED test_one\n")
		case "/download/15":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/download/16":
			// The connection drops: a network error naming the link.
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
		case "/repos/acme/widgets/actions/jobs/14/logs":
			w.WriteHeader(http.StatusInternalServerError)
		case "/repos/acme/widgets/actions/jobs/17/logs":
			// A Location go-github's own url.Parse rejects.
			w.Header().Set("Location", ":bad-link/download/17?"+sig)
			w.WriteHeader(http.StatusFound)
		case "/repos/acme/widgets/actions/jobs/12":
			_, _ = io.WriteString(w, `{"id":12,"name":"contract","steps":[{"name":"Set up job","conclusion":"success"},{"name":"Run contract tests","conclusion":"failure"},{"name":"Post","conclusion":"skipped"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widgets"})
	ctx := context.Background()

	got, err := c.DependencyBotRedJobErrorLines(ctx, "acme/widgets", 11)
	if err != nil || !got.Read || len(got.Lines) != 1 || got.Lines[0] != "e assertionerror: 'a' != 'b' at line 0" {
		t.Fatalf("job 11 = %+v, %v, want the tail's one real error line, normalized", got, err)
	}
	got, err = c.DependencyBotRedJobErrorLines(ctx, "acme/widgets", 12)
	if err != nil || !got.Read || len(got.Lines) != 1 || got.Lines[0] != "failed test_one" {
		t.Fatalf("job 12 = %+v, %v, want the whole log read after the 416", got, err)
	}
	got, err = c.DependencyBotRedJobErrorLines(ctx, "acme/widgets", 13)
	if err != nil || got.Read || len(got.Lines) != 0 {
		t.Fatalf("job 13 (no log) = %+v, %v, want nothing read and no error", got, err)
	}
	for _, id := range []int64{14, 15, 16, 17} {
		_, err := c.DependencyBotRedJobErrorLines(ctx, "acme/widgets", id)
		if err == nil || DependencyBotRedFinalError(err) {
			t.Fatalf("job %d: err = %v, want a temporary error", id, err)
		}
		if strings.Contains(err.Error(), "SIGNED-SECRET") || strings.Contains(err.Error(), "/download/") {
			t.Fatalf("job %d: the error carries the signed link: %v", id, err)
		}
	}

	if fj := c.DependencyBotRedJobSteps(ctx, "acme/widgets", 12); fj.Job != "contract" || len(fj.Steps) != 1 || fj.Steps[0] != "Run contract tests" {
		t.Fatalf("job 12 steps = %+v, want the failed step", fj)
	}
	if fj := c.DependencyBotRedJobSteps(ctx, "acme/widgets", 13); fj.Job != "" || len(fj.Steps) != 0 {
		t.Fatalf("job 13 steps = %+v, want nothing on a failed read", fj)
	}
}

// dependencyBotRedSafeErr keeps only the host of any *url.Error, wrapped or
// nested, so a signed link never reaches an error string.
func TestDependencyBotRedSafeErr(t *testing.T) {
	inner := &url.Error{Op: "Get", URL: "https://blob.example/log?sig=SIGNED-SECRET", Err: errors.New("EOF")}
	nested := &url.Error{Op: "Get", URL: "https://other.example/x?sig=SIGNED-SECRET", Err: inner}
	for _, err := range []error{inner, fmt.Errorf("wrapped %s: %w", inner.URL, inner), nested} {
		got := dependencyBotRedSafeErr(err)
		if got == nil || strings.Contains(got.Error(), "SIGNED-SECRET") || strings.Contains(got.Error(), "/log") {
			t.Fatalf("safe err = %v", got)
		}
	}
	if got := dependencyBotRedSafeErr(inner).Error(); got != "Get blob.example: EOF" {
		t.Fatalf("safe err = %q, want the operation, host and cause", got)
	}
	plain := errors.New("HTTP 503")
	if dependencyBotRedSafeErr(plain) != plain || dependencyBotRedSafeErr(nil) != nil {
		t.Fatal("an error with no link changed")
	}
}

// Which text names the PR: #N on its own, owner/repo#N with a clean left
// edge, and links to the same number as a pull request or an issue, with or
// without the scheme or www.
func TestDependencyBotRedNamesPR(t *testing.T) {
	re := dependencyBotRedNamesPR("acme/app", DependencyBotRedPersonIssueQuery{Number: 12, URL: "https://github.com/acme/app/pull/12"})
	for text, want := range map[string]bool{
		"#12":                                 true,
		"see (#12).":                          true,
		"acme/app#12":                         true,
		"Acme/App#12 again":                   true,
		"notacme/app#12":                      false,
		"other/acme/app#12":                   false,
		"other/app#12":                        false,
		"#123":                                false,
		"word#12":                             false,
		"https://github.com/acme/app/pull/12": true,
		"http://www.github.com/acme/app/pull/12.":   true,
		"github.com/acme/app/issues/12":             true,
		"https://github.com/acme/app/pull/123":      false,
		"https://notgithub.com/acme/app/pull/12":    false,
		"https://github.com/other/acme/app/pull/12": false,
	} {
		if got := re.MatchString(text); got != want {
			t.Errorf("%q names the PR = %v, want %v", text, got, want)
		}
	}
}

// The person-filed issue counts only when a member (owner, member,
// collaborator) who is not the PR's author, not a bot and not the hive
// opened it, open, in the same repository, since the PR opened, naming the
// PR (#N, owner/repo#N or its URL). The newest such issue wins.
func TestDependencyBotRedPersonIssue(t *testing.T) {
	ev := func(number int, state, login, kind, assoc, created, title, body, repoField string) string {
		return fmt.Sprintf(`{"event":"cross-referenced","source":{"type":"issue","issue":{"number":%d,"state":%q,"html_url":"https://github.com/acme/widgets/issues/%d",`+
			`"user":{"login":%q,"type":%q},"author_association":%q,"created_at":%q,"title":%q,"body":%q,%s}}}`,
			number, state, number, login, kind, assoc, created, title, body, repoField)
	}
	same := `"repository":{"full_name":"acme/widgets"}`
	events := []string{
		`{"event":"labeled"}`,
		ev(12, "open", "alice", "User", "MEMBER", "2026-11-24T10:00:00Z", "Bump #42 fails contract", "", same),
		ev(15, "open", "bob", "User", "COLLABORATOR", "2026-11-25T10:00:00Z", "contract red", "on https://github.com/acme/widgets/pull/42.", `"repository_url":"https://api.github.com/repos/acme/widgets"`),
		ev(20, "open", "frank", "User", "OWNER", "2026-11-25T09:00:00Z", "tracking", "acme/widgets#42 needs a pin", same),
		ev(16, "open", "carol", "User", "NONE", "2026-11-26T10:00:00Z", "#42", "", same),
		ev(17, "open", "zed", "User", "OWNER", "2026-11-26T10:00:00Z", "#42", "", same),
		ev(18, "open", "dave", "User", "MEMBER", "2026-11-19T10:00:00Z", "#42", "", same),
		ev(19, "open", "erin", "User", "MEMBER", "2026-11-26T10:00:00Z", "see #421", "and other/repo#42, #4200", same),
		ev(3, "open", "renovate[bot]", "Bot", "NONE", "2026-11-26T10:00:00Z", "Dependency Dashboard", "#42", same),
		ev(2, "closed", "gina", "User", "MEMBER", "2026-11-26T10:00:00Z", "#42", "", same),
		ev(4, "open", "hal", "User", "MEMBER", "2026-11-26T10:00:00Z", "#42", "", `"repository":{"full_name":"other/repo"}`),
		ev(5, "open", "hive-runner", "User", "MEMBER", "2026-11-26T10:00:00Z", "#42", "", same),
		`{"event":"cross-referenced","source":{"type":"issue","issue":{"number":1,"state":"open","user":{"login":"ivy","type":"User"},"author_association":"MEMBER","created_at":"2026-11-26T10:00:00Z","title":"#42","repository":{"full_name":"acme/widgets"},"pull_request":{"url":"https://api.github.com/repos/acme/widgets/pulls/1"}}}}`,
	}
	body := "[" + strings.Join(events, ",") + "]"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widgets/issues/42/timeline":
			if body == "" {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_, _ = io.WriteString(w, body)
		case "/repos/acme/widgets/issues/7":
			_, _ = io.WriteString(w, `{"number":7,"state":"open"}`)
		case "/repos/acme/widgets/issues/8":
			_, _ = io.WriteString(w, `{"number":8,"state":"closed"}`)
		case "/repos/acme/widgets/issues/10":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widgets"})
	c.additionalBotLogins = []string{"hive-runner"}
	ctx := context.Background()
	q := DependencyBotRedPersonIssueQuery{Number: 42, Author: "zed", URL: "https://github.com/acme/widgets/pull/42", Since: time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)}
	got, err := c.DependencyBotRedPersonIssue(ctx, "acme/widgets", q)
	if err != nil || got.Number != 15 {
		t.Fatalf("person issue = %+v, %v, want the newest member's #15", got, err)
	}
	if got, err := c.DependencyBotRedPersonIssue(ctx, "acme/widgets", DependencyBotRedPersonIssueQuery{Number: 42, Author: "zed"}); err != nil || got.Number != 0 {
		t.Fatalf("no PR creation time = %+v, %v, want none", got, err)
	}
	body = "[" + strings.Join(events[4:], ",") + "]"
	if got, err := c.DependencyBotRedPersonIssue(ctx, "acme/widgets", q); err != nil || got.Number != 0 {
		t.Fatalf("only ignored issues = %+v, %v, want none", got, err)
	}
	body = ""
	if _, err := c.DependencyBotRedPersonIssue(ctx, "acme/widgets", q); err == nil {
		t.Fatal("an unreadable timeline gave no error")
	}

	for n, want := range map[int]bool{7: true, 8: false, 9: false} {
		if open, err := c.DependencyBotRedIssueOpen(ctx, "acme/widgets", n); err != nil || open != want {
			t.Fatalf("issue %d open = %v, %v, want %v", n, open, err, want)
		}
	}
	if _, err := c.DependencyBotRedIssueOpen(ctx, "acme/widgets", 10); err == nil {
		t.Fatal("a 500 reading the issue gave no error")
	}
}

// The snapshot's failure-level excerpt keeps failure annotations only: no
// warning, no bare exit code, each check its own messages.
func TestFetchFailureExcerptsFailureLevel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/check-runs/1/annotations":
			_, _ = io.WriteString(w, `[{"annotation_level":"warning","message":"Node.js 20 actions are deprecated"},{"annotation_level":"failure","message":"Process completed with exit code 1."},{"annotation_level":"failure","message":"secret TOKEN is empty"}]`)
		case "/repos/o/r/check-runs/2/annotations":
			_, _ = io.WriteString(w, `[{"annotation_level":"warning","message":"Node.js 20 actions are deprecated"},{"annotation_level":"failure","message":"secret TOKEN is empty"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})
	all, failures := c.fetchFailureExcerpts(context.Background(), "o", "r", []int64{1, 2}, []string{"build", "test"})
	if !strings.Contains(all, "Node.js 20 actions are deprecated") {
		t.Fatalf("excerpt lost the warning: %q", all)
	}
	if failures != "build: secret TOKEN is empty\ntest: secret TOKEN is empty" {
		t.Fatalf("failure-level excerpt = %q", failures)
	}
}
