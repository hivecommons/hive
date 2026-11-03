package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

const governorHubPath = "/api/config/governor/hub"

func hubSettingsFromGet(t *testing.T, s *Server) (map[string]any, string) {
	t.Helper()
	rec := doGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET governor: %d — %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Hub struct {
			Settings map[string]any `json:"settings"`
		} `json:"hub"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Hub.Settings == nil {
		t.Fatalf("GET governor has no hub.settings: %s", rec.Body.String())
	}
	return resp.Hub.Settings, rec.Body.String()
}

// The scalar hub keys round-trip through PUT /api/config/governor/hub, the
// secrets are stored but only ever reported as set, and the identity keys are
// display-only.
func TestHubSettingsParityBatch9RoundTrip(t *testing.T) {
	s, deps := apiServer(t)
	h := &deps.Config.Hub
	h.ClusterID = "cluster-a"
	h.HiveType = "hosted"

	rec := doPut(s, governorHubPath, map[string]any{
		"contribute_wall_retention_days":             30,
		"contribute_already_done_label":              "  done-already ",
		"contribute_already_done_hold_days":          14,
		"contribute_require_explicit_accept":         true,
		"nps_enabled":                                false,
		"snapshot_interval_min":                      30,
		"task_status_push":                           false,
		"upgrade_drain_timeout_s":                    -1,
		"notifications_discord_requests_webhook_url": "https://discord.example.com/api/webhooks/1/abc",
		"nps_relay_pull_secret":                      "relay-pull-secret-value",
		"contribute_announcement":                    map[string]any{"text": "maintenance", "level": "warning", "expires_at": "2099-01-01T00:00:00Z"},
		// Display-only keys are not accepted.
		"cluster_id": "cluster-b",
		"hive_type":  "self-hosted",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("hub PUT: %d — %s", rec.Code, rec.Body.String())
	}
	if h.ContributeWallRetentionDays != 30 || h.ContributeAlreadyDoneLabel != "done-already" ||
		h.ContributeAlreadyDoneHoldDays != 14 || !h.IsContributeRequireExplicitAccept() ||
		h.NPSEnabled == nil || *h.NPSEnabled || h.SnapshotIntervalMin != 30 ||
		h.TaskStatusPush == nil || *h.TaskStatusPush || h.UpgradeDrainTimeoutS != -1 {
		t.Fatalf("hub settings not applied: %+v", *h)
	}
	if h.Notifications.Discord.RequestsWebhookURL != "https://discord.example.com/api/webhooks/1/abc" ||
		h.NPSRelayPullSecret != "relay-pull-secret-value" {
		t.Fatal("hub secrets not stored")
	}
	if h.ContributeAnnouncement.Text != "maintenance" || h.ContributeAnnouncement.Level != "warning" ||
		h.ContributeAnnouncement.ID == "" {
		t.Fatalf("announcement not applied: %+v", h.ContributeAnnouncement)
	}
	if h.ClusterID != "cluster-a" || h.HiveType != "hosted" {
		t.Fatal("a PUT changed a display-only identity key")
	}

	st, raw := hubSettingsFromGet(t, s)
	for _, secret := range []string{"relay-pull-secret-value", "discord.example.com/api/webhooks"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("GET governor echoes a hub secret (%s)", secret)
		}
	}
	if st["notifications_discord_requests_webhook_set"] != true || st["nps_relay_pull_secret_set"] != true {
		t.Fatalf("secrets not reported as set: %v", st)
	}
	if st["cluster_id"] != "cluster-a" || st["hive_type"] != "hosted" ||
		st["contribute_already_done_label"] != "done-already" ||
		st["contribute_already_done_hold_days"] != float64(14) ||
		st["contribute_require_explicit_accept"] != true || st["nps_enabled"] != false ||
		st["task_status_push"] != false || st["upgrade_drain_timeout_s"] != float64(-1) {
		t.Fatalf("GET does not reflect the saved settings: %v", st)
	}
	if ann, _ := st["contribute_announcement_config"].(map[string]any); ann["text"] != "maintenance" {
		t.Fatalf("configured announcement not returned: %v", st["contribute_announcement_config"])
	}

	// A masked echo leaves a secret untouched; an empty value clears it.
	if rec := doPut(s, governorHubPath, map[string]any{"nps_relay_pull_secret": "••••••", "notifications_discord_requests_webhook_url": ""}); rec.Code != http.StatusOK {
		t.Fatalf("secret PUT: %d — %s", rec.Code, rec.Body.String())
	}
	if h.NPSRelayPullSecret != "relay-pull-secret-value" || h.Notifications.Discord.RequestsWebhookURL != "" {
		t.Fatalf("write-only secrets mishandled: pull=%q webhook=%q", h.NPSRelayPullSecret, h.Notifications.Discord.RequestsWebhookURL)
	}

	// 0 / empty restore the defaults.
	if rec := doPut(s, governorHubPath, map[string]any{
		"contribute_wall_retention_days": 0, "contribute_already_done_label": "", "contribute_already_done_hold_days": 0,
		"snapshot_interval_min": 0, "upgrade_drain_timeout_s": 0,
	}); rec.Code != http.StatusOK {
		t.Fatalf("defaults PUT: %d — %s", rec.Code, rec.Body.String())
	}
	if h.ContributeAlreadyDoneLabelOrDefault() != config.DefaultContributeAlreadyDoneLabel ||
		h.ContributeAlreadyDoneHoldDaysOrDefault() != config.DefaultContributeAlreadyDoneHoldDays ||
		s.wallRetentionDays() != config.DefaultContributeWallRetentionDays ||
		h.SnapshotIntervalMin != 0 || h.EffectiveUpgradeDrainTimeout().Seconds() != config.DefaultUpgradeDrainTimeoutS {
		t.Fatalf("defaults not restored: %+v", *h)
	}
}

// Out-of-range values are refused before anything in the request is applied.
func TestHubSettingsParityBatch9RejectsInvalid(t *testing.T) {
	s, deps := apiServer(t)
	h := &deps.Config.Hub
	h.ContributeSuspended = false
	before := *h
	for _, bad := range []map[string]any{
		{"contribute_wall_retention_days": -1},
		{"contribute_wall_retention_days": 4000},
		{"contribute_already_done_hold_days": -1},
		{"contribute_already_done_hold_days": 366},
		{"contribute_already_done_label": strings.Repeat("x", 51)},
		{"snapshot_interval_min": 3},
		{"snapshot_interval_min": 1441},
		{"upgrade_drain_timeout_s": 86401},
		{"notifications_discord_requests_webhook_url": "http://discord.example.com/hook"},
		{"notifications_discord_requests_webhook_url": "https://192.168.1.10/hook"},
		{"contribute_announcement": map[string]any{"text": "hi", "expires_at": "tomorrow"}},
		{"contribute_suspended": true, "snapshot_interval_min": -5},
	} {
		if rec := doPut(s, governorHubPath, bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%v: expected 400, got %d — %s", bad, rec.Code, rec.Body.String())
		}
	}
	if h.ContributeSuspended || h.ContributeWallRetentionDays != before.ContributeWallRetentionDays ||
		h.SnapshotIntervalMin != before.SnapshotIntervalMin || h.ContributeAnnouncement != before.ContributeAnnouncement ||
		h.Notifications.Discord.RequestsWebhookURL != "" {
		t.Fatalf("a rejected PUT changed the config: %+v", *h)
	}

	// Clearing an announcement ignores whatever expiry it carries.
	if rec := doPut(s, governorHubPath, map[string]any{"contribute_announcement": map[string]any{"text": "", "expires_at": "never"}}); rec.Code != http.StatusOK {
		t.Fatalf("clear announcement: %d — %s", rec.Code, rec.Body.String())
	}
}

// Absent fields are left unchanged, and only carried keys are validated, so
// a value in hive.yaml the editor would refuse does not block other saves.
func TestHubSettingsParityBatch9AbsentFieldsUnchanged(t *testing.T) {
	s, deps := apiServer(t)
	h := &deps.Config.Hub
	accept, push := true, false
	h.ContributeRequireExplicitAccept = &accept
	h.TaskStatusPush = &push
	h.ContributeAlreadyDoneLabel = "kept"
	h.UpgradeDrainTimeoutS = 600
	h.NPSRelayPullSecret = "kept-secret"
	h.SnapshotIntervalMin = 2 // below the editor's floor
	if rec := doPut(s, governorHubPath, map[string]any{"nps_enabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("unrelated PUT: %d — %s", rec.Code, rec.Body.String())
	}
	if !h.IsContributeRequireExplicitAccept() || h.TaskStatusPush == nil || *h.TaskStatusPush || h.ContributeAlreadyDoneLabel != "kept" ||
		h.UpgradeDrainTimeoutS != 600 || h.NPSRelayPullSecret != "kept-secret" || h.SnapshotIntervalMin != 2 ||
		h.NPSEnabled == nil || !*h.NPSEnabled {
		t.Fatalf("absent hub fields changed: %+v", *h)
	}
}

// Only a verified owner may change the hub settings.
func TestHubSettingsParityBatch9OwnerOnly(t *testing.T) {
	s, deps := apiServer(t)
	req := httptest.NewRequest(http.MethodPut, governorHubPath, strings.NewReader(`{"nps_relay_pull_secret":"x","snapshot_interval_min":60}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", "read-write")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PUT as read-write: status = %d, want 403", rec.Code)
	}
	if deps.Config.Hub.NPSRelayPullSecret != "" || deps.Config.Hub.SnapshotIntervalMin == 60 {
		t.Fatal("a non-owner PUT changed the config")
	}
}

