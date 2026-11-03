package streamdeploy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// hivecommonsStreamConfig pins every file that configures the existing
// twitch.tv/hivecommons stream. The owner's decision on #15861 is that the
// hivecommonsfleet broadcast must not repurpose, interrupt or change that
// stream, so these bytes only move in a PR that intends to change it.
var hivecommonsStreamConfig = map[string]string{
	"docker-compose.yaml":                     "681ce70c760874b1838c752e5fd75a40d0a9eddae54d87b8f709c5b7a2f7bb5b",
	"k8s/configmap.yaml":                      "8aa4f2f18e3233097985fbd3b33d51a5530df60ca097d8af47ca5e1a6e6da9fc",
	"k8s/deployment.yaml":                     "e6e3d13d088f739db5b592b9a08e87bf917e1284212d6801b9b8deb96491df27",
	"k8s/kustomization.yaml":                  "235b0a5504ce4a9f81deb6238b2bea9d9e08e725cf78337ac0ed2b0b02c74dc7",
	"k8s/namespace.yaml":                      "8841b27156e855cd6906162ebde1b169a4a50aa5d40e2098c6f54ae3aad15490",
	"k8s/networkpolicy.yaml":                  "7bae3769413535474e6473f633e1e93c3f57bd084e5c0b0c49350480d53dd616",
	"k8s/secret.yaml":                         "0987b6d58ddfb25cf61396b05e30393f2c76dac4711629537c0f12c754241f03",
	"overlays/hive-hosted/kustomization.yaml": "3911263682ee64ebe137745e21afc49f42a7fbb62ef8fd99a13b40b7fab8fa0c",
}

func TestHivecommonsStreamConfigUnchanged(t *testing.T) {
	got := map[string]string{"docker-compose.yaml": ""}
	for _, dir := range []string{"k8s", "overlays/hive-hosted"} {
		entries, err := os.ReadDir(filepath.Join(StreamDir, dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			got[dir+"/"+e.Name()] = ""
		}
	}
	for rel := range got {
		b, err := os.ReadFile(filepath.Join(StreamDir, rel))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		got[rel] = hex.EncodeToString(sum[:])
	}
	for rel, want := range hivecommonsStreamConfig {
		switch g, ok := got[rel]; {
		case !ok:
			t.Errorf("%s is gone; the twitch.tv/hivecommons stream config must stay intact", rel)
		case g != want:
			t.Errorf("%s changed (sha256 %s, pinned %s); the twitch.tv/hivecommons stream config must stay byte-for-byte unchanged", rel, g, want)
		}
	}
	for rel := range got {
		if _, ok := hivecommonsStreamConfig[rel]; !ok {
			t.Errorf("%s was added to the twitch.tv/hivecommons stream config; put hivecommonsfleet files under fleet/", rel)
		}
	}
}

// decodeAll returns every YAML document in a manifest file.
func decodeAll(t *testing.T, rel string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(StreamDir, rel))
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var docs []map[string]any
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs
		}
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		docs = append(docs, doc)
	}
}

func decodeOne(t *testing.T, rel string) map[string]any {
	t.Helper()
	docs := decodeAll(t, rel)
	if len(docs) != 1 {
		t.Fatalf("%s: %d documents, want 1", rel, len(docs))
	}
	return docs[0]
}

// dig walks nested maps and lists; ints index lists.
func dig(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[k]
		case int:
			l, ok := v.([]any)
			if !ok || k >= len(l) {
				return nil
			}
			v = l[k]
		}
	}
	return v
}

// list is dig for a value that must be a YAML sequence.
func list(t *testing.T, v any, path ...any) []any {
	t.Helper()
	l, ok := dig(v, path...).([]any)
	if !ok {
		t.Fatalf("%v is not a list", path)
	}
	return l
}

