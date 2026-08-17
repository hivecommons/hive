package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// entrypointPath is the script under test, relative to this package.
const entrypointPath = "../../deploy/entrypoint.sh"

// runBootPrelude executes ONLY the config-resolution prelude of entrypoint.sh
// against a temp filesystem, and returns what it logged.
//
// It extracts the script up to the end of the K8s/Docker config branch rather
// than running the whole thing (which would try to create users, iptables rules
// and tmux sessions). That keeps the test honest about WHICH code it covers: it
// executes the real branching logic, not a paraphrase of it.
func runBootPrelude(t *testing.T, env map[string]string, files map[string]string) string {
	t.Helper()
	src, err := os.ReadFile(entrypointPath)
	if err != nil {
		t.Skipf("entrypoint.sh not readable from this package: %v", err)
	}
	text := string(src)
	// Cut at the cleanup marker that follows the config branch.
	end := strings.Index(text, "# ── Cleanup stale stderr logs")
	if end < 0 {
		t.Fatal("could not find the end of the config branch in entrypoint.sh; the marker moved and this test would silently cover nothing")
	}
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Rewrite the absolute paths onto the temp root.
	body := text[:end]
	body = strings.ReplaceAll(body, `"/data/`, `"`+root+`/data/`)
	body = strings.ReplaceAll(body, `/etc/hive/hive.yaml`, root+"/etc/hive/hive.yaml")

	cmd := exec.Command("bash", "-c", body)
	cmd.Env = append(os.Environ(), "HIVE_CONFIG="+root+"/etc/hive/hive.yaml")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// TestK8sBootsFromRuntimeConfigNotTheSeed pins the phase-2 precedence: the PVC
// runtime config is the boot INPUT and the ConfigMap is only a first-boot seed.
//
// Booting from the seed and merging the overlay over it on every start is what
// made "which layer wins" a live question — three read-only layers were patched
// during the 2026-07-31 incident before the writable one was found.
func TestK8sBootsFromRuntimeConfigNotTheSeed(t *testing.T) {
	out := runBootPrelude(t,
		map[string]string{"KUBERNETES_SERVICE_HOST": "10.0.0.1"},
		map[string]string{
			"data/hive.yaml.runtime": "acmm_level: 5\n",
			"etc/hive/hive.yaml":     "acmm_level: 3\n", // stale seed
		})
	if !strings.Contains(out, "booting from runtime config") {
		t.Fatalf("did not boot from the runtime config:\n%s", out)
	}
	if strings.Contains(out, "overlay merged over ConfigMap seed") {
		t.Fatalf("the per-boot merge still ran; the ConfigMap must be a seed only:\n%s", out)
	}
}

// TestK8sLegacyBakStillBoots: ~48 of 51 live spokes still carry only the old
// hive.yaml.bak name. They must keep booting while the rename rolls out.
func TestK8sLegacyBakStillBoots(t *testing.T) {
	out := runBootPrelude(t,
		map[string]string{"KUBERNETES_SERVICE_HOST": "10.0.0.1"},
		map[string]string{
			"data/hive.yaml.bak": "acmm_level: 5\n",
			"etc/hive/hive.yaml": "acmm_level: 3\n",
		})
	if !strings.Contains(out, "hive.yaml.bak") {
		t.Fatalf("a spoke with only the legacy name did not boot from it:\n%s", out)
	}
}

// TestK8sFirstBootUsesTheSeed: with no PVC config at all, the ConfigMap seed is
// the only source and must still work.
func TestK8sFirstBootUsesTheSeed(t *testing.T) {
	out := runBootPrelude(t,
		map[string]string{"KUBERNETES_SERVICE_HOST": "10.0.0.1"},
		map[string]string{"etc/hive/hive.yaml": "acmm_level: 3\n"})
	if !strings.Contains(out, "ConfigMap is the seed") {
		t.Fatalf("first boot did not fall back to the seed:\n%s", out)
	}
}

// TestK8sNoConfigAtAllFailsLoudly: silence here would start a hive with no
// project and no agents, which reads as a broken hive rather than a broken
// deploy.
func TestK8sNoConfigAtAllFailsLoudly(t *testing.T) {
	out := runBootPrelude(t,
		map[string]string{"KUBERNETES_SERVICE_HOST": "10.0.0.1"},
		map[string]string{})
	if !strings.Contains(out, "ERROR: no runtime config") {
		t.Fatalf("missing config did not fail loudly:\n%s", out)
	}
}
