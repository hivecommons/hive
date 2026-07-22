package integrated

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildDistributionCreatesSelfContainedImmutableTree(t *testing.T) {
	root := t.TempDir()
	visualCommit := strings.Repeat("a", 40)
	visualDir := writeTestVisualHiveRelease(t, filepath.Join(root, "visual"), visualCommit)
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(root, "node-runtime")
	nodeName := "node"
	if runtime.GOOS == "windows" {
		nodeName = "node.exe"
	}
	if err := copyRegularFile(testBinary, filepath.Join(runtimeDir, nodeName), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, relative := range requiredNodeRuntimeFiles(runtime.GOOS) {
		if relative == nodeName {
			continue
		}
		target := filepath.Join(runtimeDir, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("test runtime "+relative+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	nodeLicense := filepath.Join(root, "NODE-LICENSE")
	if err := os.WriteFile(nodeLicense, []byte("Node.js license\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(root, "skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: hive\ndescription: test\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(skillDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "agents", "openai.yaml"), []byte("name: Hive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "distribution")
	manifest, err := BuildDistribution(context.Background(), DistributionOptions{
		HiveBinary: testBinary, HiveCommit: strings.Repeat("b", 40), HiveVersion: "v0.4.1-integrated.11", VisualHiveDir: visualDir, VisualCommit: visualCommit,
		NodeBinary: testBinary, NodeRuntimeDir: runtimeDir, NodeLicense: nodeLicense, NodeVersion: "v22.23.1", SkillDir: skillDir, OutputDir: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != DistributionSchema || manifest.HiveVersion != "v0.4.1-integrated.11" || manifest.VisualHiveVersion != "0.2.0" ||
		manifest.HostedControllerProtocol != HostedControllerProtocol || len(manifest.Files) < 5 {
		t.Fatalf("unexpected distribution manifest: %+v", manifest)
	}
	hiveName := "hive"
	if runtime.GOOS == "windows" {
		hiveName = "hive.exe"
	}
	launcherPath := filepath.FromSlash(visualHiveLauncherPath(runtime.GOOS))
	for _, required := range []string{hiveName, filepath.Join("runtime", nodeName), filepath.Join("runtime", "LICENSE.node.txt"), filepath.Join("visual-hive", "visual-hive.mjs"), launcherPath, filepath.Join("skills", "hive", "SKILL.md"), filepath.Join("skills", "hive", "agents", "openai.yaml"), "distribution-manifest.json"} {
		if _, err := os.Stat(filepath.Join(output, required)); err != nil {
			t.Fatalf("missing distribution file %s: %v", required, err)
		}
	}
	launcherInventoried := false
	for _, file := range manifest.Files {
		if file.Path == filepath.ToSlash(launcherPath) {
			launcherInventoried = true
			break
		}
	}
	if !launcherInventoried {
		t.Fatalf("Visual Hive launcher %s is not bound into the release inventory", launcherPath)
	}
	if _, err := BuildDistribution(context.Background(), DistributionOptions{OutputDir: output}); err == nil || !strings.Contains(err.Error(), "commit") {
		t.Fatal("existing or incomplete distribution must not be overwritten")
	}
}

func TestVisualHiveLaunchersBindBundledRuntime(t *testing.T) {
	windowsRoot := filepath.Join(t.TempDir(), "Windows install with spaces")
	if err := writeVisualHiveLauncher(windowsRoot, "windows"); err != nil {
		t.Fatal(err)
	}
	windowsData, err := os.ReadFile(filepath.Join(windowsRoot, "visual-hive.cmd"))
	if err != nil {
		t.Fatal(err)
	}
	windowsLauncher := string(windowsData)
	for _, required := range []string{
		`%~dp0runtime\node.exe`,
		`%~dp0visual-hive\visual-hive.mjs`,
		`"%HIVE_NODE%" "%HIVE_VISUAL_ENTRYPOINT%" %*`,
		`exit /b %HIVE_VISUAL_EXIT%`,
		`bundled Node runtime is missing`,
		`Visual Hive entrypoint is missing`,
		`Reinstall the immutable integrated Hive release`,
	} {
		if !strings.Contains(windowsLauncher, required) {
			t.Fatalf("Windows Visual Hive launcher lost %q:\n%s", required, windowsLauncher)
		}
	}
	if strings.Contains(windowsLauncher, "\nnode ") || strings.Contains(windowsLauncher, "\r\nnode ") {
		t.Fatal("Windows Visual Hive launcher must not invoke a globally resolved node command")
	}

	if runtime.GOOS == "windows" {
		windowsLauncherPath := filepath.Join(windowsRoot, "visual-hive.cmd")
		assertFailure := func(want string) {
			t.Helper()
			command := exec.Command("cmd.exe", "/d", "/c", "call", windowsLauncherPath, "--version")
			output, runErr := command.CombinedOutput()
			exitErr, ok := runErr.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != 127 || !strings.Contains(string(output), want) || !strings.Contains(string(output), "Reinstall the immutable integrated Hive release") {
				t.Fatalf("Windows Visual Hive launcher failure was not actionable: err=%v output=%s", runErr, output)
			}
		}
		assertFailure("bundled Node runtime is missing")
		if err := os.MkdirAll(filepath.Join(windowsRoot, "runtime"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(windowsRoot, "runtime", "node.exe"), []byte("test placeholder\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		assertFailure("Visual Hive entrypoint is missing")
		return
	}
	linuxRoot := filepath.Join(t.TempDir(), "Linux install with spaces")
	if err := writeVisualHiveLauncher(linuxRoot, "linux"); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(linuxRoot, "bin", "visual-hive")
	node := filepath.Join(linuxRoot, "runtime", "node")
	entrypoint := filepath.Join(linuxRoot, "visual-hive", "visual-hive.mjs")
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(entrypoint), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeNode := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$HIVE_TEST_VISUAL_ARGS\"\nexit \"${HIVE_TEST_VISUAL_EXIT:-0}\"\n"
	if err := os.WriteFile(node, []byte(fakeNode), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entrypoint, []byte("// test Visual Hive entrypoint\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(t.TempDir(), "received arguments.txt")
	arguments := []string{"--flag", "two words", `quote"inside`}
	externalDir := filepath.Join(t.TempDir(), "external launcher with spaces")
	if err := os.MkdirAll(externalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	externalLauncher := filepath.Join(externalDir, "visual-hive")
	if err := os.Symlink(launcher, externalLauncher); err != nil {
		t.Fatal(err)
	}
	toolDir := filepath.Join(t.TempDir(), "restricted launcher tools")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dirname", "readlink"} {
		resolved, lookupErr := exec.LookPath(name)
		if lookupErr != nil {
			t.Fatal(lookupErr)
		}
		if err := os.Symlink(resolved, filepath.Join(toolDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command(externalLauncher, arguments...)
	command.Dir = t.TempDir()
	command.Env = append(os.Environ(), "PATH="+toolDir, "HIVE_TEST_VISUAL_ARGS="+argsFile, "HIVE_TEST_VISUAL_EXIT=37")
	output, runErr := command.CombinedOutput()
	exitErr, ok := runErr.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 37 {
		t.Fatalf("Linux Visual Hive launcher did not preserve the bundled process exit code: err=%v output=%s", runErr, output)
	}
	receivedData, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	received := strings.Split(strings.TrimSuffix(string(receivedData), "\n"), "\n")
	want := append([]string{entrypoint}, arguments...)
	if strings.Join(received, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("Linux Visual Hive launcher arguments = %#v, want %#v", received, want)
	}

	if err := os.Remove(node); err != nil {
		t.Fatal(err)
	}
	output, runErr = exec.Command(launcher, "--version").CombinedOutput()
	if exitErr, ok = runErr.(*exec.ExitError); !ok || exitErr.ExitCode() != 127 || !strings.Contains(string(output), "bundled Node runtime is missing or not executable") || !strings.Contains(string(output), "Reinstall the immutable integrated Hive release") {
		t.Fatalf("missing bundled Node failure was not actionable: err=%v output=%s", runErr, output)
	}
	if err := os.WriteFile(node, []byte(fakeNode), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(entrypoint); err != nil {
		t.Fatal(err)
	}
	output, runErr = exec.Command(launcher, "--version").CombinedOutput()
	if exitErr, ok = runErr.(*exec.ExitError); !ok || exitErr.ExitCode() != 127 || !strings.Contains(string(output), "Visual Hive entrypoint is missing") || !strings.Contains(string(output), "Reinstall the immutable integrated Hive release") {
		t.Fatalf("missing Visual Hive entrypoint failure was not actionable: err=%v output=%s", runErr, output)
	}
}

func TestValidateVisualHiveReleaseRejectsDigestAndTraversal(t *testing.T) {
	root := t.TempDir()
	commit := strings.Repeat("c", 40)
	visualDir := writeTestVisualHiveRelease(t, root, commit)
	entrypoint := filepath.Join(visualDir, "visual-hive.mjs")
	if _, err := ValidateVisualHiveRelease(entrypoint, commit); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entrypoint, []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateVisualHiveRelease(entrypoint, commit); err == nil {
		t.Fatal("tampered Visual Hive release was accepted")
	}

	visualDir = writeTestVisualHiveRelease(t, filepath.Join(t.TempDir(), "visual"), commit)
	manifestPath := filepath.Join(visualDir, "release-manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest VisualHiveReleaseManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Files[0].Path = "../escape"
	data, _ = json.Marshal(manifest)
	if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateVisualHiveRelease(filepath.Join(visualDir, "visual-hive.mjs"), commit); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("path traversal rejection = %v", err)
	}
}

func writeTestVisualHiveRelease(t *testing.T, root, commit string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	entrypoint := []byte("console.log('visual-hive')\n")
	if err := os.WriteFile(filepath.Join(root, "visual-hive.mjs"), entrypoint, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := VisualHiveReleaseManifest{
		SchemaVersion: VisualHiveReleaseSchema, Name: "visual-hive", Version: "0.2.0", GitCommit: commit, Node: ">=22",
		Entrypoint: "visual-hive.mjs", PlaywrightVersion: "1.60.0",
		Files: []VisualHiveReleaseFile{{Path: "visual-hive.mjs", SHA256: fmt.Sprintf("%x", sha256.Sum256(entrypoint)), Size: int64(len(entrypoint))}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "release-manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}
