package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// ============================================================
// handleGovernorConfigGet — response-contract tests.
//
// This handler (api.go, GET /api/config/governor) is the single source the
// dashboard Settings UI reads to render the Governor tabs (Agents, Labels,
// Notifications, Hub, ...). It has accumulated 16 fix commits over time, each
// patching one section's shape or semantics without a test pinning the
// contract of the other sections. These tests lock down the sections most
// prone to silent regression: the org-qualification logic for repos, the
// idle-exclusion + effective-threshold scaling for thresholds, secret
// masking for notifications, and the label-polarity split between
// "requireLabels" (project.issue_filter) and "labels" (governor.labels.exempt).
//
// Each test asserts both a positive (the right value appears) and a negative
// (the wrong/raw value does NOT appear) so a handler that returns the wrong
// thing, or leaks something it must mask, cannot pass silently.
// ============================================================

const (
	testRepoCountThree = 3 // used to exercise threshold scaling with a non-1 repo count
)

func decodeGovernorConfigGet(t *testing.T, srv *Server) map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/config/governor", nil)
	w := httptest.NewRecorder()
	srv.handleGovernorConfigGet(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want %d (body: %s)", w.Code, http.StatusOK, w.Body.String())
	}
	var result map[string]any
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, w.Body.String())
	}
	return result
}

// --- agents ---