func TestFleetDeploymentIsIndependentAndDisabled(t *testing.T) {
	kust := decodeOne(t, "fleet/kustomization.yaml")
	if ns := dig(kust, "namespace"); ns != "hive-stream-fleet" {
		t.Errorf("fleet kustomization namespace = %v, want hive-stream-fleet", ns)
	}
	for _, r := range list(t, kust, "resources") {
		res, _ := r.(string)
		if strings.Contains(res, "..") {
			t.Errorf("fleet kustomization pulls %q from outside fleet/; it must not share resources with the hivecommons stream", res)
		}
		if res == "secret.yaml" {
			t.Error("fleet kustomization applies secret.yaml; it is a template and the owner creates the Secret out of band")
		}
	}

	base := decodeOne(t, "k8s/deployment.yaml")
	dep := decodeOne(t, "fleet/deployment.yaml")
	for _, f := range []string{"name", "namespace"} {
		if dig(dep, "metadata", f) == dig(base, "metadata", f) {
			t.Errorf("fleet Deployment %s %v equals the hivecommons streamer's", f, dig(dep, "metadata", f))
		}
	}
	if r := dig(dep, "spec", "replicas"); r != 0 {
		t.Errorf("fleet Deployment replicas = %v, want 0 (disabled by default)", r)
	}
	containers := list(t, dep, "spec", "template", "spec", "containers")
	if len(containers) != 1 {
		t.Fatalf("fleet Deployment has %d containers, want 1", len(containers))
	}
	c := containers[0]
	if env := dig(c, "env"); env != nil {
		t.Errorf("fleet container sets inline env %v; configuration belongs in the ConfigMap, credentials in the Secret", env)
	}
	var refs []string
	for _, ef := range list(t, c, "envFrom") {
		if n, ok := dig(ef, "configMapRef", "name").(string); ok {
			refs = append(refs, "configmap/"+n)
		}
		if n, ok := dig(ef, "secretRef", "name").(string); ok {
			refs = append(refs, "secret/"+n)
			if dig(ef, "secretRef", "optional") == true {
				t.Errorf("secretRef %s is optional; a missing stream key must stop the pod", n)
			}
		}
	}
	sort.Strings(refs)
	if want := []string{"configmap/hive-stream-fleet", "secret/twitch-stream-hivecommonsfleet"}; strings.Join(refs, ",") != strings.Join(want, ",") {
		t.Errorf("fleet envFrom = %v, want exactly %v (no dashboard credential, no shared Secret)", refs, want)
	}

	cm := decodeOne(t, "fleet/configmap.yaml")
	data, _ := dig(cm, "data").(map[string]any)
	for k, want := range map[string]string{
		"STREAM_PROFILE":         "public-safe",
		"STREAM_ENABLED":         "false",
		"PUBLIC_SAFE_POLICY_ACK": "",
	} {
		if data[k] != want {
			t.Errorf("fleet ConfigMap %s = %v, want %q", k, data[k], want)
		}
	}
	for _, k := range []string{"DASHBOARD_TOKEN", "TWITCH_STREAM_KEY"} {
		if _, ok := data[k]; ok {
			t.Errorf("fleet ConfigMap carries %s; credentials never go in config", k)
		}
	}
	if u, _ := data["DASHBOARD_URL"].(string); !strings.HasPrefix(u, "https://") {
		t.Errorf("fleet DASHBOARD_URL = %q, want the hub's public https origin", u)
	}

	np := decodeOne(t, "fleet/networkpolicy.yaml")
	for i, rule := range list(t, np, "spec", "egress") {
		for _, to := range list(t, rule, "to") {
			ns := dig(to, "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name")
			if dig(to, "podSelector") != nil || (dig(to, "namespaceSelector") != nil && ns != "kube-system") {
				t.Errorf("fleet NetworkPolicy egress[%d] reaches in-cluster workloads (%v); the public origin is the only source", i, to)
			}
		}
	}
}

var streamKeyShape = regexp.MustCompile(`live_[A-Za-z0-9]{6,}`)

func TestFleetManifestsCarryNoCredential(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(StreamDir, "fleet"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(StreamDir, "fleet", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if streamKeyShape.Match(b) {
			t.Errorf("fleet/%s contains something shaped like a Twitch stream key", e.Name())
		}
	}
	for _, doc := range decodeAll(t, "fleet/secret.yaml") {
		sd, _ := dig(doc, "stringData").(map[string]any)
		for k, v := range sd {
			if v != "REPLACE-ME" {
				t.Errorf("fleet/secret.yaml %s holds a value; it is a template only", k)
			}
		}
	}
}

// runGate executes public-safe-gate.sh with exactly env (plus PATH).
func runGate(t *testing.T, env map[string]string) (int, string) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	cmd := exec.Command(bash, filepath.Join(StreamDir, "public-safe-gate.sh"))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &ee):
		return ee.ExitCode(), string(out)
	default:
		t.Fatal(err)
		return -1, ""
	}
}

