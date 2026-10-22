package compliance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestEvaluators(t *testing.T) {
	cases := []struct {
		name    string
		ev      string
		v       any
		rec     string
		want    Status
		wantCur string
	}{
		{"equals bool meets", EvalEquals, true, "true", StatusMeets, "true"},
		{"equals bool off", EvalEquals, false, "true", StatusOff, "false"},
		{"equals bool want false meets", EvalEquals, false, "false", StatusMeets, "false"},
		{"equals bool want false deviates", EvalEquals, true, "false", StatusDeviates, "true"},
		{"equals case-insensitive string", EvalEquals, "Merger", " merger ", StatusMeets, "Merger"},
		{"equals string deviates", EvalEquals, "owner", "merger", StatusDeviates, "owner"},
		{"equals int", EvalEquals, 3, "3", StatusMeets, "3"},
		{"equals unset", EvalEquals, nil, "true", StatusOff, "unset"},
		{"non_empty meets", EvalNonEmpty, []string{"a", "b"}, recNonEmpty, StatusMeets, "a; b"},
		{"non_empty off", EvalNonEmpty, []string{}, recNonEmpty, StatusOff, "none"},
		{"non_empty nil", EvalNonEmpty, nil, recNonEmpty, StatusOff, "none"},
		{"non_empty string meets", EvalNonEmpty, "/secrets/backup-key", recNonEmpty, StatusMeets, "/secrets/backup-key"},
		{"non_empty blank string off", EvalNonEmpty, "  ", recNonEmpty, StatusOff, "none"},
		{"empty meets", EvalEmpty, []string{}, recNone, StatusMeets, "none"},
		{"empty deviates", EvalEmpty, []string{"test_removal"}, recNone, StatusDeviates, "test_removal"},
		{"at_least meets", EvalAtLeast, 365, "365", StatusMeets, "365"},
		{"at_least deviates", EvalAtLeast, 90, "365", StatusDeviates, "90"},
		{"at_least unset", EvalAtLeast, nil, "365", StatusOff, "unset"},
		{"at_most meets", EvalAtMost, 5, "5", StatusMeets, "5"},
		{"at_most deviates", EvalAtMost, 6, "5", StatusDeviates, "6"},
		{"at_most unset", EvalAtMost, nil, "5", StatusOff, "unset"},
		{"max_owners empty", EvalMaxOwners, []string{}, "2", StatusOff, "none"},
		{"max_owners implicit first owner", EvalMaxOwners, []string{"alice", "bob"}, "2", StatusMeets, "1 owner(s) of 2 user(s)"},
		{"max_owners too many", EvalMaxOwners, []string{"a", "b:owner", "c:OWNER"}, "2", StatusDeviates, "3 owner(s) of 3 user(s)"},
		{"max_owners first with explicit role", EvalMaxOwners, []string{"a:read", "b:owner"}, "1", StatusMeets, "1 owner(s) of 2 user(s)"},
		{"max_owners provider prefix is not a role", EvalMaxOwners, []string{"ibmid:5500", "google:x"}, "1", StatusMeets, "1 owner(s) of 2 user(s)"},
		{"min_mergers meets", EvalMinMergers, []string{"alice", "bob:merger"}, "1", StatusMeets, "1 merger(s) of 2 user(s)"},
		{"min_mergers deviates", EvalMinMergers, []string{"alice", "bob:read-write"}, "1", StatusDeviates, "0 merger(s) of 2 user(s)"},
		{"min_mergers empty", EvalMinMergers, nil, "1", StatusOff, "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := evaluators[tc.ev]
			if !ok {
				t.Fatalf("no evaluator %s", tc.ev)
			}
			got, cur := ev.eval(tc.v, tc.rec)
			if got != tc.want || cur != tc.wantCur {
				t.Fatalf("eval(%v, %q) = (%s, %q), want (%s, %q)", tc.v, tc.rec, got, cur, tc.want, tc.wantCur)
			}
		})
	}
}

func TestFormatValue(t *testing.T) {
	if got := formatValue(3.5); got != "3.5" {
		t.Fatalf("formatValue(float) = %q", got)
	}
	if got := formatValue("x"); got != "x" {
		t.Fatalf("formatValue(string) = %q", got)
	}
}

