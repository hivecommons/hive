package dashboard

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStandaloneChannelDefaultAndOverrides(t *testing.T) {
	for _, channel := range []string{"stable", "candidate", "edge"} {
		t.Run(channel, func(t *testing.T) {
			ref := "ghcr.io/hivecommons/hive:" + channel
			t.Setenv("HIVE_SELF_IMAGE", ref)
			t.Setenv("HIVE_SELF_IMAGE_TRACKING", "registry")
			for _, tc := range []struct{ target, want string }{
				{"", ref}, {"abcdef123456", "ghcr.io/hivecommons/hive:abcdef1"},
				{"ghcr.io/hivecommons/hive:candidate", "ghcr.io/hivecommons/hive:candidate"},
				{"ghcr.io/hivecommons/hive:Release_1", "ghcr.io/hivecommons/hive:Release_1"},
				{"ghcr.io/hivecommons/hive@sha256:" + strings.Repeat("a", 64), "ghcr.io/hivecommons/hive@sha256:" + strings.Repeat("a", 64)},
			} {
				req := httptest.NewRequest("POST", "/api/self-upgrade", strings.NewReader(`{"target":"`+tc.target+`"}`))
				got, err := parseStandaloneUpgradeTarget(req)
				if err != nil || got != tc.want {
					t.Fatalf("target %q: got %q, %v; want %q", tc.target, got, err, tc.want)
				}
			}
			for _, body := range []string{`{"target":`, `{"target":42}`} {
				if _, err := parseStandaloneUpgradeTarget(httptest.NewRequest("POST", "/api/self-upgrade", strings.NewReader(body))); err == nil {
					t.Fatalf("malformed JSON selected the default channel: %q", body)
				}
			}
			for _, target := range []string{"ghcr.io/attacker/hive:stable", "ghcr.io/hivecommons/hive:stable;reboot", "ghcr.io/hivecommons/hive:stable --rootful"} {
				req := httptest.NewRequest("POST", "/api/self-upgrade?target="+url.QueryEscape(target), nil)
				if _, err := parseStandaloneUpgradeTarget(req); err == nil {
					t.Fatalf("accepted %q", target)
				}
			}
		})
	}
	for _, ref := range []string{"", "ghcr.io/hivecommons/hive@sha256:" + strings.Repeat("a", 64), "ghcr.io/hivecommons/hive:v5-latest", "ghcr.io/attacker/hive:stable"} {
		t.Setenv("HIVE_SELF_IMAGE", ref)
		if _, err := parseStandaloneUpgradeTarget(httptest.NewRequest("POST", "/api/self-upgrade", nil)); err == nil {
			t.Fatalf("invented channel for %q", ref)
		}
	}
}

func TestStandaloneChannelDefaultReachesHelper(t *testing.T) {
	clearDeploymentEnv(t)
	t.Setenv("HIVE_SELF_IMAGE", "ghcr.io/hivecommons/hive:stable")
	argsPath := filepath.Join(t.TempDir(), "args")
	t.Setenv("HIVE_DASHBOARD_UPGRADE_HELPER", fakeUpgradeHelperRecording(t, argsPath))
	srv := NewServer(0, slog.Default())
	srv.deps = testDeps(t)
	req := httptest.NewRequest("POST", "/api/self-upgrade", nil)
	if err := srv.runStandaloneUpgrade(req, deploymentInfo{Runtime: deploymentRuntimeDockerCompose}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "--ref ghcr.io/hivecommons/hive:stable") {
		t.Fatalf("helper args: %s", data)
	}
}

func TestVersionStandaloneChannelDoesNotUseBranchHead(t *testing.T) {
	clearDeploymentEnv(t)
	t.Setenv("HIVE_DEPLOYMENT_RUNTIME", "docker-compose")
	t.Setenv("HIVE_SELF_IMAGE", "ghcr.io/hivecommons/hive:stable")
	old := standaloneChannelRevision
	t.Cleanup(func() { standaloneChannelRevision = old })
	for _, sha := range []string{versionHash, ""} {
		standaloneChannelRevision = func(channel string) string {
			if channel != "stable" {
				t.Fatalf("channel = %q", channel)
			}
			return sha
		}
		srv := NewServer(0, slog.Default())
		srv.deps = testDeps(t)
		srv.cachedLatestHash = "abcdef1234567890"
		srv.cachedLatestAt = time.Now()
		ghcrCacheMu.Lock()
		ghcrCacheResult["abcdef1"] = true
		ghcrCacheExpiry["abcdef1"] = time.Now().Add(time.Hour)
		ghcrCacheMu.Unlock()
		t.Cleanup(func() {
			ghcrCacheMu.Lock()
			delete(ghcrCacheResult, "abcdef1")
			delete(ghcrCacheExpiry, "abcdef1")
			ghcrCacheMu.Unlock()
		})
		rec := httptest.NewRecorder()
		srv.handleVersion(rec, httptest.NewRequest("GET", "/api/version", nil))
		var body struct {
			Target upgradeTarget
			Behind bool
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Target.Source != "channel" || body.Target.Channel != "stable" || body.Target.Ref != "ghcr.io/hivecommons/hive:stable" || body.Target.SHA != sha || body.Target.Resolved != (sha != "") || body.Behind {
			t.Fatalf("version target: %s", rec.Body.String())
		}
	}
}

func TestStandaloneChannelRevisionRegistryWalk(t *testing.T) {
	for _, tc := range []struct {
		name, revision string
		fail           bool
	}{
		{"revision", "1234567890abcdef", false}, {"missing label", "", false}, {"invalid label", "not-a-commit", false}, {"registry failure", "1234567", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.fail {
					w.WriteHeader(503)
					return
				}
				switch r.URL.Path {
				case "/token":
					fmt.Fprint(w, `{"token":"test"}`)
				case "/v2/hivecommons/hive/manifests/stable":
					fmt.Fprintf(w, `{"manifests":[{"digest":"attestation","platform":{"os":"unknown","architecture":"unknown"}},{"digest":"platform","platform":{"os":"linux","architecture":%q}}]}`, runtime.GOARCH)
				case "/v2/hivecommons/hive/manifests/platform":
					fmt.Fprint(w, `{"config":{"digest":"config"}}`)
				case "/v2/hivecommons/hive/blobs/config":
					fmt.Fprintf(w, `{"config":{"Labels":{"org.opencontainers.image.revision":%q}}}`, tc.revision)
				default:
					t.Errorf("unexpected registry path: %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			want := tc.revision
			if tc.fail || !standaloneUpgradeTargetRE.MatchString(want) {
				want = ""
			}
			if got := standaloneChannelRevisionWithClient(srv.Client(), srv.URL, "stable"); got != want {
				t.Fatalf("revision %q, want %q", got, want)
			}
		})
	}
}