func TestPublicSafeGateFailsClosed(t *testing.T) {
	const key = "live_000000000_sentinelsentinelsentinel"
	enabled := func(over map[string]string) map[string]string {
		env := map[string]string{
			"STREAM_PROFILE":         "public-safe",
			"STREAM_ENABLED":         "true",
			"PUBLIC_SAFE_POLICY_ACK": PublicSafePolicyVersion,
			"DASHBOARD_URL":          "https://hub.example",
			"DASHBOARD_PATH":         "/public-view?kiosk=1",
			"TWITCH_STREAM_KEY":      key,
		}
		for k, v := range over {
			if v == "<unset>" {
				delete(env, k)
			} else {
				env[k] = v
			}
		}
		return env
	}
	cases := []struct {
		name  string
		env   map[string]string
		start bool
	}{
		{"hivecommons stream (no profile) is untouched", map[string]string{"DASHBOARD_TOKEN": "x", "TWITCH_STREAM_KEY": key}, true},
		{"fully enabled", enabled(nil), true},
		{"local dry run to null needs no key", enabled(map[string]string{"TWITCH_STREAM_KEY": "<unset>", "STREAM_OUTPUT": "null"}), true},
		{"shipped fleet defaults", map[string]string{"STREAM_PROFILE": "public-safe", "STREAM_ENABLED": "false", "PUBLIC_SAFE_POLICY_ACK": "", "DASHBOARD_URL": "https://hive.hivecommons.dev", "DASHBOARD_PATH": "CHANGE-ME-public-safe-hub-view"}, false},
		{"unknown profile", enabled(map[string]string{"STREAM_PROFILE": "public"}), false},
		{"enable unset", enabled(map[string]string{"STREAM_ENABLED": "<unset>"}), false},
		{"enable not exactly true", enabled(map[string]string{"STREAM_ENABLED": "yes"}), false},
		{"policy not acknowledged", enabled(map[string]string{"PUBLIC_SAFE_POLICY_ACK": "<unset>"}), false},
		{"stale policy acknowledgement", enabled(map[string]string{"PUBLIC_SAFE_POLICY_ACK": "hivecommonsfleet-public-safe-v0"}), false},
		{"no stream key", enabled(map[string]string{"TWITCH_STREAM_KEY": "<unset>"}), false},
		{"dashboard credential present", enabled(map[string]string{"DASHBOARD_TOKEN": "ghp_x"}), false},
		{"in-cluster http source", enabled(map[string]string{"DASHBOARD_URL": "http://hive.ns.svc.cluster.local:3002"}), false},
		{"placeholder source path", enabled(map[string]string{"DASHBOARD_PATH": "CHANGE-ME-public-safe-hub-view"}), false},
		{"empty source path", enabled(map[string]string{"DASHBOARD_PATH": "<unset>"}), false},
		{"local dry run to a file needs no key", enabled(map[string]string{"TWITCH_STREAM_KEY": "<unset>", "STREAM_OUTPUT": "/run/stream/out/test.flv"}), true},
		{"raw output bypasses the key reference", enabled(map[string]string{"TWITCH_STREAM_KEY": "<unset>", "STREAM_OUTPUT": "rtmp://elsewhere/app/k"}), false},
		{"network sink disguised as a flv file", enabled(map[string]string{"TWITCH_STREAM_KEY": "<unset>", "STREAM_OUTPUT": "rtmp://elsewhere/app/k.flv"}), false},
		{"protocol prefix without slashes", enabled(map[string]string{"TWITCH_STREAM_KEY": "<unset>", "STREAM_OUTPUT": "tcp:host:1935/x.flv"}), false},
		{"relative flv path", enabled(map[string]string{"TWITCH_STREAM_KEY": "<unset>", "STREAM_OUTPUT": "out/test.flv"}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := runGate(t, tc.env)
			if tc.start && code != 0 {
				t.Errorf("gate refused (exit %d): %s", code, out)
			}
			if !tc.start && code == 0 {
				t.Errorf("gate let the encoder start: %s", out)
			}
			if strings.Contains(out, key) {
				t.Error("gate output contains the stream key")
			}
		})
	}
}

func TestPublicSafeWiring(t *testing.T) {
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(StreamDir, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if gate := read("public-safe-gate.sh"); !strings.Contains(gate, `PUBLIC_SAFE_POLICY_VERSION="`+PublicSafePolicyVersion+`"`) {
		t.Errorf("public-safe-gate.sh does not define policy version %s", PublicSafePolicyVersion)
	}
	if doc := read("../../docs/streaming-the-activity-monitor.md"); !strings.Contains(doc, PublicSafePolicyVersion) {
		t.Errorf("streaming docs do not name policy version %s", PublicSafePolicyVersion)
	}
	entry := read("entrypoint.sh")
	gateAt := strings.Index(entry, `/public-safe-gate.sh"`)
	xvfbAt := strings.Index(entry, `Xvfb "$DISPLAY_NUM"`)
	if gateAt < 0 || xvfbAt < 0 || gateAt > xvfbAt {
		t.Error("entrypoint.sh must run public-safe-gate.sh before starting Xvfb, Chromium or ffmpeg")
	}
	if df := read("Dockerfile"); !strings.Contains(df, "src/deploy/stream/public-safe-gate.sh") {
		t.Error("Dockerfile does not copy public-safe-gate.sh beside entrypoint.sh")
	}
}
