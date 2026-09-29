package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

type prFollowUpFeatures struct {
	Features struct {
		PRFollowUpEnabled   bool `json:"prFollowUpEnabled"`
		PRFollowUpEffective bool `json:"prFollowUpEffective"`
	} `json:"features"`
}

func getPRFollowUpFeatures(t *testing.T, s *Server) prFollowUpFeatures {
	t.Helper()
	rec := doOwnerGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET governor config: %d - %s", rec.Code, rec.Body.String())
	}
	var payload prFollowUpFeatures
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding governor payload: %v", err)
	}
	return payload
}

// TestGovernorFeatures_PRFollowUpToggleRoundTrip covers the dashboard toggle
// for turn.pr_follow_up.enabled (hivecommons/hive#9583): default off,
// owner-only, persisted into config, reported back by the GET, left alone by
// a PUT that does not mention it, and turned off again by an explicit false.
func TestGovernorFeatures_PRFollowUpToggleRoundTrip(t *testing.T) {
	t.Setenv(config.PRFollowUpResumeEnvVar, "")
	s := covApiServer(t)
	if s.deps.Config.Turn.PRFollowUp.Enabled {
		t.Fatal("PR follow-up resume must default off")
	}
	if got := getPRFollowUpFeatures(t, s); got.Features.PRFollowUpEnabled || got.Features.PRFollowUpEffective {
		t.Fatalf("default payload = %+v, want off", got.Features)
	}
	if rec := putFeatures(s, map[string]any{"prFollowUpEnabled": true}, func(r *http.Request) {
		r.Header.Set("X-Hive-Role", "read-write")
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner PUT prFollowUpEnabled = %d, want 403", rec.Code)
	}
	if s.deps.Config.Turn.PRFollowUp.Enabled {
		t.Fatal("a refused PUT changed the config")
	}

	if rec := doPut(s, "/api/config/governor/features", map[string]any{"prFollowUpEnabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("PUT prFollowUpEnabled: %d - %s", rec.Code, rec.Body.String())
	}
	if !s.deps.Config.Turn.PRFollowUp.Enabled || !s.deps.Config.PRFollowUpResumeEnabled() {
		t.Fatal("turn.pr_follow_up.enabled was not persisted into config")
	}
	if got := getPRFollowUpFeatures(t, s); !got.Features.PRFollowUpEnabled || !got.Features.PRFollowUpEffective {
		t.Fatalf("payload after enable = %+v, want on", got.Features)
	}

	if rec := doPut(s, "/api/config/governor/features", map[string]any{"retroEnabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("unrelated PUT: %d", rec.Code)
	}
	if !s.deps.Config.Turn.PRFollowUp.Enabled {
		t.Fatal("absent prFollowUpEnabled key cleared the toggle")
	}

	// The env var is the one-step rollback and wins over the saved setting;
	// the payload shows both so the dialog can explain the override.
	t.Setenv(config.PRFollowUpResumeEnvVar, "false")
	if got := getPRFollowUpFeatures(t, s); !got.Features.PRFollowUpEnabled || got.Features.PRFollowUpEffective {
		t.Fatalf("payload under env override = %+v, want saved on, effective off", got.Features)
	}
	t.Setenv(config.PRFollowUpResumeEnvVar, "")

	if rec := doPut(s, "/api/config/governor/features", map[string]any{"prFollowUpEnabled": false}); rec.Code != http.StatusOK {
		t.Fatalf("PUT off: %d", rec.Code)
	}
	if s.deps.Config.Turn.PRFollowUp.Enabled {
		t.Fatal("explicit false did not turn PR follow-up resume off")
	}
}

// The Features tab renders the toggle wired to the same key the API reads.
func TestFeaturesTab_PRFollowUpToggleMarkup(t *testing.T) {
	html := featuresMarkup(t)
	for _, want := range []string{
		`data-section="features" data-key="prFollowUpEnabled"`,
		`f.prFollowUpEffective !== f.prFollowUpEnabled`,
		`HIVE_PR_FOLLOWUP_RESUME overrides this setting`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}
