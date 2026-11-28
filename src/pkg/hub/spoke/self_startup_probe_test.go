package spoke

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSelfUpgradeBackfillsStartupProbe(t *testing.T) {
	for _, operation := range []string{"image", "restart", "mutable", "same-image"} {
		for _, probe := range []struct {
			name, json string
			want       bool
		}{
			{"stale", `,"startupProbe":{"httpGet":{"path":"/api/health","port":3002},"failureThreshold":30,"periodSeconds":5}`, true},
			{"current", `,"startupProbe":{"failureThreshold":118}`, false},
			{"larger", `,"startupProbe":{"failureThreshold":200}`, false},
			// No probe at all: install the template's full probe. The port
			// follows the existing readiness probe when one is present.
			{"absent", ``, true},
			{"absent-with-readiness", `,"readinessProbe":{"httpGet":{"path":"/api/health","port":4100}}`, true},
			{"absent-with-liveness", `,"livenessProbe":{"httpGet":{"path":"/api/health","port":4200}}`, true},
			// A named port (Kubernetes IntOrString) must decode and be kept
			// as a name, so the new probe targets the same container port
			// (#18479: "dashboard" used to abort the whole upgrade).
			{"absent-with-named-port", `,"readinessProbe":{"httpGet":{"path":"/api/health","port":"dashboard"}},"livenessProbe":{"httpGet":{"path":"/api/health","port":"dashboard"}}`, true},
			{"absent-with-null-port", `,"readinessProbe":{"httpGet":{"path":"/api/health","port":null}}`, true},
			// A port that is neither a number nor a name skips the backfill,
			// but the upgrade patch itself must still apply.
			{"malformed-port", `,"readinessProbe":{"httpGet":{"path":"/api/health","port":{"bad":true}}}`, false},
		} {
			t.Run(operation+"/"+probe.name, func(t *testing.T) {
				probeJSON := probe.json
				var patched []byte
				patches := 0
				gets := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						gets++
						fmt.Fprintf(w, `{"spec":{"template":{"spec":{"containers":[{"name":"hive","image":"ghcr.io/hivecommons/hive:stable"%s},{"name":"sidecar","image":"ghcr.io/hivecommons/hive:stable","startupProbe":{"failureThreshold":30}}],"initContainers":[{"name":"init","image":"ghcr.io/hivecommons/hive:stable"}]}}}}`, probeJSON)
						return
					}
					if r.Method != http.MethodPatch {
						t.Errorf("unexpected method: %s", r.Method)
					}
					if r.Header.Get("Content-Type") != "application/strategic-merge-patch+json" {
						t.Error("expected strategic merge")
					}
					patches++
					probeJSON = `,"startupProbe":{"failureThreshold":118}`
					patched, _ = io.ReadAll(r.Body)
					w.WriteHeader(http.StatusOK)
				}))
				defer srv.Close()
				withFakeK8sAPI(t, srv)
				var err error
				switch operation {
				case "same-image":
					for i := 0; i < 2; i++ {
						err = SwitchImageSelf(slog.Default(), "ghcr.io/hivecommons/hive:stable")
						if err != nil {
							t.Fatal(err)
						}
					}
				case "image":
					err = SwitchImageSelf(slog.Default(), "ghcr.io/hivecommons/hive:candidate")
				case "restart":
					err = RolloutRestartSelf(slog.Default())
				case "mutable":
					_, err = upgradeSelfMutableToSHA(slog.Default(), "ghcr.io/hivecommons/hive:dev-latest", "abcdef1")
				}
				if err != nil {
					t.Fatal(err)
				}
				wantGets := 1
				if operation == "same-image" {
					wantGets = 2
				}
				if gets != wantGets {
					t.Fatalf("got %d Deployment GETs; want %d (one per call)", gets, wantGets)
				}
				wantPatches := 1
				if operation == "same-image" && !probe.want {
					wantPatches = 0
				}
				if patches != wantPatches {
					t.Fatalf("got %d patches; want %d", patches, wantPatches)
				}
				if patches == 0 {
					return
				}
				var patch struct {
					Spec struct {
						Template struct {
							Metadata struct {
								Annotations map[string]string `json:"annotations"`
							} `json:"metadata"`
							Spec struct {
								Containers []struct {
									Name  string         `json:"name"`
									Image string         `json:"image"`
									Probe map[string]any `json:"startupProbe"`
								} `json:"containers"`
							} `json:"spec"`
						} `json:"template"`
					} `json:"spec"`
				}
				if err := json.Unmarshal(patched, &patch); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, c := range patch.Spec.Template.Spec.Containers {
					if c.Name == "hive" {
						found = c.Probe != nil
						if found && c.Probe["failureThreshold"] != float64(118) {
							t.Errorf("unexpected probe patch: %v", c.Probe)
						}
						if found && strings.HasPrefix(probe.name, "absent") {
							var wantPort any = float64(3002)
							switch probe.name {
							case "absent-with-readiness":
								wantPort = float64(4100)
							case "absent-with-liveness":
								wantPort = float64(4200)
							case "absent-with-named-port":
								wantPort = "dashboard"
							}
							hg, _ := c.Probe["httpGet"].(map[string]any)
							if hg == nil || hg["path"] != "/api/health" || hg["port"] != wantPort ||
								c.Probe["periodSeconds"] != float64(5) || c.Probe["initialDelaySeconds"] != float64(10) {
								t.Errorf("missing probe must be installed in full (port %v): %v", wantPort, c.Probe)
							}
						} else if found && len(c.Probe) != 1 {
							t.Errorf("existing probe must only have its threshold raised: %v", c.Probe)
						}
						if operation == "image" && c.Image != "ghcr.io/hivecommons/hive:candidate" {
							t.Error("lost image update")
						}
						if operation == "same-image" && c.Image != "" {
							t.Error("probe-only backfill must not change images")
						}
					} else if c.Probe != nil {
						t.Errorf("modified sidecar probe: %v", c.Probe)
					}
				}
				if found != probe.want {
					t.Errorf("probe backfill = %v, want %v; patch %s", found, probe.want, patched)
				}
				if operation == "same-image" {
					if len(patch.Spec.Template.Metadata.Annotations) != 0 {
						t.Error("probe-only backfill must not re-stamp restart annotation")
					}
				} else if patch.Spec.Template.Metadata.Annotations["hive.hivecommons.dev/restart-at"] == "" {
					t.Error("lost restart annotation")
				}
				if operation == "mutable" && !strings.Contains(string(patched), "abcdef1") {
					t.Error("lost target SHA")
				}
			})
		}
	}
}

