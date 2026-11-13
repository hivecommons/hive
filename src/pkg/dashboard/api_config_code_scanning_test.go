package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hivecommons/hive/pkg/codescanning"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Tests for GET/PUT /api/config/code-scanning, the code scanning sweep
// settings. Before this endpoint nothing but a hive.yaml edit could change
// code_scanning, so every request here answered 404/405.

const codeScanningTestPath = "/api/config/code-scanning"

// codeScanningPutAs sends a PUT as a named, verified owner.
func codeScanningPutAs(s *Server, user string, body any) *httptest.ResponseRecorder {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, codeScanningTestPath, &b)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-User", user)
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	return rec
}

func decodeCodeScanningSettings(t *testing.T, rec *httptest.ResponseRecorder) codeScanningSettings {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out codeScanningSettings
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode code_scanning response: %v: %s", err, rec.Body.String())
	}
	return out
}

func codeScanningAuditRows(s *Server) []AuditEntry {
	return s.audit.RecentWithPrefixSince(time.Now().Add(-time.Minute), auditConfigCodeScanning)
}

func codeScanningAuditDetails(s *Server) []string {
	var out []string
	for _, row := range codeScanningAuditRows(s) {
		out = append(out, row.Detail)
	}
	sort.Strings(out)
	return out
}

func codeScanningRepoRow(t *testing.T, got codeScanningSettings, repo string) codeScanningRepoState {
	t.Helper()
	for _, row := range got.ProjectRepos {
		if row.Repo == repo {
			return row
		}
	}
	t.Fatalf("no project_repos row for %s in %+v", repo, got.ProjectRepos)
	return codeScanningRepoState{}
}

// codeScanningTwoRepoServer is a hive with two project repositories.
func codeScanningTwoRepoServer(t *testing.T) *Server {
	t.Helper()
	s := covApiServer(t)
	s.deps.Config.Project.Repos = []string{"repo1", "repo2"}
	return s
}

// GET on an untouched hive: the sweep is off everywhere, every default is
// resolved, the built-in exclusions are listed, and no repository is reported
// as readable.
func TestCodeScanningSettingsGetDefaults(t *testing.T) {
	s := codeScanningTwoRepoServer(t)
	rec := doOwnerGet(s, codeScanningTestPath)
	got := decodeCodeScanningSettings(t, rec)
	if got.Org != "myorg" || got.Enabled || got.Interval != "" || got.IntervalEffective != "1h0m0s" ||
		got.MinSeverity != "" || got.MinSeverityEffective != "high" ||
		got.MaxNewIssuesPerRun != 0 || got.MaxNewIssuesPerRunEffective != codescanning.DefaultMaxNewIssues {
		t.Fatalf("defaults wrong: %+v", got)
	}
	if !reflect.DeepEqual(got.Severities, []string{"critical", "high", "medium", "low", "note"}) {
		t.Fatalf("severities = %v", got.Severities)
	}
	if got.IntervalDefault != "1h0m0s" || got.MinSeverityDefault != "high" || got.MaxNewIssuesPerRunDefault != codescanning.DefaultMaxNewIssues {
		t.Fatalf("the defaults an unset key resolves to are not reported: %+v", got)
	}
	if !reflect.DeepEqual(got.ExcludeRulesDefault, codescanning.DefaultExcludeRules) || !reflect.DeepEqual(got.ExcludeRulesEffective, codescanning.DefaultExcludeRules) {
		t.Fatalf("the built-in exclusions are not reported: default %v effective %v", got.ExcludeRulesDefault, got.ExcludeRulesEffective)
	}
	if len(got.Repos) != 0 || len(got.ProjectRepos) != 2 {
		t.Fatalf("repos %v project_repos %+v, want no override and two rows", got.Repos, got.ProjectRepos)
	}
	for _, repo := range []string{"myorg/repo1", "myorg/repo2"} {
		if row := codeScanningRepoRow(t, got, repo); row.Enabled || row.EnabledFrom != "default" || row.AlertsAccess != codeScanningAccessOff || row.CheckedAt != "" {
			t.Fatalf("%s row = %+v, want off by default and not checked", repo, row)
		}
	}
	// Lists are [] and never null, so the dashboard can read them as lists.
	for _, want := range []string{`"include_tools":[]`, `"exclude_rules":[]`, `"repos":{}`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("GET body lacks %s: %s", want, rec.Body.String())
		}
	}
}

