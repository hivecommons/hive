package hub

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/credsidecar"
)

// Tests for #9586 phase 2 deployment wiring: the hub renders the isolated
// credential sidecar only behind HIVE_HOSTED_CRED_SIDECAR=true (default OFF,
// new spokes included), only for spokes that can run it, and a toggle-off
// render is byte-identical to the phase-1 manifest.

const testCredSidecarKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func goodCredSidecarInputs() credSidecarInputs {
	return credSidecarInputs{
		appKeyInSecret: true,
		proxyInject:    config.ProxyInjectGHAuthOnValue,
		appID:          "3568013",
		installationID: "12345",
		hmacKey:        testCredSidecarKey,
	}
}

func TestProvisionCredSidecar_DefaultOffAndGated(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == hostedCredSidecarEnv {
				return v
			}
			return ""
		}
	}
	// Default OFF: unset, false, and anything unrecognized.
	for _, v := range []string{"", "false", "TRUE", "1", "yes"} {
		var logs bytes.Buffer
		if provisionCredSidecar(goodCredSidecarInputs(), env(v), slog.New(slog.NewTextHandler(&logs, nil))) {
			t.Fatalf("hub setting %q rendered the sidecar; only %q may", v, hostedCredSidecarOnValue)
		}
		if unrecognized := v != "" && v != "false"; unrecognized != strings.Contains(logs.String(), hostedCredSidecarEnv) {
			t.Fatalf("hub setting %q: warning logged=%v, want %v", v, !unrecognized, unrecognized)
		}
	}
	if !provisionCredSidecar(goodCredSidecarInputs(), env("true"), nil) {
		t.Fatal("switch on with a capable spoke did not render the sidecar")
	}
	for name, mutate := range map[string]func(*credSidecarInputs){
		"no inline App key": func(in *credSidecarInputs) { in.appKeyInSecret = false },
		"injection off":     func(in *credSidecarInputs) { in.proxyInject = config.ProxyInjectGHAuthOffValue },
		"placeholder app":   func(in *credSidecarInputs) { in.appID = "" },
		"placeholder inst":  func(in *credSidecarInputs) { in.installationID = "pending" },
		"scc lane":          func(in *credSidecarInputs) { in.requiresSCC = true },
		"no master secret":  func(in *credSidecarInputs) { in.hmacKey = "" },
	} {
		in := goodCredSidecarInputs()
		mutate(&in)
		var logs bytes.Buffer
		if provisionCredSidecar(in, env("true"), slog.New(slog.NewTextHandler(&logs, nil))) {
			t.Fatalf("%s: rendered a sidecar the spoke cannot run", name)
		}
		if !strings.Contains(logs.String(), "not rendered") {
			t.Fatalf("%s: refusal not logged:\n%s", name, logs.String())
		}
	}
}

func TestProvisionCredSidecarKey_PerHiveAndDomainSeparated(t *testing.T) {
	t.Setenv("HIVE_HUB_SECRET", "test-master-secret-cred-sidecar")
	a, b := provisionCredSidecarKey("hive-alpha"), provisionCredSidecarKey("hive-beta")
	if len(a) < credsidecar.MinKeyBytes || a == b {
		t.Fatalf("keys %q / %q: want >= %d bytes and per-hive", a, b, credsidecar.MinKeyBytes)
	}
	if a == provisionTerminalKey("hive-alpha") || a == provisionInviteKey("hive-alpha") {
		t.Fatal("the sidecar HMAC key collides with another per-hive key")
	}
	if again := provisionCredSidecarKey("hive-alpha"); again != a {
		t.Fatal("not deterministic across provisions")
	}
	t.Setenv("HIVE_HUB_SECRET", "")
	if got := provisionCredSidecarKey("hive-alpha"); got != "" {
		t.Fatalf("no master derived %q, want empty (the sidecar is then not rendered)", got)
	}
}

