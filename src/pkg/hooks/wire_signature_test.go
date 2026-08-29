package hooks

import (
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

// SignatureFromConfig is what the wiring layer polls to decide whether to
// recompile the hook registry, so it must be stable for an unchanged config,
// empty for "no hooks", and different once the operator edits anything.
func TestSignatureFromConfig(t *testing.T) {
	if got := SignatureFromConfig(nil); got != "" {
		t.Errorf("nil config signature = %q, want \"\"", got)
	}
	if got := SignatureFromConfig(&config.Config{}); got != "" {
		t.Errorf("empty config signature = %q, want \"\"", got)
	}

	cfg := &config.Config{Hooks: []config.HookRule{{
		Name:   "notify-on-hold",
		On:     "pr_opened",
		Action: "notify",
		Params: map[string]string{"channel": "ops"},
	}}}
	first := SignatureFromConfig(cfg)
	if first == "" {
		t.Fatal("non-empty hook list must have a non-empty signature")
	}
	if again := SignatureFromConfig(cfg); again != first {
		t.Errorf("signature not stable: %q then %q", first, again)
	}

	changed := &config.Config{Hooks: []config.HookRule{{
		Name:   "notify-on-hold",
		On:     "pr_opened",
		Action: "notify",
		Params: map[string]string{"channel": "eng"},
	}}}
	if got := SignatureFromConfig(changed); got == first {
		t.Error("editing a hook param must change the signature")
	}
}
