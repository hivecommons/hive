package hostedcontrol

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/checkpoint"
	"github.com/kubestellar/hive/pkg/hostedstate"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

func testConfig(checkout, runtimeRoot string) Config {
	return Config{
		CheckoutDir: checkout,
		RuntimeRoot: runtimeRoot,
		StateBranch: "hive/state-1234abcd",
		Secret:      testSecret,
		Repository:  hostedstate.RepositoryIdentity{FullName: "owner/repository", ID: 1234},
		Release: hostedstate.ReleaseIdentity{
			Version: "v0.4.1-integrated.19", HiveCommit: strings.Repeat("1", 40), VisualHiveCommit: strings.Repeat("2", 40),
			DistributionManifestSHA256: strings.Repeat("5", 64), HostedControllerProtocol: 1,
		},
		Controller: hostedstate.ControllerIdentity{
			RunID: 99, Attempt: 1, Event: "workflow_dispatch", WorkflowPath: ".github/workflows/hive-controller.yml",
			WorkflowSHA: strings.Repeat("3", 40), DefaultHeadSHA: strings.Repeat("4", 40),
		},
		CommandTimeout: 20 * time.Second,
	}
}

func TestCheckpointRoundTripExactParentAndPortableInventory(t *testing.T) {
	remote, checkout := newStateRepository(t)
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	writePrivate(t, runtimeRoot, ".hive-state-owner.json", `{"repository":"owner/repository"}`)
	writePrivate(t, runtimeRoot, "integrated/config.json", `{"schema":"test"}`)
	writePrivate(t, runtimeRoot, "integrated/audit.jsonl", `{"action":"setup"}`+"\n")
	writePrivate(t, runtimeRoot, "repair/repair-worker-state.json", `{"attempts":{}}`)
	writePrivate(t, runtimeRoot, "visual-hive/visual-hive-lifecycle.json", `{"findings":{}}`)
	writePrivate(t, runtimeRoot, "beads/quality/beads.json", `[]`)
	writePrivate(t, runtimeRoot, "integrated/checkout/target.txt", "must not persist")
	writePrivate(t, runtimeRoot, "repair/worktrees/attempt/source.txt", "must not persist")
	writePrivate(t, runtimeRoot, "visual-hive/artifacts/report.json", "must not persist")
	writePrivate(t, runtimeRoot, "integrated/production-run.lock.v2", "must not persist")
	writePrivate(t, runtimeRoot, "integrated/daemon.json", "must not persist")

	manager, err := Open(context.Background(), testConfig(checkout, runtimeRoot))
	if err != nil {
		t.Fatalf("Open(new) error = %v", err)
	}
	var callback checkpoint.Callback = manager.Callback
	if err := callback(context.Background(), "before GitHub API POST"); err != nil {
		t.Fatalf("Checkpoint(sequence 1) error = %v", err)
	}
	first := manager.HeadSHA()
	if manager.Sequence() != 1 || !validOID(first) {
		t.Fatalf("initial checkpoint = sequence %d, head %q", manager.Sequence(), first)
	}
	if got := strings.TrimSpace(runGit(t, checkout, "ls-remote", "--heads", "origin", "refs/heads/hive/state-1234abcd")); !strings.HasPrefix(got, first+"\t") {
		t.Fatalf("remote state head = %q, want %s", got, first)
	}
	if got := runGit(t, checkout, "ls-tree", "--name-only", first); strings.TrimSpace(got) != bundleFile {
		t.Fatalf("state commit tree = %q", got)
	}

	secondCheckout := filepath.Join(t.TempDir(), "checkout")
	runCommand(t, "", "git", "clone", "--branch", "hive/state-1234abcd", "--single-branch", remote, secondCheckout)
	secondRuntime := filepath.Join(t.TempDir(), "runtime")
	config := testConfig(secondCheckout, secondRuntime)
	config.Controller.RunID = 100
	manager, err = Open(context.Background(), config)
	if err != nil {
		t.Fatalf("Open(existing) error = %v", err)
	}
	for _, relative := range []string{
		".hive-state-owner.json", "integrated/config.json", "integrated/audit.jsonl", "repair/repair-worker-state.json",
		"visual-hive/visual-hive-lifecycle.json", "beads/quality/beads.json",
	} {
		if _, err := os.Stat(filepath.Join(secondRuntime, filepath.FromSlash(relative))); err != nil {
			t.Errorf("restored durable file %q: %v", relative, err)
		}
	}
	for _, relative := range []string{
		"integrated/checkout", "repair/worktrees", "visual-hive/artifacts", "integrated/production-run.lock.v2", "integrated/daemon.json",
	} {
		if _, err := os.Lstat(filepath.Join(secondRuntime, filepath.FromSlash(relative))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("ephemeral state %q was restored: %v", relative, err)
		}
	}
	writePrivate(t, secondRuntime, "integrated/workflow-dispatch.json", `{"phase":"accepted"}`)
	if err := manager.Checkpoint(context.Background(), "before repair branch push"); err != nil {
		t.Fatalf("Checkpoint(sequence 2) error = %v", err)
	}
	second := manager.HeadSHA()
	if manager.Sequence() != 2 || second == first {
		t.Fatalf("second checkpoint = sequence %d, head %q", manager.Sequence(), second)
	}
	parents := strings.Fields(strings.TrimSpace(runGit(t, secondCheckout, "rev-list", "--parents", "-n", "1", second)))
	if len(parents) != 2 || parents[1] != first {
		t.Fatalf("second checkpoint ancestry = %v, want parent %s", parents, first)
	}
}

