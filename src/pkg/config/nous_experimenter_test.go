package config

import (
	"strings"
	"testing"
)

// TestNousResolveDispatcher pins the precedence env > yaml > default and the
// fall-through on unknown values, so an existing config with no `dispatcher`
// key (every config written before part 5) resolves to inline.
func TestNousResolveDispatcher(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		yaml string
		want string
	}{
		{"default is inline", "", "", NousDispatcherInline},
		{"yaml sdk", "", "sdk", NousDispatcherSDK},
		{"yaml normalised", "", "  SDK ", NousDispatcherSDK},
		{"yaml unknown falls through", "", "legacy", DefaultNousDispatcher},
		{"env wins", "sdk", "inline", NousDispatcherSDK},
		{"env unknown falls to yaml", "wat", "sdk", NousDispatcherSDK},
		{"env unknown and yaml unknown", "wat", "nope", DefaultNousDispatcher},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(string) string { return tc.env }
			if got := (NousConfig{Dispatcher: tc.yaml}).ResolveDispatcher(getenv); got != tc.want {
				t.Fatalf("ResolveDispatcher = %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("nil getenv skips env tier", func(t *testing.T) {
		t.Setenv(NousDispatcherEnv, NousDispatcherSDK)
		if got := (NousConfig{}).ResolveDispatcher(nil); got != DefaultNousDispatcher {
			t.Fatalf("ResolveDispatcher(nil) = %q, want %q", got, DefaultNousDispatcher)
		}
		if got := (NousConfig{}).ResolvedDispatcher(); got != NousDispatcherSDK {
			t.Fatalf("ResolvedDispatcher() = %q, want env %q", got, NousDispatcherSDK)
		}
	})
}

func TestNousConfigValidate(t *testing.T) {
	for _, ok := range []string{"", "inline", "sdk", " Inline "} {
		if err := (NousConfig{Dispatcher: ok}).Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", ok, err)
		}
	}
	err := (NousConfig{Dispatcher: "legacy"}).Validate()
	if err == nil || !strings.Contains(err.Error(), "inline, sdk") {
		t.Fatalf("Validate(legacy) = %v, want error listing valid dispatchers", err)
	}
}

func TestValidateExperimentEngine(t *testing.T) {
	cases := []struct {
		role, engine string
		wantErr      string
	}{
		{"strategist", "", ""},
		{RoleExperimenter, "", ""},
		{RoleExperimenter, "nous", ""},
		{RoleExperimenter, " NOUS ", ""},
		{RoleExperimenter, "mlflow", "invalid engine"},
		{"strategist", "nous", "only meaningful with role experimenter"},
		{"scanner", "nous", "only meaningful with role experimenter"},
	}
	for _, tc := range cases {
		err := ValidateExperimentEngine(tc.role, tc.engine)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("ValidateExperimentEngine(%q,%q) = %v, want nil", tc.role, tc.engine, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("ValidateExperimentEngine(%q,%q) = %v, want %q", tc.role, tc.engine, err, tc.wantErr)
		}
	}
}

// TestExperimenterDefaults pins the load-time shape of an experimenter: a
// bare `experimenter:` entry gets the role, the default engine, and the
// catalog metadata; a renamed agent that opts into the role gets the same
// engine default; the strategist is untouched (RFC 11585 decision: unchanged).
func TestExperimenterDefaults(t *testing.T) {
	cfg := &Config{AgentRuntimeConfig: AgentRuntimeConfig{
		Agents: map[string]AgentConfig{
			"experimenter": {},
			"lab":          {Role: RoleExperimenter},
			"strategist":   {},
		},
	}}
	cfg.applyDefaults()

	exp := cfg.Agents["experimenter"]
	if exp.Role != RoleExperimenter || exp.Engine != DefaultExperimentEngine {
		t.Fatalf("experimenter role/engine = %q/%q", exp.Role, exp.Engine)
	}
	if exp.Emoji == "" || exp.Color == "" || exp.SortOrder == 0 {
		t.Fatalf("experimenter catalog metadata missing: %+v", exp)
	}
	if !exp.IsExperimenter() || exp.ExperimentEngine() != ExperimentEngineNous {
		t.Fatalf("accessors: IsExperimenter=%v ExperimentEngine=%q", exp.IsExperimenter(), exp.ExperimentEngine())
	}
	if lab := cfg.Agents["lab"]; lab.Engine != DefaultExperimentEngine {
		t.Fatalf("role-opted agent engine = %q, want %q", lab.Engine, DefaultExperimentEngine)
	}
	st := cfg.Agents["strategist"]
	if st.Engine != "" || st.IsExperimenter() || st.ExperimentEngine() != "" {
		t.Fatalf("strategist must be unchanged: %+v", st)
	}
	if cfg.Nous.Dispatcher != "" || cfg.Nous.ResolveDispatcher(nil) != NousDispatcherInline {
		t.Fatalf("nous defaults: %+v", cfg.Nous)
	}
}

func TestExperimenterDirectConstructionEngineFallback(t *testing.T) {
	a := &AgentConfig{Role: RoleExperimenter}
	if got := a.ExperimentEngine(); got != DefaultExperimentEngine {
		t.Fatalf("ExperimentEngine() = %q, want %q", got, DefaultExperimentEngine)
	}
	var nilAgent *AgentConfig
	if nilAgent.IsExperimenter() || nilAgent.ExperimentEngine() != "" {
		t.Fatal("nil agent must not be an experimenter")
	}
}

// TestValidateExperimenterEngineInConfig exercises Validate on a whole config:
// a bare `experimenter:` entry (role filled only later by defaults) accepts an
// engine, a strategist with an engine is rejected, and a bad dispatcher is
// rejected.
func TestValidateExperimenterEngineInConfig(t *testing.T) {
	base := func() *Config {
		cfg := &Config{
			Project: ProjectConfig{Org: "o"},
			GitHub:  GitHubConfig{Token: "x"},
			AgentRuntimeConfig: AgentRuntimeConfig{
				Agents: map[string]AgentConfig{"experimenter": {Engine: "nous"}},
			},
		}
		cfg.applyDefaults()
		return cfg
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("valid experimenter config: %v", err)
	}

	cfg := base()
	cfg.Agents["strategist"] = AgentConfig{Engine: "nous"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "only meaningful with role experimenter") {
		t.Fatalf("strategist with engine: %v", err)
	}

	cfg = base()
	cfg.Nous.Dispatcher = "legacy"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "invalid dispatcher") {
		t.Fatalf("bad dispatcher: %v", err)
	}

	// Overlays are validated before defaults: a bare experimenter overlay
	// with an engine must be accepted, a scanner overlay with one rejected.
	if err := base().validateAgentOverlay("experimenter", AgentConfig{Engine: "nous"}); err != nil {
		t.Fatalf("overlay experimenter: %v", err)
	}
	if err := base().validateAgentOverlay("scanner", AgentConfig{Engine: "nous"}); err == nil {
		t.Fatal("overlay scanner with engine: want error")
	}
	c := base()
	c.Agents["lab"] = AgentConfig{Role: RoleExperimenter}
	c.ApplyAgentDefaults("lab")
	if c.Agents["lab"].Engine != DefaultExperimentEngine {
		t.Fatalf("ApplyAgentDefaults engine = %q", c.Agents["lab"].Engine)
	}
}
