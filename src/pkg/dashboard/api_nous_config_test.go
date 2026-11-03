package dashboard

import (
	"net/http"
	"testing"
)

func TestHandleNousConfigGetCarriesProjectReposAndTargets(t *testing.T) {
	s, deps := apiServerWithNous(t)
	deps.Config.Project.Org = "hivecommons"
	deps.Config.Project.Repos = []string{"hive", "kubestellar"}
	deps.Config.Project.PrimaryRepo = "hive"
	deps.Nous.Mode = "suggest"
	deps.Nous.Config = map[string]interface{}{
		"goals": map[string]interface{}{"repoGoal": "Cut CI time"},
		"repos": map[string]interface{}{"repos": []interface{}{"kubestellar"}},
	}

	rec := doGet(s, "/api/nous/config")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := decodeJSON(t, rec)
	assertStringSlice(t, got["projectRepos"], []string{"hive", "kubestellar"})
	assertStringSlice(t, got["availableRepos"], []string{"hive", "kubestellar"})
	assertStringSlice(t, got["repoTargets"], []string{"kubestellar"})
	campaign, ok := got["campaign"].(map[string]interface{})
	if !ok || campaign["mode"] != "suggest" {
		t.Fatalf("campaign = %#v, want mode suggest", got["campaign"])
	}
	repoCampaign, ok := got["repoCampaign"].(map[string]interface{})
	if !ok || repoCampaign["research_question"] != "Cut CI time" {
		t.Fatalf("repoCampaign = %#v", got["repoCampaign"])
	}
}

func TestHandleNousStatusCarriesAvailableRepos(t *testing.T) {
	s, deps := apiServerWithNous(t)
	deps.Config.Project.Org = "hivecommons"
	deps.Config.Project.Repos = []string{"hive"}
	deps.Config.Project.PrimaryRepo = "hivecommons/kubestellar"

	rec := doGet(s, "/api/nous/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := decodeJSON(t, rec)
	assertStringSlice(t, got["projectRepos"], []string{"hive", "hivecommons/kubestellar"})
	assertStringSlice(t, got["availableRepos"], []string{"hive", "hivecommons/kubestellar"})
	assertStringSlice(t, got["repoTargets"], []string{"hive", "hivecommons/kubestellar"})
}

func assertStringSlice(t *testing.T, got interface{}, want []string) {
	t.Helper()
	raw, ok := got.([]interface{})
	if !ok {
		t.Fatalf("got %#v, want []interface{}", got)
	}
	if len(raw) != len(want) {
		t.Fatalf("len = %d, want %d (%#v)", len(raw), len(want), raw)
	}
	for i := range want {
		if raw[i] != want[i] {
			t.Fatalf("index %d = %#v, want %q (all %#v)", i, raw[i], want[i], raw)
		}
	}
}