// The secret entry names must be the files the two containers read by default,
// or the sidecar and the proxy would each look for a file that is not there.
func TestCredSidecarSecretNamesMatchDefaultPaths(t *testing.T) {
	if filepath.Base(credsidecar.DefaultKeyFile) != credSidecarSecretKey || filepath.Dir(credsidecar.DefaultKeyFile) != "/secrets" {
		t.Fatalf("DefaultKeyFile %s does not match the projected secret entry %s under /secrets", credsidecar.DefaultKeyFile, credSidecarSecretKey)
	}
	if filepath.Base(credsidecar.DefaultAppKeyFile) != credSidecarAppKeySecretKey || filepath.Dir(credsidecar.DefaultAppKeyFile) != "/secrets" {
		t.Fatalf("DefaultAppKeyFile %s does not match the projected secret entry %s", credsidecar.DefaultAppKeyFile, credSidecarAppKeySecretKey)
	}
}

func TestCredSidecarGitHubHosts(t *testing.T) {
	if got := credSidecarGitHubHosts("https://GitHub.example.com", "https://github.example.com/api/v3", "", "::bad"); got != "github.example.com" {
		t.Fatalf("hosts = %q, want the deduplicated GHE host", got)
	}
	if got := credSidecarGitHubHosts(); got != "" {
		t.Fatalf("no URLs gave %q", got)
	}
}

func renderWithCredSidecar(t *testing.T, enabled bool, extra map[string]any) string {
	t.Helper()
	return renderProvisionManifestData(t, func(d map[string]any) {
		for k, v := range credSidecarTemplateData(enabled, testCredSidecarKey, "", "") {
			d[k] = v
		}
		for k, v := range extra {
			d[k] = v
		}
	})
}

// Toggle OFF (the default everywhere): the manifest is byte-identical to the
// phase-1 render, with no trace of the sidecar.
func TestCredSidecarOffRendersPhaseOneManifest(t *testing.T) {
	off := renderWithCredSidecar(t, false, nil)
	if base := renderProvisionManifestData(t, nil); off != base {
		t.Fatal("sidecar OFF changed the rendered manifest; existing behavior must be untouched")
	}
	for _, s := range []string{"cred-sidecar", "NetworkPolicy", "HIVE_CRED_SIDECAR", credSidecarSecretKey, testCredSidecarKey, "default-container"} {
		if strings.Contains(off, s) {
			t.Fatalf("sidecar OFF manifest mentions %q", s)
		}
	}
}

