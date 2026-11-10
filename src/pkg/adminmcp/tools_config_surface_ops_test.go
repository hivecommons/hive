package adminmcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// surfaceProvider answers settings_read section governor with a JSON round
// trip of governor (so lists arrive as []any, as from the hive), and records
// writes.
type surfaceProvider struct {
	governor map[string]any
	requests []WriteRequest
}

func (p *surfaceProvider) Read(_ context.Context, tool string, args map[string]any) (any, error) {
	if tool != ToolSettingsRead || args["section"] != SettingsSectionDefault {
		return map[string]any{}, nil
	}
	data, err := json.Marshal(p.governor)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = json.Unmarshal(data, &out)
	return out, err
}

func (p *surfaceProvider) ExecuteWrite(_ context.Context, req WriteRequest) (any, error) {
	p.requests = append(p.requests, req)
	return map[string]any{"status": "updated"}, nil
}

func surfaceGovernor() map[string]any {
	return map[string]any{
		"requireLabels": []string{"triage/accepted", "hive/approved"},
		"hardSuppressLabels": map[string]any{
			"needsHuman": []string{"needs-human"}, "needsDirection": []string{"needs-direction"},
			"needsDecision": []string{"stage/045-needs-decision"}, "needsSpec": []string{"needs-spec"},
		},
		"reporterTrust": map[string]any{
			"enabled":                false,
			"trustedAssociations":    []string{"OWNER", "MEMBER", "COLLABORATOR"},
			"trustedLogins":          []string{"ext-maint", "other-maint"},
			"untrustedRequireLabels": []string{"triage/accepted"},
		},
		"hub": map[string]any{"settings": map[string]any{"hub_managed": false}},
		"security": map[string]any{"sentinel": map[string]any{
			"label":                 "sentinel-alert",
			"sensitivePaths":        []string{"OWNERS", ".github/workflows/**", "**/.github/workflows/**", "hive.yaml"},
			"sensitivePathsDefault": false,
			"defaultSensitivePaths": []string{"OWNERS", ".github/workflows/**"},
			"behaviors": []any{
				map[string]any{"name": "sensitive_path", "enabled": true},
				map[string]any{"name": "permission_escalation", "enabled": true},
				map[string]any{"name": "test_removal", "enabled": false},
			},
			"exemptLogins":        []string{"dependabot[bot]"},
			"trustedAuthorsBlock": true,
		}},
	}
}

func surfaceHandler(p *surfaceProvider) *Handler {
	return NewHandler(p, WithWritesEnabled(true), WithPendingStore(NewMemoryPendingStore()), WithHiveID("hive-a"))
}

func bodyJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestConfigSurfaceOpsRegisteredOnTheirEndpointsOnly(t *testing.T) {
	for name, path := range map[string]string{
		WriteOpProjectIssueFilter: "/api/config/governor/labels",
		WriteOpSentinelSettings:   "/api/config/governor/security",
	} {
		if _, ok := DefaultWriteRegistry().Get(name); !ok {
			t.Fatalf("%s is not registered", name)
		}
		if !strings.Contains(mustOp(t, name).Description(), path) || !strings.Contains(mustOp(t, name).Description(), "Owner only") {
			t.Errorf("%s description does not name %s and the owner-only rule", name, path)
		}
	}
}

func mustOp(t *testing.T, name string) WriteOp {
	t.Helper()
	op, ok := DefaultWriteRegistry().Get(name)
	if !ok {
		t.Fatalf("%s is not registered", name)
	}
	return op
}

// ---- project.issue_filter ----

