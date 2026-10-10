package compliance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

var postureNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

type fakePostureGitHub struct {
	prs      map[string][]PostureMergedPR
	labels   map[string][]string
	prErr    error
	labelErr error
	since    []time.Time
}

func (f *fakePostureGitHub) MergedPRsSince(_ context.Context, repo string, since time.Time) ([]PostureMergedPR, error) {
	f.since = append(f.since, since)
	if f.prErr != nil {
		return nil, f.prErr
	}
	return f.prs[repo], nil
}

func (f *fakePostureGitHub) RepoLabels(_ context.Context, repo string) ([]string, error) {
	if f.labelErr != nil {
		return nil, f.labelErr
	}
	return f.labels[repo], nil
}

func postureTestDeps(cfg *config.Config, gh PostureGitHub) PostureDeps {
	if cfg == nil {
		cfg = &config.Config{}
	}
	if cfg.Project.Org == "" {
		cfg.Project.Org = "acme"
	}
	return PostureDeps{
		Config: cfg,
		Now:    func() time.Time { return postureNow },
		GitHub: gh,
		Repos:  []string{"widgets", "acme/widgets", " ", "other/gadgets"},
	}
}

func runCheck(t *testing.T, id string, d PostureDeps) Result {
	t.Helper()
	for _, c := range PostureChecks() {
		if c.ID == id {
			return RunPostureCheck(context.Background(), c, d)
		}
	}
	t.Fatalf("no posture check %q", id)
	return Result{}
}

func wantStatus(t *testing.T, r Result, want PostureStatus, detailHas string) {
	t.Helper()
	if r.Status != want || r.Pass != (want == PosturePass) {
		t.Fatalf("%s: status = %s pass = %v, want %s (detail=%q)", r.CheckID, r.Status, r.Pass, want, r.Detail)
	}
	if detailHas != "" && !strings.Contains(r.Detail, detailHas) {
		t.Fatalf("%s: detail = %q, want containing %q", r.CheckID, r.Detail, detailHas)
	}
	if !r.At.Equal(postureNow) || r.Title == "" || len(r.ControlIDs) == 0 {
		t.Fatalf("%s: identity not stamped: %+v", r.CheckID, r)
	}
}

// TestPostureCatalogueMapsToSOC2Controls pins every check to controls that
// exist in the shipped SOC 2 profile, so a renamed control cannot leave a
// check pointing at nothing.
func TestPostureCatalogueMapsToSOC2Controls(t *testing.T) {
	p, ok := ProfileByID("soc2-type2")
	if !ok {
		t.Fatal("soc2-type2 profile missing")
	}
	controls := map[string]bool{}
	for _, c := range p.Controls {
		controls[c.ID] = true
	}
	ids := map[string]bool{}
	cat := PostureCatalogue()
	if len(cat) != 8 || len(PostureChecks()) != len(cat) {
		t.Fatalf("catalogue has %d checks, want 8", len(cat))
	}
	for _, c := range cat {
		if ids[c.ID] || c.Title == "" || len(c.ControlIDs) == 0 {
			t.Fatalf("bad catalogue entry %+v", c)
		}
		ids[c.ID] = true
		for _, cid := range c.ControlIDs {
			if !controls[cid] {
				t.Fatalf("check %s maps unknown control %s", c.ID, cid)
			}
		}
	}
}

