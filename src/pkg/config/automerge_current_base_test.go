package config

import (
	"reflect"
	"testing"
)

// TestRequireCurrentBaseFor pins the effective-value table of the current-base
// gate (hivecommons/hive#15335): explicit lists beat the default, the explicit
// "on" list beats the skip list, and the unset default is OFF.
func TestRequireCurrentBaseFor(t *testing.T) {
	listed := AutoMergeConfig{RequireCurrentBase: []string{" HiveCommons/Hive ", ""}}
	skipped := AutoMergeConfig{SkipCurrentBase: []string{"Acme/Legacy"}}
	defaultOn := AutoMergeConfig{RequireCurrentBaseDefault: boolPtr(true), SkipCurrentBase: []string{"acme/legacy"}}
	defaultOff := AutoMergeConfig{RequireCurrentBaseDefault: boolPtr(false), RequireCurrentBase: []string{"hivecommons/hive"}}
	both := AutoMergeConfig{RequireCurrentBase: []string{"acme/widget"}, SkipCurrentBase: []string{"acme/widget"}}
	tests := []struct {
		name string
		cfg  AutoMergeConfig
		repo string
		want bool
	}{
		{"unset defaults off", AutoMergeConfig{}, "hivecommons/hive", false},
		{"unlisted repo defaults off", listed, "acme/widget", false},
		{"listed case-insensitive", listed, "hivecommons/hive", true},
		{"listed with spaces", listed, " HIVECOMMONS/HIVE ", true},
		{"empty repo", listed, "", false},
		{"skip list opts out", skipped, "acme/legacy", false},
		{"default on covers unlisted", defaultOn, "acme/widget", true},
		{"skip beats default on", defaultOn, "acme/legacy", false},
		{"default off keeps unlisted off", defaultOff, "acme/widget", false},
		{"explicit on beats default off", defaultOff, "hivecommons/hive", true},
		{"explicit on beats skip", both, "acme/widget", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.RequireCurrentBaseFor(tc.repo); got != tc.want {
				t.Fatalf("RequireCurrentBaseFor(%q) = %v, want %v", tc.repo, got, tc.want)
			}
		})
	}
}

func TestRequireCurrentBaseStrictFor(t *testing.T) {
	cfg := AutoMergeConfig{RequireCurrentBase: []string{"HiveCommons/Hive"}, RequireCurrentBaseDefault: boolPtr(true)}
	if !cfg.RequireCurrentBaseStrictFor("hivecommons/hive") {
		t.Fatal("listed repo must be strict")
	}
	if cfg.RequireCurrentBaseStrictFor("acme/widget") {
		t.Fatal("default-gated repo must not be strict")
	}
}

func TestCurrentBaseOverrideRoundTrip(t *testing.T) {
	var a AutoMergeConfig
	if a.CurrentBaseOverrideFor("acme/widget") != nil {
		t.Fatal("fresh config must inherit")
	}
	a.SetCurrentBaseOverride("acme/widget", boolPtr(true))
	a.SetCurrentBaseOverride("acme/legacy", boolPtr(false))
	if v := a.CurrentBaseOverrideFor("ACME/widget"); v == nil || !*v {
		t.Fatalf("override(widget) = %v, want on", v)
	}
	if v := a.CurrentBaseOverrideFor("acme/legacy"); v == nil || *v {
		t.Fatalf("override(legacy) = %v, want off", v)
	}
	// Flipping moves the repo between the lists rather than leaving it in both.
	a.SetCurrentBaseOverride("Acme/Widget", boolPtr(false))
	if !reflect.DeepEqual(a.RequireCurrentBase, []string(nil)) || !reflect.DeepEqual(a.SkipCurrentBase, []string{"acme/legacy", "Acme/Widget"}) {
		t.Fatalf("after flip: require=%v skip=%v", a.RequireCurrentBase, a.SkipCurrentBase)
	}
	a.SetCurrentBaseOverride("acme/widget", nil)
	a.SetCurrentBaseOverride("acme/legacy", nil)
	if a.RequireCurrentBase != nil || a.SkipCurrentBase != nil {
		t.Fatalf("after clearing: require=%v skip=%v, want both empty", a.RequireCurrentBase, a.SkipCurrentBase)
	}
	if a.CurrentBaseOverrideFor("acme/widget") != nil {
		t.Fatal("cleared repo must inherit")
	}
}
