package config

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/stageengine"
)

// governor.labels.stage.engine.mode (ADR-0025): off|shadow|enforce,
// default enforce, validated at load so hivectl config catches a typo
// before the hive boots with it.
func TestStageEngineModeConfig(t *testing.T) {
	var zero StageEngineConfig
	if zero.EffectiveMode() != stageengine.ModeEnforce || zero.EffectiveMode() != DefaultStageEngineMode || zero.Validate() != nil {
		t.Fatalf("zero value: mode %s err %v, want enforce/nil", zero.EffectiveMode(), zero.Validate())
	}
	if blank := (StageEngineConfig{Mode: "  "}); blank.EffectiveMode() != stageengine.ModeEnforce {
		t.Fatalf("blank mode = %s, want the enforce default", blank.EffectiveMode())
	}
	for in, want := range map[string]stageengine.Mode{"off": stageengine.ModeOff, " Shadow ": stageengine.ModeShadow, "ENFORCE": stageengine.ModeEnforce} {
		c := StageEngineConfig{Mode: in}
		if err := c.Validate(); err != nil || c.EffectiveMode() != want {
			t.Fatalf("%q: mode %s err %v, want %s", in, c.EffectiveMode(), err, want)
		}
	}
	bad := StageEngineConfig{Mode: "enforced"}
	err := bad.Validate()
	if err == nil || !strings.Contains(err.Error(), "governor.labels.stage.engine.mode") || !strings.Contains(err.Error(), "enforced") {
		t.Fatalf("bad mode error = %v", err)
	}
	if bad.EffectiveMode() != stageengine.ModeShadow {
		t.Fatalf("an invalid mode must fall back to shadow, not enforce: %s", bad.EffectiveMode())
	}
}

func TestStageEngineModeLoadAndValidate(t *testing.T) {
	path := writeTempConfig(t, minimalValidYAML("my-org", "ghp_tok")+`
governor:
  labels:
    stage:
      engine:
        mode: enforce
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Governor.Labels.Stage.Engine.EffectiveMode(); got != stageengine.ModeEnforce {
		t.Fatalf("mode = %s, want enforce", got)
	}

	path = writeTempConfig(t, minimalValidYAML("my-org", "ghp_tok")+`
governor:
  labels:
    stage:
      engine:
        mode: sometimes
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "governor.labels.stage.engine.mode") {
		t.Fatalf("Load with a bad engine mode: err = %v, want a validation error naming the key", err)
	}
}
