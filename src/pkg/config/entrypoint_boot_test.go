package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hivecommons/hive/internal/testutil"
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
	return runBootPreludeRO(t, env, files, nil)
}

// runBootPreludeRO is runBootPrelude with a set of files made read-only after
// they are written, so a test can exercise the branches that fire when the
// config path cannot be written — a bind-mounted hive.yaml under docker/podman
// is the ordinary way that happens.
func runBootPreludeRO(t *testing.T, env map[string]string, files map[string]string, readOnly []string) string {
	t.Helper()
	out, _ := runBootPreludeRoot(t, env, files, readOnly)
	return out
}

// runBootPreludeRoot is runBootPreludeRO that also returns the temp root, so a
// test can inspect the FILES the prelude left behind (permissions, contents)
// and not just what it logged.
func runBootPreludeRoot(t *testing.T, env map[string]string, files map[string]string, readOnly []string) (string, string) {
	t.Helper()
	src, err := os.ReadFile(entrypointPath)
	if err != nil {
		testutil.SkipfUnlessRequired(t, "entrypoint.sh not readable from this package: %v", err)
	}
	text := string(src)
	// Cut at the cleanup marker that follows the config branch.
	end := strings.Index(text, "# ── Cleanup stale stderr logs")
	if end < 0 {
		t.Fatal("could not find the end of the config branch in entrypoint.sh; the marker moved and this test would silently cover nothing")
	}
	root := t.TempDir()
	// /data always exists on a real hive (it is the PVC mount point), so
	// create it even when a case seeds no files into it. Otherwise the
	// entrypoint's `cp ... /data/hive.yaml.runtime` fails on the missing
	// directory and a permissions assertion would pass/fail for the wrong
	// reason.
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
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
	// Rewrite the serviceaccount-token probe too, so IS_KUBERNETES is decided
	// by the test's KUBERNETES_SERVICE_HOST alone. Without this, the real
	// in-cluster token file flips every "Docker mode" test into the K8s
	// branch on in-cluster CI runners and dev hives.
	body = strings.ReplaceAll(body,
		"/var/run/secrets/kubernetes.io/serviceaccount/token",
		root+"/var/run/secrets/kubernetes.io/serviceaccount/token")
	// Map the runtime uid onto the sandbox. The prelude's "is this file owned
	// by the runtime user" fast paths compare against the literal uid 1001
	// (dev in the shipped image). Under `go test` every seeded file is owned
	// by the CURRENT uid, and a non-root test cannot chown, so on any host
	// where tests do not happen to run as uid 1001 hive_harden_runtime_config
	// takes its cannot-chown branch by design (#5360) and the 0600 assertions
	// fail for a reason that has nothing to do with the hardening under test.
	body = strings.ReplaceAll(body, `"1001"`, `"`+strconv.Itoa(os.Getuid())+`"`)

	for _, rel := range readOnly {
		p := filepath.Join(root, rel)
		if err := os.Chmod(p, 0o444); err != nil {
			t.Fatal(err)
		}
		// The parent directory must be read-only too, or `cp` simply unlinks
		// and recreates the file rather than failing — which is precisely the
		// distinction the read-only branch turns on.
		if err := os.Chmod(filepath.Dir(p), 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Dir(p), 0o755) })
	}

	cmd := exec.Command("bash", "-c", body)
	// Drop the host's own KUBERNETES_SERVICE_HOST so IS_KUBERNETES is decided
	// by the case's env alone. On an in-cluster runner or a live hive host the
	// inherited variable flips every "Docker mode" case into the K8s branch —
	// the env-var half of the same leak the serviceaccount-token path rewrite
	// above closes for the file half of the probe.
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "KUBERNETES_SERVICE_HOST=") {
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	cmd.Env = append(cmd.Env, "HIVE_CONFIG="+root+"/etc/hive/hive.yaml")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, _ := cmd.CombinedOutput()
	return string(out), root
}