// The hive-wide switch turns the sweep on for every project repository, is
// audited once with the actor, and a repeat of the same PUT changes and
// audits nothing.
func TestCodeScanningSettingsPutGlobalEnable(t *testing.T) {
	s := codeScanningTwoRepoServer(t)
	got := decodeCodeScanningSettings(t, codeScanningPutAs(s, "alice", map[string]any{"enabled": true}))
	if !got.Enabled || !s.deps.Config.CodeScanning.Enabled {
		t.Fatalf("enabled not applied: response %v config %v", got.Enabled, s.deps.Config.CodeScanning.Enabled)
	}
	for _, repo := range []string{"repo1", "repo2"} {
		if _, on := s.deps.Config.CodeScanning.For("myorg", repo); !on {
			t.Fatalf("the sweep is not on for %s", repo)
		}
		// On, but no sweep pass has run: that is not evidence of access.
		if row := codeScanningRepoRow(t, got, "myorg/"+repo); !row.Enabled || row.EnabledFrom != "default" || row.AlertsAccess != codeScanningAccessNotChecked {
			t.Fatalf("%s row = %+v, want on by default and not_checked", repo, row)
		}
	}
	rows := codeScanningAuditRows(s)
	if len(rows) != 1 || rows[0].User != "alice" || rows[0].Detail != "section=code_scanning, key=enabled, before=false, after=true" {
		t.Fatalf("audit rows = %+v, want one enabled change by alice", rows)
	}

	decodeCodeScanningSettings(t, codeScanningPutAs(s, "alice", map[string]any{"enabled": true}))
	if rows := codeScanningAuditRows(s); len(rows) != 1 {
		t.Fatalf("a PUT that changed nothing was audited: %+v", rows)
	}
}

// Turning one repository on leaves the others off.
func TestCodeScanningSettingsPutPerRepoEnable(t *testing.T) {
	s := codeScanningTwoRepoServer(t)
	got := decodeCodeScanningSettings(t, codeScanningPutAs(s, "alice", map[string]any{"repos": map[string]any{"repo1": map[string]any{"enabled": true}}}))
	if _, on := s.deps.Config.CodeScanning.For("myorg", "repo1"); !on {
		t.Fatal("the sweep is not on for repo1")
	}
	if _, on := s.deps.Config.CodeScanning.For("myorg", "repo2"); on {
		t.Fatal("turning repo1 on turned repo2 on")
	}
	if s.deps.Config.CodeScanning.Enabled {
		t.Fatal("a per-repository change moved the hive-wide switch")
	}
	if entry, ok := got.Repos["repo1"]; !ok || entry.Enabled == nil || !*entry.Enabled || len(got.Repos) != 1 {
		t.Fatalf("repos = %+v, want only repo1 on", got.Repos)
	}
	if row := codeScanningRepoRow(t, got, "myorg/repo1"); !row.Enabled || row.EnabledFrom != "repo" || row.AlertsAccess != codeScanningAccessNotChecked {
		t.Fatalf("repo1 row = %+v, want on by its own override and not_checked", row)
	}
	if row := codeScanningRepoRow(t, got, "myorg/repo2"); row.Enabled || row.AlertsAccess != codeScanningAccessOff {
		t.Fatalf("repo2 row = %+v, want off", row)
	}
	if got := codeScanningAuditDetails(s); !reflect.DeepEqual(got, []string{"section=code_scanning, key=repos[repo1], before=no override, after=enabled=true"}) {
		t.Fatalf("audit details = %v", got)
	}
}