func TestIssueFilterOpRejectsUnknownFields(t *testing.T) {
	h := surfaceHandler(&surfaceProvider{governor: surfaceGovernor()})
	for name, args := range map[string]map[string]any{
		"top-level typo":    {"require_label": map[string]any{"add": []any{"x"}}},
		"writing guide":     {"writing_guide": "anything"},
		"nested typo":       {"reporter_trust": map[string]any{"trusted_login": map[string]any{"add": []any{"x"}}}},
		"clanker field":     {"reporter_trust": map[string]any{"clanker_requested": true}},
		"bucket typo":       {"hard_suppress_labels": map[string]any{"needs_humans": map[string]any{"add": []any{"x"}}}},
		"list change verb":  {"require_labels": map[string]any{"set": []any{"x"}}},
		"list not object":   {"require_labels": []any{"x"}},
		"empty change":      {"require_labels": map[string]any{}},
		"clear and remove":  {"require_labels": map[string]any{"clear": true, "remove": []any{"hive/approved"}}},
		"no change at all":  {},
		"bogus association": {"reporter_trust": map[string]any{"trusted_associations": map[string]any{"add": []any{"MAINTAINER"}}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpProjectIssueFilter, "args": args}); err == nil {
				t.Fatalf("args %v accepted", args)
			}
		})
	}
}

// A list change sends the whole merged list (the endpoint replaces lists),
// keeping every entry it does not name, and sends no field it does not name.
func TestIssueFilterOpMergeKeepsUntouchedEntries(t *testing.T) {
	h := surfaceHandler(&surfaceProvider{governor: surfaceGovernor()})
	p, _ := capPreview(t, h, WriteOpProjectIssueFilter, map[string]any{
		"require_labels": map[string]any{"remove": []any{"hive/approved"}},
		"reporter_trust": map[string]any{"enabled": true, "trusted_logins": map[string]any{"add": []any{"hivecommons-hive[bot]"}}},
	})
	if p.Request.Method != http.MethodPut || p.Request.Path != "/api/config/governor/labels" {
		t.Fatalf("request = %s %s", p.Request.Method, p.Request.Path)
	}
	want := `{"reporter_trust":{"enabled":true,"trusted_logins":["ext-maint","other-maint","hivecommons-hive[bot]"]},"require_labels":["triage/accepted"]}`
	if got := bodyJSON(t, p.Request.Body); got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
	joined := strings.Join(p.Effects, "\n")
	for _, line := range []string{
		"require_labels: [triage/accepted, hive/approved] → [triage/accepted]",
		"reporter_trust.trusted_logins: [ext-maint, other-maint] → [ext-maint, other-maint, hivecommons-hive[bot]]",
		"reporter_trust.enabled: off → on",
	} {
		if !strings.Contains(joined, line) {
			t.Errorf("effects miss %q:\n%s", line, joined)
		}
	}
	if p.StateFingerprint == "" {
		t.Fatal("preview records no basis, so a stale confirm could not be refused")
	}
	// Removing an entry that is not there is refused rather than ignored.
	if _, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpProjectIssueFilter, "args": map[string]any{"require_labels": map[string]any{"remove": []any{"triage/acceptd"}}}}); err == nil || !strings.Contains(err.Error(), "not in the current list") {
		t.Fatalf("removing a missing label: err = %v", err)
	}
}

