package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/cline"
)

const fixtureCredential = "fixture-only-provider-key"

func clineLaunchFixture(t *testing.T) (LaunchSpec, string) {
	t.Helper()
	root, workspace := t.TempDir(), t.TempDir()
	t.Setenv("HOME", root)
	source := filepath.Join(root, "source")
	settings := filepath.Join(source, "data", "settings")
	if err := os.MkdirAll(settings, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settings, "providers.json"),
		[]byte(`{"version":1,"providers":{"openrouter":{"settings":{"apiKey":"`+fixtureCredential+`"},"tokenSource":"manual"},"other":{"settings":{"apiKey":"must-not-share"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := cline.StageProviderFile(source, "openrouter/test", workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	file, err := p.MountFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	return LaunchSpec{Image: "fixture-image", Workspace: workspace, ClineProvider: p,
		Command: []string{"crux", cline.Subcommand, "--provider-file", cline.ProviderMountPath,
			"--state-dir", cline.ProviderStatePath, "--model", "openrouter/test",
			"--prompt-file", ".hive/kick-prompt.txt", "--timeout", "1m"}}, file
}

func TestClineReadOnlyProviderArgv(t *testing.T) {
	spec, file := clineLaunchFixture(t)
	spec.Env = map[string]string{"ANTHROPIC_API_KEY": "unrelated-fixture-key", cline.ProviderSecretEnv: fixtureCredential}
	spec.Env = map[string]string{"HOME": "/workspace", cline.ProviderSecretEnv: fixtureCredential}
	args, err := PodmanArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "|")
	for _, want := range []string{
		"-v|" + spec.Workspace + ":/workspace:Z",
		"-v|" + file + ":" + cline.ProviderMountPath + ":ro,Z",
		"--tmpfs|" + cline.ProviderStatePath + ":rw,nosuid,nodev,noexec,mode=0700",
		fmt.Sprintf("--user|%d:%d", os.Getuid(), os.Getgid()),
		"--userns=keep-id", "--env|HOME=" + cline.ProviderStatePath,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing scoped option %s", want)
		}
	}
	if strings.Contains(joined, fixtureCredential) || strings.Contains(joined, cline.ProviderSecretEnv) ||
		strings.Contains(joined, "--env|HOME|") {
		t.Fatal("provider value or HOME override entered argv")
	}
	spec.WorkspaceMount = "/elsewhere"
	if _, err := PodmanArgs(spec); err == nil {
		t.Fatal("custom workspace mount accepted with provider capability")
	}
}

func TestClineProviderCommandDenials(t *testing.T) {
	spec, _ := clineLaunchFixture(t)
	for _, command := range [][]string{
		nil, {"sh", "-c", "cat /run/hive-cline-provider/providers.json"},
		{"crux", cline.Subcommand, "--provider-file"},
		{"crux", cline.Subcommand, "--provider-file", "/other"},
		{"crux", cline.Subcommand, "--state-dir", "/workspace"},
		{"crux", cline.Subcommand, "--model", "other/model"},
		{"crux", cline.Subcommand, "--timeout", "0s"},
		{"crux", cline.Subcommand, "--thinking", "invalid"},
		{"crux", cline.Subcommand, "--prompt-file", "/other"},
		{"crux", cline.Subcommand, "--config", "/host"},
		append(append([]string(nil), spec.Command...), "--provider-file", cline.ProviderMountPath),
	} {
		spec.Command = command
		if _, err := PodmanArgs(spec); err == nil {
			t.Fatalf("unsafe command accepted: %v", command)
		}
	}
}

func TestClinePodmanFixtureLifecycle(t *testing.T) {
	for _, mode := range []string{"success", "failure", "cancel", "cleanup-failure", "missing-binary"} {
		t.Run(mode, func(t *testing.T) {
			spec, file := clineLaunchFixture(t)
			trace := filepath.Join(t.TempDir(), "argv")
			binary := filepath.Join(t.TempDir(), "podman-fixture")
			body := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" >> %q\n", trace)
			body += "test -z \"${ANTHROPIC_API_KEY+x}\" || exit 92\n"
			body += "test -z \"${HIVE_CLINE_PROVIDERS_B64+x}\" || exit 93\n"
			body += "if [ \"$1\" = rm ]; then "
			if mode == "cleanup-failure" {
				body += "exit 2; "
			} else {
				body += "exit 0; "
			}
			body += "fi\n"
			body += "printf '%s\\n' '" + fixtureCredential + "'\nprintf '%s\\n' '" + fixtureCredential + "' >&2\n"
			switch mode {
			case "cancel":
				body += "exec sleep 30\n"
			case "failure":
				body += "exit 1\n"
			default:
				body += "exit 0\n"
			}
			if mode != "missing-binary" {
				if err := os.WriteFile(binary, []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			var cancellationDone <-chan struct{}
			if mode == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				done := make(chan struct{})
				cancellationDone = done
				go func() {
					defer close(done)
					const startupTimeout = 10 * time.Second
					const startupPoll = 10 * time.Millisecond
					deadline := time.NewTimer(startupTimeout)
					defer deadline.Stop()
					ticker := time.NewTicker(startupPoll)
					defer ticker.Stop()
					for {
						raw, _ := os.ReadFile(trace)
						if strings.Contains(string(raw), "run\n") {
							cancel()
							return
						}
						select {
						case <-deadline.C:
							cancel()
							return
						case <-ticker.C:
						}
					}
				}()
			}
			res, err := (PodmanLauncher{Binary: binary}).Run(ctx, spec)
			if cancellationDone != nil {
				<-cancellationDone
			}
			if (err == nil) != (mode == "success") {
				t.Fatalf("mode %s: %v", mode, err)
			}
			if strings.Contains(res.Stdout+res.Stderr, fixtureCredential) {
				t.Fatal("credential reached returned output")
			}
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Fatal("provider stage survived launcher completion")
			}
			if mode != "missing-binary" {
				recorded, err := os.ReadFile(trace)
				if err != nil || !strings.Contains(string(recorded), "rm\n--force\n--ignore\nhive-cline-provider-") {
					t.Fatal("container not force-cleaned before stage removal", err)
				}
				if strings.Contains(string(recorded), fixtureCredential) {
					t.Fatal("credential appeared in process arguments")
				}
				if mode == "cancel" && !strings.Contains(string(recorded), "run\n") {
					t.Fatal("cancellation did not exercise an in-flight workload")
				}
			}
		})
	}
}
