package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestRepeatFailureConfigDefaultsOff(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("agents: {}\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.RepeatFailure != nil {
		t.Fatalf("absent repeat_failure block must stay nil, got %+v", cfg.RepeatFailure)
	}
	rf := cfg.RepeatFailure
	if rf.IsEnabled() {
		t.Fatal("repeat-failure check must be off by default")
	}
	if got := rf.EffectiveThreshold(); got != 5 {
		t.Fatalf("threshold default = %d, want 5", got)
	}
	if got := rf.EffectiveWindow(); got != 600*time.Second {
		t.Fatalf("window default = %s, want 10m", got)
	}
	if got := rf.EffectiveNoDataAfter(); got != 900*time.Second {
		t.Fatalf("no-data default = %s, want 15m", got)
	}
	if got, want := rf.EffectiveExclude(), []string{"gh pr checks", "gh run watch"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("exclude default = %v, want %v", got, want)
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "repeat_failure") {
		t.Fatalf("an absent block must not be written back:\n%s", out)
	}
}

func TestRepeatFailureConfigParsesBlock(t *testing.T) {
	var cfg Config
	src := `repeat_failure:
  enabled: true
  threshold: 3
  window_s: 120
  no_data_after_s: 60
  exclude: ["npm test --watch"]
`
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatal(err)
	}
	rf := cfg.RepeatFailure
	if !rf.IsEnabled() {
		t.Fatal("enabled: true must turn the check on")
	}
	if rf.EffectiveThreshold() != 3 || rf.EffectiveWindow() != 2*time.Minute || rf.EffectiveNoDataAfter() != time.Minute {
		t.Fatalf("configured values not honoured: %+v", rf)
	}
	got := rf.EffectiveExclude()
	if !reflect.DeepEqual(got, []string{"npm test --watch"}) {
		t.Fatalf("exclude = %v", got)
	}
	got[0] = "mutated"
	if rf.Exclude[0] != "npm test --watch" {
		t.Fatal("EffectiveExclude must return a copy")
	}
}

func TestRepeatFailureConfigInvalidValuesFallBack(t *testing.T) {
	rf := &RepeatFailureConfig{Enabled: false, Threshold: -1, WindowS: 0, NoDataAfterS: -5, Exclude: []string{}}
	if rf.IsEnabled() {
		t.Fatal("enabled false must stay off")
	}
	if rf.EffectiveThreshold() != DefaultRepeatFailureThreshold ||
		rf.EffectiveWindow() != DefaultRepeatFailureWindowS*time.Second ||
		rf.EffectiveNoDataAfter() != DefaultRepeatFailureNoDataAfterS*time.Second {
		t.Fatalf("non-positive values must fall back to defaults: %+v", rf)
	}
	if !reflect.DeepEqual(rf.EffectiveExclude(), DefaultRepeatFailureExclude()) {
		t.Fatal("an empty exclude list must fall back to the defaults")
	}
	d := DefaultRepeatFailureExclude()
	d[0] = "mutated"
	if DefaultRepeatFailureExclude()[0] != "gh pr checks" {
		t.Fatal("DefaultRepeatFailureExclude must return a fresh slice")
	}
}