func TestRunPostureCheckGuards(t *testing.T) {
	d := postureTestDeps(nil, nil)
	base := PostureCheck{ID: "x", Title: "X", ControlIDs: []string{"CC8.1"}}

	panicky := base
	panicky.Run = func(context.Context, PostureDeps) Result { panic("boom") }
	wantStatus(t, RunPostureCheck(context.Background(), panicky, d), PostureError, "panicked: boom")

	wantStatus(t, RunPostureCheck(context.Background(), base, d), PostureError, "no Run")

	blank := base
	blank.Run = func(context.Context, PostureDeps) Result { return Result{} }
	wantStatus(t, RunPostureCheck(context.Background(), blank, d), PostureError, "no status")

	long := base
	long.Run = func(context.Context, PostureDeps) Result {
		refs := make([]string, maxEvidenceRefs+5)
		return Result{Status: PosturePass, Detail: strings.Repeat("d", maxDetailLen*2), EvidenceRefs: refs}
	}
	r := RunPostureCheck(context.Background(), long, d)
	wantStatus(t, r, PosturePass, "")
	if len(r.Detail) != maxDetailLen || !strings.HasSuffix(r.Detail, "...") || len(r.EvidenceRefs) != maxEvidenceRefs {
		t.Fatalf("not bounded: detail %d refs %d", len(r.Detail), len(r.EvidenceRefs))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	long.Run = func(context.Context, PostureDeps) Result { t.Fatal("ran after cancel"); return Result{} }
	wantStatus(t, RunPostureCheck(ctx, long, d), PostureError, "cancelled")
}

func TestPostureDepsDefaults(t *testing.T) {
	d := PostureDeps{}.withDefaults()
	if d.Config == nil || d.Getenv("X") != "" || d.Now().IsZero() || d.ReadFile == nil {
		t.Fatalf("defaults not applied: %+v", d)
	}
	if d.HoldLabel != "hold" || d.Window != 30*24*time.Hour {
		t.Fatalf("hold=%q window=%s", d.HoldLabel, d.Window)
	}
	q := postureTestDeps(nil, nil).withDefaults().qualifiedRepos()
	if want := []string{"acme/widgets", "other/gadgets"}; !reflect.DeepEqual(q, want) {
		t.Fatalf("qualifiedRepos = %v, want %v", q, want)
	}
}

func TestCheckNonAuthorReview(t *testing.T) {
	wantStatus(t, runCheck(t, CheckNonAuthorReview, postureTestDeps(nil, nil)), PostureSkip, "no GitHub client")

	noRepos := postureTestDeps(nil, &fakePostureGitHub{})
	noRepos.Repos = nil
	wantStatus(t, runCheck(t, CheckNonAuthorReview, noRepos), PostureSkip, "no repositories")

	wantStatus(t, runCheck(t, CheckNonAuthorReview, postureTestDeps(nil, &fakePostureGitHub{prErr: errors.New("rate limited")})), PostureError, "rate limited")

	gh := &fakePostureGitHub{prs: map[string][]PostureMergedPR{
		"acme/widgets": {
			{Number: 1, Author: "alice", Reviewers: []string{"Alice", "bob"}, MergedAt: postureNow.Add(-time.Hour)},
			{Number: 2, Author: "alice", Reviewers: []string{"ALICE", " "}, MergedAt: postureNow.Add(-time.Hour)},
			{Number: 3, Author: "carol", MergedAt: postureNow.Add(-40 * 24 * time.Hour)}, // outside the window
		},
		"other/gadgets": {{Repo: "other/gadgets", Number: 9, URL: "https://example/pr/9", Author: "dave"}},
	}}
	r := runCheck(t, CheckNonAuthorReview, postureTestDeps(nil, gh))
	wantStatus(t, r, PostureFail, "2 of 3 PR(s) merged in the last 30d")
	if want := []string{"https://github.com/acme/widgets/pull/2", "https://example/pr/9"}; !reflect.DeepEqual(r.EvidenceRefs, want) {
		t.Fatalf("refs = %v, want %v", r.EvidenceRefs, want)
	}
	if len(gh.since) != 2 || !gh.since[0].Equal(postureNow.Add(-30*24*time.Hour)) {
		t.Fatalf("since = %v", gh.since)
	}

	ok := &fakePostureGitHub{prs: map[string][]PostureMergedPR{"acme/widgets": {{Number: 1, Author: "alice", Reviewers: []string{"bob"}}}}}
	wantStatus(t, runCheck(t, CheckNonAuthorReview, postureTestDeps(nil, ok)), PosturePass, "all 1 PR(s)")
}

func TestCheckOwnerAutoMerge(t *testing.T) {
	wantStatus(t, runCheck(t, CheckOwnerAutoMerge, postureTestDeps(nil, &fakePostureGitHub{})), PostureSkip, "no dashboard owners")

	cfg := func() *config.Config {
		c := &config.Config{}
		c.Dashboard.AuthorizedUsers = []string{"alice", "bob:merger", "github:erin:owner"}
		c.Project.AIAuthor = "hive-author"
		return c
	}
	wantStatus(t, runCheck(t, CheckOwnerAutoMerge, postureTestDeps(cfg(), nil)), PostureSkip, "no GitHub client")

	gh := &fakePostureGitHub{prs: map[string][]PostureMergedPR{"acme/widgets": {
		{Number: 1, Author: "alice", MergedByBot: true},         // owner, App merge
		{Number: 2, Author: "Erin", MergedBy: "Hive-Author"},    // owner, hive identity merge
		{Number: 3, Author: "bob", MergedBy: "renovate[bot]"},   // merger, auto
		{Number: 4, Author: "alice", MergedBy: "bob"},           // human merge
		{Number: 5, Author: "alice", MergedBy: ""},              // unknown merger
		{Number: 6, Author: "mallory", MergedBy: "other-human"}, // human merge
	}}}
	r := runCheck(t, CheckOwnerAutoMerge, postureTestDeps(cfg(), gh))
	wantStatus(t, r, PostureFail, "owner(s) alice, erin authored 2 of 3 auto-merged")
	if len(r.EvidenceRefs) != 2 {
		t.Fatalf("refs = %v", r.EvidenceRefs)
	}

	clean := &fakePostureGitHub{prs: map[string][]PostureMergedPR{"acme/widgets": {{Number: 3, Author: "bob", MergedByBot: true}}}}
	wantStatus(t, runCheck(t, CheckOwnerAutoMerge, postureTestDeps(cfg(), clean)), PosturePass, "none of 1 auto-merged")
}

func TestCheckAuditRetention(t *testing.T) {
	wantStatus(t, runCheck(t, CheckAuditRetention, postureTestDeps(nil, nil)), PostureSkip, "no selected profile")

	cfg := &config.Config{Compliance: config.ComplianceConfig{Frameworks: []string{"soc2-type2", "not-shipped"}}}
	r := runCheck(t, CheckAuditRetention, postureTestDeps(cfg, nil))
	wantStatus(t, r, PostureFail, "90 days is below the 365-day floor (soc2-type2 CC7.2)")
	if len(r.EvidenceRefs) != 1 {
		t.Fatalf("refs = %v", r.EvidenceRefs)
	}
}

func TestCheckAgentConfinement(t *testing.T) {
	wantStatus(t, runCheck(t, CheckAgentConfinement, postureTestDeps(nil, nil)), PostureSkip, "no enabled agents")

	on := true
	cfg := &config.Config{
		AgentSandbox: config.AgentSandboxConfig{Enabled: true},
		Agents: map[string]config.AgentConfig{
			"boxed":    {Enabled: true, Backend: "claude", Sandbox: &config.AgentSandboxOverride{Enabled: &on}},
			"host":     {Enabled: true, Backend: "copilot"},
			"nobknd":   {Enabled: true},
			"disabled": {Enabled: false},
		},
	}
	r := runCheck(t, CheckAgentConfinement, postureTestDeps(cfg, nil))
	wantStatus(t, r, PostureFail, "2 of 3 enabled agent(s) run above T2: host (T3, copilot); nobknd (T3)")

	d := postureTestDeps(cfg, nil)
	d.Getenv = func(name string) string {
		if name == config.ProxyInjectGHAuthEnv {
			return "true"
		}
		return ""
	}
	wantStatus(t, runCheck(t, CheckAgentConfinement, d), PosturePass, "all 3 enabled agent(s) run at T2 or better")
	if got := AgentConfinementTier(cfg, cfg.Agents["boxed"], nil); got != ConfinementT1 {
		t.Fatalf("boxed tier = %s", got)
	}
}

func TestCheckSentinel(t *testing.T) {
	off := false
	cfg := &config.Config{Sentinel: config.SentinelConfig{Enabled: &off}}
	wantStatus(t, runCheck(t, CheckSentinel, postureTestDeps(cfg, nil)), PostureFail, "sentinel is disabled")

	cfg = &config.Config{Sentinel: config.SentinelConfig{DisabledBehaviors: []string{" ", "secret_exposure"}}}
	wantStatus(t, runCheck(t, CheckSentinel, postureTestDeps(cfg, nil)), PostureFail, "disabled: secret_exposure")

	wantStatus(t, runCheck(t, CheckSentinel, postureTestDeps(nil, nil)), PosturePass, "not verified")

	label := config.SentinelConfig{}.LabelOrDefault()
	gh := &fakePostureGitHub{labels: map[string][]string{"acme/widgets": {strings.ToUpper(label)}, "other/gadgets": {"bug"}}}
	wantStatus(t, runCheck(t, CheckSentinel, postureTestDeps(nil, gh)), PostureFail, "missing on 1 of 2 repo(s): other/gadgets: "+label)

	gh.labels["other/gadgets"] = []string{label}
	wantStatus(t, runCheck(t, CheckSentinel, postureTestDeps(nil, gh)), PosturePass, "present on all 2 repo(s)")

	wantStatus(t, runCheck(t, CheckSentinel, postureTestDeps(nil, &fakePostureGitHub{labelErr: errors.New("403")})), PostureError, "403")
}

func TestCheckHoldLabels(t *testing.T) {
	wantStatus(t, runCheck(t, CheckHoldLabels, postureTestDeps(nil, nil)), PostureSkip, "no GitHub client")
	noRepos := postureTestDeps(nil, &fakePostureGitHub{})
	noRepos.Repos = nil
	wantStatus(t, runCheck(t, CheckHoldLabels, noRepos), PostureSkip, "no repositories")
	wantStatus(t, runCheck(t, CheckHoldLabels, postureTestDeps(nil, &fakePostureGitHub{labelErr: errors.New("boom")})), PostureError, "boom")

	gh := &fakePostureGitHub{labels: map[string][]string{
		"acme/widgets":  {"hive/hold", "needs-human"},
		"other/gadgets": {"hive/hold"},
	}}
	d := postureTestDeps(nil, gh)
	d.HoldLabel = "hive/hold"
	wantStatus(t, runCheck(t, CheckHoldLabels, d), PostureFail, "1 label(s) missing across 2 repo(s): other/gadgets: needs-human")
	gh.labels["other/gadgets"] = append(gh.labels["other/gadgets"], "Needs-Human")
	wantStatus(t, runCheck(t, CheckHoldLabels, d), PosturePass, "hive/hold, needs-human present on all 2")
}

func TestCheckDashboardAuth(t *testing.T) {
	cases := []struct {
		name string
		dash config.DashboardConfig
		want PostureStatus
		has  string
	}{
		{"off", config.DashboardConfig{}, PostureFail, "authentication is off"},
		{"hub proxied", config.DashboardConfig{HubProxied: true}, PosturePass, "hub_proxied"},
		{"device flow", config.DashboardConfig{AuthorizedUsers: []string{"alice"}}, PosturePass, "device-flow"},
		{"token", config.DashboardConfig{AuthToken: "x"}, PosturePass, "auth token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantStatus(t, runCheck(t, CheckDashboardAuth, postureTestDeps(&config.Config{Dashboard: tc.dash}, nil)), tc.want, tc.has)
		})
	}
}

