package runs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/kubejob"
	"github.com/hivecommons/hive/crux/sandbox"
)

func TestLoadConfig(t *testing.T) {
	cases := []struct {
		name string
		body string
		err  string
	}{
		{"podman defaults", `{"image":"img"}`, ""},
		{"job", `{"backend":"job","image":"img","job":{"workspace_claim":"pvc"}}`, ""},
		{"job host network", `{"backend":"job","image":"img","job":{"workspace_claim":"pvc"},"job_network":"host"}`, ""},
		{"job bad network", `{"backend":"job","image":"img","job":{"workspace_claim":"pvc"},"job_network":"bridge"}`, "job_network must be"},
		{"unknown field", `{"image":"img","bogus":1}`, "bogus"},
		{"no image", `{"backend":"podman"}`, "image is required"},
		{"bad backend", `{"backend":"docker","image":"img"}`, "backend must be"},
		{"job without block", `{"backend":"job","image":"img"}`, "needs a job block"},
		{"timeout order", `{"image":"img","default_timeout_ms":5,"max_timeout_ms":1}`, "default_timeout_ms exceeds"},
		{"output order", `{"image":"img","default_max_output_bytes":5,"max_output_bytes_cap":1}`, "default_max_output_bytes exceeds"},
		{"relative root", `{"image":"img","workspace_roots":["ws"]}`, "must be absolute"},
		{"allowlist dot segment", `{"image":"img","repo_allowlist":["https://github.com/org/../"]}`, "repo_allowlist entry"},
		{"allowlist credentials", `{"image":"img","repo_allowlist":["https://user:token@github.com/org/"]}`, "repo_allowlist entry"},
		{"allowlist scheme", `{"image":"img","repo_allowlist":["ssh://git@github.com/org/"]}`, "repo_allowlist entry"},
		{"too big", `{"image":"` + strings.Repeat("x", maxConfigBytes) + `"}`, "configuration size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadConfig(strings.NewReader(tc.body))
			if tc.err == "" {
				if err != nil {
					t.Fatal(err)
				}
				if cfg.DefaultTimeoutMs != DefaultTimeout.Milliseconds() || cfg.MaxOutputBytesCap != DefaultMaxOutputCap {
					t.Fatalf("defaults not applied: %+v", cfg)
				}
				if tc.name == "job" && cfg.JobNetwork != kubejob.NetworkNone {
					t.Fatalf("job_network default = %q, want none", cfg.JobNetwork)
				}
				if tc.name == "job host network" && cfg.JobNetwork != kubejob.NetworkHost {
					t.Fatalf("job_network = %q", cfg.JobNetwork)
				}
				return
			}
			if err == nil || !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("err = %v, want %q", err, tc.err)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "runs.json")
	if err := os.WriteFile(path, []byte(`{"image":"img","repo_allowlist":["https://github.com/"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil || cfg.Backend != BackendPodman || len(cfg.RepoAllowlist) != 1 {
		t.Fatalf("LoadConfig = %+v, %v", cfg, err)
	}
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json")); !errors.Is(err, ErrConfig) {
		t.Fatalf("missing file = %v", err)
	}
}

func TestBackendFromConfig(t *testing.T) {
	podman, err := BackendFromConfig(Config{Backend: BackendPodman, Image: "img"})
	if err != nil {
		t.Fatal(err)
	}
	if podman.Name != BackendPodman || podman.Teardown == nil {
		t.Fatalf("podman = %+v", podman)
	}
	if l, ok := podman.Launcher.(sandbox.PodmanLauncher); !ok || l.Binary != sandbox.PodmanBinary {
		t.Fatalf("launcher = %#v", podman.Launcher)
	}
	if _, ok := podman.Supports(api.RunNetworkNone); !ok {
		t.Fatal("podman cannot do network none")
	}
	if what, ok := podman.Supports(api.RunNetworkRestricted); ok || !strings.Contains(what, `"restricted"`) {
		t.Fatalf("restricted without the flag: %q %v", what, ok)
	}
	if what, ok := podman.Supports("bridge"); ok || !strings.Contains(what, "unknown network mode") {
		t.Fatalf("unknown mode: %q %v", what, ok)
	}
	restricted, _ := BackendFromConfig(Config{Backend: BackendPodman, Image: "img", NetworkRestricted: true, PodmanBinary: "/opt/podman"})
	if _, ok := restricted.Supports(api.RunNetworkRestricted); !ok {
		t.Fatal("restricted flag ignored")
	}
	if l := restricted.Launcher.(sandbox.PodmanLauncher); l.Binary != "/opt/podman" {
		t.Fatalf("binary override = %q", l.Binary)
	}

	job := &config.SandboxJobConfig{WorkspaceClaim: "pvc", TTLSeconds: 60}
	job.Tolerations = []config.SandboxJobToleration{{Key: "k", Operator: "Exists", Effect: "NoSchedule"}}
	job.Volumes = []config.SandboxJobVolume{{Name: "cache", Claim: "c", MountPath: "/cache", ReadOnly: true}}
	kube, err := BackendFromConfig(Config{Backend: BackendJob, Image: "img", Job: job})
	if err != nil {
		t.Fatal(err)
	}
	if kube.Teardown != nil || kube.Name != BackendJob {
		t.Fatalf("job backend = %+v", kube)
	}
	// Unset job_network is "none": the launcher isolates each run itself.
	if _, ok := kube.Supports(api.RunNetworkNone); !ok {
		t.Fatal("job none by default is not claimed")
	}
	opts := kube.Launcher.(*kubejob.Launcher).Options
	if opts.WorkspaceClaim != "pvc" || opts.TTLSeconds != 60 || len(opts.Tolerations) != 1 || opts.Tolerations[0].Key != "k" ||
		len(opts.ExtraVolumes) != 1 || opts.ExtraVolumes[0].MountPath != "/cache" || !opts.ExtraVolumes[0].ReadOnly || opts.Network != kubejob.NetworkNone {
		t.Fatalf("job options = %+v", opts)
	}
	// On the cluster network only the operator's own policy can claim it.
	open, _ := BackendFromConfig(Config{Backend: BackendJob, Image: "img", Job: job, JobNetwork: kubejob.NetworkDefault})
	if what, ok := open.Supports(api.RunNetworkNone); ok || !strings.Contains(what, `cannot enforce network "none"`) {
		t.Fatalf("job default without a policy: %q %v", what, ok)
	}
	if open.Launcher.(*kubejob.Launcher).Options.Network != kubejob.NetworkDefault {
		t.Fatal("job_network default not passed through")
	}
	policy, _ := BackendFromConfig(Config{Backend: BackendJob, Image: "img", Job: job, JobNetwork: kubejob.NetworkHost, JobNetworkPolicy: true})
	if _, ok := policy.Supports(api.RunNetworkNone); !ok {
		t.Fatal("job_network_policy ignored")
	}
	if _, ok := policy.Supports(api.RunNetworkRestricted); ok {
		t.Fatal("job claims restricted")
	}

	for name, cfg := range map[string]Config{
		"job without block": {Backend: BackendJob, Image: "img"},
		"job invalid":       {Backend: BackendJob, Image: "img", Job: &config.SandboxJobConfig{}},
		"job bad network":   {Backend: BackendJob, Image: "img", Job: job, JobNetwork: "bridge"},
		"unknown":           {Backend: "docker", Image: "img"},
	} {
		if _, err := BackendFromConfig(cfg); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestPodmanTeardownReportsFailures(t *testing.T) {
	for _, bin := range []string{"true", "false"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	if err := podmanTeardown("true")(context.Background(), "crux-run-x"); err != nil {
		t.Fatalf("true: %v", err)
	}
	err := podmanTeardown("false")(context.Background(), "crux-run-x")
	if err == nil || !strings.Contains(err.Error(), "podman rm crux-run-x") {
		t.Fatalf("false: %v", err)
	}
}

func TestGitProvisionerWithARecordedRunner(t *testing.T) {
	dir := t.TempDir()
	var calls [][]string
	runner := func(_ context.Context, cwd, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{filepath.Base(cwd), name}, args...))
		if args[0] == "rev-parse" {
			return []byte(testSHA + "\n"), nil
		}
		return nil, nil
	}
	wt := api.ContainedRunWorktree{Repo: "https://github.com/hivecommons/hive", Commit: testSHA}
	path, err := GitProvisioner{Run: runner}.Provision(context.Background(), dir, wt)
	if err != nil || path != filepath.Join(dir, WorkspaceDirName) {
		t.Fatalf("Provision = %q, %v", path, err)
	}
	want := [][]string{
		{filepath.Base(dir), "git", "clone", "--no-checkout", "--", wt.Repo, path},
		{WorkspaceDirName, "git", "checkout", "--detach", testSHA},
		{WorkspaceDirName, "git", "rev-parse", "HEAD"},
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v", calls)
	}
	for i := range want {
		if strings.Join(calls[i], " ") != strings.Join(want[i], " ") {
			t.Fatalf("call %d = %v, want %v", i, calls[i], want[i])
		}
	}

	failing := func(step string, out string) Runner {
		return func(_ context.Context, _, _ string, args ...string) ([]byte, error) {
			if args[0] == step {
				return []byte(out), errors.New("exit 128")
			}
			if args[0] == "rev-parse" {
				return []byte("deadbeef\n"), nil
			}
			return nil, nil
		}
	}
	for _, tc := range []struct{ step, want string }{
		{"clone", "git clone: exit 128: not found"},
		{"checkout", "git checkout " + testSHA + ": exit 128: not found"},
		{"rev-parse", "git rev-parse HEAD: exit 128"},
		{"none", "checkout pinned " + testSHA + " but HEAD is deadbeef"},
	} {
		if _, err := (GitProvisioner{Run: failing(tc.step, "not found")}).Provision(context.Background(), t.TempDir(), wt); err == nil || err.Error() != tc.want {
			t.Errorf("%s: err = %v, want %q", tc.step, err, tc.want)
		}
	}
	if _, err := (GitProvisioner{Run: runner}).Provision(context.Background(), filepath.Join(writeTempFile(t), "sub"), wt); err == nil {
		t.Fatal("mkdir under a file succeeded")
	}
}

func TestGitProvisionerClonesALocalRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	src := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "f")
	git("commit", "-q", "-m", "one")
	sha := git("rev-parse", "HEAD")

	dir := t.TempDir()
	path, err := GitProvisioner{}.Provision(context.Background(), dir, api.ContainedRunWorktree{Repo: src, Commit: sha})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(path, "f")); err != nil || string(b) != "1" {
		t.Fatalf("checked-out file = %q, %v", b, err)
	}
	if _, err := (GitProvisioner{}).Provision(context.Background(), t.TempDir(), api.ContainedRunWorktree{Repo: src, Commit: testSHA}); err == nil || !strings.Contains(err.Error(), "git checkout") {
		t.Fatalf("unknown commit = %v", err)
	}
}

func writeTempFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
