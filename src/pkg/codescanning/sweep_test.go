package codescanning

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func loadAlerts(t *testing.T, name string) []Alert {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var out []Alert
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

type fakeIssue struct {
	title, body, closeReason, closeComment string
	labels                                 []string
	open                                   bool
}

// fakeForge is an in-memory GitHub: alerts by state, the hive's issues, and
// files by repo@ref/path.
type fakeForge struct {
	open, dismissed []Alert
	listErr         error
	maxPagesSeen    int
	pageLimit       int // alerts beyond this many make the listing incomplete
	issues          map[int]*fakeIssue
	next            int
	files           map[string]string
	upstream        string
	lastCommit      string
	writes          int
	dismissedReads  int
}

func newFakeForge(open []Alert) *fakeForge {
	return &fakeForge{open: open, issues: map[int]*fakeIssue{}, next: 100, files: map[string]string{}}
}

func (f *fakeForge) ListAlerts(_ context.Context, _, state string, maxPages int) ([]Alert, bool, error) {
	if f.listErr != nil {
		return nil, false, f.listErr
	}
	f.maxPagesSeen = maxPages
	if state == "dismissed" {
		f.dismissedReads++
		return f.dismissed, true, nil
	}
	if f.pageLimit > 0 && len(f.open) > f.pageLimit {
		return f.open[:f.pageLimit], false, nil
	}
	return f.open, true, nil
}

func (f *fakeForge) ListIssues(context.Context, string) ([]TrackedIssue, error) {
	var out []TrackedIssue
	nums := make([]int, 0, len(f.issues))
	for n := range f.issues {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	for _, n := range nums {
		is := f.issues[n]
		m, ok := ParseMarker(is.body)
		if !ok {
			continue
		}
		out = append(out, TrackedIssue{Number: n, Open: is.open, Body: is.body, Marker: m})
	}
	return out, nil
}

func (f *fakeForge) CreateIssue(_ context.Context, _, title, body string, labels []string) (int, error) {
	f.writes++
	f.next++
	f.issues[f.next] = &fakeIssue{title: title, body: body, labels: labels, open: true}
	return f.next, nil
}

func (f *fakeForge) EditIssueBody(_ context.Context, _ string, n int, body string) error {
	f.writes++
	f.issues[n].body = body
	return nil
}

func (f *fakeForge) CloseIssue(_ context.Context, _ string, n int, comment, reason string) error {
	f.writes++
	is := f.issues[n]
	is.open, is.closeComment, is.closeReason = false, comment, reason
	return nil
}

func (f *fakeForge) FileAt(_ context.Context, repo, path, ref string) (string, bool, error) {
	c, ok := f.files[repo+"@"+ref+"/"+path]
	return c, ok, nil
}

func (f *fakeForge) Upstream(context.Context, string) (string, error) { return f.upstream, nil }

func (f *fakeForge) LastCommit(context.Context, string, string) (string, error) {
	return f.lastCommit, nil
}

func (f *fakeForge) openIssues() []*fakeIssue {
	var out []*fakeIssue
	for _, is := range f.issues {
		if is.open {
			out = append(out, is)
		}
	}
	return out
}

func run(t *testing.T, f *fakeForge, o Options) Status {
	t.Helper()
	if o.Repo == "" {
		o.Repo = "example/exult"
	}
	if o.Now.IsZero() {
		o.Now = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	}
	st, err := Run(context.Background(), f, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return st
}

func qualifying(alerts []Alert, p Policy) []Alert {
	var out []Alert
	for _, a := range alerts {
		if p.Judge(a) == Qualifies {
			out = append(out, a)
		}
	}
	return out
}

func TestLevelMapsBothSeverityFields(t *testing.T) {
	cases := []struct {
		sec, sev string
		want     Level
	}{
		{"critical", "warning", LevelCritical},
		{"high", "error", LevelHigh},
		{"medium", "error", LevelMedium},
		{"low", "error", LevelLow},
		{"", "error", LevelMedium},
		{"", "warning", LevelLow},
		{"", "note", LevelNote},
		{"", "", LevelNone},
	}
	for _, c := range cases {
		a := Alert{Rule: Rule{SecuritySeverityLevel: c.sec, Severity: c.sev}}
		if got := a.Level(); got != c.want {
			t.Errorf("security=%q severity=%q: level %s, want %s", c.sec, c.sev, got, c.want)
		}
	}
	for in, want := range map[string]Level{"high": LevelHigh, "Critical": LevelCritical, "error": LevelMedium, "warning": LevelLow, "note": LevelNote} {
		if got, ok := ParseLevel(in); !ok || got != want {
			t.Errorf("ParseLevel(%q) = %s, %v; want %s", in, got, ok, want)
		}
	}
	if _, ok := ParseLevel("severe"); ok {
		t.Error("ParseLevel accepted an unknown level")
	}
}

// The real Danathar/exult mix (2026-11-05): 84 high/critical alerts over 13
// rules; SARIF errors, warnings and notes without a security level stay out.
func TestExultHighSelectsTheSecurityAlertsAndGroupsThemSmall(t *testing.T) {
	alerts := loadAlerts(t, "exult-open-alerts.json")
	q := qualifying(alerts, Policy{})
	rules := map[string]bool{}
	for _, a := range q {
		rules[a.Rule.ID] = true
	}
	if len(q) != 84 || len(rules) != 13 {
		t.Fatalf("min_severity high selected %d alerts over %d rules, want 84 over 13", len(q), len(rules))
	}
	groups := Cluster(q)
	covered := map[int]int{}
	for _, g := range groups {
		if len(g.Paths) > MaxFilesPerIssue {
			t.Errorf("group %s spans %d files, max %d", Title(g), len(g.Paths), MaxFilesPerIssue)
		}
		if len(g.Rules) != 1 {
			t.Errorf("code group %s mixes rules %v", Title(g), g.Rules)
		}
		for _, a := range g.Alerts {
			covered[a.Number]++
		}
	}
	for _, a := range q {
		if covered[a.Number] != 1 {
			t.Errorf("alert #%d is in %d groups, want 1", a.Number, covered[a.Number])
		}
	}
	if groups[0].Level() != LevelCritical {
		t.Errorf("first group is %s, want the critical ones first", groups[0].Level())
	}
	if len(groups) < 13 || len(groups) > 30 {
		t.Errorf("%d groups for 84 alerts; want one or a few per rule", len(groups))
	}
}

func TestClusterSplitsByDirectoryAndKeepsDirectoriesTogether(t *testing.T) {
	mk := func(n int, p string) Alert {
		return Alert{Number: n, Tool: Tool{Name: "CodeQL"}, Rule: Rule{ID: "r", SecuritySeverityLevel: "high"}, Instance: Instance{Location: Location{Path: p, StartLine: 1}}}
	}
	var as []Alert
	for i, p := range []string{"a/1.c", "a/2.c", "b/1.c", "b/2.c", "b/3.c", "b/4.c", "c/1.c"} {
		as = append(as, mk(i+1, p))
	}
	got := Cluster(as)
	var paths []string
	for _, g := range got {
		paths = append(paths, strings.Join(g.Paths, " "))
	}
	want := []string{"a/1.c a/2.c", "b/1.c b/2.c b/3.c b/4.c c/1.c"}
	if strings.Join(paths, "|") != strings.Join(want, "|") {
		t.Fatalf("clusters = %q, want %q", paths, want)
	}
	// A single directory over the limit splits into runs in path order.
	as = nil
	for i := 0; i < 7; i++ {
		as = append(as, mk(i+1, ".github/workflows/w"+string(rune('a'+i))+".yml"))
	}
	got = Cluster(as)
	if len(got) != 2 || len(got[0].Paths) != 5 || len(got[1].Paths) != 2 {
		t.Fatalf("7 files in one directory: %d groups (%v)", len(got), got)
	}
}

func TestRunFilesAtMostTheCapAndDedupesAcrossRuns(t *testing.T) {
	alerts := loadAlerts(t, "exult-open-alerts.json")
	f := newFakeForge(alerts)
	want := len(Cluster(qualifying(alerts, Policy{})))
	total := 0
	for pass := 1; pass <= 20; pass++ {
		st := run(t, f, Options{})
		if st.Filed > DefaultMaxNewIssues {
			t.Fatalf("pass %d filed %d issues, cap %d", pass, st.Filed, DefaultMaxNewIssues)
		}
		total += st.Filed
		if st.Filed == 0 {
			if st.Untracked != 0 || st.Tracked != 84 {
				t.Fatalf("settled with untracked=%d tracked=%d, want 0 and 84", st.Untracked, st.Tracked)
			}
			break
		}
		if st.Updated != 0 || st.Closed != 0 {
			t.Fatalf("pass %d updated %d / closed %d issues with no alert change", pass, st.Updated, st.Closed)
		}
	}
	if total != want || len(f.issues) != want {
		t.Fatalf("filed %d issues (%d stored), want %d grouped issues", total, len(f.issues), want)
	}
	seen := map[int]int{}
	for _, is := range f.issues {
		m, _ := ParseMarker(is.body)
		for _, n := range m.Alerts {
			seen[n]++
		}
		if !strings.Contains(is.body, "may be false positives") || !strings.Contains(is.body, "## Acceptance criteria") {
			t.Fatalf("issue body lacks the false-positive warning or acceptance criteria:\n%s", is.body)
		}
	}
	for n, c := range seen {
		if c != 1 {
			t.Errorf("alert #%d is on %d issues", n, c)
		}
	}
	// A restart rebuilds the same state from the markers: nothing new.
	before := f.writes
	if st := run(t, f, Options{}); st.Filed != 0 || f.writes != before {
		t.Fatalf("re-scan wrote %d times (filed %d)", f.writes-before, st.Filed)
	}
}

func TestRunWithNoQualifyingAlertsFilesNothing(t *testing.T) {
	var low []Alert
	for _, a := range loadAlerts(t, "exult-open-alerts.json") {
		if a.Level() < LevelHigh {
			low = append(low, a)
		}
	}
	f := newFakeForge(low)
	st := run(t, f, Options{})
	if f.writes != 0 || st.Filed != 0 || st.Qualifying != 0 || st.OpenAlerts != len(low) {
		t.Fatalf("writes=%d filed=%d qualifying=%d open=%d", f.writes, st.Filed, st.Qualifying, st.OpenAlerts)
	}
	if st.BelowThreshold != len(low) {
		t.Fatalf("below threshold %d, want %d", st.BelowThreshold, len(low))
	}
}

func onlyRule(alerts []Alert, rule string) []Alert {
	var out []Alert
	for _, a := range alerts {
		if a.Rule.ID == rule {
			out = append(out, a)
		}
	}
	return out
}

func TestRunClosesFixedAndDismissedIssuesAndNeverRefiles(t *testing.T) {
	alerts := loadAlerts(t, "exult-open-alerts.json")
	use := onlyRule(alerts, "cpp/use-after-free")        // 1 alert
	scanf := onlyRule(alerts, "cpp/missing-check-scanf") // 8 alerts, 3 files
	f := newFakeForge(append(append([]Alert(nil), use...), scanf...))
	if st := run(t, f, Options{}); st.Filed != 2 {
		t.Fatalf("filed %d, want 2", st.Filed)
	}
	// use-after-free fixed; scanf alerts dismissed as false positives.
	f.open = nil
	f.dismissed = scanf
	st := run(t, f, Options{})
	if st.Closed != 2 || len(f.openIssues()) != 0 {
		t.Fatalf("closed %d, open %d; want both closed", st.Closed, len(f.openIssues()))
	}
	for _, is := range f.issues {
		switch {
		case strings.Contains(is.title, "use-after-free"):
			if is.closeReason != "completed" || !strings.Contains(is.closeComment, "fixed: #") {
				t.Errorf("fixed issue closed %q with %q", is.closeReason, is.closeComment)
			}
		default:
			if is.closeReason != "not_planned" || !strings.Contains(is.closeComment, "dismissed by a maintainer") {
				t.Errorf("dismissed issue closed %q with %q", is.closeReason, is.closeComment)
			}
		}
	}
	// A dismissed alert reopened on GitHub is not filed again, and its
	// closed issue is not reopened.
	f.open, f.dismissed = scanf, nil
	before := f.writes
	st = run(t, f, Options{})
	if f.writes != before || st.Filed != 0 || st.TrackedClosed != len(scanf) {
		t.Fatalf("reappearing alerts: writes=%d filed=%d tracked_closed=%d", f.writes-before, st.Filed, st.TrackedClosed)
	}
}

func TestRunUpdatesIssueWhenAlertsAreAddedOrFixed(t *testing.T) {
	scanf := onlyRule(loadAlerts(t, "exult-open-alerts.json"), "cpp/missing-check-scanf")
	sort.Slice(scanf, func(i, j int) bool { return scanf[i].Number < scanf[j].Number })
	// The newest alert in a file the issue already covers arrives later.
	var late Alert
	rest := scanf[:0:0]
	for _, a := range scanf {
		if late.Number == 0 && a.Path() == scanf[0].Path() && a.Number != scanf[0].Number {
			late = a
			continue
		}
		rest = append(rest, a)
	}
	f := newFakeForge(rest)
	run(t, f, Options{})
	if len(f.issues) != 1 {
		t.Fatalf("%d issues, want 1", len(f.issues))
	}
	var num int
	for n := range f.issues {
		num = n
	}
	f.open = append(append([]Alert(nil), rest...), late)
	st := run(t, f, Options{})
	if st.Filed != 0 || st.Updated != 1 {
		t.Fatalf("new alert in a covered file: filed %d updated %d, want 0 and 1", st.Filed, st.Updated)
	}
	m, _ := ParseMarker(f.issues[num].body)
	if !containsInt(m.Alerts, late.Number) {
		t.Fatalf("marker %v lacks the added alert #%d", m.Alerts, late.Number)
	}
	// One alert fixed: its row says so, the issue stays open.
	f.open = f.open[1:]
	if st := run(t, f, Options{}); st.Updated != 1 || st.Closed != 0 {
		t.Fatalf("partial fix: updated %d closed %d", st.Updated, st.Closed)
	}
	if !strings.Contains(f.issues[num].body, "| fixed |") || !strings.Contains(f.issues[num].body, "## Wanted behaviour") {
		t.Fatalf("updated body lost the fixed row or the filed text:\n%s", f.issues[num].body)
	}
	if st := run(t, f, Options{}); st.Updated != 0 {
		t.Fatal("an unchanged pass edited the issue again")
	}
}

func TestRunWithTruncatedListingNeverCloses(t *testing.T) {
	use := onlyRule(loadAlerts(t, "exult-open-alerts.json"), "cpp/use-after-free")
	f := newFakeForge(use)
	run(t, f, Options{})
	f.open = nil
	f.pageLimit = 1
	f.open = make([]Alert, 2) // more than the bound: incomplete, and #use missing
	f.open[0] = Alert{Number: 9001, Rule: Rule{Severity: "note"}}
	f.open[1] = Alert{Number: 9002, Rule: Rule{Severity: "note"}}
	st := run(t, f, Options{})
	if st.Closed != 0 || !st.Truncated || len(f.openIssues()) != 1 {
		t.Fatalf("truncated listing closed %d issues (truncated=%v)", st.Closed, st.Truncated)
	}
}

func TestRunReportsMissingPermissionWithoutWriting(t *testing.T) {
	f := newFakeForge(nil)
	f.listErr = ErrNoAccess
	st, err := Run(context.Background(), f, Options{Repo: "example/exult"})
	if err == nil || st.NoticeKind != NoticeMissingPermission || !strings.Contains(st.Notice, "Code scanning alerts") || f.writes != 0 {
		t.Fatalf("err=%v notice=%q kind=%q writes=%d", err, st.Notice, st.NoticeKind, f.writes)
	}
	f.listErr = ErrNotEnabled
	st, _ = Run(context.Background(), f, Options{Repo: "example/exult"})
	if st.NoticeKind != NoticeNotEnabled {
		t.Fatalf("not-enabled notice kind %q", st.NoticeKind)
	}
}

// The projectbluefin mix (2026-11-07): OSSF Scorecard repo-posture checks,
// TokenPermissions on many workflows, and Trivy CVEs in built binaries.
func TestScorecardPostureChecksFileNothingAndFixableOnesGroup(t *testing.T) {
	actions := loadAlerts(t, "scorecard-actions.json")
	f := newFakeForge(actions)
	st := run(t, f, Options{Repo: "example/actions", WorkflowsLane: "scanner", WorkflowsLaneLabel: "hive/workflows-lane"})
	for _, is := range f.issues {
		for _, posture := range []string{"BranchProtectionID", "CodeReviewID", "MaintainedID", "SASTID", "FuzzingID", "CIIBestPracticesID", "VulnerabilitiesID", "SecurityPolicyID"} {
			if strings.Contains(is.title, posture) {
				t.Fatalf("posture check %s was filed: %s", posture, is.title)
			}
		}
		if !strings.Contains(is.title, "TokenPermissionsID") {
			t.Fatalf("unexpected issue %q", is.title)
		}
		if !strings.Contains(is.body, "GitHub Actions workflow files") || !containsString(is.labels, "hive/workflows-lane") {
			t.Fatalf("workflow issue lacks the workflows note or routing label: labels=%v", is.labels)
		}
	}
	tokenFiles := distinctPaths(onlyRule(actions, "TokenPermissionsID"))
	wantGroups := (len(tokenFiles) + MaxFilesPerIssue - 1) / MaxFilesPerIssue
	if st.Filed != wantGroups || st.Excluded == 0 {
		t.Fatalf("filed %d (want %d TokenPermissions groups over %d workflows), excluded %d", st.Filed, wantGroups, len(tokenFiles), st.Excluded)
	}

	// No lane can push workflows: no label, and the body says to park.
	f = newFakeForge(onlyRule(actions, "TokenPermissionsID"))
	run(t, f, Options{Repo: "example/actions", WorkflowsLaneLabel: "hive/workflows-lane"})
	for _, is := range f.issues {
		if containsString(is.labels, "hive/workflows-lane") || !strings.Contains(is.body, "park the issue with the workflows blocker") {
			t.Fatalf("no workflows lane: labels=%v body lacks the park advice", is.labels)
		}
	}

	common := loadAlerts(t, "scorecard-trivy-common.json")
	f = newFakeForge(common)
	st = run(t, f, Options{Repo: "example/common"})
	// High only: the two Trivy packages with a high CVE; Pinned-Dependencies
	// (medium) and the posture checks stay out.
	if st.Filed != 2 {
		t.Fatalf("common filed %d, want 2 Trivy package issues", st.Filed)
	}
	var titles []string
	for _, is := range f.issues {
		titles = append(titles, is.title)
		if !strings.Contains(is.body, "Containerfile") || !strings.Contains(is.title, "known vulnerabilit") {
			t.Fatalf("Trivy issue %q does not point at the package source", is.title)
		}
	}
	sort.Strings(titles)
	if !strings.Contains(titles[0], "golang.org/x/net") || !strings.Contains(titles[1], "stdlib") {
		t.Fatalf("titles %q", titles)
	}

	// With min_severity medium, both x/net CVEs share one issue and
	// Pinned-Dependencies files one workflow issue.
	f = newFakeForge(common)
	st = run(t, f, Options{Repo: "example/common", Policy: Policy{MinLevel: LevelMedium}})
	if st.Filed != 3 {
		t.Fatalf("medium filed %d, want 3", st.Filed)
	}
	for _, is := range f.issues {
		if strings.Contains(is.title, "golang.org/x/net") && !strings.Contains(is.title, "2 known vulnerabilities") {
			t.Fatalf("x/net CVEs not grouped: %q", is.title)
		}
	}
}

func TestExcludeRulesAddsAndBangReincludes(t *testing.T) {
	sast := Alert{Tool: Tool{Name: "Scorecard"}, Rule: Rule{ID: "SASTID", SecuritySeverityLevel: "high"}}
	goto_ := Alert{Tool: Tool{Name: "CodeQL"}, Rule: Rule{ID: "cpp/path-injection", SecuritySeverityLevel: "high"}}
	if (Policy{}).Judge(sast) != Excluded {
		t.Fatal("SASTID not excluded by default")
	}
	if (Policy{ExcludeRules: []string{"!sastid"}}).Judge(sast) != Qualifies {
		t.Fatal("!SASTID did not re-include it")
	}
	if (Policy{ExcludeRules: []string{"cpp/*"}}).Judge(goto_) != Excluded {
		t.Fatal("cpp/* did not exclude a CodeQL cpp rule")
	}
	if (Policy{IncludeTools: []string{"scorecard"}}).Judge(goto_) != Excluded {
		t.Fatal("include_tools did not leave CodeQL out")
	}
	if err := ValidateRulePatterns([]string{"cpp/[", "x"}); err == nil {
		t.Fatal("bad pattern accepted")
	}
}

func TestMarkerRoundTrips(t *testing.T) {
	m := Marker{Tool: "CodeQL", Rules: []string{"cpp/path-injection"}, Paths: []string{"a b/c,d.cc", "x-->y.c"}, Alerts: []int{3, 12}}
	body := "intro\n" + m.String() + "\nrest"
	got, ok := ParseMarker(body)
	if !ok || got.Tool != m.Tool || strings.Join(got.Paths, "|") != strings.Join(m.Paths, "|") || len(got.Alerts) != 2 {
		t.Fatalf("round trip: %+v (ok=%v) from %q", got, ok, m.String())
	}
	if strings.Count(m.String(), "-->") != 1 {
		t.Fatalf("a path ended the comment early: %q", m.String())
	}
}

func TestRunOnForkSaysWhetherUpstreamChangedTheCode(t *testing.T) {
	use := onlyRule(loadAlerts(t, "exult-open-alerts.json"), "cpp/use-after-free")
	a := use[0]
	lines := make([]string, a.Instance.Location.StartLine+5)
	for i := range lines {
		lines[i] = "int filler_" + string(rune('a'+i%26)) + "();"
	}
	lines[a.Instance.Location.StartLine-1] = "\tdelete shape; use(shape->frames);"
	src := strings.Join(lines, "\n")

	f := newFakeForge(use)
	f.files["example/exult@"+a.Instance.CommitSHA+"/"+a.Path()] = src
	f.upstream = "upstream/exult"
	f.files["upstream/exult@/"+a.Path()] = strings.Replace(src, "use(shape->frames)", "shape = nullptr", 1)
	f.lastCommit = "0123456789abcdef0123456789abcdef01234567"
	run(t, f, Options{})
	if len(f.issues) != 1 {
		t.Fatalf("%d issues filed, want 1", len(f.issues))
	}
	for _, is := range f.issues {
		if !strings.Contains(is.body, "port upstream 0123456789ab") || !strings.Contains(is.body, "> ") {
			t.Fatalf("fork issue lacks the upstream port note or excerpt:\n%s", is.body)
		}
	}

	f = newFakeForge(use)
	f.files["example/exult@"+a.Instance.CommitSHA+"/"+a.Path()] = src
	f.upstream = "upstream/exult"
	f.files["upstream/exult@/"+a.Path()] = src
	run(t, f, Options{})
	for _, is := range f.issues {
		if !strings.Contains(is.body, "Upstream HEAD still has the flagged code") {
			t.Fatalf("fork issue does not say upstream is unchanged:\n%s", is.body)
		}
	}
}
