package config

import "testing"

// ── kubestellar/hive#3845 convergence feature toggle ───────────────────────────
//
// The overriding rollout requirement is DEFAULT OFF: an operator who configured
// nothing, or anything unrecognised, must resolve to "off" so existing v4 hives
// see zero behaviour change. The env override exists so shadow can be flipped
// per-process without editing hive.yaml.

// TestConvergenceMode_DefaultIsShadow pins the #7260 default. A hive that has
// never touched the knob runs in shadow so its soak ring actually fills; "off"
// returns before computing anything, so the evidence the documented promotion
// path depends on never existed on any hive whose owner had not already opted
// in.
func TestConvergenceMode_DefaultIsShadow(t *testing.T) {
	t.Setenv(ConvergenceModeEnvVar, "")
	var nilCfg *Config
	if got := nilCfg.ConvergenceMode(); got != ConvergenceModeShadow {
		t.Fatalf("nil config resolved %q, want shadow", got)
	}
	if got := (&Config{}).ConvergenceMode(); got != ConvergenceModeShadow {
		t.Fatalf("zero config resolved %q, want shadow", got)
	}
	// Whitespace-only is still "never chose", not "chose something invalid".
	for _, raw := range []string{"", " ", "\t", "\n  "} {
		cfg := &Config{Convergence: ConvergenceConfig{Mode: raw}}
		if got := cfg.ConvergenceMode(); got != ConvergenceModeShadow {
			t.Fatalf("blank mode %q resolved %q, want shadow", raw, got)
		}
	}
	if DefaultConvergenceRolloutMode != ConvergenceModeShadow {
		t.Fatalf("DefaultConvergenceRolloutMode = %q, want shadow", DefaultConvergenceRolloutMode)
	}
}

// TestConvergenceMode_ExplicitOffIsHonoured is the other half of #7260: moving
// the DEFAULT must not take "off" away. It stays selectable as the rollback
// and as the control arm of #4263's fixed-commit A/B comparison, and an
// operator who wrote it must keep getting it with no migration.
func TestConvergenceMode_ExplicitOffIsHonoured(t *testing.T) {
	t.Setenv(ConvergenceModeEnvVar, "")
	for _, raw := range []string{"off", "OFF", "  Off  "} {
		cfg := &Config{Convergence: ConvergenceConfig{Mode: raw}}
		if got := cfg.ConvergenceMode(); got != ConvergenceModeOff {
			t.Fatalf("explicit mode %q resolved %q, want off", raw, got)
		}
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

// TestConvergenceMode_UnrecognisedValueFailsSafeToOff guards the distinction
// #7260 introduced: UNSET takes the default (shadow), but a NON-EMPTY value
// this build cannot parse still fails safe to off. A typo or a mode from a
// newer build must never silently select a posture nobody asked for, so these
// two cases must not collapse into one another.
func TestConvergenceMode_UnrecognisedValueFailsSafeToOff(t *testing.T) {
	t.Setenv(ConvergenceModeEnvVar, "")
	for _, raw := range []string{"on", "true", "garbage", "enforced", "enforce-all", "shadowy", "0"} {
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
