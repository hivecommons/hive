package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// governor.labels.stage.adoption.supersede.close_first (hivecommons/hive#15135):
// on by default; false keeps the original open until the copy merges.
func TestStageAdoptionSupersedeCloseFirst(t *testing.T) {
	if !(StageAdoptionConfig{}).Supersede.CloseFirstEnabled() {
		t.Fatal("close_first default is not true")
	}
	for in, want := range map[string]bool{
		"supersede: {}\n":                    true,
		"supersede:\n  close_first: true\n":  true,
		"supersede:\n  close_first: false\n": false,
	} {
		var cfg StageAdoptionConfig
		if err := yaml.NewDecoder(strings.NewReader(in)).Decode(&cfg); err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got := cfg.Supersede.CloseFirstEnabled(); got != want {
			t.Errorf("%q: CloseFirstEnabled = %v, want %v", in, got, want)
		}
	}
}
