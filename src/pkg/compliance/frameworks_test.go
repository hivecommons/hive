package compliance

import "testing"

func TestEveryShippedProfileHasDescription(t *testing.T) {
	ps, err := Profiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if FrameworkDescription(p.ID) == "" {
			t.Errorf("profile %s has no FrameworkDescription entry", p.ID)
		}
	}
	if FrameworkDescription(" SOC2-TYPE2 ") == "" {
		t.Error("FrameworkDescription must normalise case and whitespace")
	}
	if FrameworkDescription("not-shipped") != "" {
		t.Error("unknown profile must have no description")
	}
}

func TestBuildReportCarriesDescriptionsAndKinds(t *testing.T) {
	r := BuildReport(hardenedConfig(), injectOn)
	if len(r.Controls) == 0 {
		t.Fatal("hardened config evaluated no controls")
	}
	for _, a := range r.Available {
		if a.Description == "" {
			t.Errorf("available %s has no description", a.ID)
		}
	}
	for _, c := range r.Controls {
		for _, s := range c.Settings {
			if s.Kind == "" || s.Kind != settings[s.SettingPath].Kind {
				t.Errorf("%s %s kind = %q", c.ControlID, s.SettingPath, s.Kind)
			}
		}
	}
}