func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }

func soc2Config() *config.Config {
	return &config.Config{Compliance: config.ComplianceConfig{Frameworks: []string{"soc2-type2"}}}
}

// testBackupKeyFile is the key_file path hardenedConfig points at; the
// TestEvaluateSOC2 readKeyFile stub makes it (and only it) resolvable.
const testBackupKeyFile = "/test/secrets/backup_encryption_key"

// stubKeyFiles swaps readKeyFile so the named paths resolve to a key and
// every other path is missing, restoring the real reader on cleanup.
func stubKeyFiles(t *testing.T, present ...string) {
	t.Helper()
	prev := readKeyFile
	readKeyFile = func(path string) ([]byte, error) {
		for _, p := range present {
			if p == path {
				return []byte("0123abcd\n"), nil
			}
		}
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() { readKeyFile = prev })
}

func hardenedConfig() *config.Config {
	cfg := soc2Config()
	cfg.Dashboard.AuthorizedUsers = []string{"alice", "bob:merger", "carol:read"}
	cfg.Review.RequireApproval = true
	cfg.Review.PostComments = true
	cfg.AutoMerge.SelfAuthored = boolPtr(false)
	cfg.AutoMerge.RequiredChecks = []string{"build-gate"}
	cfg.AutoMerge.HumanMergePaths = map[string][]string{"o/r": {".github/**", " "}, " ": {"x"}, "o/empty": {" "}}
	cfg.AgentSandbox.Enabled = true
	cfg.ToolApproval.Enabled = true
	cfg.ACMMLevel = intPtr(4)
	cfg.Governor.Backup.KeyFile = testBackupKeyFile
	return cfg
}

func injectOn(name string) string {
	if name == config.ProxyInjectGHAuthEnv {
		return "true"
	}
	return ""
}

func statusByID(statuses []ControlStatus) map[string]Status {
	out := map[string]Status{}
	for _, s := range statuses {
		out[s.ControlID] = s.Status
	}
	return out
}

func soc2ExpectedStatuses(t *testing.T, covered map[string]Status) map[string]Status {
	t.Helper()
	out := make(map[string]Status, len(mustProfile(t, "soc2-type2").Controls))
	for _, control := range mustProfile(t, "soc2-type2").Controls {
		if control.NotCovered {
			out[control.ID] = StatusNotCovered
		}
	}
	for id, status := range covered {
		out[id] = status
	}
	return out
}

func TestEvaluateSOC2(t *testing.T) {
	stubKeyFiles(t, testBackupKeyFile)
	cases := []struct {
		name   string
		cfg    *config.Config
		getenv func(string) string
		want   map[string]Status
	}{
		{
			name:   "defaults",
			cfg:    soc2Config(),
			getenv: func(string) string { return "" },
			want: soc2ExpectedStatuses(t, map[string]Status{
				"CC4.1": StatusDeviates,
				"CC6.1": StatusOff, "CC6.2": StatusOff, "CC6.3": StatusDeviates,
				"CC6.6": StatusOff, "CC6.7": StatusOff, "CC6.8": StatusMeets,
				"CC7.1": StatusDeviates, "CC7.2": StatusDeviates, "CC7.3": StatusMeets,
				"CC8.1": StatusDeviates, "CC9.1": StatusOff,
				"A1.2": StatusOff,
			}),
		},
		{
			name:   "hardened",
			cfg:    hardenedConfig(),
			getenv: injectOn,
			want: soc2ExpectedStatuses(t, map[string]Status{
				"CC4.1": StatusMeets,
				"CC6.1": StatusMeets, "CC6.2": StatusMeets, "CC6.3": StatusMeets,
				"CC6.6": StatusMeets, "CC6.7": StatusMeets, "CC6.8": StatusMeets,
				"CC7.1": StatusMeets,
				// Audit retention is fixed at 90 days until #11077 makes it configurable.
				"CC7.2": StatusDeviates,
				"CC7.3": StatusMeets, "CC8.1": StatusMeets, "CC9.1": StatusMeets,
				"A1.2": StatusMeets,
			}),
		},
		{
			name: "risky",
			cfg: func() *config.Config {
				c := hardenedConfig()
				c.Dashboard.AuthorizedUsers = []string{"a", "b:owner", "c:owner"}
				c.AutoMerge.TrustedAuthors.Enabled = true
				c.Sentinel.Enabled = boolPtr(false)
				c.Sentinel.DisabledBehaviors = []string{"test_removal"}
				c.Escalation.Disabled = true
				c.ACMMLevel = intPtr(6)
				return c
			}(),
			getenv: injectOn,
			want: soc2ExpectedStatuses(t, map[string]Status{
				"CC4.1": StatusDeviates,
				"CC6.1": StatusDeviates, "CC6.2": StatusMeets, "CC6.3": StatusDeviates,
				"CC6.6": StatusMeets, "CC6.7": StatusMeets, "CC6.8": StatusDeviates,
				"CC7.1": StatusDeviates, "CC7.2": StatusDeviates, "CC7.3": StatusDeviates,
				"CC8.1": StatusDeviates, "CC9.1": StatusDeviates,
				"A1.2": StatusMeets,
			}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := statusByID(EvaluateWith(tc.cfg, tc.getenv))
			if !reflect.DeepEqual(got, tc.want) {
				for id, w := range tc.want {
					if got[id] != w {
						t.Errorf("%s: got %s, want %s", id, got[id], w)
					}
				}
				for id := range got {
					if _, ok := tc.want[id]; !ok {
						t.Errorf("unexpected control %s", id)
					}
				}
			}
		})
	}
}

