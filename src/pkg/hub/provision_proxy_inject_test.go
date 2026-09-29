package hub

import (
	"bytes"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// Tests for #9586 phase 1: newly provisioned hosted spokes are born with
// proxy-side GitHub credential injection ON (App spokes) or the explicit
// opt-out (PAT spokes, hub-level opt-out), and the rendered pod spec never
// carries a usable GitHub token into the hive container's environment.

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

// TestFreshHostedAppSpokePodSpecInjectsByDefault is the #9586 acceptance
// invariant on the rendered pod spec: a fresh App-authenticated hosted spoke
// starts with injection ON, holds no GitHub token in its container env, and the
// posture it is born with passes the spoke's own startup guard.
func TestFreshHostedAppSpokePodSpecInjectsByDefault(t *testing.T) {
	for _, tc := range []struct {
		name               string
		useApp, useAppFull bool
	}{
		{"app-with-inline-key", true, true},
		{"app-without-inline-key", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := renderProvisionManifest(t, tc.useApp, tc.useAppFull)
			plain, fromRef := hostedHiveContainerEnv(t, manifest)

			if got := plain[config.ProxyInjectGHAuthEnv]; got != config.ProxyInjectGHAuthOnValue {
				t.Fatalf("fresh hosted App spoke renders %s=%q, want %q", config.ProxyInjectGHAuthEnv, got, config.ProxyInjectGHAuthOnValue)
			}
			for _, name := range githubCredentialEnvNames {
				if _, ok := plain[name]; ok || fromRef[name] {
					t.Errorf("fresh hosted App spoke's container env carries %s - a GitHub token must not be delivered by env under injection", name)
				}
			}
			for name, value := range plain {
				if githubTokenShape.MatchString(value) {
					t.Errorf("container env %s holds a GitHub-token-shaped value", name)
				}
			}
			if err := config.ValidateProxyInjectGHAuth(func(k string) string { return plain[k] }); err != nil {
				t.Fatalf("the posture a fresh spoke is born with fails its own startup guard: %v", err)
			}
			if w := config.ProxyInjectGHAuthWarnings(func(k string) string { return plain[k] }); w != nil {
				t.Fatalf("the posture a fresh spoke is born with draws credential warnings: %v", w)
			}
		})
	}
}

// A PAT-authenticated hosted spoke has no App-minted per-agent tokens to
// inject, so it is born with the EXPLICIT opt-out - visible on the pod spec -
// rather than a switch that would strip every agent request and attach nothing.
func TestFreshHostedPATSpokePodSpecOptsOutExplicitly(t *testing.T) {
	manifest := renderProvisionManifestWithToken(t, false, false, "ghp_test")
	plain, _ := hostedHiveContainerEnv(t, manifest)
	if got := plain[config.ProxyInjectGHAuthEnv]; got != config.ProxyInjectGHAuthOffValue {
		t.Fatalf("PAT hosted spoke renders %s=%q, want the explicit opt-out %q", config.ProxyInjectGHAuthEnv, got, config.ProxyInjectGHAuthOffValue)
	}
	if err := config.ValidateProxyInjectGHAuth(func(k string) string { return plain[k] }); err != nil {
		t.Fatalf("PAT spoke posture fails the startup guard: %v", err)
	}
}

// Existing spokes are not flipped: a template data set without the key (every
// manifest rendered before #9586, and any render path that does not opt in)
// emits NO injection variable at all, so the spoke keeps today's unset = off.
func TestProvisionTemplateOmitsInjectionWhenUnset(t *testing.T) {
	manifest := renderProvisionManifest(t, true, true)
	if !strings.Contains(manifest, config.ProxyInjectGHAuthEnv) {
		t.Fatal("fixture is wrong: the default render must carry the injection variable")
	}
	stripped := renderManifestWithout(t, "ProxyInjectGHAuth")
	if strings.Contains(stripped, config.ProxyInjectGHAuthEnv) {
		t.Fatalf("template rendered %s without the ProxyInjectGHAuth data key", config.ProxyInjectGHAuthEnv)
	}
}

// renderManifestWithout renders the App manifest with one data key removed.
func renderManifestWithout(t *testing.T, key string) string {
	t.Helper()
	full := renderProvisionManifest(t, true, true)
	// Re-render through the real template with the key deleted, rather than
	// editing the rendered text.
	manifest := renderProvisionManifestData(t, func(data map[string]any) { delete(data, key) })
	if manifest == full {
		t.Fatalf("removing %q did not change the render", key)
	}
	return manifest
}

