package config

import (
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/hivecommons/hive/pkg/gfi"
)

func TestUmbrellaPolicyForDefaultsAndPerRepo(t *testing.T) {
	var hub HubConfig
	// Nothing configured: defaults, enabled, advertisement withdrawn.
	if p := hub.UmbrellaPolicyFor("acme/widgets"); p.Disabled || p.KeepAdvertisement || len(p.Labels) != 0 {
		t.Fatalf("default policy = %+v", p)
	}
	if _, ok := hub.UmbrellaPolicyFor("acme/widgets").UmbrellaReason("x", []string{"epic"}, "", 0); !ok {
		t.Fatal("default policy must recognise an epic label")
	}

	const doc = `
contribute_umbrella:
  labels: [umbrella, program]
  task_list_min: 3
  withdraw_advertisement: true
  per_repo:
    Acme/Widgets:
      labels: [rollup]
      withdraw_advertisement: false
    acme/off:
      enabled: false
`
	if err := yaml.Unmarshal([]byte(doc), &hub); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name         string
		repo         string
		label        string
		wantUmbrella bool
		wantKeep     bool
		wantDisabled bool
		wantMin      int
	}{
		{"hive-wide label", "acme/other", "program", true, false, false, 3},
		{"hive-wide replaces defaults", "acme/other", "epic", false, false, false, 3},
		{"per-repo label replaces hive-wide, case-insensitive key", "acme/widgets", "rollup", true, true, false, 3},
		{"per-repo inherits unset task_list_min", "acme/widgets", "program", false, true, false, 3},
		{"per-repo disabled", "acme/off", "program", false, false, true, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := hub.UmbrellaPolicyFor(tc.repo)
			_, got := p.UmbrellaReason("x", []string{tc.label}, "", 0)
			if got != tc.wantUmbrella || p.KeepAdvertisement != tc.wantKeep || p.Disabled != tc.wantDisabled || p.TaskListMin != tc.wantMin {
				t.Fatalf("policy %+v umbrella=%v, want umbrella=%v keep=%v disabled=%v min=%d", p, got, tc.wantUmbrella, tc.wantKeep, tc.wantDisabled, tc.wantMin)
			}
		})
	}
	var _ gfi.UmbrellaPolicy = hub.UmbrellaPolicyFor("")
}