// Toggle ON: the pod gets the sidecar as containers[1] (the hub's reconcilers
// patch containers[0]), the hive container gets sidecar mode, the Secret gets
// the HMAC key, the sidecar mounts ONLY the two secret entries it needs, and a
// NetworkPolicy admits traffic only on the served ports.
func TestCredSidecarOnRendersSidecarAndPolicy(t *testing.T) {
	manifest := renderWithCredSidecar(t, true, nil)

	deploy := manifestObject(t, manifest, "Deployment", "hive")
	spec := manifestMap(t, deploy["spec"], "Deployment spec")
	tmpl := manifestMap(t, spec["template"], "template")
	meta := manifestMap(t, tmpl["metadata"], "template metadata")
	if ann := manifestMap(t, meta["annotations"], "annotations"); ann["kubectl.kubernetes.io/default-container"] != "hive" {
		t.Fatalf("default-container annotation = %v", ann)
	}
	podSpec := manifestMap(t, tmpl["spec"], "pod spec")
	containers := manifestSlice(t, podSpec["containers"], "containers")
	if len(containers) != 2 {
		t.Fatalf("pod has %d containers, want hive + cred-sidecar", len(containers))
	}
	if manifestMap(t, containers[0], "c0")["name"] != "hive" {
		t.Fatal("containers[0] is not hive; the NET_ADMIN and env reconcilers patch containers[0]")
	}
	sidecar := manifestMap(t, containers[1], "c1")
	if sidecar["name"] != "cred-sidecar" {
		t.Fatalf("containers[1] = %v", sidecar["name"])
	}
	cmd := manifestSlice(t, sidecar["command"], "command")
	if len(cmd) != 2 || cmd[1] != credsidecar.Subcommand {
		t.Fatalf("sidecar command = %v", cmd)
	}
	sc := manifestMap(t, sidecar["securityContext"], "sidecar securityContext")
	if sc["runAsUser"] != credSidecarUID || sc["allowPrivilegeEscalation"] != false || sc["readOnlyRootFilesystem"] != true {
		t.Fatalf("sidecar securityContext = %v", sc)
	}
	caps := manifestMap(t, sc["capabilities"], "caps")
	if drop := manifestSlice(t, caps["drop"], "drop"); len(drop) != 1 || drop[0] != "ALL" {
		t.Fatalf("sidecar capabilities = %v, want drop ALL", caps)
	}
	if _, ok := caps["add"]; ok {
		t.Fatal("sidecar adds capabilities")
	}

	sidecarEnv := map[string]string{}
	for _, e := range manifestSlice(t, sidecar["env"], "sidecar env") {
		entry := manifestMap(t, e, "env")
		v, _ := entry["value"].(string)
		sidecarEnv[entry["name"].(string)] = v
	}
	for k, want := range map[string]string{
		credsidecar.ListenEnv:         credsidecar.DefaultListenAddr,
		credsidecar.KeyFileEnv:        credsidecar.DefaultKeyFile,
		credsidecar.AppIDEnv:          "3568013",
		credsidecar.InstallationIDEnv: "12345",
		credsidecar.AppKeyFileEnv:     credsidecar.DefaultAppKeyFile,
	} {
		if sidecarEnv[k] != want {
			t.Fatalf("sidecar env %s = %q, want %q", k, sidecarEnv[k], want)
		}
	}
	if _, ok := sidecarEnv[credsidecar.ExtraHostsEnv]; ok {
		t.Fatal("github.com spoke renders a GHE host allowlist")
	}

	// The sidecar mounts only its two entries, never the whole Secret.
	var sidecarVolume map[string]any
	for _, v := range manifestSlice(t, podSpec["volumes"], "volumes") {
		vol := manifestMap(t, v, "volume")
		if vol["name"] == "cred-sidecar-secrets" {
			sidecarVolume = manifestMap(t, vol["secret"], "secret volume")
		}
	}
	if sidecarVolume == nil {
		t.Fatal("no cred-sidecar-secrets volume")
	}
	items := manifestSlice(t, sidecarVolume["items"], "items")
	keys := map[string]bool{}
	for _, it := range items {
		keys[manifestMap(t, it, "item")["key"].(string)] = true
	}
	if len(keys) != 2 || !keys[credSidecarSecretKey] || !keys[credSidecarAppKeySecretKey] {
		t.Fatalf("sidecar secret items = %v, want only the HMAC key and the App key", keys)
	}
	volIdx := strings.Index(manifest, "- name: cred-sidecar-secrets\n        secret:")
	if volIdx < 0 {
		t.Fatal("cred-sidecar-secrets volume block not found")
	}
	volBlock := manifest[volIdx:]
	if end := strings.Index(volBlock, "\n---"); end >= 0 {
		volBlock = volBlock[:end]
	}
	if !strings.Contains(volBlock, "defaultMode: 0440") {
		t.Fatalf("sidecar secret volume is not projected 0440 (the App key would be readable by agent UIDs):\n%s", volBlock)
	}
	mounts := manifestSlice(t, sidecar["volumeMounts"], "sidecar mounts")
	if len(mounts) != 1 || manifestMap(t, mounts[0], "mount")["name"] != "cred-sidecar-secrets" {
		t.Fatalf("sidecar mounts = %v, want only cred-sidecar-secrets (no /data, no config)", mounts)
	}

	// Hive container: sidecar mode on top of injection, and the rendered
	// posture passes the spoke's own boot guard.
	plain, _ := hostedHiveContainerEnv(t, manifest)
	if plain[credsidecar.URLEnv] != credsidecar.DefaultURL || plain[config.ProxyInjectGHAuthEnv] != config.ProxyInjectGHAuthOnValue {
		t.Fatalf("hive env: %s=%q %s=%q", credsidecar.URLEnv, plain[credsidecar.URLEnv], config.ProxyInjectGHAuthEnv, plain[config.ProxyInjectGHAuthEnv])
	}
	secret := manifestObject(t, manifest, "Secret", "hive-secrets")
	stringData := manifestMap(t, secret["stringData"], "stringData")
	key, _ := stringData[credSidecarSecretKey].(string)
	if key != testCredSidecarKey {
		t.Fatalf("Secret %s = %q", credSidecarSecretKey, key)
	}
	keyFile := filepath.Join(t.TempDir(), credSidecarSecretKey)
	if err := os.WriteFile(keyFile, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	guardEnv := map[string]string{}
	for k, v := range plain {
		guardEnv[k] = v
	}
	guardEnv[credsidecar.KeyFileEnv] = keyFile // the projected file, on this host
	if err := config.ValidateCredSidecar(func(k string) string { return guardEnv[k] }); err != nil {
		t.Fatalf("the rendered hive env would refuse to boot: %v", err)
	}
	if err := config.ValidateProxyInjectGHAuth(func(k string) string { return guardEnv[k] }); err != nil {
		t.Fatalf("the rendered hive env fails the injection guard: %v", err)
	}
	for _, name := range githubCredentialEnvNames {
		if _, ok := plain[name]; ok {
			t.Fatalf("hive container carries %s", name)
		}
	}

	np := manifestObject(t, manifest, "NetworkPolicy", "hive-cred-sidecar")
	npSpec := manifestMap(t, np["spec"], "policy spec")
	sel := manifestMap(t, manifestMap(t, npSpec["podSelector"], "podSelector")["matchLabels"], "matchLabels")
	if sel["app"] != "hive" || sel["hive-id"] != "test-hive" {
		t.Fatalf("NetworkPolicy selects %v", sel)
	}
	if types := manifestSlice(t, npSpec["policyTypes"], "policyTypes"); len(types) != 1 || types[0] != "Ingress" {
		t.Fatalf("policyTypes = %v", types)
	}
	ingress := manifestSlice(t, npSpec["ingress"], "ingress")
	ports := map[int]bool{}
	for _, rule := range ingress {
		for _, p := range manifestSlice(t, manifestMap(t, rule, "rule")["ports"], "ports") {
			ports[manifestMap(t, p, "port")["port"].(int)] = true
		}
	}
	if len(ports) != 2 || !ports[7681] || !ports[3002] {
		t.Fatalf("NetworkPolicy admits ports %v, want only terminal 7681 and dashboard 3002", ports)
	}
	if ports[18445] || ports[18443] {
		t.Fatal("NetworkPolicy admits the sidecar or proxy port")
	}
}

// A GHE hive's sidecar is told the GHE API URL and allowlists its host.
func TestCredSidecarOnGHE(t *testing.T) {
	manifest := renderProvisionManifestData(t, func(d map[string]any) {
		for k, v := range credSidecarTemplateData(true, testCredSidecarKey, "https://github.example.com", "https://github.example.com/api/v3") {
			d[k] = v
		}
		d["HasGHE"] = true
		d["GitHubBaseURL"] = "https://github.example.com"
		d["GitHubAPIURL"] = "https://github.example.com/api/v3"
	})
	for _, want := range []string{
		"name: " + credsidecar.GitHubAPIURLEnv + "\n          value: \"https://github.example.com/api/v3\"",
		"name: " + credsidecar.ExtraHostsEnv + "\n          value: \"github.example.com\"",
	} {
		if !strings.Contains(manifest, want) {
			t.Fatalf("GHE sidecar render missing %q", want)
		}
	}
}

// provisionHive must actually wire the switch and merge the template data
// (a source pin: provisionHive needs a live cluster to run end to end).
func TestProvisionHiveWiresCredSidecar(t *testing.T) {
	src, err := os.ReadFile("saas_provision.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"provisionCredSidecarFromEnv(credSidecarInputs{", "credSidecarTemplateData(credSidecarOn,"} {
		if !strings.Contains(string(src), want) {
			t.Fatalf("provisionHive no longer contains %q", want)
		}
	}
}