// repos, when sent, is the whole override map. An entry is dropped only when
// it sets nothing; enabled null with other fields keeps those fields and
// inherits the switch. A PUT without repos leaves the map alone.
func TestCodeScanningSettingsReposReplacesTheWholeMap(t *testing.T) {
	s := codeScanningTwoRepoServer(t)
	on, off := true, false
	s.deps.Config.CodeScanning.Repos = map[string]config.CodeScanningRepoConfig{
		"repo1": {Enabled: &on, MinSeverity: "medium", MaxNewIssuesPerRun: 2, ExcludeRules: []string{"cpp/x-*"}},
		"repo2": {Enabled: &off},
	}

	// A PUT that does not name repos keeps both overrides.
	decodeCodeScanningSettings(t, codeScanningPutAs(s, "alice", map[string]any{"min_severity": "low"}))
	if r := s.deps.Config.CodeScanning.Repos; len(r) != 2 || r["repo1"].MinSeverity != "medium" {
		t.Fatalf("a PUT without repos changed the map: %+v", r)
	}

	// repo1 back to "inherit": enabled null, its other overrides sent back.
	// repo2 is not in the map sent, so its override goes.
	decodeCodeScanningSettings(t, codeScanningPutAs(s, "alice", map[string]any{"repos": map[string]any{
		"repo1": map[string]any{"enabled": nil, "min_severity": "medium", "max_new_issues_per_run": 2, "exclude_rules": []string{"cpp/x-*"}},
	}}))
	r := s.deps.Config.CodeScanning.Repos
	if len(r) != 1 || r["repo1"].Enabled != nil || r["repo1"].MinSeverity != "medium" || r["repo1"].MaxNewIssuesPerRun != 2 || !reflect.DeepEqual(r["repo1"].ExcludeRules, []string{"cpp/x-*"}) {
		t.Fatalf("repos = %+v, want repo1 inheriting enabled with its other overrides kept, and repo2 gone", r)
	}
	policy, enabled := s.deps.Config.CodeScanning.For("myorg", "repo1")
	if enabled || policy.MinLevel != codescanning.LevelMedium || policy.MaxNewIssues != 2 {
		t.Fatalf("repo1 policy = %+v enabled=%v, want the hive-wide switch (off) with medium and a cap of 2", policy, enabled)
	}

	// An entry that sets nothing, and a null entry, are dropped.
	decodeCodeScanningSettings(t, codeScanningPutAs(s, "alice", map[string]any{"repos": map[string]any{
		"repo1": map[string]any{"enabled": nil, "min_severity": "", "include_tools": []string{}, "max_new_issues_per_run": 0},
		"repo2": nil,
	}}))
	if r := s.deps.Config.CodeScanning.Repos; r != nil {
		t.Fatalf("repos = %+v, want no override left", r)
	}

	// {} clears every override.
	s.deps.Config.CodeScanning.Repos = map[string]config.CodeScanningRepoConfig{"repo2": {Enabled: &on}}
	decodeCodeScanningSettings(t, codeScanningPutAs(s, "alice", map[string]any{"repos": map[string]any{}}))
	if r := s.deps.Config.CodeScanning.Repos; r != nil {
		t.Fatalf("repos = %+v after {}, want none", r)
	}
}