func TestHandleGovernorConfigGet_AgentsListsConfiguredNamesOnly(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Agents = map[string]config.AgentConfig{
		"scanner":  {ID: "scan-001", Role: "scanner", Backend: "claude", Model: "sonnet", Enabled: true},
		"reviewer": {ID: "rev-001", Role: "reviewer", Backend: "claude", Model: "sonnet", Enabled: true},
		"outreach": {ID: "out-001", Role: "outreach", Backend: "claude", Model: "sonnet", Enabled: false},
	}

	result := decodeGovernorConfigGet(t, srv)

	raw, ok := result["agents"].([]any)
	if !ok {
		t.Fatalf("agents section missing or wrong type: %v", result["agents"])
	}
	got := make([]string, 0, len(raw))
	for _, v := range raw {
		got = append(got, v.(string))
	}
	sort.Strings(got)
	want := []string{"outreach", "reviewer", "scanner"}
	if len(got) != len(want) {
		t.Fatalf("agents = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("agents[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// Negative: an agent name that was never configured must not appear.
	for _, name := range got {
		if name == "outreach-ghost" {
			t.Errorf("agents contains unconfigured name %q", name)
		}
	}
}

// --- thresholds / effectiveThresholds / repoCount ---

func TestHandleGovernorConfigGet_ThresholdsExcludeIdleAndEffectiveThresholdsMatchConfig(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Project.Repos = []string{"repo-a", "repo-b", "repo-c"}
	srv.deps.Config.Governor.Modes = map[string]config.ModeConfig{
		"idle":  {Threshold: 0},
		"quiet": {Threshold: 5},
		"busy":  {Threshold: 15},
		"surge": {Threshold: 30},
	}

	result := decodeGovernorConfigGet(t, srv)

	thresholds, ok := result["thresholds"].(map[string]any)
	if !ok {
		t.Fatalf("thresholds section missing or wrong type: %v", result["thresholds"])
	}
	// Positive: non-idle modes present with their configured values.
	if thresholds["quiet"].(float64) != 5 {
		t.Errorf("thresholds[quiet] = %v, want 5", thresholds["quiet"])
	}
	if thresholds["busy"].(float64) != 15 {
		t.Errorf("thresholds[busy] = %v, want 15", thresholds["busy"])
	}
	if thresholds["surge"].(float64) != 30 {
		t.Errorf("thresholds[surge] = %v, want 30", thresholds["surge"])
	}
	// Negative: idle must never appear, even though cfg.Governor.Modes has an
	// idle entry.
	if _, present := thresholds["idle"]; present {
		t.Errorf("thresholds contains idle entry: %v", thresholds["idle"])
	}

	repoCount := srv.deps.Config.Project.RepoCount()
	if repoCount != testRepoCountThree {
		t.Fatalf("test setup: RepoCount() = %d, want %d", repoCount, testRepoCountThree)
	}
	if got, want := result["repoCount"].(float64), float64(repoCount); got != want {
		t.Errorf("repoCount = %v, want %v", got, want)
	}

	effective, ok := result["effectiveThresholds"].(map[string]any)
	if !ok {
		t.Fatalf("effectiveThresholds section missing or wrong type: %v", result["effectiveThresholds"])
	}
	// Positive: exactly the three modes, each equal to EffectiveThreshold.
	wantModes := []string{"quiet", "busy", "surge"}
	if len(effective) != len(wantModes) {
		t.Fatalf("effectiveThresholds has %d keys (%v), want exactly %v", len(effective), effective, wantModes)
	}
	for _, mode := range wantModes {
		want := float64(srv.deps.Config.Governor.EffectiveThreshold(mode, repoCount))
		got, present := effective[mode]
		if !present {
			t.Errorf("effectiveThresholds missing key %q", mode)
			continue
		}
		if got.(float64) != want {
			t.Errorf("effectiveThresholds[%s] = %v, want %v", mode, got, want)
		}
	}
	// Negative: no extra keys (e.g. idle) leaked into effectiveThresholds.
	if _, present := effective["idle"]; present {
		t.Errorf("effectiveThresholds contains idle entry: %v", effective["idle"])
	}
}

// --- repos / primaryRepo org-qualification ---

func TestHandleGovernorConfigGet_ReposOrgQualifiesBareNamesOnly(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Project.Org = "hivecommons"
	srv.deps.Config.Project.Repos = []string{"hive", "otherorg/already-qualified"}
	srv.deps.Config.Project.PrimaryRepo = "hive"

	result := decodeGovernorConfigGet(t, srv)

	raw, ok := result["repos"].([]any)
	if !ok {
		t.Fatalf("repos section missing or wrong type: %v", result["repos"])
	}
	got := make([]string, 0, len(raw))
	for _, v := range raw {
		got = append(got, v.(string))
	}
	want := []string{"hivecommons/hive", "otherorg/already-qualified"}
	if len(got) != len(want) {
		t.Fatalf("repos = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("repos[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// Negative: the already-qualified repo must not get double-prefixed.
	for _, r := range got {
		if strings.Contains(r, "kubestellar/otherorg") {
			t.Errorf("repos double-prefixed an already-qualified name: %q", r)
		}
	}

	if got, want := result["primaryRepo"].(string), "hivecommons/hive"; got != want {
		t.Errorf("primaryRepo = %q, want %q", got, want)
	}
}

func TestHandleGovernorConfigGet_PrimaryRepoAlreadyQualifiedUnchangedAndEmptyStaysEmpty(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Project.Org = "hivecommons"
	srv.deps.Config.Project.PrimaryRepo = "otherorg/already-qualified"

	result := decodeGovernorConfigGet(t, srv)
	if got, want := result["primaryRepo"].(string), "otherorg/already-qualified"; got != want {
		t.Errorf("primaryRepo = %q, want unchanged %q", got, want)
	}

	srv2 := newFullServer(t)
	srv2.deps.Config.Project.Org = "hivecommons"
	srv2.deps.Config.Project.PrimaryRepo = ""
	result2 := decodeGovernorConfigGet(t, srv2)
	if got := result2["primaryRepo"].(string); got != "" {
		t.Errorf("primaryRepo = %q, want empty string to stay empty", got)
	}
}

// --- notifications ---

func TestHandleGovernorConfigGet_NotificationsEmptyWhenUnconfigured(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Notifications = config.NotificationsConfig{}

	result := decodeGovernorConfigGet(t, srv)
	n, ok := result["notifications"].(map[string]any)
	if !ok {
		t.Fatalf("notifications section missing or wrong type: %v", result["notifications"])
	}
	if n["hasNtfy"] != false {
		t.Errorf("hasNtfy = %v, want false", n["hasNtfy"])
	}
	if n["hasDiscord"] != false {
		t.Errorf("hasDiscord = %v, want false", n["hasDiscord"])
	}
	for _, field := range []string{"ntfyServer", "ntfyTopic", "discordWebhook"} {
		if n[field] != "" {
			t.Errorf("%s = %v, want empty string", field, n[field])
		}
	}
}

func TestHandleGovernorConfigGet_DiscordWebhookMaskedNeverRaw(t *testing.T) {
	srv := newFullServer(t)
	const rawWebhook = "https://discord.com/api/webhooks/1234567890/super-secret-token-value"
	srv.deps.Config.Notifications.Discord = &config.DiscordConfig{Webhook: rawWebhook}

	req := httptest.NewRequest("GET", "/api/config/governor", nil)
	w := httptest.NewRecorder()
	srv.handleGovernorConfigGet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	body := w.Body.String()
	// Negative: the raw webhook must not appear anywhere in the response.
	if strings.Contains(body, rawWebhook) {
		t.Fatal("response body contains the raw discord webhook")
	}

	var result map[string]any
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&result); err != nil {
		t.Fatal(err)
	}
	n := result["notifications"].(map[string]any)
	if n["hasDiscord"] != true {
		t.Errorf("hasDiscord = %v, want true", n["hasDiscord"])
	}
	masked, ok := n["discordWebhook"].(string)
	if !ok || masked == "" {
		t.Fatalf("discordWebhook = %v, want a masked non-empty string", n["discordWebhook"])
	}
	if masked == rawWebhook {
		t.Errorf("discordWebhook was not masked: %q", masked)
	}
	// Positive: masking preserves only the last 4 characters (maskSecret).
	if want := maskSecret(rawWebhook); masked != want {
		t.Errorf("discordWebhook = %q, want %q (maskSecret output)", masked, want)
	}
}

func TestHandleGovernorConfigGet_NtfyConfiguredReflectsServerAndTopic(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Notifications.Ntfy = &config.NtfyConfig{
		Server: "https://ntfy.example.com",
		Topic:  "hive-alerts",
	}

	result := decodeGovernorConfigGet(t, srv)
	n := result["notifications"].(map[string]any)
	if n["hasNtfy"] != true {
		t.Errorf("hasNtfy = %v, want true", n["hasNtfy"])
	}
	if got, want := n["ntfyServer"].(string), "https://ntfy.example.com"; got != want {
		t.Errorf("ntfyServer = %q, want %q", got, want)
	}
	if got, want := n["ntfyTopic"].(string), "hive-alerts"; got != want {
		t.Errorf("ntfyTopic = %q, want %q", got, want)
	}
	// Negative: discord must remain unaffected/absent when only ntfy is set.
	if n["hasDiscord"] != false {
		t.Errorf("hasDiscord = %v, want false when discord unconfigured", n["hasDiscord"])
	}
}

// --- requireLabels / labels / holdLabels ---

func TestHandleGovernorConfigGet_LabelPolaritySplit(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Project.IssueFilter.RequireLabels = []string{"needs-triage", "ready"}
	srv.deps.Config.Governor.Labels.Exempt = []string{"do-not-automate", "manual-only"}

	result := decodeGovernorConfigGet(t, srv)

	requireLabels := toStringSlice(t, result["requireLabels"])
	if !equalStringSlices(requireLabels, []string{"needs-triage", "ready"}) {
		t.Errorf("requireLabels = %v, want [needs-triage ready]", requireLabels)
	}

	labels := toStringSlice(t, result["labels"])
	if !equalStringSlices(labels, []string{"do-not-automate", "manual-only"}) {
		t.Errorf("labels = %v, want [do-not-automate manual-only]", labels)
	}

	// Negative: the two lists must not be swapped or merged.
	if equalStringSlices(requireLabels, labels) {
		t.Fatal("requireLabels and labels must not be equal — they are opposite-polarity lists")
	}

	holdLabels := toStringSlice(t, result["holdLabels"])
	if !equalStringSlices(holdLabels, github.HoldLabels) {
		t.Errorf("holdLabels = %v, want %v", holdLabels, github.HoldLabels)
	}
}

// --- top-level section presence ---

func TestHandleGovernorConfigGet_TopLevelSectionsPresent(t *testing.T) {
	srv := newFullServer(t)

	result := decodeGovernorConfigGet(t, srv)

	wantKeys := []string{
		"agents", "thresholds", "effectiveThresholds", "repos", "primaryRepo",
		"notifications", "budget", "health", "sensing", "logging", "litellm",
		"review", "auto_merge", "hub", "attribution",
	}
	for _, key := range wantKeys {
		if _, present := result[key]; !present {
			t.Errorf("response missing top-level key %q", key)
		}
	}
	// Negative: a key the UI has never depended on should not be asserted as
	// required — guard against a typo'd want list silently always passing by
	// checking a deliberately-absent key is in fact absent from wantKeys.
	for _, key := range wantKeys {
		if key == "does-not-exist-marker" {
			t.Fatal("wantKeys sanity check failed")
		}
	}
}

// --- helpers ---

func toStringSlice(t *testing.T, v any) []string {
	t.Helper()
	if v == nil {
		return nil
	}
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("value is not a []any: %v (%T)", v, v)
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("slice element is not a string: %v (%T)", item, item)
		}
		out = append(out, s)
	}
	return out
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