// TestEntrypointHardensRuntimeConfigItRecreates is the regression guard for
// #5331: every `cp` that (re)creates /data/hive.yaml.runtime must leave it
// 0600.
//
// The boot-time chmod loop only covers files that already exist when the
// script starts. These three branches CREATE the runtime copy from a 0644
// source (the ConfigMap seed or the bind-mounted hive.yaml), and cp gives a
// newly created destination the source's mode — so without the explicit
// hardening the file is re-widened to 0644 on every boot, after the loop has
// already run. The runtime config carries dashboard.auth_token, and /data is
// world-traversable, so 0644 hands the dashboard owner credential to every
// unprivileged agent uid on the host.
//
// The harness writes its seed files 0644, which is exactly the production
// mode, so this test fails against the unhardened script.
func TestEntrypointHardensRuntimeConfigItRecreates(t *testing.T) {
	cases := []struct {
		name  string
		env   map[string]string
		files map[string]string
	}{
		{
			// K8s first boot: seeded from the 0644 ConfigMap seed.
			name: "k8s seeds runtime from ConfigMap",
			env:  map[string]string{"KUBERNETES_SERVICE_HOST": "10.0.0.1"},
			files: map[string]string{
				"etc/hive/hive.yaml": "acmm_level: 3\n",
			},
		},
		{
			// Docker first boot: seeded from the 0644 bind-mounted config.
			name:  "docker first boot seeds runtime",
			env:   map[string]string{},
			files: map[string]string{"etc/hive/hive.yaml": "acmm_level: 3\n"},
		},
		{
			// Docker migration: the new name is created from legacy .bak.
			name: "docker migration seeds runtime from legacy bak",
			env:  map[string]string{},
			files: map[string]string{
				"etc/hive/hive.yaml": "acmm_level: 3\n",
				"data/hive.yaml.bak": "acmm_level: 5\n",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, root := runBootPreludeRoot(t, tc.env, tc.files, nil)
			p := filepath.Join(root, "data/hive.yaml.runtime")
			info, err := os.Stat(p)
			if err != nil {
				t.Fatalf("the prelude did not create the runtime config (%v); this branch no longer covers what the test claims:\n%s", err, out)
			}
			if got := info.Mode().Perm(); got != 0o600 {
				t.Fatalf("runtime config is %04o, want 0600 — it carries dashboard.auth_token and /data is world-traversable (#5331):\n%s", got, out)
			}
		})
	}
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

// ─────────────────────────────────────────────────────────────────────────
// #4973 — the entrypoint's read-only redirect must reach the Go binary
// ─────────────────────────────────────────────────────────────────────────

// requireUnprivileged skips a test that depends on file permissions actually
// denying a write. Root bypasses the mode bits, so the read-only branch under
// test would never be taken and the assertion would be vacuous rather than
// wrong — say so instead of passing for the wrong reason.
func requireUnprivileged(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny writes, so the read-only branch cannot be exercised")
	}
}

// TestDockerReadOnlyConfigPathRedirectsToRuntimeConfig is the PRECONDITION of
// #4973: on a non-Kubernetes hive whose config path is read-only, the
// entrypoint must fall back to reading the PVC runtime config directly, and it
// signals that by exporting HIVE_CONFIG.
//
// The config path keeps its stale contents in this branch — that is the whole
// point, it could not be written — so everything downstream depends on the
// export being honoured.
func TestDockerReadOnlyConfigPathRedirectsToRuntimeConfig(t *testing.T) {
	requireUnprivileged(t)
	out := runBootPreludeRO(t,
		map[string]string{"KUBERNETES_SERVICE_HOST": ""},
		map[string]string{
			"data/hive.yaml.runtime": "acmm_level: 4\n",
			"etc/hive/hive.yaml":     "acmm_level: 3\n", // stale, and unwritable
		},
		[]string{"etc/hive/hive.yaml"})

	if !strings.Contains(out, "Config path is read-only") {
		t.Fatalf("read-only config path did not take the redirect branch:\n%s", out)
	}
	if !strings.Contains(out, "hive.yaml.runtime") {
		t.Fatalf("the redirect did not name the runtime config:\n%s", out)
	}
}

// launchArgvAfterReconciliation runs ONLY the argv-reconciliation block that
// precedes the `hive "$@" &` launch, with argv preset to the image's default
// CMD, and returns the argv the binary would actually receive.
//
// Extracted from the real script by marker, like runBootPrelude, so the test
// covers the shipped line rather than a paraphrase of it.
func launchArgvAfterReconciliation(t *testing.T, hiveConfigEnv string) []string {
	t.Helper()
	src, err := os.ReadFile(entrypointPath)
	if err != nil {
		testutil.SkipfUnlessRequired(t, "entrypoint.sh not readable from this package: %v", err)
	}
	text := string(src)

	const startMarker = "# ── Config-path argv reconciliation (#4973)"
	const endMarker = `echo "[entrypoint] Starting Go binary`
	start := strings.Index(text, startMarker)
	if start < 0 {
		t.Fatal("entrypoint.sh no longer contains the config-path argv reconciliation block; the #4973 redirect is inert again")
	}
	end := strings.Index(text[start:], endMarker)
	if end < 0 {
		t.Fatal("could not find the Go binary launch after the reconciliation block; the markers moved and this test would silently cover nothing")
	}
	block := text[start : start+end]

	// The image's CMD, verbatim from src/Dockerfile.
	script := `set -- "--config" "/etc/hive/hive.yaml"` + "\n" + block + "\n" + `printf '%s\n' "$@"`

	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "HIVE_CONFIG="+hiveConfigEnv)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("running the reconciliation block: %v", err)
	}
	var argv []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if strings.HasPrefix(line, "[entrypoint]") {
			continue // the block's own log line
		}
		argv = append(argv, line)
	}
	return argv
}