// Every invalid body is 400 with nothing changed, nothing written to
// hive.yaml and nothing audited. The values hive.yaml loading refuses are
// refused with that loader's own message.
func TestCodeScanningSettingsPutInvalidChangesNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		body  string
		want  string // the whole error when exact, else a part of it
		exact bool
	}{
		"interval under ten minutes":     {`{"interval": "5m"}`, "code_scanning.interval: must be at least 10m0s, got 5m0s", true},
		"negative interval":              {`{"interval": "-30m"}`, "code_scanning.interval: must be at least 10m0s, got -30m0s", true},
		"unknown severity":               {`{"min_severity": "severe"}`, `code_scanning.min_severity: "severe" is not one of critical, high, medium, low, note (or error, warning)`, true},
		"negative cap":                   {`{"max_new_issues_per_run": -1}`, "code_scanning.max_new_issues_per_run: must not be negative, got -1", true},
		"malformed rule pattern":         {`{"exclude_rules": ["cpp/["]}`, "code_scanning.exclude_rules: bad rule pattern cpp/[: syntax error in pattern", true},
		"empty rule pattern":             {`{"exclude_rules": [" "]}`, "code_scanning.exclude_rules: empty rule pattern", true},
		"repository not in project":      {`{"repos": {"other": {"enabled": true}}}`, `code_scanning.repos: "other" is not listed in project.repos`, true},
		"per-repository severity":        {`{"repos": {"repo1": {"min_severity": "x"}}}`, `code_scanning.repos[repo1].min_severity: "x" is not one of critical, high, medium, low, note (or error, warning)`, true},
		"per-repository negative cap":    {`{"repos": {"repo1": {"max_new_issues_per_run": -2}}}`, "code_scanning.repos[repo1].max_new_issues_per_run: must not be negative, got -2", true},
		"per-repository rule pattern":    {`{"repos": {"repo1": {"exclude_rules": ["!"]}}}`, "code_scanning.repos[repo1].exclude_rules: empty rule pattern", true},
		"interval not a duration":        {`{"interval": "soon"}`, "interval must be a duration", false},
		"same repository twice":          {`{"repos": {"repo1": {"enabled": true}, "MyOrg/Repo1": {"enabled": false}}}`, "the same repository twice", false},
		"placeholder in a tool name":     {`{"include_tools": ["${HIVE_KEY}"]}`, "must not contain ${...}", false},
		"placeholder in a repo rule":     {`{"repos": {"repo1": {"exclude_rules": ["${HIVE_KEY}"]}}}`, "must not contain ${...}", false},
		"unknown top-level field":        {`{"enable": true}`, "invalid body", false},
		"unknown repository field":       {`{"repos": {"repo1": {"enabled": true, "llm": true}}}`, "invalid body", false},
		"enabled not a boolean":          {`{"enabled": "yes"}`, "invalid body", false},
		"cap not a whole number":         {`{"max_new_issues_per_run": 2.5}`, "invalid body", false},
		"two objects":                    {`{"enabled": false} {"enabled": true}`, "one JSON object", false},
		"not JSON":                       {`{nope`, "invalid body", false},
		"valid change beside a bad one":  {`{"enabled": true, "min_severity": "severe"}`, "code_scanning.min_severity", false},
		"valid repo beside a bad one":    {`{"repos": {"repo1": {"enabled": true}, "other": {"enabled": true}}}`, "is not listed in project.repos", false},
		"valid field beside a bad shape": {`{"enabled": true, "interval": "soon"}`, "interval must be a duration", false},
	} {
		t.Run(name, func(t *testing.T) {
			// Two separately built values, so a handler that changed the
			// live block in place cannot also change the expectation.
			seed := func() config.CodeScanningConfig {
				off := false
				return config.CodeScanningConfig{
					MinSeverity:  "medium",
					ExcludeRules: []string{"!SASTID"},
					Repos:        map[string]config.CodeScanningRepoConfig{"repo2": {Enabled: &off, IncludeTools: []string{"CodeQL"}}},
				}
			}
			s := codeScanningTwoRepoServer(t)
			s.deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
			s.deps.Config.CodeScanning = seed()
			rec := doPutRaw(s, codeScanningTestPath, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
			var refusal struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("decode refusal: %v: %s", err, rec.Body.String())
			}
			if tc.exact && refusal.Error != tc.want {
				t.Fatalf("error = %q, want the hive.yaml loading message %q", refusal.Error, tc.want)
			}
			if !strings.Contains(refusal.Error, tc.want) {
				t.Fatalf("error = %q, want it to contain %q", refusal.Error, tc.want)
			}
			if !reflect.DeepEqual(s.deps.Config.CodeScanning, seed()) {
				t.Fatalf("a refused PUT changed the config: %+v", s.deps.Config.CodeScanning)
			}
			if _, err := os.Stat(s.deps.Config.SourcePath); !os.IsNotExist(err) {
				t.Fatalf("a refused PUT wrote hive.yaml (stat err = %v)", err)
			}
			if rows := codeScanningAuditRows(s); len(rows) != 0 {
				t.Fatalf("a refused PUT was audited: %+v", rows)
			}
		})
	}
}

