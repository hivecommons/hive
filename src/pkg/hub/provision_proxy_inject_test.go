package hub

import (
	"regexp"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// Tests for #9586: proxy-side GitHub credential injection is OPT-IN only. The
// hub renders NO HIVE_PROXY_INJECT_GH_AUTH onto newly provisioned hosted
// spokes (App or PAT), so a new spoke starts with injection off exactly like
// every other hive; the brief default-on from #9597 is reverted.

// hostedHiveContainerEnv returns the rendered hive container's env as
// name -> plain value, plus the set of names sourced via valueFrom (secret or
// downward API), whose values are not visible in the manifest.
func hostedHiveContainerEnv(t *testing.T, manifest string) (plain map[string]string, fromRef map[string]bool) {
	t.Helper()
	deploy := manifestObject(t, manifest, "Deployment", "hive")
	spec := manifestMap(t, deploy["spec"], "Deployment spec")
	tmpl := manifestMap(t, spec["template"], "Deployment template")
	podSpec := manifestMap(t, tmpl["spec"], "Pod spec")
	plain, fromRef = map[string]string{}, map[string]bool{}
	found := false
	for _, c := range manifestSlice(t, podSpec["containers"], "Pod containers") {
		container := manifestMap(t, c, "container")
		if container["name"] != "hive" {
			continue
		}
		found = true
		for _, e := range manifestSlice(t, container["env"], "hive container env") {
			entry := manifestMap(t, e, "env entry")
			name, _ := entry["name"].(string)
			if _, ok := entry["valueFrom"]; ok {
				fromRef[name] = true
				continue
			}
			value, _ := entry["value"].(string)
			plain[name] = value
		}
	}
	if !found {
		t.Fatal("rendered Deployment has no container named hive")
	}
	return plain, fromRef
}

// githubCredentialEnvNames are the variables through which a GitHub token
// could reach the hive process (and, absent the tmux/scrub removals, an agent).
var githubCredentialEnvNames = []string{"HIVE_GITHUB_TOKEN", "GITHUB_TOKEN", "GH_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// githubTokenShape matches the documented GitHub token prefixes (classic and
// fine-grained PAT, OAuth, user-to-server, installation, refresh).
var githubTokenShape = regexp.MustCompile(`\b(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]|\bgithub_pat_`)

// hostedHubInjectSwitchEnv is the hub-wide switch #9597 added and this revert
// removed; the hub must not read or render it.
const hostedHubInjectSwitchEnv = "HIVE_HOSTED_PROXY_INJECT_GH_AUTH"

// TestFreshHostedSpokePodSpecRendersNoInjectionEnv: a fresh hosted spoke's
// rendered pod spec carries no injection variable at all - App or PAT, inline
// key or not - so the spoke resolves injection OFF (opt-in) at boot. The App
// spoke's container env also still carries no GitHub token.
func TestFreshHostedSpokePodSpecRendersNoInjectionEnv(t *testing.T) {
	for _, tc := range []struct {
		name               string
		useApp, useAppFull bool
	}{
		{"app-with-inline-key", true, true},
		{"app-without-inline-key", true, false},
		{"pat", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := renderProvisionManifest(t, tc.useApp, tc.useAppFull)
			plain, fromRef := hostedHiveContainerEnv(t, manifest)

			if v, ok := plain[config.ProxyInjectGHAuthEnv]; ok || fromRef[config.ProxyInjectGHAuthEnv] {
				t.Fatalf("fresh hosted spoke renders %s=%q; injection must stay opt-in (no value rendered)", config.ProxyInjectGHAuthEnv, v)
			}
			if d := config.ResolveProxyInjectGHAuth(func(k string) string { return plain[k] }); d.Enabled {
				t.Fatalf("the posture a fresh spoke is born with resolves injection ON: %+v", d)
			}
			if err := config.ValidateProxyInjectGHAuth(func(k string) string { return plain[k] }); err != nil {
				t.Fatalf("the posture a fresh spoke is born with fails its own startup guard: %v", err)
			}
			if !tc.useApp {
				return
			}
			for _, name := range githubCredentialEnvNames {
				if _, ok := plain[name]; ok || fromRef[name] {
					t.Errorf("fresh hosted App spoke's container env carries %s", name)
				}
			}
			for name, value := range plain {
				if githubTokenShape.MatchString(value) {
					t.Errorf("container env %s holds a GitHub-token-shaped value", name)
				}
			}
		})
	}
}

// TestProvisionTemplateHasNoInjectionSwitch: neither the spoke variable nor
// the reverted hub-wide switch appears anywhere in the provisioning template,
// so no hub setting can turn injection on for new spokes.
func TestProvisionTemplateHasNoInjectionSwitch(t *testing.T) {
	for _, name := range []string{config.ProxyInjectGHAuthEnv, hostedHubInjectSwitchEnv, "ProxyInjectGHAuth"} {
		if strings.Contains(k8sManifestTemplate, name) {
			t.Errorf("provisioning template references %s; injection must stay opt-in", name)
		}
	}
}