// TestBackupKeySourceFollowsResolution pins A1.2 to the key source the
// backup path actually resolves, not the raw key_file setting.
func TestBackupKeySourceFollowsResolution(t *testing.T) {
	a12 := func(cfg *config.Config, getenv func(string) string) (Status, string) {
		for _, cs := range EvaluateWith(cfg, getenv) {
			if cs.ControlID != "A1.2" {
				continue
			}
			if len(cs.Settings) != 1 {
				t.Fatalf("A1.2 settings = %d, want 1", len(cs.Settings))
			}
			return cs.Status, cs.Settings[0].Current
		}
		t.Fatal("A1.2 not evaluated")
		return "", ""
	}
	noEnv := func(string) string { return "" }
	backupEnv := func(name string) string {
		if name == config.BackupKeyEnv {
			return "0123abcd"
		}
		return ""
	}
	altEnv := func(name string) string {
		if name == "HIVE_ALT_BACKUP_KEY" {
			return "0123abcd"
		}
		return ""
	}
	cases := []struct {
		name       string
		keyFile    string
		keyEnv     string
		present    []string
		getenv     func(string) string
		wantStatus Status
		wantSource string
	}{
		{"unset everywhere is off", "", "", nil, noEnv, StatusOff, recNone},
		{"key_file pointing at a missing file is off", testBackupKeyFile, "", nil, noEnv, StatusOff, recNone},
		{"key_file present meets", testBackupKeyFile, "", []string{testBackupKeyFile}, noEnv, StatusMeets, "file:" + testBackupKeyFile},
		{"default Secret mount meets with key_file unset", "", "", []string{config.DefaultBackupKeyFile}, noEnv, StatusMeets, "file:" + config.DefaultBackupKeyFile},
		{"dashboard-written PVC file meets with key_file unset", "", "", []string{config.WritableBackupKeyFile}, noEnv, StatusMeets, "file:" + config.WritableBackupKeyFile},
		{"HIVE_BACKUP_KEY fallback meets with key_file unset", "", "", nil, backupEnv, StatusMeets, "env:" + config.BackupKeyEnv},
		{"HIVE_BACKUP_KEY fallback meets with key_file missing", testBackupKeyFile, "", nil, backupEnv, StatusMeets, "env:" + config.BackupKeyEnv},
		{"key_env fallback meets", "", "HIVE_ALT_BACKUP_KEY", nil, altEnv, StatusMeets, "env:HIVE_ALT_BACKUP_KEY"},
		{"nil getenv falls back safely", "", "", nil, nil, StatusOff, recNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubKeyFiles(t, tc.present...)
			cfg := soc2Config()
			cfg.Governor.Backup.KeyFile = tc.keyFile
			cfg.Governor.Backup.KeyEnv = tc.keyEnv
			status, current := a12(cfg, tc.getenv)
			if status != tc.wantStatus {
				t.Errorf("A1.2 status = %s, want %s", status, tc.wantStatus)
			}
			if current != tc.wantSource {
				t.Errorf("A1.2 current = %q, want %q", current, tc.wantSource)
			}
		})
	}
}