// A PUT writes one audit entry per changed key, each with the actor and the
// value before and after; a key sent with its current value writes none.
func TestCodeScanningSettingsAuditsEachChangedKey(t *testing.T) {
	s := codeScanningTwoRepoServer(t)
	off := false
	s.deps.Config.CodeScanning = config.CodeScanningConfig{
		MinSeverity: "medium",
		Repos:       map[string]config.CodeScanningRepoConfig{"repo2": {Enabled: &off}},
	}
	body := map[string]any{
		"enabled":                true,
		"interval":               "30m",
		"min_severity":           "medium", // unchanged
		"include_tools":          []string{"CodeQL", " Trivy "},
		"exclude_rules":          []string{"!SASTID"},
		"max_new_issues_per_run": 3,
		"repos": map[string]any{
			"repo1": map[string]any{"enabled": true, "min_severity": "low"},
			"repo2": map[string]any{"enabled": false}, // unchanged
		},
	}
	decodeCodeScanningSettings(t, codeScanningPutAs(s, "alice", body))
	want := []string{
		"section=code_scanning, key=enabled, before=false, after=true",
		`section=code_scanning, key=exclude_rules, before=the default list, after=["!SASTID"]`,
		`section=code_scanning, key=include_tools, before=every tool, after=["CodeQL" "Trivy"]`,
		"section=code_scanning, key=interval, before=default (1h0m0s), after=30m0s",
		"section=code_scanning, key=max_new_issues_per_run, before=default (5), after=3",
		"section=code_scanning, key=repos[repo1], before=no override, after=enabled=true min_severity=low",
	}
	if got := codeScanningAuditDetails(s); !reflect.DeepEqual(got, want) {
		t.Fatalf("audit details =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, row := range codeScanningAuditRows(s) {
		if row.User != "alice" || row.Action != auditConfigCodeScanning {
			t.Fatalf("audit row %+v, want actor alice and action %s", row, auditConfigCodeScanning)
		}
	}
	if !auditSensitiveActions[auditConfigCodeScanning] {
		t.Fatalf("%s must be a sensitive audit action, as the other config_ changes are", auditConfigCodeScanning)
	}

	// The same body again changes nothing, so it writes no entry.
	decodeCodeScanningSettings(t, codeScanningPutAs(s, "alice", body))
	if got := codeScanningAuditDetails(s); len(got) != len(want) {
		t.Fatalf("a PUT that changed nothing was audited: %v", got)
	}
}

// codeScanningFullBody sets every key, with complete repos entries, so what
// GET returns can be compared with it field by field.
func codeScanningFullBody() map[string]any {
	return map[string]any{
		"enabled":                true,
		"interval":               "45m0s",
		"min_severity":           "medium",
		"include_tools":          []string{"CodeQL", "Trivy"},
		"exclude_rules":          []string{"!SASTID", "cpp/commented-out-*"},
		"max_new_issues_per_run": 3,
		"repos": map[string]any{
			"repo1":       map[string]any{"enabled": false, "min_severity": "", "include_tools": []string{}, "exclude_rules": []string{}, "max_new_issues_per_run": 0},
			"myorg/repo2": map[string]any{"enabled": nil, "min_severity": "critical", "include_tools": []string{"CodeQL"}, "exclude_rules": []string{"js/x"}, "max_new_issues_per_run": 1},
		},
	}
}

// GET after PUT returns exactly what was saved: the PUT's own answer and a
// later GET are the same bytes, and the as-set fields are the body's values.
func TestCodeScanningSettingsGetAfterPutIsWhatWasSaved(t *testing.T) {
	s := codeScanningTwoRepoServer(t)
	body := codeScanningFullBody()
	put := codeScanningPutAs(s, "alice", body)
	if put.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d: %s", put.Code, put.Body.String())
	}
	get := doOwnerGet(s, codeScanningTestPath)
	if get.Code != http.StatusOK || get.Body.String() != put.Body.String() {
		t.Fatalf("GET after PUT differs from the PUT's answer:\nPUT %s\nGET %s", put.Body.String(), get.Body.String())
	}
	var saved map[string]any
	if err := json.Unmarshal(get.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	sent, _ := json.Marshal(body)
	var want map[string]any
	_ = json.Unmarshal(sent, &want)
	for key, value := range want {
		if !reflect.DeepEqual(saved[key], value) {
			t.Errorf("GET %s = %v, want what was saved: %v", key, saved[key], value)
		}
	}
	got := decodeCodeScanningSettings(t, get)
	if got.IntervalEffective != "45m0s" || got.MinSeverityEffective != "medium" || got.MaxNewIssuesPerRunEffective != 3 {
		t.Fatalf("effective values do not follow the saved ones: %+v", got)
	}
	// The effective exclusions are the defaults, minus the "!" entry, plus the saved pattern.
	if strings.Contains(strings.Join(got.ExcludeRulesEffective, " "), "SASTID") || got.ExcludeRulesEffective[len(got.ExcludeRulesEffective)-1] != "cpp/commented-out-*" {
		t.Fatalf("effective exclude_rules = %v", got.ExcludeRulesEffective)
	}
	// repo1 is off by its override; repo2 inherits the switch (on) and keeps its other overrides.
	if row := codeScanningRepoRow(t, got, "myorg/repo1"); row.Enabled || row.EnabledFrom != "repo" {
		t.Fatalf("repo1 row = %+v", row)
	}
	if row := codeScanningRepoRow(t, got, "myorg/repo2"); !row.Enabled || row.EnabledFrom != "default" {
		t.Fatalf("repo2 row = %+v", row)
	}
}

// A saved change is in hive.yaml, with the same keys, and a hive started from
// that file answers GET as the hive that saved it did.
func TestCodeScanningSettingsPersistRoundTrip(t *testing.T) {
	s := codeScanningTwoRepoServer(t)
	s.deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	put := codeScanningPutAs(s, "alice", codeScanningFullBody())
	if put.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d: %s", put.Code, put.Body.String())
	}
	data, err := os.ReadFile(s.deps.Config.SourcePath)
	if err != nil {
		t.Fatalf("read saved hive.yaml: %v", err)
	}
	var onDisk struct {
		CodeScanning config.CodeScanningConfig `yaml:"code_scanning"`
	}
	if err := yaml.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("decode saved hive.yaml: %v", err)
	}
	if !reflect.DeepEqual(onDisk.CodeScanning, s.deps.Config.CodeScanning) {
		t.Fatalf("hive.yaml holds\n%+v\nthe live config holds\n%+v", onDisk.CodeScanning, s.deps.Config.CodeScanning)
	}
	if onDisk.CodeScanning.Interval != 45*time.Minute || !onDisk.CodeScanning.Enabled || len(onDisk.CodeScanning.Repos) != 2 {
		t.Fatalf("hive.yaml code_scanning = %+v", onDisk.CodeScanning)
	}
	// What the file holds passes the check hive.yaml loading runs.
	if err := config.ValidateCodeScanningSettings("myorg", []string{"repo1", "repo2"}, onDisk.CodeScanning); err != nil {
		t.Fatalf("the saved block would not load: %v", err)
	}

	// A restart: a second hive whose code_scanning block is the one read
	// back from the file.
	restarted := codeScanningTwoRepoServer(t)
	restarted.deps.Config.CodeScanning = onDisk.CodeScanning
	if get := doOwnerGet(restarted, codeScanningTestPath); get.Code != http.StatusOK || get.Body.String() != put.Body.String() {
		t.Fatalf("after a restart GET answers\n%s\nbefore it the PUT answered\n%s", get.Body.String(), put.Body.String())
	}
}