// Every hub key this batch exposes renders with its YAML-key hint, either as a
// control wired to the Hub tab save or read-only with its note; secrets are
// password inputs that never render a value.
func TestParityBatch9SettingsRenderEveryKey(t *testing.T) {
	html := indexHTML(t)
	body := extractJSFunction(t, html, "renderHubSettingsParity")
	for _, c := range []struct{ yaml, key string }{
		{"hub.contribute_require_explicit_accept", "contribute_require_explicit_accept"},
		{"hub.contribute_already_done_label", "contribute_already_done_label"},
		{"hub.contribute_already_done_hold_days", "contribute_already_done_hold_days"},
		{"hub.contribute_wall_retention_days", "contribute_wall_retention_days"},
		{"hub.snapshot_interval_min", "snapshot_interval_min"},
		{"hub.upgrade_drain_timeout_s", "upgrade_drain_timeout_s"},
		{"hub.task_status_push", "task_status_push"},
		{"hub.nps_enabled", "nps_enabled"},
	} {
		if !strings.Contains(body, "('"+c.key+"', ") || !strings.Contains(body, "'"+c.yaml+"'") {
			t.Errorf("%s has no Hub control with its YAML hint", c.yaml)
		}
	}
	for _, c := range []struct{ yaml, key string }{
		{"hub.notifications.discord.requests_webhook_url", "notifications_discord_requests_webhook_url"},
		{"hub.nps_relay_pull_secret", "nps_relay_pull_secret"},
	} {
		if !strings.Contains(body, "secret('"+c.key+"', ") || !strings.Contains(body, "'"+c.yaml+"'") {
			t.Errorf("%s is not a write-only secret control", c.yaml)
		}
	}
	if !strings.Contains(body, `type="password"`) || strings.Contains(extractSecretHelper(body), "value=") {
		t.Error("hub secrets must be password inputs that never render a value")
	}
	for _, yaml := range []string{
		"hub.cluster_id",
		"hub.hive_type",
		"hub.contribute_already_done_unverified_hold_days",
		"hub.contribute_close_already_done",
		"hub.contribute_announcement.id",
	} {
		if !strings.Contains(body, "roRow(") || !strings.Contains(body, "'"+yaml+"'") {
			t.Errorf("%s is not shown read-only with its YAML hint", yaml)
		}
	}
	for _, note := range []string{"managed by the hub", "set in hive.yaml; requires restart"} {
		if !strings.Contains(body, note) {
			t.Errorf("identity keys do not carry the %q note", note)
		}
	}
	for _, yaml := range []string{
		"hub.contribute_announcement.text",
		"hub.contribute_announcement.level",
		"hub.contribute_announcement.expires_at",
	} {
		if !strings.Contains(body, "yamlHint('"+yaml+"')") {
			t.Errorf("%s has no announcement control with its YAML hint", yaml)
		}
	}
	ann := extractJSFunction(t, html, "hubAnnouncementChanged")
	if !strings.Contains(ann, "markDirty('hub', 'contribute_announcement'") {
		t.Error("announcement edits are not saved through the Hub tab")
	}
	if !strings.Contains(extractJSFunction(t, html, "renderGovHub"), "renderHubSettingsParity(hub)") {
		t.Error("the Hub tab does not render the hub settings block")
	}
}

func extractSecretHelper(body string) string {
	start := strings.Index(body, "const secret = ")
	if start < 0 {
		return ""
	}
	rest := body[start:]
	if end := strings.Index(rest, "</div>`;"); end >= 0 {
		return rest[:end]
	}
	return rest
}