// Every change that admits more issues without triage says so; one that
// admits none does not.
func TestIssueFilterOpFlagsWidening(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(g map[string]any)
		args   map[string]any
		widen  bool
		phrase string
	}{
		{"require_labels cleared", nil, map[string]any{"require_labels": map[string]any{"clear": true}}, true, "label gate turns off"},
		{"require label added", nil, map[string]any{"require_labels": map[string]any{"add": []any{"good-first-issue"}}}, true, "good-first-issue now pass"},
		{"trusted login added", nil, map[string]any{"reporter_trust": map[string]any{"trusted_logins": map[string]any{"add": []any{"new-maint"}}}}, true, "filed by new-maint skip"},
		{"trusted association added", nil, map[string]any{"reporter_trust": map[string]any{"trusted_associations": map[string]any{"add": []any{"contributor"}}}}, true, "CONTRIBUTOR skip"},
		{"reporter trust turned off", func(g map[string]any) { g["reporterTrust"].(map[string]any)["enabled"] = true }, map[string]any{"reporter_trust": map[string]any{"enabled": false}}, true, "trust is turned off"},
		{"hard-suppress label removed", nil, map[string]any{"hard_suppress_labels": map[string]any{"needs_spec": map[string]any{"add": []any{"needs-design"}, "remove": []any{"needs-spec"}}}}, true, "needs-spec are no longer parked"},
		{"reporter trust turned on", nil, map[string]any{"reporter_trust": map[string]any{"enabled": true}}, false, "reporter trust is turned on"},
		{"require label removed, gate stays on", nil, map[string]any{"require_labels": map[string]any{"remove": []any{"hive/approved"}}}, false, "no longer pass require_labels"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := surfaceGovernor()
			if tc.mutate != nil {
				tc.mutate(g)
			}
			p, _ := capPreview(t, surfaceHandler(&surfaceProvider{governor: g}), WriteOpProjectIssueFilter, tc.args)
			if isWidening(p) != tc.widen {
				t.Fatalf("widening = %v, want %v: %q", isWidening(p), tc.widen, p.WideningDisclosure)
			}
			if tc.widen && !strings.HasPrefix(p.WideningDisclosure, "Widening: more issues admitted without triage.") {
				t.Fatalf("disclosure does not lead with the plain statement: %q", p.WideningDisclosure)
			}
			if !strings.Contains(p.WideningDisclosure, tc.phrase) {
				t.Fatalf("disclosure %q does not say %q", p.WideningDisclosure, tc.phrase)
			}
		})
	}
}

// A bot login added to trusted_logins is disclosed as widening and also says
// reporter trust never judges bot-filed issues, so the login changes neither
// admission nor the merge hold.
func TestIssueFilterOpExplainsBotTrustedLogin(t *testing.T) {
	p, _ := capPreview(t, surfaceHandler(&surfaceProvider{governor: surfaceGovernor()}), WriteOpProjectIssueFilter, map[string]any{
		"reporter_trust": map[string]any{"trusted_logins": map[string]any{"add": []any{"hivecommons-hive[bot]"}}},
	})
	if !isWidening(p) || !strings.Contains(p.WideningDisclosure, "look like bot accounts") {
		t.Fatalf("disclosure = %q", p.WideningDisclosure)
	}
}

// A confirm whose preview read settings that have changed since is refused,
// and nothing is written; a fresh preview merges onto the new values.
func TestIssueFilterOpRefusesStalePreview(t *testing.T) {
	provider := &surfaceProvider{governor: surfaceGovernor()}
	h := surfaceHandler(provider)
	args := map[string]any{"reporter_trust": map[string]any{"trusted_logins": map[string]any{"add": []any{"new-maint"}}}}
	_, id := capPreview(t, h, WriteOpProjectIssueFilter, args)

	// Somebody else saves the Labels tab in between.
	provider.governor["reporterTrust"].(map[string]any)["trustedLogins"] = []string{"ext-maint", "other-maint", "added-meanwhile"}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
		t.Fatalf("stale confirm err = %v, want ErrConfirmationStale", err)
	}
	if len(provider.requests) != 0 {
		t.Fatalf("a stale confirm wrote %+v", provider.requests)
	}

	_, id = capPreview(t, h, WriteOpProjectIssueFilter, args)
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("fresh confirm: %v", err)
	}
	want := `{"reporter_trust":{"trusted_logins":["ext-maint","other-maint","added-meanwhile","new-maint"]}}`
	if len(provider.requests) != 1 || bodyJSON(t, provider.requests[0].Body) != want {
		t.Fatalf("executed %+v, want one write with body %s", provider.requests, want)
	}
}

