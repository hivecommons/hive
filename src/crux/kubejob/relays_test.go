package kubejob

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/sandbox"
)

const (
	testSelfPod      = "hive-7d9f-abcde"
	testHiveDigest   = "ghcr.io/hivecommons/hive@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	vanillaSelfPod   = `{"metadata":{"annotations":{}},"spec":{"containers":[{"name":"hive","image":"ghcr.io/hivecommons/hive:stable"}],"imagePullSecrets":[{"name":"ghcr-pull"}]},"status":{"containerStatuses":[{"name":"sidecar","image":"other:1","imageID":"other@sha256:aa"},{"name":"hive","image":"ghcr.io/hivecommons/hive:stable","imageID":"docker-pullable://` + testHiveDigest + `"}]}}`
	openshiftSelfPod = `{"metadata":{"annotations":{"openshift.io/scc":"restricted-v2"}},"spec":{"containers":[{"name":"hive","image":"ghcr.io/hivecommons/hive:stable"}]},"status":{"containerStatuses":[{"name":"hive","image":"ghcr.io/hivecommons/hive:stable","imageID":"` + testHiveDigest + `"}]}}`
)

func relaySpec() sandbox.LaunchSpec {
	spec := specFor("/data/agents/sandbox/reviewer-1")
	spec.Relays = true
	return spec
}