// TestRedirectedConfigPathReachesTheBinaryArgv is the #4973 regression.
//
// The entrypoint exported HIVE_CONFIG and stopped there. main.go reads that
// variable only to choose the DEFAULT of -config, and the image's CMD passes
// -config EXPLICITLY, so the explicit value won: the hive loaded the stale
// read-only file the entrypoint had just decided not to use. Config.Save()
// then wrote that stale config back over /data/hive.yaml.runtime, so an ACMM
// level set from the dashboard reverted on the next restart and stayed
// reverted.
//
// The last occurrence of a repeated flag wins in flag.Parse, so appending the
// redirect is what makes it effective.
func TestRedirectedConfigPathReachesTheBinaryArgv(t *testing.T) {
	argv := launchArgvAfterReconciliation(t, "/data/hive.yaml.runtime")

	if len(argv) < 2 {
		t.Fatalf("argv = %q, want the CMD plus an appended --config", argv)
	}
	gotFlag, gotValue := argv[len(argv)-2], argv[len(argv)-1]
	if gotFlag != "--config" || gotValue != "/data/hive.yaml.runtime" {
		t.Errorf("argv = %q; the LAST --config must be the entrypoint's redirect (/data/hive.yaml.runtime), or flag.Parse keeps the stale CMD value", argv)
	}
}

// TestUnsetHiveConfigLeavesArgvAlone: when the config path was writable the
// entrypoint copies the runtime config over it and exports nothing, so the
// image CMD must apply unchanged. This is the ordinary boot, and the fix must
// not perturb it.
func TestUnsetHiveConfigLeavesArgvAlone(t *testing.T) {
	argv := launchArgvAfterReconciliation(t, "")

	want := []string{"--config", "/etc/hive/hive.yaml"}
	if len(argv) != len(want) || argv[0] != want[0] || argv[1] != want[1] {
		t.Errorf("argv = %q, want the untouched CMD %q", argv, want)
	}
}

// TestLaunchUsesTheReconciledArgv guards the other half of the fix: rewriting
// "$@" only helps if the launch still passes "$@". A launch rebuilt from
// explicit arguments would silently drop the redirect and restore #4973 with
// every test above still green.
func TestLaunchUsesTheReconciledArgv(t *testing.T) {
	src, err := os.ReadFile(entrypointPath)
	if err != nil {
		testutil.SkipfUnlessRequired(t, "entrypoint.sh not readable from this package: %v", err)
	}
	if !strings.Contains(string(src), `hive "$@" &`) {
		t.Error(`entrypoint.sh no longer launches the binary as 'hive "$@" &'; the argv reconciliation above it can no longer reach the process`)
	}
}

// TestDockerfileCMDAndEntrypointAgreeOnConfigPath pins the COUPLING that broke.
//
// The bug was not in either file alone: the Dockerfile passing -config
// explicitly is fine, and the entrypoint exporting HIVE_CONFIG is fine. It is
// the combination that made the export inert. So if the image ever again ships
// an explicit -config in its CMD, the entrypoint must still carry the
// reconciliation that outranks it.
func TestDockerfileCMDAndEntrypointAgreeOnConfigPath(t *testing.T) {
	dockerfile, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		testutil.SkipfUnlessRequired(t, "Dockerfile not readable from this package: %v", err)
	}
	if !strings.Contains(string(dockerfile), `CMD ["--config"`) {
		// Fail rather than skip (#5388): this condition depends only on repo
		// content, so it is identical on every machine — a skip here is never
		// "unsuitable environment", it means the coupling this test pins moved.
		// If the CMD change is deliberate, retire or re-point this test in the
		// same PR instead of letting it silently stop asserting anything.
		t.Fatal("the image CMD no longer passes -config explicitly; the coupling this " +
			"test guards is gone — update or retire TestDockerfileCMDAndEntrypointAgreeOnConfigPath " +
			"in the PR that changed the Dockerfile CMD")
	}
	entry, err := os.ReadFile(entrypointPath)
	if err != nil {
		testutil.SkipfUnlessRequired(t, "entrypoint.sh not readable from this package: %v", err)
	}
	if !strings.Contains(string(entry), `set -- "$@" --config "$HIVE_CONFIG"`) {
		t.Error("the image CMD passes -config explicitly but entrypoint.sh no longer appends its own; HIVE_CONFIG is inert again (#4973)")
	}
}
