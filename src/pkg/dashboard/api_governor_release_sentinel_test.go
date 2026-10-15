package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

const releaseSentinelSettings = "/api/config/governor/release-sentinel"

func TestReleaseSentinelSettingsRoundTrip(t *testing.T) {
	t.Setenv(config.ReleaseSentinelEnabledEnvVar, "")
	t.Setenv(config.ReleaseSentinelRetagEnabledEnvVar, "")
	s, _ := apiServer(t)
	s.deps.Config.ReleaseSentinel = config.ReleaseSentinelConfig{}

	rec := doPut(s, releaseSentinelSettings, map[string]any{
		"enabled":                         true,
		"repo":                            " acme/widgets ",
		"agent":                           " release-fixer ",
		"max_rounds":                      7,
		"round_timeout":                   "90m",
		"poll_interval":                   " 10m ",
		"ignore_workflows":                []string{" Advisory ", "", "Advisory", "Stale"},
		"release_workflows":               []string{"tagged-release.yml"},
		"release_branch":                  " release/v5 ",
		"retag_enabled":                   true,
		"retag_allow_intervening_commits": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}

	rs := s.deps.Config.ReleaseSentinel
	if !rs.Enabled || rs.Repo != "acme/widgets" || rs.Agent != "release-fixer" || rs.MaxRounds != 7 ||
		rs.RoundTimeout != "90m" || rs.PollInterval != "10m" || rs.ReleaseBranch != "release/v5" ||
		!rs.RetagEnabled || !rs.RetagAllowInterveningCommits {
		t.Fatalf("release sentinel config = %+v", rs)
	}
	if strings.Join(rs.IgnoreWorkflows, ",") != "Advisory,Stale" || strings.Join(rs.ReleaseWorkflows, ",") != "tagged-release.yml" {
		t.Fatalf("release sentinel lists = %+v", rs)
	}

	get := doOwnerGet(s, "/api/config/governor")
	if get.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", get.Code, get.Body.String())
	}
	var payload struct {
		ReleaseSentinel map[string]any `json:"release_sentinel"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	got := payload.ReleaseSentinel
	if got["enabled"] != true || got["effective_enabled"] != true || got["effective_repo"] != "acme/widgets" ||
		got["effective_retag_enabled"] != true || got["round_timeout"] != "90m" || got["max_rounds"] != float64(7) {
		t.Fatalf("release_sentinel payload = %#v", got)
	}
}

func TestReleaseSentinelSettingsAbsentFieldsUnchangedAndEmptyClears(t *testing.T) {
	s, _ := apiServer(t)
	s.deps.Config.ReleaseSentinel = config.ReleaseSentinelConfig{
		Enabled:         true,
		Repo:            "acme/widgets",
		RoundTimeout:    "1h",
		IgnoreWorkflows: []string{"Advisory"},
	}

	rec := doPut(s, releaseSentinelSettings, map[string]any{"round_timeout": "", "ignore_workflows": []string{}})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}
	rs := s.deps.Config.ReleaseSentinel
	if !rs.Enabled || rs.Repo != "acme/widgets" {
		t.Fatalf("absent fields changed: %+v", rs)
	}
	if rs.RoundTimeout != "" || len(rs.IgnoreWorkflows) != 0 {
		t.Fatalf("explicit empty did not clear: %+v", rs)
	}
}

func TestReleaseSentinelSettingsRejectsInvalidValuesWithoutPartialApply(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{"bad repo", map[string]any{"enabled": true, "repo": "not-a-repo"}},
		{"negative rounds", map[string]any{"enabled": true, "max_rounds": -1}},
		{"too many rounds", map[string]any{"enabled": true, "max_rounds": 51}},
		{"bad round timeout", map[string]any{"enabled": true, "round_timeout": "soon"}},
		{"non-positive poll interval", map[string]any{"enabled": true, "poll_interval": "0s"}},
		{"bad branch", map[string]any{"enabled": true, "release_branch": "bad branch"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := apiServer(t)
			s.deps.Config.ReleaseSentinel = config.ReleaseSentinelConfig{}
			rec := doPut(s, releaseSentinelSettings, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
			}
			if s.deps.Config.ReleaseSentinel.Enabled {
				t.Fatalf("rejected save mutated config: %+v", s.deps.Config.ReleaseSentinel)
			}
		})
	}
}

func TestReleaseSentinelSettingsRequiresOwner(t *testing.T) {
	s := covApiServer(t)
	rec := doPutNoRole(s, releaseSentinelSettings, `{"enabled":true}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if s.deps.Config.ReleaseSentinel.Enabled {
		t.Fatal("non-owner save mutated config")
	}
}