// Without a reading of the current filter there is nothing to merge onto,
// so the preview is refused rather than guessed.
func TestIssueFilterOpNeedsTheCurrentFilter(t *testing.T) {
	args := map[string]any{"require_labels": map[string]any{"clear": true}}
	if _, err := (issueFilterOp{}).PreviewWithProvider(context.Background(), args, nil); err == nil {
		t.Fatal("previewed without reading the current filter")
	}
	// The provider-less Preview (used by write_confirm for the method and
	// path) sends nothing and claims nothing.
	p, err := (issueFilterOp{}).Preview(context.Background(), args)
	if err != nil || p.Request.Body != nil || p.StateFingerprint != "" || !strings.HasPrefix(p.WideningDisclosure, "Potential widening:") {
		t.Fatalf("provider-less preview = %+v, %v", p, err)
	}
	g := surfaceGovernor()
	delete(g, "reporterTrust")
	if _, err := surfaceHandler(&surfaceProvider{governor: g}).PreviewWrite(context.Background(), map[string]any{"operation": WriteOpProjectIssueFilter, "args": map[string]any{"require_labels": map[string]any{"clear": true}}}); err == nil || !strings.Contains(err.Error(), "reporterTrust") {
		t.Fatalf("missing reporterTrust: err = %v", err)
	}
}

// On a hub-managed hive the preview warns that the hub may overwrite it.
func TestIssueFilterOpNotesHubManaged(t *testing.T) {
	g := surfaceGovernor()
	g["hub"] = map[string]any{"settings": map[string]any{"hub_managed": true}}
	p, _ := capPreview(t, surfaceHandler(&surfaceProvider{governor: g}), WriteOpProjectIssueFilter, map[string]any{"require_labels": map[string]any{"add": []any{"x"}}})
	if !strings.Contains(strings.Join(p.Effects, "\n"), "Hub-managed: may be overwritten.") || p.Details["hub_managed"] != true {
		t.Fatalf("hub-managed hive preview does not warn: %v", p.Effects)
	}
	p, _ = capPreview(t, surfaceHandler(&surfaceProvider{governor: surfaceGovernor()}), WriteOpProjectIssueFilter, map[string]any{"require_labels": map[string]any{"add": []any{"x"}}})
	if strings.Contains(strings.Join(p.Effects, "\n"), "Hub-managed") {
		t.Fatalf("a hive the hub does not manage was warned: %v", p.Effects)
	}
}

// ---- sentinel.settings ----

func TestSentinelSettingsOpRejectsUnknownFields(t *testing.T) {
	h := surfaceHandler(&surfaceProvider{governor: surfaceGovernor()})
	for name, args := range map[string]map[string]any{
		"enabled is not offered": {"enabled": false},
		"repos is not offered":   {"repos": map[string]any{"add": []any{"o/r"}}},
		"camelCase field":        {"sensitivePaths": map[string]any{"add": []any{"x"}}},
		"list change verb":       {"exempt_logins": map[string]any{"replace": []any{"x"}}},
		"unknown behavior":       {"disabled_behaviors": map[string]any{"add": []any{"everything"}}},
		"non-bool block":         {"trusted_authors_block": "off"},
		"no change at all":       {},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpSentinelSettings, "args": args}); err == nil {
				t.Fatalf("args %v accepted", args)
			}
		})
	}
}

func TestSentinelSettingsOpMergeKeepsUntouchedEntries(t *testing.T) {
	h := surfaceHandler(&surfaceProvider{governor: surfaceGovernor()})
	p, _ := capPreview(t, h, WriteOpSentinelSettings, map[string]any{
		"sensitive_paths":    map[string]any{"add": []any{"**/release*.yml"}},
		"disabled_behaviors": map[string]any{"remove": []any{"Test_Removal"}},
	})
	if p.Request.Method != http.MethodPut || p.Request.Path != "/api/config/governor/security" {
		t.Fatalf("request = %s %s", p.Request.Method, p.Request.Path)
	}
	want := `{"sentinel":{"disabledBehaviors":[],"sensitivePaths":["OWNERS",".github/workflows/**","**/.github/workflows/**","hive.yaml","**/release*.yml"]}}`
	if got := bodyJSON(t, p.Request.Body); got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
	if isWidening(p) {
		t.Fatalf("flagging more pull requests disclosed as widening: %q", p.WideningDisclosure)
	}
	if p.StateFingerprint == "" {
		t.Fatal("preview records no basis")
	}
}