func TestUpgradeProbeReadFailureDoesNotPatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected write after failed probe read: %s", r.Method)
		}
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "denied")
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)
	if err := RolloutRestartSelf(slog.Default()); err == nil {
		t.Fatal("expected read error")
	}
}

// A Deployment whose probes cannot be decoded must not abort the upgrade: the
// backfill is skipped with a warning and the caller's patch still applies
// unchanged (#18479).
func TestUpgradeProbeDecodeFailureStillPatches(t *testing.T) {
	for _, current := range []struct{ name, body string }{
		{"malformed-document", "{"},
		{"malformed-port", `{"spec":{"template":{"spec":{"containers":[{"name":"hive","livenessProbe":{"httpGet":{"port":true}}}]}}}}`},
		{"fractional-port", `{"spec":{"template":{"spec":{"containers":[{"name":"hive","livenessProbe":{"httpGet":{"port":3002.5}}}]}}}}`},
	} {
		t.Run(current.name, func(t *testing.T) {
			var patched []byte
			patches := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPatch {
					patches++
					patched, _ = io.ReadAll(r.Body)
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			withFakeK8sAPI(t, srv)
			body := []byte(`{"spec":{"template":{"metadata":{"annotations":{"hive.hivecommons.dev/restart-at":"now"}}}}}`)
			if err := k8sUpgradePatchWithDeployment("/apis/apps/v1/namespaces/hive/deployments/hive", body, []byte(current.body)); err != nil {
				t.Fatalf("decode failure aborted the upgrade: %v", err)
			}
			if patches != 1 || string(patched) != string(body) {
				t.Fatalf("got %d patches %s; want the caller's patch unchanged", patches, patched)
			}
			// A probe-only backfill with nothing decodable has nothing to send.
			patches = 0
			if err := k8sUpgradePatchWithDeployment("/apis/apps/v1/namespaces/hive/deployments/hive", nil, []byte(current.body)); err != nil {
				t.Fatalf("probe-only backfill returned %v", err)
			}
			if patches != 0 {
				t.Fatalf("probe-only backfill patched %d times after a decode failure", patches)
			}
		})
	}
}

func TestProbePortUnmarshal(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    probePort
		wantErr bool
	}{
		{`3002`, probePort{Num: 3002}, false},
		{`"dashboard"`, probePort{Name: "dashboard"}, false},
		{`null`, probePort{}, false},
		{` 8080 `, probePort{Num: 8080}, false},
		{`true`, probePort{}, true},
		{`{}`, probePort{}, true},
		{`[1]`, probePort{}, true},
		{`1.5`, probePort{}, true},
		{`"unterminated`, probePort{}, true},
	} {
		var p probePort
		err := p.UnmarshalJSON([]byte(tc.in))
		if (err != nil) != tc.wantErr {
			t.Errorf("UnmarshalJSON(%s) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && p != tc.want {
			t.Errorf("UnmarshalJSON(%s) = %+v, want %+v", tc.in, p, tc.want)
		}
	}
}

func TestStartupProbePort(t *testing.T) {
	handler := func(port probePort) *probeHandler {
		h := &probeHandler{}
		h.HTTPGet = &struct {
			Port probePort `json:"port"`
		}{Port: port}
		return h
	}
	for _, tc := range []struct {
		name   string
		probes []*probeHandler
		want   any
	}{
		{"none", nil, HostedDashboardPort},
		{"nil-probes", []*probeHandler{nil, nil}, HostedDashboardPort},
		{"no-httpget", []*probeHandler{{}}, HostedDashboardPort},
		{"numeric", []*probeHandler{handler(probePort{Num: 4100})}, int64(4100)},
		{"named", []*probeHandler{handler(probePort{Name: "dashboard"})}, "dashboard"},
		{"zero-falls-through", []*probeHandler{handler(probePort{}), handler(probePort{Num: 4200})}, int64(4200)},
		{"negative-default", []*probeHandler{handler(probePort{Num: -1})}, HostedDashboardPort},
	} {
		if got := startupProbePort(tc.probes...); got != tc.want {
			t.Errorf("%s: startupProbePort = %v (%T), want %v (%T)", tc.name, got, got, tc.want, tc.want)
		}
	}
}

// #20162: the self-upgrade patch backfills fsGroupChangePolicy onto a spoke
// provisioned before the template set it, in the same template update as the
// upgrade, and leaves the SCC lane (no fsGroup) and an explicit policy alone.
func TestBackfillFsGroupChangePolicy(t *testing.T) {
	dep := func(sc string) []byte {
		return []byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive"}]` + sc + `}}}}`)
	}
	image := []byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive","image":"new"}]}}}}`)
	restart := []byte(`{"spec":{"template":{"metadata":{"annotations":{"a":"b"}}}}}`)
	for _, tc := range []struct {
		name string
		body []byte
		sc   string
		want bool
	}{
		{"image/fsgroup-no-policy", image, `,"securityContext":{"fsGroup":1002}`, true},
		{"restart/fsgroup-no-policy", restart, `,"securityContext":{"fsGroup":1002}`, true},
		{"nil/fsgroup-no-policy", nil, `,"securityContext":{"fsGroup":1002}`, true},
		{"image/already-set", image, `,"securityContext":{"fsGroup":1002,"fsGroupChangePolicy":"OnRootMismatch"}`, false},
		{"image/explicit-always", image, `,"securityContext":{"fsGroup":1002,"fsGroupChangePolicy":"Always"}`, false},
		{"image/no-fsgroup", image, `,"securityContext":{"runAsNonRoot":true}`, false},
		{"nil/no-security-context", nil, ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := backfillFsGroupChangePolicy(tc.body, dep(tc.sc))
			if err != nil {
				t.Fatal(err)
			}
			if !tc.want {
				if string(got) != string(tc.body) {
					t.Fatalf("patch changed without a backfill: %s", got)
				}
				return
			}
			var p struct {
				Spec struct {
					Template struct {
						Metadata json.RawMessage `json:"metadata"`
						Spec     struct {
							Containers      []map[string]any `json:"containers"`
							SecurityContext map[string]any   `json:"securityContext"`
						} `json:"spec"`
					} `json:"template"`
				} `json:"spec"`
			}
			if err := json.Unmarshal(got, &p); err != nil {
				t.Fatal(err)
			}
			if p.Spec.Template.Spec.SecurityContext["fsGroupChangePolicy"] != HostedFsGroupChangePolicy {
				t.Fatalf("missing fsGroupChangePolicy: %s", got)
			}
			if _, ok := p.Spec.Template.Spec.SecurityContext["fsGroup"]; ok {
				t.Fatalf("backfill must not rewrite fsGroup: %s", got)
			}
			if string(tc.body) == string(image) && (len(p.Spec.Template.Spec.Containers) != 1 || p.Spec.Template.Spec.Containers[0]["image"] != "new") {
				t.Fatalf("image change lost: %s", got)
			}
			if string(tc.body) == string(restart) && len(p.Spec.Template.Metadata) == 0 {
				t.Fatalf("restart annotation lost: %s", got)
			}
		})
	}
}
