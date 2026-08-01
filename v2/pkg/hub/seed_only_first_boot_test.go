package hub

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// initCommand extracts the copy-config init container's shell command from the
// provisioning template, so the test runs the REAL command rather than a
// paraphrase of it. A copy pasted into the test would keep passing after the
// template changed.
func initCommand(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("saas_provision.go")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	re := regexp.MustCompile(`command: \["sh", "-c", "((?:[^"\\]|\\.)*)"\]`)
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		if strings.Contains(m[1], "hive-seed") {
			return strings.ReplaceAll(m[1], `\"`, `"`)
		}
	}
	t.Fatal("could not find the copy-config command in saas_provision.go — it moved, and this test would silently cover nothing")
	return ""
}

// runInit executes the init command against a temp filesystem and returns what
// it echoed plus whether the seed was copied over the config path.
func runInit(t *testing.T, files map[string]string) (string, bool) {
	t.Helper()
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
	if err := os.MkdirAll(filepath.Join(root, "etc/hive"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := initCommand(t)
	cmd = strings.ReplaceAll(cmd, "/data/", root+"/data/")
	cmd = strings.ReplaceAll(cmd, "/etc/hive-seed/", root+"/etc/hive-seed/")
	cmd = strings.ReplaceAll(cmd, "/etc/hive/", root+"/etc/hive/")

	out, _ := exec.Command("sh", "-c", cmd).CombinedOutput()
	copied := false
	if b, err := os.ReadFile(filepath.Join(root, "etc/hive/hive.yaml")); err == nil && len(b) > 0 {
		copied = true
	}
	return string(out), copied
}

// TestSeedIsCopiedOnlyOnFirstBoot pins phase 3: the ConfigMap is a SEED, not a
// per-boot input.
//
// It used to be copied over the config path unconditionally on every boot, so
// the frozen seed overwrote the config before the entrypoint could prefer the
// PVC. Phase 2 made the entrypoint prefer the runtime config; this makes the
// copy stop happening at all once one exists.
func TestSeedIsCopiedOnlyOnFirstBoot(t *testing.T) {
	seed := map[string]string{"etc/hive-seed/hive.yaml": "acmm_level: 3\n"}

	t.Run("first boot: no PVC config, seed IS copied", func(t *testing.T) {
		out, copied := runInit(t, seed)
		if !copied {
			t.Fatalf("seed was not copied on first boot; the hive has nothing to start from:\n%s", out)
		}
		if !strings.Contains(out, "configmap-copied-first-boot") {
			t.Errorf("first boot not reported: %s", out)
		}
	})

	t.Run("steady state: runtime config exists, seed is NOT copied", func(t *testing.T) {
		files := map[string]string{"data/hive.yaml.runtime": "acmm_level: 5\n"}
		for k, v := range seed {
			files[k] = v
		}
		out, copied := runInit(t, files)
		if copied {
			t.Fatalf("the frozen seed overwrote the config path on a hive that has its own runtime config:\n%s", out)
		}
		if !strings.Contains(out, "runtime-config-exists-for-recovery") {
			t.Errorf("runtime config not reported: %s", out)
		}
	})

	// THE MIGRATION CASE. ~16 of 50 live spokes still carry only the legacy
	// hive.yaml.bak. Keying first-boot detection on the new name alone would
	// treat every one of them as new and re-seed a hive that has real config.
	t.Run("legacy .bak only: still NOT first boot", func(t *testing.T) {
		files := map[string]string{"data/hive.yaml.bak": "acmm_level: 5\n"}
		for k, v := range seed {
			files[k] = v
		}
		out, copied := runInit(t, files)
		if copied {
			t.Fatalf("a spoke carrying only the LEGACY runtime name was re-seeded — that is ~16 of 50 live spokes losing their config:\n%s", out)
		}
		if !strings.Contains(out, "legacy-runtime-config-exists-for-recovery") {
			t.Errorf("legacy runtime config not reported: %s", out)
		}
	})

	// An empty file is not a config. Watchtower zeroing the file is a real
	// failure mode the entrypoint already recovers from; the init container
	// must not mistake it for "this hive has config".
	t.Run("zero-byte runtime config is treated as absent", func(t *testing.T) {
		files := map[string]string{"data/hive.yaml.runtime": ""}
		for k, v := range seed {
			files[k] = v
		}
		_, copied := runInit(t, files)
		if !copied {
			t.Fatal("a zero-byte runtime config blocked the seed; the hive would boot with no config at all")
		}
	})
}
