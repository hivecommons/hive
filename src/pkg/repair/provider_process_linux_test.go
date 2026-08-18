//go:build linux

package repair

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/agent"
)

func TestCodexSpecialistStartsAcrossFilesystemWithSourceAdjacentSandboxAlias(t *testing.T) {
	tempRoot := t.TempDir()
	invocationRoot, err := os.MkdirTemp("/dev/shm", "hive-codex-cross-device-")
	if err != nil {
		t.Skipf("separate Linux tmpfs is unavailable: %v", err)
	}
	defer os.RemoveAll(invocationRoot)

	probe := filepath.Join(tempRoot, "cross-device-probe")
	if err := os.WriteFile(probe, []byte("probe"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkedProbe := filepath.Join(invocationRoot, "cross-device-probe")
	linkErr := os.Link(probe, linkedProbe)
	if linkErr == nil {
		_ = os.Remove(linkedProbe)
		t.Skip("/dev/shm and the Go temporary directory share a filesystem")
	}
	if !errors.Is(linkErr, syscall.EXDEV) {
		t.Skipf("cannot establish the cross-filesystem regression fixture: %v", linkErr)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMPDIR", tempRoot)
	t.Setenv("TMP", tempRoot)
	t.Setenv("TEMP", tempRoot)
	t.Setenv("GO_WANT_CODEX_PROVIDER_HELPER", "1")
	t.Setenv("HIVE_TEST_CODEX_MODEL_OUTPUT", "DENIED")
	executor, err := NewCodexSpecialistChildExecutor(CodexProvider{
		Command: os.Args[0], Prefix: []string{"--model", "proof-model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	identity, err := executor.Check(ctx)
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	process, err := executor.Start(ctx, agent.SpecialistChildExecutionRequest{
		Root: invocationRoot, Prompt: "Return DENIED", ContainmentProfile: agent.SpecialistContainmentProfileV1,
		AuthorizationID: identity.AuthorizationID, ProviderSHA256: identity.ProviderSHA256,
		ConfigurationSHA256: identity.ConfigurationSHA256, Model: identity.Model,
	})
	if err != nil {
		t.Fatalf("cross-filesystem specialist start failed: %v", err)
	}
	child, ok := process.(*codexSpecialistChildProcess)
	if !ok || child.process == nil || child.process.sealed == nil {
		t.Fatalf("specialist process did not expose its sealed runtime: %T", process)
	}
	sealedPath := child.process.sealed.SealedCommand.Path
	sealRoot := child.process.sealed.SealRoot
	aliasPath := child.process.sandboxAlias
	cwd := child.process.cwd
	sealedInfo, sealedErr := os.Lstat(sealedPath)
	aliasInfo, aliasErr := os.Lstat(aliasPath)
	if sealedErr != nil || aliasErr != nil || filepath.Dir(sealedPath) != sealRoot || filepath.Dir(aliasPath) != sealRoot ||
		!sealedInfo.Mode().IsRegular() || sealedInfo.Mode()&os.ModeSymlink != 0 || !aliasInfo.Mode().IsRegular() || aliasInfo.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(sealedInfo, aliasInfo) {
		t.Fatalf("sandbox alias is not the exact sealed provider inode: seal=%q alias=%q sealed_err=%v alias_err=%v", sealedPath, aliasPath, sealedErr, aliasErr)
	}
	if _, err := os.Lstat(filepath.Join(cwd, "codex-linux-sandbox")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sandbox alias remained in the durable specialist cwd: %v", err)
	}
	result, err := process.Wait()
	if err != nil || string(result.Response) != "DENIED" || result.TurnID != identity.AuthorizationID {
		t.Fatalf("cross-filesystem specialist result=%+v err=%v", result, err)
	}
	for name, path := range map[string]string{"sandbox alias": aliasPath, "provider seal": sealRoot, "neutral cwd": cwd} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s was not cleaned after exact wait: %v", name, err)
		}
	}
	entries, err := os.ReadDir(tempRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "hive-codex-provider-seals-") {
			t.Fatalf("source-adjacent sandbox alias prevented provider seal cleanup: %s", entry.Name())
		}
	}
}