// Each change that flags fewer pull requests is widening and names the pull
// requests that would no longer be held.
func TestSentinelSettingsOpFlagsNarrowingWithThePRsNoLongerHeld(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   map[string]any
		phrase string
	}{
		{"workflow paths removed", map[string]any{"sensitive_paths": map[string]any{"remove": []any{".github/workflows/**", "**/.github/workflows/**"}}}, "whose only sensitive-path match is .github/workflows/**, **/.github/workflows/** is no longer flagged"},
		{"behavior disabled", map[string]any{"disabled_behaviors": map[string]any{"add": []any{"permission_escalation"}}}, "permission_escalation rule is switched off"},
		{"exempt login added", map[string]any{"exempt_logins": map[string]any{"add": []any{"renovate[bot]"}}}, "opened by renovate[bot] are never flagged"},
		{"trusted authors no longer blocked", map[string]any{"trusted_authors_block": false}, "informational notice only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := capPreview(t, surfaceHandler(&surfaceProvider{governor: surfaceGovernor()}), WriteOpSentinelSettings, tc.args)
			if !strings.HasPrefix(p.WideningDisclosure, "Widening: fewer pull requests flagged by Sentinel") {
				t.Fatalf("disclosure = %q", p.WideningDisclosure)
			}
			if !strings.Contains(p.WideningDisclosure, tc.phrase) || !strings.Contains(p.WideningDisclosure, "would no longer be held") {
				t.Fatalf("disclosure %q does not name the pull requests (%q)", p.WideningDisclosure, tc.phrase)
			}
			if !strings.Contains(strings.Join(p.Effects, "\n"), "removes no label") {
				t.Fatalf("effects do not say existing labels stay: %v", p.Effects)
			}
		})
	}
}

// Emptying sensitive_paths is stored as unset by the endpoint, which is the
// shipped defaults; the preview shows that, not an empty list.
func TestSentinelSettingsOpEmptiedPathsGoBackToDefaults(t *testing.T) {
	p, _ := capPreview(t, surfaceHandler(&surfaceProvider{governor: surfaceGovernor()}), WriteOpSentinelSettings, map[string]any{"sensitive_paths": map[string]any{"clear": true}})
	joined := strings.Join(p.Effects, "\n")
	if !strings.Contains(joined, "→ [OWNERS, .github/workflows/**]") || !strings.Contains(joined, "shipped defaults") {
		t.Fatalf("effects = %s", joined)
	}
	if !strings.Contains(p.WideningDisclosure, "**/.github/workflows/**, hive.yaml") {
		t.Fatalf("paths the defaults drop are not named: %q", p.WideningDisclosure)
	}
}

func TestSentinelSettingsOpRefusesStalePreview(t *testing.T) {
	provider := &surfaceProvider{governor: surfaceGovernor()}
	h := surfaceHandler(provider)
	args := map[string]any{"exempt_logins": map[string]any{"add": []any{"renovate[bot]"}}}
	_, id := capPreview(t, h, WriteOpSentinelSettings, args)

	provider.governor["security"].(map[string]any)["sentinel"].(map[string]any)["trustedAuthorsBlock"] = false
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
		t.Fatalf("stale confirm err = %v, want ErrConfirmationStale", err)
	}
	if len(provider.requests) != 0 {
		t.Fatalf("a stale confirm wrote %+v", provider.requests)
	}
	_, id = capPreview(t, h, WriteOpSentinelSettings, args)
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil || len(provider.requests) != 1 {
		t.Fatalf("fresh confirm err = %v, requests = %+v", err, provider.requests)
	}
}
