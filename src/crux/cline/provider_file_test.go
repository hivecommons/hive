package cline

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScopedProviderFileIsolationAndCleanup(t *testing.T) {
	root, workspace := t.TempDir(), t.TempDir()
	t.Setenv("HOME", root)
	source := filepath.Join(root, "source")
	credentials(t, source, providerConfig)
	a, err := StageProviderFile(source, "openrouter/test", workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := StageProviderFile(source, "openrouter/test", workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	pa, err := a.MountFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := b.MountFile(workspace)
	if err != nil || pa == pb {
		t.Fatal("parallel provider stages collided", err)
	}
	raw, err := os.ReadFile(pa)
	if err != nil || strings.Contains(string(raw), "must-not-copy") {
		t.Fatal("full config copied", err)
	}
	if a.Redact("key=test-key") != "key=[REDACTED]" || strings.Contains(a.String(), "test-key") {
		t.Fatal("provider value not redacted")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pa); !os.IsNotExist(err) {
		t.Fatal("stage retained after close")
	}
	if _, err := a.MountFile(workspace); err == nil {
		t.Fatal("closed capability reusable")
	}
	if _, err := b.MountFile(workspace); err != nil {
		t.Fatal("one stage cleanup destroyed another", err)
	}
	original, err := os.ReadFile(filepath.Join(source, "data", "settings", "providers.json"))
	if err != nil || string(original) != providerConfig {
		t.Fatal("operator credential source changed")
	}
}

func TestScopedProviderFileDenials(t *testing.T) {
	root, workspace := t.TempDir(), t.TempDir()
	t.Setenv("HOME", root)
	source := filepath.Join(root, "source")
	credentials(t, source, providerConfig)
	for _, dir := range []string{"", "relative"} {
		if _, err := StageProviderFile(dir, "openrouter/test", workspace); err == nil {
			t.Fatal("implicit credential discovery accepted")
		}
	}
	credentials(t, workspace, providerConfig)
	link := filepath.Join(root, "workspace-source")
	if err := os.Symlink(workspace, link); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{workspace, link} {
		if _, err := StageProviderFile(dir, "openrouter/test", workspace); err == nil {
			t.Fatal("workspace credential source accepted")
		}
	}
	if _, err := StageProviderFile(source, "auto", workspace); err == nil {
		t.Fatal("implicit model accepted")
	}
	p, err := StageProviderFile(source, "openrouter/test", workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	file, _ := p.MountFile(workspace)
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := p.MountFile(workspace); err == nil {
		t.Fatal("world-readable stage accepted")
	}
	_ = os.Chmod(file, 0o600)
	if err := os.WriteFile(file, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.MountFile(workspace); err == nil {
		t.Fatal("changed stage accepted")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workspace, "must-preserve")
	if err := os.WriteFile(target, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, file); err != nil {
		t.Fatal(err)
	}
	if _, err := p.MountFile(workspace); err == nil {
		t.Fatal("symlinked stage accepted")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	preserved, _ := os.ReadFile(target)
	if string(preserved) != "safe" {
		t.Fatal("cleanup followed a symlink")
	}
	credentials(t, source, `{"version":1,"providers":{"openrouter":{"settings":{},"tokenSource":"oauth"}}}`)
	if _, err := StageProviderFile(source, "openrouter/test", workspace); err == nil {
		t.Fatal("OAuth stage accepted")
	}
	credentials(t, source, `{"version":1,"providers":{"openrouter":{"settings":{"apiKey":"dummy","refreshToken":"must-not-share"},"tokenSource":"manual"}}}`)
	if _, err := StageProviderFile(source, "openrouter/test", workspace); err == nil {
		t.Fatal("refresh token copied despite a manual label")
	}
}

func TestMountedProviderWorker(t *testing.T) {
	root, workspace := t.TempDir(), t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("HIVE_CLINE_CONFIG_DIR", "")
	source := filepath.Join(root, "source")
	credentials(t, source, providerConfig)
	p, err := StageProviderFile(source, "openrouter/test", workspace)
	if err != nil {
		t.Fatal(err)
	}

	defer p.Close()
	file, _ := p.MountFile(workspace)
	binary := fakeCLI(t, `cat >/dev/null; echo '{"type":"run_result","finishReason":"completed","text":"done"}'`)
	var out, errOut bytes.Buffer
	if code := Run([]string{"--cline", binary, "--model", "openrouter/test", "--provider-file", file,
		"--cwd", workspace, "--state-dir", filepath.Join(root, "state")}, strings.NewReader("bounded task"), &out, &errOut); code != 0 {
		t.Fatalf("mounted provider turn failed: %s", &errOut)
	}
	if !strings.Contains(out.String(), "done") {
		t.Fatal("turn did not complete")
	}
	if code := Run([]string{"--cline", binary, "--model", "openrouter/test", "--provider-file", file,
		"--provider-secret"}, nil, &out, &errOut); code == 0 {
		t.Fatal("mixed credential capabilities accepted")
	}
}

func TestProviderStagingRootAndRedactionFailures(t *testing.T) {
	root, workspace := t.TempDir(), t.TempDir()
	source := filepath.Join(root, "source")
	credentials(t, source, providerConfig)
	t.Setenv("HOME", "")
	if _, err := StageProviderFile(source, "openrouter/test", workspace); err == nil {
		t.Fatal("missing private home accepted")
	}
	blocked := filepath.Join(root, "blocked")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", blocked)
	if _, err := StageProviderFile(source, "openrouter/test", workspace); err == nil {
		t.Fatal("unwritable staging root accepted")
	}
	t.Setenv("HOME", workspace)
	if _, err := StageProviderFile(source, "openrouter/test", workspace); err == nil {
		t.Fatal("HOME in workspace accepted")
	}
	t.Setenv("HOME", root)
	parent := filepath.Join(root, ".local", "state", "crux", "cline", "provider-mounts")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := StageProviderFile(source, "openrouter/test", workspace); err == nil {
		t.Fatal("public staging root accepted")
	}
	if _, err := (&ProviderFile{}).MountFile(workspace); err == nil {
		t.Fatal("forged zero capability accepted")
	}
	p := &ProviderFile{secrets: credentialValues([]byte(`{"settings":{"apiKey":["short","short-long"],"tokenSource":"manual","model":"public"}}`))}
	if p.Redact("short-long short public manual") != "[REDACTED] [REDACTED] public manual" {
		t.Fatal("nested or overlapping secret redaction failed")
	}
}

func TestMountedProviderWorkerDenials(t *testing.T) {
	root, workspace := t.TempDir(), t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("HIVE_CLINE_CONFIG_DIR", "")
	source := filepath.Join(root, "source")
	credentials(t, source, providerConfig)
	p, err := StageProviderFile(source, "openrouter/test", workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	mounted, err := p.MountFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	narrowed, err := os.ReadFile(mounted)
	if err != nil {
		t.Fatal(err)
	}
	// A denied provider file must never reach inference: the stub leaves a
	// marker when launched, and none of the cases below may create it.
	launched := filepath.Join(root, "launched")
	binary := fakeCLI(t, "touch "+launched+`; cat >/dev/null; echo '{"type":"run_result","finishReason":"completed","text":"done"}'`)
	write := func(path string, body []byte) string {
		t.Helper()
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	inWorkspace := write(filepath.Join(workspace, "providers.json"), narrowed)
	viaSymlink := filepath.Join(root, "outside-link")
	if err := os.Symlink(inWorkspace, viaSymlink); err != nil {
		t.Fatal(err)
	}
	unreadable := filepath.Join(root, "directory-not-file")
	if err := os.Mkdir(unreadable, 0o700); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, file, want string
	}{
		{"inside workspace", inWorkspace, "outside the Git workspace"},
		{"symlink into workspace", viaSymlink, "outside the Git workspace"},
		{"missing", filepath.Join(root, "missing"), "cannot resolve"},
		{"unreadable", unreadable, "cannot read the mounted selected Cline provider"},
		{"unsupported schema", write(filepath.Join(root, "schema.json"), []byte(`{"version":2}`)), "unsupported Cline providers.json schema"},
		{"other provider only", write(filepath.Join(root, "other.json"), []byte(`{"version":1,"providers":{"other":{"settings":{"apiKey":"test-key"}}}}`)), "not configured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := Run([]string{"--cline", binary, "--model", "openrouter/test", "--provider-file", tc.file,
				"--cwd", workspace, "--state-dir", filepath.Join(root, "state")}, strings.NewReader("bounded task"), &out, &errOut)
			if code != 1 || !strings.Contains(errOut.String(), tc.want) {
				t.Fatalf("code %d, stderr %q", code, &errOut)
			}
			if strings.Contains(errOut.String(), "test-key") || strings.Contains(out.String(), "test-key") {
				t.Fatal("denial leaked the provider credential")
			}
			if _, err := os.Stat(launched); !os.IsNotExist(err) {
				t.Fatal("denied provider file still launched the Cline CLI")
			}
		})
	}
}