// runRelayJob runs one relay kick against the fake API and returns the Job
// manifest's pod spec.
func runRelayJob(t *testing.T, api *fakeAPI, opts Options) map[string]any {
	t.Helper()
	api.statuses = []string{statusComplete}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	if _, err := newLauncher(srv, opts).Run(context.Background(), relaySpec()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return podSpecOf(t, api.created)
}

func podSpecOf(t *testing.T, job map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(job)
	var m struct {
		Spec struct {
			Template struct {
				Spec map[string]any `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m.Spec.Template.Spec
}

func asJSON(v any) string { raw, _ := json.Marshal(v); return string(raw) }

func TestRelayJobSpecOnVanillaCluster(t *testing.T) {
	api := newFakeAPI()
	api.selfPod = vanillaSelfPod
	pod := runRelayJob(t, api, Options{})

	inits, _ := pod["initContainers"].([]any)
	if len(inits) != 1 {
		t.Fatalf("initContainers = %s, want one relay init container", asJSON(pod["initContainers"]))
	}
	initC := asJSON(inits[0])
	for _, want := range []string{
		`"name":"hive-relays"`,
		`"image":"` + testHiveDigest + `"`, // the running digest, not the :stable tag
		`"imagePullPolicy":"IfNotPresent"`,
		`"command":["cp","/usr/local/bin/hive-review","/usr/local/bin/hive-open-issue","/usr/local/bin/hive-topic","/opt/hive/relays/"]`,
		`"runAsNonRoot":true`,
		`"readOnlyRootFilesystem":true`,
		`"allowPrivilegeEscalation":false`,
		`"capabilities":{"drop":["ALL"]}`,
		`"seccompProfile":{"type":"RuntimeDefault"}`,
		`"runAsUser":65534`,
		`"mountPath":"/opt/hive/relays","name":"hive-relays"`,
	} {
		if !strings.Contains(initC, want) {
			t.Errorf("relay init container missing %s\n%s", want, initC)
		}
	}
	if strings.Contains(initC, `"privileged"`) || strings.Contains(initC, `"add"`) {
		t.Errorf("relay init container must not ask for privileges: %s", initC)
	}

	vols := asJSON(pod["volumes"])
	if !strings.Contains(vols, `"emptyDir":{"sizeLimit":"16Mi"},"name":"hive-relays"`) {
		t.Errorf("volumes missing the relay emptyDir: %s", vols)
	}
	if got := asJSON(pod["imagePullSecrets"]); got != `[{"name":"ghcr-pull"}]` {
		t.Errorf("imagePullSecrets = %s, want hive's own pull secret for the hive image", got)
	}

	containers, _ := pod["containers"].([]any)
	kick := asJSON(containers[0])
	if !strings.Contains(kick, `"mountPath":"/opt/hive/relays","name":"hive-relays","readOnly":true`) {
		t.Errorf("kick container must mount the relays read-only: %s", kick)
	}
	var c struct {
		Command []string `json:"command"`
	}
	_ = json.Unmarshal([]byte(kick), &c)
	want := append([]string(nil), sandbox.RelayCommand(relaySpec().Command)...)
	if strings.Join(c.Command, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("kick command = %q, want the relay preflight wrapper %q", c.Command, want)
	}
	if c.Command[len(c.Command)-1] != "@.hive/kick-prompt.txt" {
		t.Errorf("the agent command must still run last: %q", c.Command)
	}
}

func TestRelayJobOnOpenShiftLeavesUIDToTheSCC(t *testing.T) {
	api := newFakeAPI()
	api.selfPod = openshiftSelfPod
	pod := runRelayJob(t, api, Options{})
	initC := asJSON(pod["initContainers"])
	if strings.Contains(initC, "runAsUser") {
		t.Errorf("on OpenShift the SCC assigns the uid; a fixed one is rejected: %s", initC)
	}
	if !strings.Contains(initC, `"runAsNonRoot":true`) || !strings.Contains(initC, `"image":"`+testHiveDigest+`"`) {
		t.Errorf("relay init container = %s", initC)
	}
	if _, ok := pod["imagePullSecrets"]; ok {
		t.Errorf("no pull secrets on hive's pod means none on the Job: %s", asJSON(pod["imagePullSecrets"]))
	}
}

func TestRelayJobConfiguredImageAndUIDSkipThePodRead(t *testing.T) {
	api := newFakeAPI()
	uid := int64(1000680000)
	pod := runRelayJob(t, api, Options{RelayImage: "registry.internal/hive@sha256:beef", RelayRunAsUser: &uid})
	if api.selfPodGets != 0 {
		t.Errorf("hive's pod was read %d times; both values were configured", api.selfPodGets)
	}
	initC := asJSON(pod["initContainers"])
	if !strings.Contains(initC, `"image":"registry.internal/hive@sha256:beef"`) || !strings.Contains(initC, `"runAsUser":1000680000`) {
		t.Errorf("relay init container = %s, want the configured image and uid", initC)
	}
}

func TestRelayJobConfiguredImageStillDetectsOpenShift(t *testing.T) {
	api := newFakeAPI()
	api.selfPod = openshiftSelfPod
	pod := runRelayJob(t, api, Options{RelayImage: "registry.internal/hive:v5"})
	initC := asJSON(pod["initContainers"])
	if !strings.Contains(initC, `"image":"registry.internal/hive:v5"`) || strings.Contains(initC, "runAsUser") {
		t.Errorf("relay init container = %s", initC)
	}
	if _, ok := pod["imagePullSecrets"]; ok {
		t.Error("a configured relay image does not borrow hive's pull secrets")
	}
}

func TestRelayJobNoneSkipsRelays(t *testing.T) {
	api := newFakeAPI()
	pod := runRelayJob(t, api, Options{RelayImage: RelayImageNone})
	if _, ok := pod["initContainers"]; ok || strings.Contains(asJSON(pod), "hive-relays") {
		t.Errorf("relay_image: none must add nothing: %s", asJSON(pod))
	}
	if api.selfPodGets != 0 {
		t.Error("relay_image: none must not read hive's pod")
	}
}

func TestJobWithoutRelaysIsUnchanged(t *testing.T) {
	api := newFakeAPI()
	api.statuses = []string{statusComplete}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	if _, err := newLauncher(srv, Options{}).Run(context.Background(), specFor("/data/agents/sandbox/deps-1")); err != nil {
		t.Fatal(err)
	}
	if s := asJSON(api.created); strings.Contains(s, "initContainers") || strings.Contains(s, "hive-relays") {
		t.Errorf("a launch without Relays must not get relays: %s", s)
	}
}

func TestRelayJobFailsClearlyWhenHivePodUnreadable(t *testing.T) {
	api := newFakeAPI() // selfPod "" → 404
	api.statuses = []string{statusComplete}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	_, err := newLauncher(srv, Options{}).Run(context.Background(), relaySpec())
	if err == nil || !strings.Contains(err.Error(), "relay_image") || !strings.Contains(err.Error(), "hive-review") {
		t.Fatalf("Run error = %v, want a message naming hive-review and job.relay_image", err)
	}
	if api.created != nil {
		t.Error("no Job may be created when the relays cannot be resolved")
	}
}

func TestRelayJobFailsWhenHivePodHasNoImage(t *testing.T) {
	api := newFakeAPI()
	api.selfPod = `{"metadata":{},"spec":{},"status":{}}`
	api.statuses = []string{statusComplete}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	_, err := newLauncher(srv, Options{}).Run(context.Background(), relaySpec())
	if err == nil || !strings.Contains(err.Error(), "no image") {
		t.Fatalf("Run error = %v, want the no-image message", err)
	}
}

func TestRelayJobBadPodJSON(t *testing.T) {
	api := newFakeAPI()
	api.selfPod = `{not json`
	api.statuses = []string{statusComplete}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	_, err := newLauncher(srv, Options{}).Run(context.Background(), relaySpec())
	if err == nil || !strings.Contains(err.Error(), "decoding pod") {
		t.Fatalf("Run error = %v, want a decode error", err)
	}
}

func TestValidateRejectsRootRelayUID(t *testing.T) {
	for _, uid := range []int64{0, -1} {
		u := uid
		if err := (Options{WorkspaceClaim: "c", RelayRunAsUser: &u}).Validate(); err == nil || !strings.Contains(err.Error(), "relay_run_as_user") {
			t.Errorf("uid %d: Validate = %v, want a relay_run_as_user error", uid, err)
		}
	}
}

func TestHiveImageSelection(t *testing.T) {
	cases := []struct {
		name, pod, want string
	}{
		{"digest from imageID", vanillaSelfPod, testHiveDigest},
		{"bare image ID falls back to status image", `{"status":{"containerStatuses":[{"name":"hive","image":"ghcr.io/x:v1","imageID":"sha256:abc"}]}}`, "ghcr.io/x:v1"},
		{"status image that is only an ID falls back to spec", `{"spec":{"containers":[{"name":"hive","image":"ghcr.io/x:spec"}]},"status":{"containerStatuses":[{"name":"hive","image":"sha256:abc","imageID":""}]}}`, "ghcr.io/x:spec"},
		{"no hive container: first status", `{"status":{"containerStatuses":[{"name":"main","imageID":"containerd://reg/x@sha256:ff"}]}}`, "reg/x@sha256:ff"},
		{"no status: first spec container", `{"spec":{"containers":[{"name":"main","image":"reg/x:1"}]}}`, "reg/x:1"},
		{"nothing", `{}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p selfPod
			if err := json.Unmarshal([]byte(tc.pod), &p); err != nil {
				t.Fatal(err)
			}
			if got := p.hiveImage(); got != tc.want {
				t.Errorf("hiveImage = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPullableDigest(t *testing.T) {
	for in, want := range map[string]string{
		"docker-pullable://reg/x@sha256:ab": "reg/x@sha256:ab",
		"reg/x@sha256:ab":                   "reg/x@sha256:ab",
		"sha256:ab":                         "",
		"@sha256:ab":                        "",
		"reg/x@md5:ab":                      "",
		"":                                  "",
	} {
		if got := pullableDigest(in); got != want {
			t.Errorf("pullableDigest(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPodNameFallbacks(t *testing.T) {
	t.Setenv("POD_NAME", "")
	t.Setenv("HOSTNAME", "")
	if got, err := (&Launcher{PodName: "explicit"}).podName(); err != nil || got != "explicit" {
		t.Errorf("podName = %q, %v", got, err)
	}
	t.Setenv("HOSTNAME", "from-hostname")
	if got, _ := (&Launcher{}).podName(); got != "from-hostname" {
		t.Errorf("podName = %q, want $HOSTNAME", got)
	}
	t.Setenv("POD_NAME", "from-downward-api")
	if got, _ := (&Launcher{}).podName(); got != "from-downward-api" {
		t.Errorf("podName = %q, want $POD_NAME first", got)
	}
	t.Setenv("POD_NAME", "")
	t.Setenv("HOSTNAME", "")
	if got, err := (&Launcher{}).podName(); err != nil || got == "" {
		t.Errorf("podName = %q, %v, want os.Hostname", got, err)
	}
}

func TestRelayJobPodReadUsesNamespaceAndName(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		_, _ = w.Write([]byte(vanillaSelfPod))
	}))
	defer srv.Close()
	l := &Launcher{APIServer: srv.URL, HTTPClient: srv.Client(), PodName: "hive-0", TokenPath: "/nonexistent"}
	plan, err := l.resolveRelays(context.Background(), "tenant")
	if err != nil || plan == nil {
		t.Fatalf("resolveRelays = %v, %v", plan, err)
	}
	if seen != "/api/v1/namespaces/tenant/pods/hive-0" {
		t.Errorf("read %s, want hive's own pod in its namespace", seen)
	}
}
