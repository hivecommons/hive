package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestFlueBindingDefaultsOff(t *testing.T) {
	var nilCfg *Config
	if nilCfg.FlueBindingEnabled() || nilCfg.FlueBindingMode() != FlueBindingModeOff {
		t.Fatal("nil config must be off")
	}
	var cfg Config
	if cfg.FlueBindingEnabled() || cfg.FlueBindingMode() != FlueBindingModeOff {
		t.Fatalf("zero config: enabled=%v mode=%q", cfg.FlueBindingEnabled(), cfg.FlueBindingMode())
	}
	if err := yaml.Unmarshal([]byte("project:\n  org: x\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.FlueBindingEnabled() {
		t.Fatal("absent runs block must not enable the binding")
	}
}

func TestFlueBindingModes(t *testing.T) {
	cases := []struct {
		enabled bool
		mode    string
		want    string
	}{
		{false, "", FlueBindingModeOff},
		{false, FlueBindingModeReportOnly, FlueBindingModeOff},
		{true, "", FlueBindingModeShadow},
		{true, "  shadow ", FlueBindingModeShadow},
		{true, FlueBindingModeReportOnly, FlueBindingModeReportOnly},
		{true, "enforce", FlueBindingModeOff},
		{true, "publish", FlueBindingModeOff},
	}
	for _, tc := range cases {
		got := (FlueBindingConfig{Enabled: tc.enabled, Mode: tc.mode}).EffectiveMode()
		if got != tc.want {
			t.Errorf("enabled=%v mode=%q: got %q want %q", tc.enabled, tc.mode, got, tc.want)
		}
	}
	if !ValidFlueBindingMode("report-only") || ValidFlueBindingMode("off") || ValidFlueBindingMode("") {
		t.Fatal("ValidFlueBindingMode: off and empty are not operator-selectable")
	}
	if modes := FlueBindingModes(); len(modes) != 2 {
		t.Fatalf("FlueBindingModes = %v", modes)
	}
}

func TestFlueBindingYAMLRoundTrip(t *testing.T) {
	src := "runs:\n  external:\n    flue:\n      enabled: true\n      mode: report-only\n      endpoint: http://127.0.0.1:8585\n      workflow_version: flue-fixture/1.0.0\n"
	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatal(err)
	}
	f := cfg.Runs.External.Flue
	if !cfg.FlueBindingEnabled() || cfg.FlueBindingMode() != FlueBindingModeReportOnly || f.Endpoint != "http://127.0.0.1:8585" || f.WorkflowVersion != "flue-fixture/1.0.0" {
		t.Fatalf("parsed = %+v mode=%q", f, cfg.FlueBindingMode())
	}
	out, err := yaml.Marshal(cfg.Runs.External)
	if err != nil {
		t.Fatal(err)
	}
	var back ExternalRunsConfig
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if back != cfg.Runs.External {
		t.Fatalf("round trip changed the block: %+v vs %+v", back, cfg.Runs.External)
	}
}