func TestCheckNoConfigSecrets(t *testing.T) {
	// Built by concatenation so the fixture is not itself a committed secret.
	ghToken := "gh" + "p_" + strings.Repeat("Z", 24)
	pem := "-----BEGIN " + "RSA PRIVATE KEY-----\n" + strings.Repeat("A", 40) + "\n-----END " + "RSA PRIVATE KEY-----"
	files := map[string]string{
		"/cfg/hive.yaml": "github:\n  token: " + ghToken + "\n# token: " + ghToken + "\ndashboard:\n  auth_token: ${DASHBOARD_AUTH_TOKEN}\n",
		"/cfg/overlay.yaml": "jira:\n  api_token: literalvalue12345\n  password: short\n  credential_secret: hive-secrets/jira\n" +
			"agents:\n  - max_token: 123456789012\n    client_secret: <redacted-later>\n",
		"/cfg/agent.yaml": "key: |\n  " + strings.ReplaceAll(pem, "\n", "\n  ") + "\n",
		"/cfg/clean.yaml": "project:\n  org: acme\n",
	}
	read := func(p string) ([]byte, error) {
		if s, ok := files[p]; ok {
			return []byte(s), nil
		}
		return nil, os.ErrNotExist
	}
	d := postureTestDeps(nil, nil)
	d.ReadFile = read

	d.ConfigFiles = []string{"/missing.yaml", ""}
	wantStatus(t, runCheck(t, CheckNoConfigSecrets, d), PostureSkip, "no config files")

	d.ConfigFiles = []string{"/cfg/clean.yaml", "/cfg/clean.yaml"}
	wantStatus(t, runCheck(t, CheckNoConfigSecrets, d), PosturePass, "in 1 config file(s)")

	d.ConfigFiles = []string{"/cfg/hive.yaml", "/cfg/overlay.yaml", "/cfg/agent.yaml", "/missing.yaml"}
	r := runCheck(t, CheckNoConfigSecrets, d)
	wantStatus(t, r, PostureFail, "3 secret-looking value(s) in 3 config file(s)")
	want := []string{
		"/cfg/hive.yaml:2 (github-token)",
		"/cfg/overlay.yaml:2 (literal value for jira.api_token)",
		"/cfg/agent.yaml (private-key)",
	}
	if !reflect.DeepEqual(r.EvidenceRefs, want) {
		t.Fatalf("refs = %q\nwant %q", r.EvidenceRefs, want)
	}
	all := r.Detail + strings.Join(r.EvidenceRefs, " ")
	if strings.Contains(all, ghToken) || strings.Contains(all, "literalvalue12345") {
		t.Fatal("finding leaked a secret value")
	}
}

