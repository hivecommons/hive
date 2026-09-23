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

func TestOMPBindingDefaultsOffAndModes(t *testing.T) {
	var nilCfg *Config
	if nilCfg.OMPBindingEnabled() || nilCfg.OMPBindingMode() != FlueBindingModeOff {
		t.Fatal("nil config must be off")
	}
	var cfg Config
	if cfg.OMPBindingEnabled() || cfg.OMPBindingMode() != FlueBindingModeOff {
		t.Fatalf("zero config: enabled=%v mode=%q", cfg.OMPBindingEnabled(), cfg.OMPBindingMode())
	}
	// Enabling Flue does not enable OMP and the reverse: the two hosts are
	// independent toggles.
	cfg.Runs.External.Flue.Enabled = true
	if cfg.OMPBindingEnabled() {
		t.Fatal("the Flue toggle must not enable the OMP host")
	}
	cfg = Config{}
	cfg.Runs.External.OMP.Enabled = true
	if cfg.FlueBindingEnabled() || cfg.OMPBindingMode() != FlueBindingModeShadow {
		t.Fatalf("OMP enabled: flue=%v omp=%q", cfg.FlueBindingEnabled(), cfg.OMPBindingMode())
	}
	cases := []struct {
		enabled bool
		mode    string
		want    string
	}{
		{false, "", FlueBindingModeOff},
		{false, FlueBindingModeReportOnly, FlueBindingModeOff},
		{true, "", FlueBindingModeShadow},
		{true, " report-only ", FlueBindingModeReportOnly},
		{true, "enforce", FlueBindingModeOff},
	}
	for _, tc := range cases {
		if got := (OMPBindingConfig{Enabled: tc.enabled, Mode: tc.mode}).EffectiveMode(); got != tc.want {
			t.Errorf("enabled=%v mode=%q: got %q want %q", tc.enabled, tc.mode, got, tc.want)
		}
	}
	if !ValidExternalBindingMode("shadow") || ValidExternalBindingMode("off") || len(ExternalBindingModes()) != 2 {
		t.Fatal("ExternalBindingModes: shadow and report-only are the selectable modes")
	}
	src := "runs:\n  external:\n    omp:\n      enabled: true\n      mode: report-only\n      workflow_version: omp-workbench/1.0.0\n"
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.OMPBindingEnabled() || cfg.OMPBindingMode() != FlueBindingModeReportOnly || cfg.Runs.External.OMP.WorkflowVersion != "omp-workbench/1.0.0" {
		t.Fatalf("parsed = %+v mode=%q", cfg.Runs.External.OMP, cfg.OMPBindingMode())
	}
}
