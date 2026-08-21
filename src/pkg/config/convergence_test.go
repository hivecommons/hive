package config

import "testing"

// ── kubestellar/hive#3845 convergence feature toggle ───────────────────────────
//
// The overriding rollout requirement is DEFAULT OFF: an operator who configured
// nothing, or anything unrecognised, must resolve to "off" so existing v4 hives
// see zero behaviour change. The env override exists so shadow can be flipped
// per-process without editing hive.yaml.

func TestConvergenceMode_DefaultIsOff(t *testing.T) {
	t.Setenv(ConvergenceModeEnvVar, "")
	var nilCfg *Config
	if got := nilCfg.ConvergenceMode(); got != ConvergenceModeOff {
		t.Fatalf("nil config resolved %q, want off", got)
	}
	if got := (&Config{}).ConvergenceMode(); got != ConvergenceModeOff {
		t.Fatalf("zero config resolved %q, want off", got)
	}
}

func TestConvergenceMode_ConfiguredShadow(t *testing.T) {
	t.Setenv(ConvergenceModeEnvVar, "")
	cfg := &Config{Convergence: ConvergenceConfig{Mode: "shadow"}}
	if got := cfg.ConvergenceMode(); got != ConvergenceModeShadow {
		t.Fatalf("resolved %q, want shadow", got)
	}
	// Case/whitespace-insensitive: operators type YAML by hand.
	cfg.Convergence.Mode = "  Shadow "
	if got := cfg.ConvergenceMode(); got != ConvergenceModeShadow {
		t.Fatalf("resolved %q, want shadow", got)
	}
}

func TestConvergenceMode_UnrecognisedValueFailsSafeToOff(t *testing.T) {
	t.Setenv(ConvergenceModeEnvVar, "")
	for _, raw := range []string{"on", "true", "garbage", "enforced", "enforce-all"} {
		cfg := &Config{Convergence: ConvergenceConfig{Mode: raw}}
		if got := cfg.ConvergenceMode(); got != ConvergenceModeOff {
			t.Fatalf("mode %q resolved %q, want off", raw, got)
		}
	}
}

func TestConvergenceMode_EnvOverridesConfig(t *testing.T) {
	cfg := &Config{Convergence: ConvergenceConfig{Mode: "off"}}
	t.Setenv(ConvergenceModeEnvVar, "shadow")
	if got := cfg.ConvergenceMode(); got != ConvergenceModeShadow {
		t.Fatalf("env=shadow over config=off resolved %q, want shadow", got)
	}

	cfg.Convergence.Mode = "shadow"
	t.Setenv(ConvergenceModeEnvVar, "off")
	if got := cfg.ConvergenceMode(); got != ConvergenceModeOff {
		t.Fatalf("env=off over config=shadow resolved %q, want off", got)
	}

	// An unrecognised env value must NOT clobber a valid configured mode —
	// the env only wins when it names a mode this build knows.
	t.Setenv(ConvergenceModeEnvVar, "garbage")
	if got := cfg.ConvergenceMode(); got != ConvergenceModeShadow {
		t.Fatalf("unrecognised env over config=shadow resolved %q, want shadow", got)
	}

	// #4263: "enforce" is a known mode and the env override may select it.
	t.Setenv(ConvergenceModeEnvVar, "enforce")
	if got := cfg.ConvergenceMode(); got != ConvergenceModeEnforce {
		t.Fatalf("env=enforce over config=shadow resolved %q, want enforce", got)
	}
}