func TestLiteralSecretValue(t *testing.T) {
	for v, want := range map[string]bool{
		"literalvalue12345": true,
		"short":             false,
		"${TOKEN_FROM_ENV}": false,
		"$TOKEN_FROM_ENVAR": false,
		"/var/run/secret":   false,
		"12345678901":       false,
		"REDACTED-VALUE":    false,
		"<your-token-here>": false,
	} {
		if got := literalSecretValue(v); got != want {
			t.Errorf("literalSecretValue(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestSummarizePosture(t *testing.T) {
	got := SummarizePosture([]Result{{Status: PosturePass}, {Status: PostureFail}, {Status: PostureSkip}, {Status: PostureError}, {Status: "odd"}})
	if got != (PostureSummary{Pass: 1, Fail: 1, Skip: 1, Error: 2}) {
		t.Fatalf("summary = %+v", got)
	}
}

func postureRun(at time.Time, status PostureStatus) PostureRun {
	return PostureRun{At: at, Trigger: TriggerSchedule, Results: []Result{{CheckID: "c", Status: status, Pass: status == PosturePass, At: at}}}
}

func TestPostureHistoryPersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "posture.jsonl")
	h, err := NewPostureHistory(path, 0, 0)
	if err != nil || h.Len() != 0 {
		t.Fatalf("new: %v len=%d", err, h.Len())
	}
	if _, ok := h.Latest(); ok {
		t.Fatal("empty history has a latest run")
	}
	for i := 0; i < 3; i++ {
		if err := h.Append(postureRun(postureNow.Add(time.Duration(i)*time.Hour), PosturePass)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{torn\n\n{}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	re, err := NewPostureHistory(path, 0, 0)
	if err != nil || re.Len() != 3 {
		t.Fatalf("reload: %v len=%d", err, re.Len())
	}
	latest, ok := re.Latest()
	if !ok || !latest.At.Equal(postureNow.Add(2*time.Hour)) {
		t.Fatalf("latest = %+v", latest)
	}
	got, truncated := re.Since(postureNow.Add(time.Hour), 0)
	if len(got) != 2 || truncated || !got[0].At.Equal(postureNow.Add(time.Hour)) {
		t.Fatalf("since = %+v truncated=%v", got, truncated)
	}
	got, truncated = re.Since(time.Time{}, 1)
	if len(got) != 1 || !truncated || !got[0].At.Equal(latest.At) {
		t.Fatalf("limited since = %+v truncated=%v", got, truncated)
	}

	if _, err := NewPostureHistory(t.TempDir(), 0, 0); err == nil {
		t.Fatal("reading a directory must error")
	}
}

func TestPostureHistoryRetentionCapAndCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posture.jsonl")
	h, _ := NewPostureHistory(path, 10*time.Hour, 5)
	total := postureCompactSlack + 20
	for i := 0; i < total; i++ {
		if err := h.Append(postureRun(postureNow.Add(time.Duration(i)*time.Hour), PosturePass)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if h.Len() != 5 {
		t.Fatalf("len = %d, want cap 5", h.Len())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(raw), "\n"); lines > 5+postureCompactSlack+1 {
		t.Fatalf("file has %d lines; compaction did not run", lines)
	}
	re, _ := NewPostureHistory(path, 0, 0)
	latest, _ := re.Latest()
	if !latest.At.Equal(postureNow.Add(time.Duration(total-1) * time.Hour)) {
		t.Fatalf("reloaded latest = %s", latest.At)
	}

	h.SetRetention(2 * time.Hour)
	if err := h.Append(postureRun(postureNow.Add(time.Duration(total)*time.Hour), PosturePass)); err != nil {
		t.Fatal(err)
	}
	if h.Len() != 3 {
		t.Fatalf("len after retention = %d, want 3", h.Len())
	}

	mem, _ := NewPostureHistory("", 0, -1)
	if err := mem.Append(postureRun(postureNow, PosturePass)); err != nil || mem.Len() != 1 {
		t.Fatalf("memory history: %v len=%d", err, mem.Len())
	}
}

func TestPostureHistoryWriteErrors(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, _ := NewPostureHistory(filepath.Join(blocker, "posture.jsonl"), 0, 0)
	if err := h.Append(postureRun(postureNow, PosturePass)); err == nil {
		t.Fatal("append under a file must error")
	}
	if h.Len() != 1 {
		t.Fatal("in-memory history must keep the run when persisting fails")
	}
	h.fileLines = postureCompactSlack * 2
	if err := h.Append(postureRun(postureNow.Add(time.Hour), PosturePass)); err == nil {
		t.Fatal("compaction under a file must error")
	}

	dir := filepath.Join(t.TempDir(), "asdir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	hd := &PostureHistory{path: dir, maxRuns: 10}
	if err := hd.Append(postureRun(postureNow, PosturePass)); err == nil {
		t.Fatal("appending to a directory path must error")
	}
}

func scriptedCheck(id string, status *atomic.Value) PostureCheck {
	return PostureCheck{ID: id, Title: id, ControlIDs: []string{"CC8.1"}, Run: func(context.Context, PostureDeps) Result {
		return Result{Status: status.Load().(PostureStatus), Detail: id}
	}}
}

func TestPostureRunnerTransitions(t *testing.T) {
	var a, b atomic.Value
	a.Store(PosturePass)
	b.Store(PostureFail)
	cfg := &config.Config{Compliance: config.ComplianceConfig{Frameworks: []string{"soc2-type2"}}}
	r := NewPostureRunner([]PostureCheck{scriptedCheck("a", &a), scriptedCheck("b", &b)}, nil,
		func() PostureDeps { return PostureDeps{Config: cfg, Now: func() time.Time { return postureNow }} })
	var transitions []string
	r.OnTransition = func(prev, cur Result) {
		transitions = append(transitions, prev.CheckID+":"+string(prev.Status)+"->"+string(cur.Status))
	}

	run, err := r.Run(context.Background(), TriggerManual)
	if err != nil || run.Trigger != TriggerManual || run.Summary != (PostureSummary{Pass: 1, Fail: 1}) || !run.At.Equal(postureNow) {
		t.Fatalf("run = %+v err=%v", run, err)
	}
	if len(transitions) != 0 {
		t.Fatalf("first run must not report transitions: %v", transitions)
	}
	a.Store(PostureFail)
	b.Store(PosturePass)
	if _, err := r.Run(context.Background(), TriggerSchedule); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a:pass->fail"}; !reflect.DeepEqual(transitions, want) {
		t.Fatalf("transitions = %v, want %v", transitions, want)
	}
	if r.History().Len() != 2 {
		t.Fatalf("history len = %d", r.History().Len())
	}
}

func TestPostureRunnerRefusesOverlapAndReportsPersistErrors(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	blocking := PostureCheck{ID: "slow", Title: "slow", ControlIDs: []string{"CC8.1"}, Run: func(context.Context, PostureDeps) Result {
		close(entered)
		<-release
		return Result{Status: PosturePass}
	}}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	hist, _ := NewPostureHistory(filepath.Join(blocker, "h.jsonl"), 0, 0)
	r := NewPostureRunner([]PostureCheck{blocking}, hist, nil)
	persistErrs := make(chan error, 1)
	r.OnPersistError = func(err error) { persistErrs <- err }

	done := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), TriggerSchedule)
		done <- err
	}()
	<-entered
	if _, err := r.Run(context.Background(), TriggerManual); !errors.Is(err, ErrPostureRunInProgress) {
		t.Fatalf("overlapping run err = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := <-persistErrs; err == nil {
		t.Fatal("want a persist error")
	}
	if len(NewPostureRunner(nil, nil, nil).checks) != len(postureChecks) {
		t.Fatal("nil checks must default to the registry")
	}
}

func TestPostureRunnerLoop(t *testing.T) {
	var st atomic.Value
	st.Store(PosturePass)
	r := NewPostureRunner([]PostureCheck{scriptedCheck("a", &st)}, nil, nil)

	ticks := make(chan time.Time)
	waits := make(chan time.Duration, 4)
	after := func(d time.Duration) <-chan time.Time { waits <- d; return ticks }
	var enabled atomic.Bool
	enabled.Store(true)
	var interval atomic.Int64
	interval.Store(int64(10 * time.Minute))
	schedule := func() (time.Duration, bool) { return time.Duration(interval.Load()), enabled.Load() }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Loop(ctx, schedule, after); close(done) }()

	if d := <-waits; d != PostureInitialDelay {
		t.Fatalf("first wait = %s", d)
	}
	ticks <- postureNow
	if d := <-waits; d != 10*time.Minute {
		t.Fatalf("second wait = %s", d)
	}
	if r.History().Len() != 1 {
		t.Fatalf("enabled tick ran %d passes", r.History().Len())
	}
	enabled.Store(false)
	interval.Store(0)
	ticks <- postureNow
	if d := <-waits; d != PostureInitialDelay {
		t.Fatalf("zero interval wait = %s", d)
	}
	if r.History().Len() != 1 {
		t.Fatal("disabled tick must not run a pass")
	}
	cancel()
	<-done

	stopped, stop := context.WithCancel(context.Background())
	stop()
	r.Loop(stopped, schedule, nil)
}