// Only a verified owner may read or change the settings.
func TestCodeScanningSettingsRefusesNonOwner(t *testing.T) {
	s := codeScanningTwoRepoServer(t)
	if rec := doGetNoRole(s, codeScanningTestPath); rec.Code != http.StatusForbidden {
		t.Fatalf("GET with no role: expected 403, got %d", rec.Code)
	}
	if rec := doPutNoRole(s, codeScanningTestPath, `{"enabled": true}`); rec.Code != http.StatusForbidden {
		t.Fatalf("PUT with no role: expected 403, got %d", rec.Code)
	}
	for _, role := range []string{"owner", "merger", "read"} {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(method, codeScanningTestPath, strings.NewReader(`{"enabled": true}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Hive-Role", role) // claimed, never verified
			s.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s with unverified role %s: expected 403, got %d", method, role, rec.Code)
			}
		}
	}
	if s.deps.Config.CodeScanning.Enabled {
		t.Fatal("a refused PUT turned the sweep on")
	}
	if rows := codeScanningAuditRows(s); len(rows) != 0 {
		t.Fatalf("a refused PUT was audited: %+v", rows)
	}
}

// codeScanningGitHubStub answers the two reads a sweep pass makes on a
// repository with no open alerts, refuses the alert listing of the
// repositories in denied, and counts every request.
type codeScanningGitHubStub struct {
	mu       sync.Mutex
	denied   map[string]bool
	requests int
	listed   map[string]int
}

func (g *codeScanningGitHubStub) start(t *testing.T) *httptest.Server {
	t.Helper()
	g.listed = map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.requests++
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/myorg/"), "/")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/code-scanning/alerts"):
			g.listed[parts[0]]++
			if g.denied[parts[0]] {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
				return
			}
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues"):
			_, _ = w.Write([]byte(`[]`))
		default:
			// With no open alert the sweep has nothing to write.
			if r.Method != http.MethodGet {
				t.Errorf("unexpected GitHub write: %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (g *codeScanningGitHubStub) count() (requests int, listed map[string]int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[string]int{}
	for k, v := range g.listed {
		out[k] = v
	}
	return g.requests, out
}

func codeScanningSweepClient(t *testing.T, g *codeScanningGitHubStub, repos []string) *github.Client {
	t.Helper()
	t.Cleanup(github.ResetRESTAccountingForTest(nil))
	return github.NewClientForTest(g.start(t).URL, "myorg", repos, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// The per-repository access state is what the last sweep pass recorded, and
// reading it makes no GitHub call. A repository the sweep is on for but has
// not reached is not_checked, never readable.
func TestCodeScanningSettingsAlertsAccessIsWhatTheLastPassRecorded(t *testing.T) {
	s := covApiServer(t)
	repos := []string{"repo1", "repo2", "repo3", "repo4"}
	s.deps.Config.Project.Repos = repos
	stub := &codeScanningGitHubStub{denied: map[string]bool{"repo2": true}}
	client := codeScanningSweepClient(t, stub, repos)
	s.deps.GHClient = client
	decodeCodeScanningSettings(t, codeScanningPutAs(s, "alice", map[string]any{"repos": map[string]any{
		"repo1": map[string]any{"enabled": true},
		"repo2": map[string]any{"enabled": true},
	}}))
	passAt := time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC)
	client.RunCodeScanningSweep(context.Background(), s.deps.Config, passAt)
	afterPass, _ := stub.count()
	if afterPass == 0 {
		t.Fatal("the sweep pass made no GitHub call; the stub proves nothing")
	}

	// repo3 is turned on after the pass.
	got := decodeCodeScanningSettings(t, codeScanningPutAs(s, "alice", map[string]any{"repos": map[string]any{
		"repo1": map[string]any{"enabled": true},
		"repo2": map[string]any{"enabled": true},
		"repo3": map[string]any{"enabled": true},
	}}))
	if row := codeScanningRepoRow(t, got, "myorg/repo1"); row.AlertsAccess != codeScanningAccessReadable || row.AlertsAccessDetail != "" || row.CheckedAt != "2026-11-13T15:00:00Z" {
		t.Fatalf("repo1 row = %+v, want readable as of the pass", row)
	}
	if row := codeScanningRepoRow(t, got, "myorg/repo2"); row.AlertsAccess != codescanning.NoticeMissingPermission || !strings.Contains(row.AlertsAccessDetail, "security_events") {
		t.Fatalf("repo2 row = %+v, want the missing-permission notice", row)
	}
	if row := codeScanningRepoRow(t, got, "myorg/repo3"); !row.Enabled || row.AlertsAccess != codeScanningAccessNotChecked || row.CheckedAt != "" {
		t.Fatalf("repo3 row = %+v, want on and not_checked", row)
	}
	if row := codeScanningRepoRow(t, got, "myorg/repo4"); row.Enabled || row.AlertsAccess != codeScanningAccessOff {
		t.Fatalf("repo4 row = %+v, want off", row)
	}
	decodeCodeScanningSettings(t, doOwnerGet(s, codeScanningTestPath))
	if now, _ := stub.count(); now != afterPass {
		t.Fatalf("reading or saving the settings made %d GitHub call(s)", now-afterPass)
	}

	// Turned off again: the row says off, whatever the last pass recorded.
	got = decodeCodeScanningSettings(t, codeScanningPutAs(s, "alice", map[string]any{"repos": map[string]any{}}))
	if row := codeScanningRepoRow(t, got, "myorg/repo2"); row.Enabled || row.AlertsAccess != codeScanningAccessOff || row.AlertsAccessDetail != "" {
		t.Fatalf("repo2 row after turning it off = %+v", row)
	}
}

// A saved change is what the next sweep pass runs with. The sweep workers
// run on a snapshot of the live config (cmd/hive publishSweepConfig calls
// Config.Snapshot at the top of every governor tick), so each pass here takes
// a fresh snapshot of the config the handler changed.
func TestCodeScanningSettingsChangeIsWhatTheNextSweepPassRunsWith(t *testing.T) {
	s := codeScanningTwoRepoServer(t)
	stub := &codeScanningGitHubStub{}
	client := codeScanningSweepClient(t, stub, []string{"repo1", "repo2"})
	pass := func() map[string]int {
		t.Helper()
		snapshot, err := s.deps.Config.Snapshot()
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		client.RunCodeScanningSweep(context.Background(), snapshot, time.Now())
		_, listed := stub.count()
		return listed
	}
	if listed := pass(); len(listed) != 0 {
		t.Fatalf("a sweep that is off listed alerts: %v", listed)
	}

	decodeCodeScanningSettings(t, codeScanningPutAs(s, "alice", map[string]any{"repos": map[string]any{"repo1": map[string]any{"enabled": true}}}))
	if listed := pass(); listed["repo1"] != 1 || listed["repo2"] != 0 {
		t.Fatalf("after turning repo1 on the pass listed %v, want repo1 once and repo2 never", listed)
	}

	decodeCodeScanningSettings(t, codeScanningPutAs(s, "alice", map[string]any{"repos": map[string]any{"repo1": map[string]any{"enabled": false}}}))
	if listed := pass(); listed["repo1"] != 1 || listed["repo2"] != 0 {
		t.Fatalf("after turning repo1 off the pass listed %v, want nothing new", listed)
	}
}

// The governor settings bundle the Features tab loads carries the block, so
// the section renders without a second request.
func TestCodeScanningSettingsAreInTheGovernorBundle(t *testing.T) {
	s := codeScanningTwoRepoServer(t)
	decodeCodeScanningSettings(t, codeScanningPutAs(s, "alice", map[string]any{"repos": map[string]any{"repo1": map[string]any{"enabled": true}}}))
	rec := doOwnerGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config/governor: %d", rec.Code)
	}
	var bundle struct {
		CodeScanning *codeScanningSettings `json:"code_scanning"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &bundle); err != nil || bundle.CodeScanning == nil {
		t.Fatalf("the governor settings bundle has no code_scanning block: %v", err)
	}
	want := decodeCodeScanningSettings(t, doOwnerGet(s, codeScanningTestPath))
	if !reflect.DeepEqual(*bundle.CodeScanning, want) {
		t.Fatalf("bundle code_scanning =\n%+v\nGET %s =\n%+v", *bundle.CodeScanning, codeScanningTestPath, want)
	}
}
