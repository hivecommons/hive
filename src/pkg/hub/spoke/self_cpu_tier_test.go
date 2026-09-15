package spoke

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCPUTierForLevel(t *testing.T) {
	for level := 0; level <= 6; level++ {
		req, lim := CPUTierForLevel(level)
		if level >= HighCPUACMMLevel {
			if req != CPURequestHigh || lim != CPULimitHigh {
				t.Errorf("L%d: got %s/%s, want high tier", level, req, lim)
			}
		} else if req != CPURequestBase || lim != CPULimitBase {
			t.Errorf("L%d: got %s/%s, want base tier", level, req, lim)
		}
	}
}

// stubSelfDeployment points the k8s client at an httptest server serving a
// Deployment whose hive container carries cpuLimit, and records PATCH bodies.
func stubSelfDeployment(t *testing.T, cpuLimit string) *[]string {
	t.Helper()
	dir := t.TempDir()
	nsPath := filepath.Join(dir, "namespace")
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(nsPath, []byte("test-ns"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte("fake-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	var patches []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			b, _ := io.ReadAll(r.Body)
			patches = append(patches, string(b))
			w.WriteHeader(http.StatusOK)
			return
		}
		limits := ""
		if cpuLimit != "" {
			limits = `,"resources":{"limits":{"cpu":"` + cpuLimit + `"}}`
		}
		_, _ = w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"copy-config","image":"x"},{"name":"hive","image":"y"` + limits + `}]}}}}`))
	}))
	t.Cleanup(srv.Close)

	oldServer, oldToken, oldCA, oldNS := k8sAPIServer, k8sTokenPath, k8sCACertPath, k8sNamespacePath
	k8sAPIServer = srv.URL
	k8sTokenPath = tokenPath
	k8sCACertPath = filepath.Join(dir, "ca.crt")
	writeTestK8sCACert(t, k8sCACertPath)
	k8sNamespacePath = nsPath
	t.Cleanup(func() {
		k8sAPIServer, k8sTokenPath, k8sCACertPath, k8sNamespacePath = oldServer, oldToken, oldCA, oldNS
	})
	return &patches
}

func TestEnsureCPUTierSelf_GrowsBaseTierAtL5(t *testing.T) {
	patches := stubSelfDeployment(t, "2")
	patched, err := EnsureCPUTierSelf(slog.Default(), 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !patched || len(*patches) != 1 {
		t.Fatalf("patched=%v patches=%d, want one patch", patched, len(*patches))
	}
	var body struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name      string `json:"name"`
						Resources struct {
							Requests map[string]string `json:"requests"`
							Limits   map[string]string `json:"limits"`
						} `json:"resources"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte((*patches)[0]), &body); err != nil {
		t.Fatalf("patch is not JSON: %v\n%s", err, (*patches)[0])
	}
	cs := body.Spec.Template.Spec.Containers
	if len(cs) != 1 || cs[0].Name != "hive" {
		t.Fatalf("patch must target only the hive container by name: %s", (*patches)[0])
	}
	if cs[0].Resources.Requests["cpu"] != CPURequestHigh || cs[0].Resources.Limits["cpu"] != CPULimitHigh {
		t.Errorf("patch resources = %v, want %s/%s", cs[0].Resources, CPURequestHigh, CPULimitHigh)
	}
}

func TestEnsureCPUTierSelf_NoopWhenAlreadyHigh(t *testing.T) {
	patches := stubSelfDeployment(t, "4")
	patched, err := EnsureCPUTierSelf(slog.Default(), 6)
	if err != nil || patched || len(*patches) != 0 {
		t.Fatalf("patched=%v err=%v patches=%d, want no-op", patched, err, len(*patches))
	}
}

func TestEnsureCPUTierSelf_NoopBelowL5(t *testing.T) {
	patches := stubSelfDeployment(t, "2")
	for _, level := range []int{0, 2, 4} {
		patched, err := EnsureCPUTierSelf(slog.Default(), level)
		if err != nil || patched {
			t.Fatalf("L%d: patched=%v err=%v, want no-op", level, patched, err)
		}
	}
	if len(*patches) != 0 {
		t.Fatalf("no PATCH expected below L%d, got %d", HighCPUACMMLevel, len(*patches))
	}
}

func TestEnsureCPUTierSelf_SetsLimitWhenAbsent(t *testing.T) {
	patches := stubSelfDeployment(t, "")
	patched, err := EnsureCPUTierSelf(slog.Default(), 6)
	if err != nil || !patched || len(*patches) != 1 {
		t.Fatalf("patched=%v err=%v patches=%d, want one patch", patched, err, len(*patches))
	}
}

func TestEnsureCPUTierSelf_OffClusterIsSilentNoop(t *testing.T) {
	old := k8sTokenPath
	k8sTokenPath = filepath.Join(t.TempDir(), "absent-token")
	t.Cleanup(func() { k8sTokenPath = old })
	patched, err := EnsureCPUTierSelf(slog.Default(), 6)
	if err != nil || patched {
		t.Fatalf("patched=%v err=%v, want silent no-op off-cluster", patched, err)
	}
}