func TestEvaluateCarriesCurrentAndRecommended(t *testing.T) {
	st := EvaluateWith(hardenedConfig(), injectOn)
	var cc72 ControlStatus
	for _, s := range st {
		if s.ControlID == "CC7.2" {
			cc72 = s
		}
		if s.Framework != "soc2-type2" || s.Title == "" || s.Text == "" || s.Citation == "" || s.Domain == "" {
			t.Fatalf("incomplete status %+v", s)
		}
		if s.NotCovered != (s.Status == StatusNotCovered) || (s.NotCovered && len(s.Settings) != 0) {
			t.Fatalf("not-covered shape wrong: %+v", s)
		}
	}
	if len(cc72.Settings) != 2 {
		t.Fatalf("CC7.2 settings = %+v", cc72.Settings)
	}
	audit := cc72.Settings[0]
	want := SettingStatus{
		SettingPath: "audit.retention_days", Kind: kindInt, Description: settings["audit.retention_days"].Description,
		Evaluator: EvalAtLeast, Current: "90", Recommended: "365", Status: StatusDeviates, Builtin: true,
	}
	if audit != want {
		t.Fatalf("audit mapping = %+v, want %+v", audit, want)
	}
	var cc81 ControlStatus
	for _, s := range st {
		if s.ControlID == "CC8.1" {
			cc81 = s
		}
	}
	for _, ss := range cc81.Settings {
		if ss.SettingPath == "auto_merge.human_merge_paths" && ss.Current != "o/r: .github/**" {
			t.Fatalf("human_merge_paths current = %q", ss.Current)
		}
	}
}

func TestEvaluateEdgeCases(t *testing.T) {
	if Evaluate(nil) != nil {
		t.Fatal("nil config must yield nil")
	}
	if got := Evaluate(&config.Config{}); got != nil {
		t.Fatalf("no frameworks selected must yield nil, got %d", len(got))
	}
	cfg := &config.Config{Compliance: config.ComplianceConfig{Frameworks: []string{"unknown-fw", "soc2-type2"}}}
	t.Setenv(config.ProxyInjectGHAuthEnv, "true")
	got := Evaluate(cfg)
	if len(got) != len(mustProfile(t, "soc2-type2").Controls) {
		t.Fatalf("unknown framework must be skipped, got %d statuses", len(got))
	}
	if statusByID(got)["CC6.1"] != StatusDeviates {
		// authorized_users empty → off, injection on → meets: mixed = deviates.
		t.Fatalf("Evaluate must read the process env: CC6.1 = %s", statusByID(got)["CC6.1"])
	}
	// nil cfg and getenv are tolerated by EvaluateProfile.
	if st := EvaluateProfile(mustProfile(t, "soc2-type2"), nil, nil); len(st) == 0 {
		t.Fatal("EvaluateProfile(nil, nil) must still evaluate defaults")
	}
}

func TestEvaluateMappingUnknownFailsClosed(t *testing.T) {
	ss := evaluateMapping(Mapping{SettingPath: "nope", Evaluator: EvalEquals, Recommended: "x"}, &config.Config{}, nil)
	if ss.Status != StatusDeviates {
		t.Fatalf("unknown setting must deviate, got %s", ss.Status)
	}
	ss = evaluateMapping(Mapping{SettingPath: "sentinel.enabled", Evaluator: "nope", Recommended: "x"}, &config.Config{}, nil)
	if ss.Status != StatusDeviates {
		t.Fatalf("unknown evaluator must deviate, got %s", ss.Status)
	}
}

func TestSummarize(t *testing.T) {
	got := Summarize([]ControlStatus{
		{Status: StatusMeets}, {Status: StatusMeets}, {Status: StatusDeviates},
		{Status: StatusOff}, {Status: StatusNotCovered}, {Status: "weird"},
	})
	want := Summary{Meets: 2, Deviates: 2, Off: 1, NotCovered: 1}
	if got != want {
		t.Fatalf("Summarize = %+v, want %+v", got, want)
	}
}

