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

func TestEvaluateSOC2(t *testing.T) {
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
			want: map[string]Status{
				"CC6.1": StatusOff, "CC6.2": StatusOff, "CC6.3": StatusDeviates, "CC6.4": StatusNotCovered,
				"CC6.6": StatusOff, "CC6.8": StatusMeets, "CC7.1": StatusDeviates, "CC7.2": StatusDeviates,
				"CC7.3": StatusMeets, "CC7.4": StatusNotCovered, "CC7.5": StatusNotCovered,
				"CC8.1": StatusDeviates, "CC9.1": StatusOff, "CC9.2": StatusNotCovered,
			},
		},
		{
			name:   "hardened",
			cfg:    hardenedConfig(),
			getenv: injectOn,
			want: map[string]Status{
				"CC6.1": StatusMeets, "CC6.2": StatusMeets, "CC6.3": StatusMeets, "CC6.4": StatusNotCovered,
				"CC6.6": StatusMeets, "CC6.8": StatusMeets, "CC7.1": StatusMeets,
				// Audit retention is fixed at 90 days until #11077 makes it configurable.
				"CC7.2": StatusDeviates,
				"CC7.3": StatusMeets, "CC7.4": StatusNotCovered, "CC7.5": StatusNotCovered,
				"CC8.1": StatusMeets, "CC9.1": StatusMeets, "CC9.2": StatusNotCovered,
			},
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
			want: map[string]Status{
				"CC6.1": StatusDeviates, "CC6.2": StatusMeets, "CC6.3": StatusDeviates, "CC6.4": StatusNotCovered,
				"CC6.6": StatusMeets, "CC6.8": StatusDeviates, "CC7.1": StatusDeviates, "CC7.2": StatusDeviates,
				"CC7.3": StatusDeviates, "CC7.4": StatusNotCovered, "CC7.5": StatusNotCovered,
				"CC8.1": StatusDeviates, "CC9.1": StatusDeviates, "CC9.2": StatusNotCovered,
			},
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

func TestEvaluateCarriesCurrentAndRecommended(t *testing.T) {
	st := EvaluateWith(hardenedConfig(), injectOn)
	var cc72 ControlStatus
	for _, s := range st {
		if s.ControlID == "CC7.2" {
			cc72 = s
		}
		if s.Framework != "soc2-type2" || s.Title == "" || s.Citation == "" || s.Domain == "" {
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
	want := SettingStatus{SettingPath: "audit.retention_days", Evaluator: EvalAtLeast, Current: "90", Recommended: "365", Status: StatusDeviates, Builtin: true}
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
	cfg := hardenedConfig()
	cfg.Compliance.Frameworks = append(cfg.Compliance.Frameworks, "not-shipped")
	r := BuildReport(cfg, injectOn)
	if r.Disclaimer != Disclaimer || !strings.Contains(r.Disclaimer, "not certified") {
		t.Fatalf("disclaimer = %q", r.Disclaimer)
	}
	if !reflect.DeepEqual(r.Frameworks, []string{"soc2-type2"}) || !reflect.DeepEqual(r.UnknownFrameworks, []string{"not-shipped"}) {
		t.Fatalf("frameworks = %v unknown = %v", r.Frameworks, r.UnknownFrameworks)
	}
	if len(r.Available) == 0 || r.Available[0].ID != "soc2-type2" || r.Available[0].Controls == 0 {
		t.Fatalf("available = %+v", r.Available)
	}
	if r.PostureCheckInterval != "1h0m0s" || !r.PostureChecksPending || r.ProfileLoadError != "" {
		t.Fatalf("report meta = %+v", r)
	}
	if r.Summary != Summarize(r.Controls) || r.Summary.NotCovered != 4 {
		t.Fatalf("summary = %+v", r.Summary)
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
