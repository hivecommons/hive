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
	for _, operation := range []string{"image", "restart", "mutable"} {
		for _, probe := range []struct {
			name, json string
			want       bool
		}{
			{"stale", `,"startupProbe":{"httpGet":{"path":"/api/health","port":3002},"failureThreshold":30,"periodSeconds":5}`, true},
			{"current", `,"startupProbe":{"failureThreshold":118}`, false},
			{"larger", `,"startupProbe":{"failureThreshold":200}`, false},
			{"absent", ``, false},
		} {
			t.Run(operation+"/"+probe.name, func(t *testing.T) {
				var patched []byte
				patches := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						fmt.Fprintf(w, `{"spec":{"template":{"spec":{"containers":[{"name":"hive","image":"ghcr.io/hivecommons/hive:old"%s},{"name":"sidecar","image":"ghcr.io/hivecommons/hive:old","startupProbe":{"failureThreshold":30}}],"initContainers":[{"name":"init","image":"ghcr.io/hivecommons/hive:old"}]}}}}`, probe.json)
						return
					}
					if r.Method != http.MethodPatch {
						t.Errorf("unexpected method: %s", r.Method)
					}
					if r.Header.Get("Content-Type") != "application/strategic-merge-patch+json" {
						t.Error("expected strategic merge")
					}
					patches++
					patched, _ = io.ReadAll(r.Body)
					w.WriteHeader(http.StatusOK)
				}))
				defer srv.Close()
				withFakeK8sAPI(t, srv)
				var err error
				switch operation {
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
				if patches != 1 {
					t.Fatalf("got %d patches; want a single atomic rollout", patches)
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
						if found && (len(c.Probe) != 1 || c.Probe["failureThreshold"] != float64(118)) {
							t.Errorf("unexpected probe patch: %v", c.Probe)
						}
						if operation == "image" && c.Image != "ghcr.io/hivecommons/hive:candidate" {
							t.Error("lost image update")
						}
					} else if c.Probe != nil {
						t.Errorf("modified sidecar probe: %v", c.Probe)
					}
				}
				if found != probe.want {
					t.Errorf("probe backfill = %v, want %v; patch %s", found, probe.want, patched)
				}
				if patch.Spec.Template.Metadata.Annotations["hive.hivecommons.dev/restart-at"] == "" {
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
	for _, response := range []struct {
		name, body string
		status     int
	}{
		{"forbidden", "denied", http.StatusForbidden},
		{"malformed", "{", http.StatusOK},
	} {
		t.Run(response.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected write after failed probe read: %s", r.Method)
				}
				w.WriteHeader(response.status)
				io.WriteString(w, response.body)
			}))
			defer srv.Close()
			withFakeK8sAPI(t, srv)
			if err := RolloutRestartSelf(slog.Default()); err == nil {
				t.Fatal("expected read error")
			}
		})
	}
}
