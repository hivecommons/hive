package config

import "testing"

func TestRequireCurrentBaseFor(t *testing.T) {
	off := false
	listed := AutoMergeConfig{RequireCurrentBase: []string{" HiveCommons/Hive ", ""}}
	skipped := AutoMergeConfig{SkipCurrentBase: []string{"Acme/Legacy"}}
	defaultOff := AutoMergeConfig{RequireCurrentBaseDefault: &off, RequireCurrentBase: []string{"hivecommons/hive"}}
	both := AutoMergeConfig{RequireCurrentBase: []string{"acme/widget"}, SkipCurrentBase: []string{"acme/widget"}}
	tests := []struct {
		name string
		cfg  AutoMergeConfig
		repo string
		want bool
	}{
		{"unset defaults on", AutoMergeConfig{}, "hivecommons/hive", true},
		{"unlisted repo defaults on", listed, "acme/widget", true},
		{"listed case-insensitive", listed, "hivecommons/hive", true},
		{"listed with spaces", listed, " HIVECOMMONS/HIVE ", true},
		{"empty repo", listed, "", false},
		{"skip list opts out", skipped, "acme/legacy", false},
		{"skip list leaves others on", skipped, "acme/widget", true},
		{"default false turns unlisted off", defaultOff, "acme/widget", false},
		{"default false keeps listed on", defaultOff, "hivecommons/hive", true},
		{"explicit list beats skip list", both, "acme/widget", true},
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
	cfg := AutoMergeConfig{RequireCurrentBase: []string{"HiveCommons/Hive"}}
	if !cfg.RequireCurrentBaseStrictFor("hivecommons/hive") {
		t.Fatal("listed repo must be strict")
	}
	if cfg.RequireCurrentBaseStrictFor("acme/widget") {
		t.Fatal("default-gated repo must not be strict")
	}
	if (AutoMergeConfig{}).RequireCurrentBaseStrictFor("hivecommons/hive") {
		t.Fatal("unset list must not make any repo strict")
	}
}