func TestCheckpointFailsClosedOnRemoteRace(t *testing.T) {
	remote, checkout := newStateRepository(t)
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	writePrivate(t, runtimeRoot, "integrated/config.json", `{}`)
	manager, err := Open(context.Background(), testConfig(checkout, runtimeRoot))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Checkpoint(context.Background(), "initialize"); err != nil {
		t.Fatal(err)
	}
	original := manager.HeadSHA()

	clone := func(runID uint64) *Manager {
		dir := filepath.Join(t.TempDir(), "checkout")
		runCommand(t, "", "git", "clone", "--branch", "hive/state-1234abcd", "--single-branch", remote, dir)
		config := testConfig(dir, filepath.Join(t.TempDir(), "runtime"))
		config.Controller.RunID = runID
		value, err := Open(context.Background(), config)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	first, stale := clone(101), clone(102)
	writePrivate(t, first.config.RuntimeRoot, "integrated/merge-intent.json", `{"first":true}`)
	if err := first.Checkpoint(context.Background(), "winner"); err != nil {
		t.Fatal(err)
	}
	winner := first.HeadSHA()
	writePrivate(t, stale.config.RuntimeRoot, "integrated/merge-intent.json", `{"stale":true}`)
	if err := stale.Checkpoint(context.Background(), "loser"); err == nil || !strings.Contains(err.Error(), "compare-and-swap") {
		t.Fatalf("stale Checkpoint() error = %v", err)
	}
	if stale.HeadSHA() != original || stale.Sequence() != 1 {
		t.Fatalf("stale manager advanced to %s/%d", stale.HeadSHA(), stale.Sequence())
	}
	if got := strings.TrimSpace(runGit(t, first.config.CheckoutDir, "ls-remote", "--heads", "origin", "refs/heads/hive/state-1234abcd")); !strings.HasPrefix(got, winner+"\t") {
		t.Fatalf("race changed remote head: %q, want %s", got, winner)
	}
}

func TestOpenRejectsWrongReleaseAndExtraCommitContent(t *testing.T) {
	remote, checkout := newStateRepository(t)
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	writePrivate(t, runtimeRoot, "integrated/config.json", `{}`)
	manager, err := Open(context.Background(), testConfig(checkout, runtimeRoot))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Checkpoint(context.Background(), "initialize"); err != nil {
		t.Fatal(err)
	}

	wrongCheckout := cloneStateBranch(t, remote)
	wrong := testConfig(wrongCheckout, filepath.Join(t.TempDir(), "runtime"))
	wrong.Release.HiveCommit = strings.Repeat("a", 40)
	if _, err := Open(context.Background(), wrong); err == nil || !strings.Contains(err.Error(), "release") {
		t.Fatalf("Open(wrong release) error = %v", err)
	}

	tamper := cloneStateBranch(t, remote)
	runCommand(t, tamper, "git", "config", "user.name", "attacker")
	runCommand(t, tamper, "git", "config", "user.email", "attacker@example.com")
	writePrivate(t, tamper, "extra.txt", "unsigned extra")
	runCommand(t, tamper, "git", "add", "extra.txt")
	runCommand(t, tamper, "git", "commit", "-m", "add extra")
	runCommand(t, tamper, "git", "push", "origin", "HEAD:refs/heads/hive/state-1234abcd")
	verifier := cloneStateBranch(t, remote)
	if _, err := Open(context.Background(), testConfig(verifier, filepath.Join(t.TempDir(), "runtime"))); err == nil || !strings.Contains(err.Error(), "exactly state.json") {
		t.Fatalf("Open(extra commit content) error = %v", err)
	}
}

func TestExplicitReleaseTransitionAndStrictSequence(t *testing.T) {
	remote, checkout := newStateRepository(t)
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	writePrivate(t, runtimeRoot, "integrated/config.json", `{}`)
	oldConfig := testConfig(checkout, runtimeRoot)
	manager, err := Open(context.Background(), oldConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Checkpoint(context.Background(), "old release"); err != nil {
		t.Fatal(err)
	}

	upgradeCheckout := cloneStateBranch(t, remote)
	upgrade := testConfig(upgradeCheckout, filepath.Join(t.TempDir(), "runtime"))
	priorRelease := oldConfig.Release
	upgrade.RestoreRelease = &priorRelease
	upgrade.Release = hostedstate.ReleaseIdentity{
		Version: "v0.4.1-integrated.20", HiveCommit: strings.Repeat("a", 40), VisualHiveCommit: strings.Repeat("b", 40),
		DistributionManifestSHA256: strings.Repeat("c", 64), HostedControllerProtocol: 1,
	}
	upgrade.Controller.RunID++
	manager, err = Open(context.Background(), upgrade)
	if err != nil {
		t.Fatalf("Open(explicit release transition) error = %v", err)
	}
	if err := manager.Checkpoint(context.Background(), "release transition"); err != nil {
		t.Fatalf("Checkpoint(release transition) error = %v", err)
	}
	currentCheckout := cloneStateBranch(t, remote)
	current := upgrade
	current.CheckoutDir = currentCheckout
	current.RuntimeRoot = filepath.Join(t.TempDir(), "runtime")
	current.RestoreRelease = nil
	if _, err := Open(context.Background(), current); err != nil {
		t.Fatalf("Open(new release) error = %v", err)
	}

	// Build a correctly signed but skipped sequence on the exact current
	// parent. Open must still reject it; HMAC authenticity alone does not make
	// a non-monotonic controller history valid.
	manager, err = Open(context.Background(), func() Config {
		value := current
		value.CheckoutDir = cloneStateBranch(t, remote)
		value.RuntimeRoot = filepath.Join(t.TempDir(), "runtime")
		return value
	}())
	if err != nil {
		t.Fatal(err)
	}
	paths, err := PortableInventory(manager.config.RuntimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := hostedstate.Export(hostedstate.ExportOptions{
		Root: manager.config.RuntimeRoot, Paths: paths, Secret: manager.config.Secret,
		Metadata: hostedstate.Metadata{
			Repository: manager.config.Repository, StateBranch: manager.config.StateBranch, Sequence: 9,
			PriorStateCommit: manager.head, ExecutionOwner: "hosted", Release: manager.config.Release,
			Controller: manager.config.Controller, CreatedAt: time.Now().UTC(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	jump, err := manager.createCommit(context.Background(), encoded, 9, "invalid sequence jump")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.pushCAS(context.Background(), jump); err != nil {
		t.Fatal(err)
	}
	jumpCheckout := cloneStateBranch(t, remote)
	jumpConfig := current
	jumpConfig.CheckoutDir = jumpCheckout
	jumpConfig.RuntimeRoot = filepath.Join(t.TempDir(), "runtime")
	if _, err := Open(context.Background(), jumpConfig); err == nil || !strings.Contains(err.Error(), "exactly one greater") {
		t.Fatalf("Open(sequence jump) error = %v", err)
	}
}

func TestBootstrapSequenceTransitionsToHostedController(t *testing.T) {
	remote, checkout := newStateRepository(t)
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	writePrivate(t, runtimeRoot, "integrated/config.json", `{}`)
	config := testConfig(checkout, runtimeRoot)
	manager, err := Open(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := PortableInventory(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapController := config.Controller
	bootstrapController.RunID = 0
	bootstrapController.Attempt = 0
	bootstrapController.Event = "bootstrap"
	encoded, err := hostedstate.Export(hostedstate.ExportOptions{
		Root: runtimeRoot, Paths: paths, Secret: config.Secret,
		Metadata: hostedstate.Metadata{
			Repository: config.Repository, StateBranch: config.StateBranch, Sequence: 1,
			ExecutionOwner: "bootstrap", Release: config.Release, Controller: bootstrapController, CreatedAt: time.Now().UTC(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	bootstrapCommit, err := manager.createCommit(context.Background(), encoded, 1, "bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.pushCAS(context.Background(), bootstrapCommit); err != nil {
		t.Fatal(err)
	}

	hostedCheckout := cloneStateBranch(t, remote)
	hostedConfig := testConfig(hostedCheckout, filepath.Join(t.TempDir(), "runtime"))
	hosted, err := Open(context.Background(), hostedConfig)
	if err != nil {
		t.Fatalf("Open(bootstrap state) error = %v", err)
	}
	if hosted.Sequence() != 1 || hosted.HeadSHA() != bootstrapCommit {
		t.Fatalf("bootstrap restore = %d/%s", hosted.Sequence(), hosted.HeadSHA())
	}
	if err := hosted.Checkpoint(context.Background(), "first hosted controller barrier"); err != nil {
		t.Fatalf("Checkpoint(hosted sequence 2) error = %v", err)
	}
	if hosted.Sequence() != 2 {
		t.Fatalf("hosted sequence = %d, want 2", hosted.Sequence())
	}
	current, err := hostedstate.Decode([]byte(runGit(t, hostedCheckout, "show", hosted.HeadSHA()+":"+bundleFile)))
	if err != nil {
		t.Fatal(err)
	}
	if current.Metadata.ExecutionOwner != "hosted" || current.Metadata.Controller.RunID != hostedConfig.Controller.RunID {
		t.Fatalf("sequence 2 owner/controller = %q/%d", current.Metadata.ExecutionOwner, current.Metadata.Controller.RunID)
	}
}

func TestPortableInventoryRejectsUnknownLinksAndHardlinks(t *testing.T) {
	t.Run("unknown file", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "runtime")
		writePrivate(t, root, "integrated/config.json", `{}`)
		writePrivate(t, root, "credential.txt", "must not silently persist")
		if _, err := PortableInventory(root); err == nil || !strings.Contains(err.Error(), "unclassified") {
			t.Fatalf("PortableInventory() error = %v", err)
		}
	})

	t.Run("excluded link", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "runtime")
		writePrivate(t, root, "integrated/config.json", `{}`)
		outside := t.TempDir()
		link := filepath.Join(root, "integrated", "checkout")
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if _, err := PortableInventory(root); err == nil || !strings.Contains(err.Error(), "link") {
			t.Fatalf("PortableInventory() error = %v", err)
		}
	})

	t.Run("hardlink", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "runtime")
		writePrivate(t, root, "integrated/config.json", `{}`)
		source := filepath.Join(root, "integrated", "config.json")
		alias := filepath.Join(t.TempDir(), "alias")
		if err := os.Link(source, alias); err != nil {
			t.Skipf("hardlink unavailable: %v", err)
		}
		if _, err := PortableInventory(root); err == nil || !strings.Contains(err.Error(), "hard links") {
			t.Fatalf("PortableInventory() error = %v", err)
		}
	})
}

func newStateRepository(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	checkout := filepath.Join(root, "checkout")
	runCommand(t, "", "git", "init", "--bare", remote)
	runCommand(t, "", "git", "init", checkout)
	runCommand(t, checkout, "git", "remote", "add", "origin", remote)
	return remote, checkout
}

func cloneStateBranch(t *testing.T, remote string) string {
	t.Helper()
	checkout := filepath.Join(t.TempDir(), "checkout")
	runCommand(t, "", "git", "clone", "--branch", "hive/state-1234abcd", "--single-branch", remote, checkout)
	return checkout
}

func writePrivate(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	for current := filepath.Dir(path); strings.HasPrefix(filepath.Clean(current), filepath.Clean(root)); current = filepath.Dir(current) {
		if err := os.Chmod(current, 0o700); err != nil {
			t.Fatal(err)
		}
		if samePath(current, root) {
			break
		}
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return runCommand(t, dir, "git", args...)
}

func runCommand(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("%s timed out", name)
	}
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, output)
	}
	return string(output)
}

func TestPathsOverlapIsPortable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if !pathsOverlap(root, filepath.Join(root, "child")) || pathsOverlap(root, filepath.Join(t.TempDir(), "other")) {
		t.Fatal("path containment classification failed")
	}
	if runtime.GOOS == "windows" && !samePath(strings.ToUpper(root), strings.ToLower(root)) {
		t.Fatal("Windows path comparison is not case-insensitive")
	}
}

func TestPortableInventoryCarriesOnlyContentBoundRepairBundles(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "repair", "portable-git")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	name := strings.Repeat("a", 24) + "-r0-a1-validated-" + strings.Repeat("b", 24) + ".bundle"
	if err := os.WriteFile(filepath.Join(directory, name), []byte("portable git bundle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err := PortableInventory(root)
	if err != nil {
		t.Fatal(err)
	}
	want := "repair/portable-git/" + name
	if len(paths) != 1 || paths[0] != want {
		t.Fatalf("portable repair inventory=%v, want %s", paths, want)
	}
	if err := os.WriteFile(filepath.Join(directory, "unbound.bundle"), []byte("unsafe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PortableInventory(root); err == nil {
		t.Fatal("unbound repair bundle was accepted into portable state")
	}
}