func TestProvisionProxyInjectGHAuth(t *testing.T) {
	for _, tc := range []struct {
		name   string
		useApp bool
		hubEnv string
		want   string
		warns  bool
	}{
		{name: "app spoke, hub default", useApp: true, hubEnv: "", want: config.ProxyInjectGHAuthOnValue},
		{name: "app spoke, hub explicit on", useApp: true, hubEnv: "true", want: config.ProxyInjectGHAuthOnValue},
		{name: "app spoke, hub opt-out", useApp: true, hubEnv: "false", want: config.ProxyInjectGHAuthOffValue},
		{name: "app spoke, hub opt-out padded", useApp: true, hubEnv: " false\n", want: config.ProxyInjectGHAuthOffValue},
		{name: "app spoke, unrecognized hub value keeps secure default", useApp: true, hubEnv: "0", want: config.ProxyInjectGHAuthOnValue, warns: true},
		{name: "pat spoke, hub default", useApp: false, hubEnv: "", want: config.ProxyInjectGHAuthOffValue},
		{name: "pat spoke, hub explicit on", useApp: false, hubEnv: "true", want: config.ProxyInjectGHAuthOffValue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logBuf, nil))
			env := map[string]string{hostedProxyInjectGHAuthEnv: tc.hubEnv}
			got := provisionProxyInjectGHAuth(tc.useApp, func(k string) string { return env[k] }, logger)
			if got != tc.want {
				t.Fatalf("provisionProxyInjectGHAuth(useApp=%v, %s=%q) = %q, want %q", tc.useApp, hostedProxyInjectGHAuthEnv, tc.hubEnv, got, tc.want)
			}
			if warned := strings.Contains(logBuf.String(), hostedProxyInjectGHAuthEnv); warned != tc.warns {
				t.Errorf("warned=%v, want %v (log: %s)", warned, tc.warns, logBuf.String())
			}
			// Whatever the hub renders must be a value the spoke boots with.
			if err := config.ValidateProxyInjectGHAuth(func(k string) string {
				if k == config.ProxyInjectGHAuthEnv {
					return got
				}
				return ""
			}); err != nil {
				t.Fatalf("rendered value %q fails the spoke startup guard: %v", got, err)
			}
			if w := config.ProxyInjectGHAuthWarnings(func(k string) string {
				if k == config.ProxyInjectGHAuthEnv {
					return got
				}
				return ""
			}); w != nil {
				t.Fatalf("rendered value %q draws credential warnings: %v", got, w)
			}
		})
	}
	// A nil logger (the test render helper) must not panic on the warn path.
	if got := provisionProxyInjectGHAuth(true, func(string) string { return "bogus" }, nil); got != config.ProxyInjectGHAuthOnValue {
		t.Fatalf("nil-logger unrecognized value = %q, want on", got)
	}
}

func TestProvisionProxyInjectGHAuthFromEnvReadsHubEnv(t *testing.T) {
	t.Setenv(hostedProxyInjectGHAuthEnv, config.ProxyInjectGHAuthOffValue)
	if got := provisionProxyInjectGHAuthFromEnv(true, nil); got != config.ProxyInjectGHAuthOffValue {
		t.Fatalf("hub opt-out via the process env = %q, want %q", got, config.ProxyInjectGHAuthOffValue)
	}
	t.Setenv(hostedProxyInjectGHAuthEnv, "")
	if got := provisionProxyInjectGHAuthFromEnv(true, nil); got != config.ProxyInjectGHAuthOnValue {
		t.Fatalf("hub default via the process env = %q, want %q", got, config.ProxyInjectGHAuthOnValue)
	}
}

// provisionHive builds its template data inline, so pin that the real
// provisioning path feeds the template from the helper above - a render test
// alone would pass even if provisionHive never set the key.
func TestProvisionHiveWiresProxyInjectGHAuth(t *testing.T) {
	src, err := os.ReadFile("saas_provision.go")
	if err != nil {
		t.Fatalf("read saas_provision.go: %v", err)
	}
	wire := regexp.MustCompile(`"ProxyInjectGHAuth":\s+provisionProxyInjectGHAuthFromEnv\(useApp, logger\)`)
	if !wire.Match(src) {
		t.Fatal("provisionHive no longer sets ProxyInjectGHAuth from provisionProxyInjectGHAuthFromEnv(useApp, logger) - fresh hosted spokes would lose default-on injection (#9586)")
	}
	if !strings.Contains(string(src), "value: \"{{.ProxyInjectGHAuth}}\"") {
		t.Fatal("k8sManifestTemplate no longer renders HIVE_PROXY_INJECT_GH_AUTH from .ProxyInjectGHAuth")
	}
}
