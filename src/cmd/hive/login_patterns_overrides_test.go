package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/snapshot"
)

// #4041: the legacy state-overrides migration replays a persisted
// sensing_login list over the loaded config. A list byte-identical to the
// pre-#3959 defaults is the old defaults materialized by an earlier save —
// replaying it would re-pin the false-positive-prone generic patterns over
// the corrected code defaults. It must be skipped; a genuinely customized
// list must still replay verbatim.

// legacySensingLogin mirrors pkg/config's legacyDefaultLoginPatterns (frozen;
// duplicated here because the source list is deliberately unexported).
var legacySensingLogin = []string{
	"please log in",
	"authentication required",
	"not logged in",
	"login required",
	"session expired",
	"token expired",
	"unauthorized.*401",
	"gh auth login",
	"claude login",
	"copilot auth",
}

func TestApplyConfigOverrides_SkipsLegacyDefaultSensingLogin(t *testing.T) {
	// Sanity: the local copy must still match the config package's frozen set,
	// or this test would silently stop guarding the replay path.
	if !config.IsLegacyDefaultLoginPatterns(legacySensingLogin) {
		t.Fatal("legacySensingLogin fixture no longer matches config's frozen legacy set")
	}

	cfg := &config.Config{
		Governor: config.GovernorConfig{
			Sensing: config.SensingConfig{
				// What applyDefaults left in place: the corrected defaults.
				LoginPatterns: []string{"Please run /login", "Not logged in"},
			},
		},
	}
	applyConfigOverrides(cfg, &snapshot.ConfigOverrides{
		SensingLogin: append([]string(nil), legacySensingLogin...),
	})
	got := cfg.Governor.Sensing.LoginPatterns
	if len(got) != 2 || got[0] != "Please run /login" || got[1] != "Not logged in" {
		t.Fatalf("legacy-default sensing_login was replayed over the corrected defaults: %v", got)
	}
}

// Positive control: a customized sensing_login override still replays.
func TestApplyConfigOverrides_ReplaysCustomSensingLogin(t *testing.T) {
	cfg := &config.Config{
		Governor: config.GovernorConfig{
			Sensing: config.SensingConfig{
				LoginPatterns: []string{"Please run /login"},
			},
		},
	}
	custom := []string{"my own pattern", "copilot auth"}
	applyConfigOverrides(cfg, &snapshot.ConfigOverrides{
		SensingLogin: append([]string(nil), custom...),
	})
	got := cfg.Governor.Sensing.LoginPatterns
	if len(got) != len(custom) {
		t.Fatalf("custom sensing_login not replayed: %v", got)
	}
	for i := range custom {
		if got[i] != custom[i] {
			t.Fatalf("custom sensing_login mutated: got %v, want %v", got, custom)
		}
	}
}
