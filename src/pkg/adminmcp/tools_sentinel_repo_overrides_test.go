package adminmcp

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func surfaceGovernorWithRepoOverrides() map[string]any {
	g := surfaceGovernor()
	g["security"].(map[string]any)["sentinel"].(map[string]any)["repoSensitivePaths"] = []any{
		map[string]any{"repo": "acme/api", "sensitivePaths": []any{"OWNERS", ".github/workflows/**", "**/.github/workflows/**", "hive.yaml"}},
		map[string]any{"repo": "acme/website", "sensitivePaths": []any{"OWNERS"}},
	}
	return g
}

// A hive-wide sentinel.settings change names the repositories that keep their
// own list, both in the effects and in the widening disclosure, and what a
// looser per-repository list already leaves out. On v6 tip the preview never
// reads the per-repository lists, so none of the repositories is named.
func TestSentinelSettingsOpNamesReposWithTheirOwnList(t *testing.T) {
	p, _ := capPreview(t, surfaceHandler(&surfaceProvider{governor: surfaceGovernorWithRepoOverrides()}), WriteOpSentinelSettings, map[string]any{
		"sensitive_paths": map[string]any{"remove": []any{"hive.yaml"}},
	})
	if !strings.HasPrefix(p.WideningDisclosure, "Widening:") {
		t.Fatalf("disclosure = %q", p.WideningDisclosure)
	}
	if !strings.Contains(p.WideningDisclosure, "except in acme/api, acme/website, which keep their own list") {
		t.Fatalf("widening disclosure does not name the repositories the change skips: %q", p.WideningDisclosure)
	}
	if !strings.Contains(p.WideningDisclosure, "acme/website does not flag .github/workflows/**, **/.github/workflows/**") {
		t.Fatalf("widening disclosure does not name what acme/website's own list leaves out: %q", p.WideningDisclosure)
	}
	if strings.Contains(p.WideningDisclosure, "acme/api does not flag") {
		t.Fatalf("acme/api's list covers the new hive-wide list and must not be named as leaving anything out: %q", p.WideningDisclosure)
	}
	if !strings.Contains(strings.Join(p.Effects, "\n"), "Not changed by this write: acme/api, acme/website") {
		t.Fatalf("effects do not name the repositories with their own list: %v", p.Effects)
	}
}

// The per-repository lists are part of the preview's basis: one added after
// the preview refuses the confirm.
func TestSentinelSettingsOpRepoOverrideChangeMakesPreviewStale(t *testing.T) {
	provider := &surfaceProvider{governor: surfaceGovernorWithRepoOverrides()}
	h := surfaceHandler(provider)
	_, id := capPreview(t, h, WriteOpSentinelSettings, map[string]any{"exempt_logins": map[string]any{"add": []any{"renovate[bot]"}}})
	sn := provider.governor["security"].(map[string]any)["sentinel"].(map[string]any)
	sn["repoSensitivePaths"] = append(sn["repoSensitivePaths"].([]any), map[string]any{"repo": "acme/new", "sensitivePaths": []any{}})
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
		t.Fatalf("stale confirm err = %v, want ErrConfirmationStale", err)
	}
	if len(provider.requests) != 0 {
		t.Fatalf("a stale confirm wrote %+v", provider.requests)
	}
}
