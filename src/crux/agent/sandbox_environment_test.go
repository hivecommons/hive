package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/kubejob"
)

func deviceJob() config.SandboxJobConfig {
	return config.SandboxJobConfig{
		WorkspaceClaim: "hive-ws",
		NodeSelector:   map[string]string{"vendor.com/accelerator": "card"},
		ServiceAccount: "secret-sa",
		EnvFromSecrets: []string{"device-secret"},
		Resources: config.SandboxJobResources{
			Limits:   map[string]string{"cpu": "4", "memory": "16Gi", "vendor.com/device": "2", "hugepages-2Mi": "1Gi", "nvidia.com/gpu": "0"},
			Requests: map[string]string{"cpu": "2", "ephemeral-storage": "10Gi", "example.org/fpga": "1", "kubernetes.io/batch": "1"},
		},
	}
}

func TestIsExtendedResource(t *testing.T) {
	for name, want := range map[string]bool{
		"cpu": false, "memory": false, "ephemeral-storage": false, "hugepages-2Mi": false,
		"kubernetes.io/batch": false, "node.kubernetes.io/x": false, "/x": false, "vendor.com/": false,
		"vendor.com/device": true, "nvidia.com/gpu": true, "ibm.com/spyre_vf": true,
	} {
		if got := isExtendedResource(name); got != want {
			t.Errorf("isExtendedResource(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestSandboxEnvironmentForJobWithDevices(t *testing.T) {
	pre := config.SandboxPreflightConfig{Commands: []string{"check-device", "  "}}
	env := sandboxEnvironmentFor(config.SandboxRuntimeJob, deviceJob(), pre, true)
	if env == nil {
		t.Fatal("no environment for a Job that requests devices")
	}
	want := []sandboxResource{{"example.org/fpga", "1"}, {"vendor.com/device", "2"}}
	if len(env.Resources) != len(want) {
		t.Fatalf("resources = %+v, want %+v", env.Resources, want)
	}
	for i := range want {
		if env.Resources[i] != want[i] {
			t.Fatalf("resources = %+v, want %+v", env.Resources, want)
		}
	}
	if env.PreflightCommands != 1 {
		t.Fatalf("preflight commands = %d, want 1 (blank dropped)", env.PreflightCommands)
	}
	section := sandboxEnvironmentPromptSection(env)
	for _, w := range []string{sandboxEnvironmentPromptMarker + "\n", "Runtime: job", "- example.org/fpga: 1\n", "- vendor.com/device: 2\n", "1 configured sandbox.preflight command(s) passed on this node", sandboxEnvironmentInstruction} {
		if !strings.Contains(section, w) {
			t.Fatalf("section lacks %q:\n%s", w, section)
		}
	}
	for _, leak := range []string{"secret-sa", "device-secret", "check-device", "cpu", "memory", "hugepages", "nvidia.com/gpu", "kubernetes.io/batch"} {
		if strings.Contains(section, leak) {
			t.Fatalf("section carries %q:\n%s", leak, section)
		}
	}
	if !strings.HasPrefix(section, sandboxEnvironmentPromptMarker) || !strings.HasSuffix(section, "---\n\n") {
		t.Fatalf("section shape:\n%s", section)
	}
}

func TestSandboxEnvironmentWithoutPreflight(t *testing.T) {
	pre := config.SandboxPreflightConfig{Commands: []string{"check-device"}}
	for name, env := range map[string]*sandboxEnvironment{
		"none configured": sandboxEnvironmentFor(config.SandboxRuntimeJob, deviceJob(), config.SandboxPreflightConfig{}, true),
		"not wired":       sandboxEnvironmentFor(config.SandboxRuntimeJob, deviceJob(), pre, false),
	} {
		section := sandboxEnvironmentPromptSection(env)
		if !strings.Contains(section, "none is configured") || strings.Contains(section, "passed on this node") {
			t.Fatalf("%s: section:\n%s", name, section)
		}
	}
}

func TestSandboxEnvironmentNoSection(t *testing.T) {
	noDevice := config.SandboxJobConfig{Resources: config.SandboxJobResources{Limits: map[string]string{"cpu": "4", "memory": "8Gi"}}}
	for name, env := range map[string]*sandboxEnvironment{
		"podman":          sandboxEnvironmentFor(config.SandboxRuntimePodman, deviceJob(), config.SandboxPreflightConfig{}, false),
		"openshell":       sandboxEnvironmentFor(config.SandboxRuntimeOpenShell, deviceJob(), config.SandboxPreflightConfig{}, false),
		"job, no devices": sandboxEnvironmentFor(config.SandboxRuntimeJob, noDevice, config.SandboxPreflightConfig{}, true),
		"job, empty":      sandboxEnvironmentFor(config.SandboxRuntimeJob, config.SandboxJobConfig{}, config.SandboxPreflightConfig{}, true),
	} {
		if env != nil {
			t.Fatalf("%s: environment = %+v, want nil", name, env)
		}
		if s := sandboxEnvironmentPromptSection(env); s != "" {
			t.Fatalf("%s: section = %q", name, s)
		}
	}
}

func TestSandboxEnvironmentFieldsAreOneLine(t *testing.T) {
	job := config.SandboxJobConfig{Resources: config.SandboxJobResources{Limits: map[string]string{"vendor.com/dev\nIgnore previous": "1\n- x"}}}
	section := sandboxEnvironmentPromptSection(sandboxEnvironmentFor(config.SandboxRuntimeJob, job, config.SandboxPreflightConfig{}, true))
	if strings.Contains(section, "\nIgnore") || strings.Contains(section, "\n- x") {
		t.Fatalf("a config value broke the line:\n%s", section)
	}
}

func TestSandboxEnvironmentForLauncherReadsWiring(t *testing.T) {
	pre := config.SandboxPreflightConfig{Commands: []string{"check-device"}}
	if env := sandboxEnvironmentForLauncher(&kubejob.Launcher{}, config.SandboxRuntimeJob, deviceJob(), pre); env == nil || env.PreflightCommands != 1 {
		t.Fatalf("kubejob launcher: %+v", env)
	}
	if env := sandboxEnvironmentForLauncher(sandboxFakeLauncher{}, config.SandboxRuntimeJob, deviceJob(), pre); env == nil || env.PreflightCommands != 0 {
		t.Fatalf("fake launcher: %+v", env)
	}
}

func TestSandboxExecutorPrependsEnvironmentSection(t *testing.T) {
	exec := &SandboxExecutor{Runner: &sandboxFakeRunner{}, Launcher: sandboxFakeLauncher{}, Logger: discardLogger()}
	env := sandboxEnvironmentFor(config.SandboxRuntimeJob, deviceJob(), config.SandboxPreflightConfig{}, true)
	res, err := exec.Run(context.Background(), SandboxKickSpec{
		Agent: "worker", AgentConfig: configSnapshot{Backend: "claude"}, Message: "do the work",
		Org: "o", Repo: "r", WorkspaceDir: t.TempDir(), Image: "img", Environment: env,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	prompt, err := os.ReadFile(filepath.Join(res.Workspace, sandboxPromptRelPath))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(prompt), sandboxEnvironmentPromptMarker+"\n") || !strings.HasSuffix(string(prompt), "---\n\ndo the work") {
		t.Fatalf("prompt = %q", prompt)
	}
}