func TestBuildReport(t *testing.T) {
	stubKeyFiles(t, testBackupKeyFile)
	cfg := hardenedConfig()
	cfg.Compliance.Frameworks = append(cfg.Compliance.Frameworks, "not-shipped")
	r := BuildReport(cfg, injectOn)
	if r.Disclaimer != Disclaimer || !strings.Contains(r.Disclaimer, "not certified") {
		t.Fatalf("disclaimer = %q", r.Disclaimer)
	}
	if !reflect.DeepEqual(r.Frameworks, []string{"soc2-type2"}) || !reflect.DeepEqual(r.UnknownFrameworks, []string{"not-shipped"}) {
		t.Fatalf("frameworks = %v unknown = %v", r.Frameworks, r.UnknownFrameworks)
	}
	if len(r.Available) == 0 || len(r.Available) != 3 || r.Available[2].ID != "soc2-type2" || r.Available[0].Controls == 0 {
		t.Fatalf("available = %+v", r.Available)
	}
	if r.PostureCheckInterval != "1h0m0s" || r.PostureChecksPending || r.PostureChecksTracking != "" || r.ProfileLoadError != "" {
		t.Fatalf("report meta = %+v", r)
	}
	wantNotCovered := 0
	for _, c := range mustProfile(t, "soc2-type2").Controls {
		if c.NotCovered {
			wantNotCovered++
		}
	}
	if r.Summary != Summarize(r.Controls) || r.Summary.NotCovered != wantNotCovered || r.Summary.Off != 0 {
		t.Fatalf("summary = %+v, want not_covered=%d off=0", r.Summary, wantNotCovered)
	}
	empty := BuildReport(nil, nil)
	if empty.Controls == nil || empty.Frameworks == nil || len(empty.Controls) != 0 {
		t.Fatalf("nil config report must have empty, non-nil slices: %+v", empty)
	}
	raw, err := json.Marshal(empty)
	if err != nil || !strings.Contains(string(raw), `"controls":[]`) {
		t.Fatalf("json = %s (%v)", raw, err)
	}
}

// TestComplianceDocTablesMatchProfiles keeps the generated mapping tables in
// docs/compliance.md identical to the embedded profiles. Regenerate with
// src/scripts/render-compliance-tables.py.
func TestComplianceDocTablesMatchProfiles(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "compliance.md"))
	if err != nil {
		t.Fatalf("reading docs/compliance.md: %v", err)
	}
	doc := string(raw)
	ps, err := Profiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		begin, end := TableMarkers(p.ID)
		b := strings.Index(doc, begin+"\n")
		e := strings.Index(doc, end)
		if b < 0 || e < b {
			t.Errorf("docs/compliance.md has no generated table for %s (markers %q / %q)", p.ID, begin, end)
			continue
		}
		got := doc[b+len(begin)+1 : e]
		if want := RenderTable(p); got != want {
			t.Errorf("docs/compliance.md table for %s is stale; run src/scripts/render-compliance-tables.py\n--- got\n%s\n--- want\n%s", p.ID, got, want)
		}
	}
}

func TestRenderTableEscapesAndNotCovered(t *testing.T) {
	p := Profile{ID: "x", Controls: []Control{
		{ID: "A.1", Title: "a | b", Domain: "Access control", NotCovered: true, Rationale: "line one\n  line two"},
		{ID: "A.2", Title: "t", Domain: "Change management", Rationale: "why", Mappings: []Mapping{
			{SettingPath: "s.a", Recommended: "1", Evaluator: "e"},
			{SettingPath: "s.b", Recommended: "2", Evaluator: "e"},
		}},
	}}
	got := RenderTable(p)
	for _, want := range []string{
		"| A.1 — a \\| b | Access control | _not covered by Hive_ | — | — | line one line two |\n",
		"| A.2 — t | Change management | `s.a` | `1` | `e` | why |\n",
		"| A.2 — t | Change management | `s.b` | `2` | `e` |  |\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderTable missing row %q in:\n%s", want, got)
		}
	}
}
