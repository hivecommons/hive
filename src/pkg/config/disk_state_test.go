package config

import (
	"bytes"
	"context"
	"errors"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hivecommons/hive#19561 regression tests: a hand edit to the config file
// must survive the next persist, and the watcher must see it.

// diskStateTestTimeout bounds every wait on the real fsnotify watcher.
const diskStateTestTimeout = 10 * time.Second

// isolateDiskStateLayers points the PVC layer files at a fresh temp dir and
// keeps the test out of Kubernetes mode.
func isolateDiskStateLayers(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	origRuntime, origOverlay := RuntimeConfigFile, DashboardOverlayFile
	RuntimeConfigFile = filepath.Join(dir, "hive.yaml.runtime")
	DashboardOverlayFile = filepath.Join(dir, "hive.yaml.dashboard")
	t.Cleanup(func() { RuntimeConfigFile, DashboardOverlayFile = origRuntime, origOverlay })
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Cleanup(SetSATokenFileForTest(filepath.Join(dir, "no-such-sa-token")))
	return dir
}

func captureConfigLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	return &buf
}

func loadForDiskStateTest(t *testing.T, path string) *Config {
	t.Helper()
	cfg, err := LoadWithDashboardOverlay(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg
}

func TestSaveRefusesToOverwriteFileEditedSinceLoad(t *testing.T) {
	dir := isolateDiskStateLayers(t)
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML("loaded-org", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := loadForDiskStateTest(t, path)

	var notified []string
	SetExternalChangeObserver(func(p string) { notified = append(notified, p) })
	t.Cleanup(func() { SetExternalChangeObserver(nil) })
	logs := captureConfigLog(t)

	edited := []byte(minimalValidYAML("edited-org", "ghp_tok"))
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if p, changed := cfg.ChangedOnDisk(); !changed || p != ResolvedConfigPath(path) {
		t.Fatalf("ChangedOnDisk = (%q, %v), want (%q, true)", p, changed, path)
	}

	before := SaveSeq()
	if err := cfg.Save(); !errors.Is(err, ErrConfigChangedOnDisk) {
		t.Fatalf("Save = %v, want ErrConfigChangedOnDisk", err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, edited) {
		t.Fatalf("file was overwritten:\n%s", got)
	}
	if _, err := os.Stat(RuntimeConfigFile); !os.IsNotExist(err) {
		t.Fatalf("runtime config written by a refused save (stat err %v)", err)
	}
	if SaveSeq() != before {
		t.Fatal("a refused save counted as a write")
	}
	if len(notified) != 1 {
		t.Fatalf("external-change notifications = %v, want one", notified)
	}
	if !strings.Contains(logs.String(), "config persist skipped") {
		t.Fatalf("skip not logged:\n%s", logs.String())
	}

	// A second refusal of the same content is not logged again.
	logs.Reset()
	_ = cfg.Save()
	if strings.Contains(logs.String(), "config persist skipped") {
		t.Fatalf("skip logged twice for the same on-disk content:\n%s", logs.String())
	}

	// Once the edit is loaded the save goes through and keeps it.
	reloaded := loadForDiskStateTest(t, path)
	if err := reloaded.Save(); err != nil {
		t.Fatalf("Save after reload: %v", err)
	}
	if got := loadForDiskStateTest(t, path).Project.Org; got != "edited-org" {
		t.Fatalf("org after save = %q, want edited-org", got)
	}
}

// A second Config instance saving the same file is this process, not an
// operator; the first instance may still save over it as before.
func TestSaveAllowsOverwriteOfThisProcessesOwnWrite(t *testing.T) {
	dir := isolateDiskStateLayers(t)
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML("org-a", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}
	first := loadForDiskStateTest(t, path)
	second := loadForDiskStateTest(t, path)
	second.Project.Org = "org-b"
	if err := second.Save(); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	if _, changed := first.ChangedOnDisk(); changed {
		t.Fatal("this process's own write reported as an external change")
	}
	if err := first.Save(); err != nil {
		t.Fatalf("first Save: %v", err)
	}
}

// A config built in code (never loaded) has no baseline and saves as before.
func TestSaveWithoutLoadBaselineIsUnguarded(t *testing.T) {
	dir := isolateDiskStateLayers(t)
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML("on-disk", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		SourcePath: path,
		Project:    ProjectConfig{Org: "in-code", Repos: []string{"repo-a"}},
		GitHub:     GitHubConfig{Token: "ghp_tok"},
		AgentRuntimeConfig: AgentRuntimeConfig{
			Agents: map[string]AgentConfig{"worker": {Backend: "claude", Enabled: true}},
		},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// On Kubernetes the dashboard overlay's agents win over the config file, so
// an overlay edit must be protected too.
func TestSaveRefusesToOverwriteEditedDashboardOverlay(t *testing.T) {
	dir := isolateDiskStateLayers(t)
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML("org", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DashboardOverlayFile, []byte(minimalValidYAML("org", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := loadForDiskStateTest(t, path)

	edited := []byte(minimalValidYAML("org", "ghp_tok") + "    model: edited-model\n")
	if err := os.WriteFile(DashboardOverlayFile, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); !errors.Is(err, ErrConfigChangedOnDisk) {
		t.Fatalf("Save = %v, want ErrConfigChangedOnDisk", err)
	}
	if got, _ := os.ReadFile(DashboardOverlayFile); !bytes.Equal(got, edited) {
		t.Fatalf("overlay was overwritten:\n%s", got)
	}
}

// startTestWatcher runs a real watcher on path and returns the channel of
// reloaded orgs.
func startTestWatcher(t *testing.T, path string) <-chan string {
	t.Helper()
	reloaded := make(chan string, 16)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	w := NewWatcher(path, func(cfg *Config) { reloaded <- cfg.Project.Org }, logger)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.Start(ctx)
	// Write only once the directory watch is registered.
	<-w.ready
	return reloaded
}

func waitForReloadedOrg(t *testing.T, reloaded <-chan string, want string) {
	t.Helper()
	deadline := time.After(diskStateTestTimeout)
	for {
		select {
		case org := <-reloaded:
			if org == want {
				return
			}
		case <-deadline:
			t.Fatalf("watcher did not reload %q", want)
		}
	}
}

// The watch used to be on the file's inode, so the first Save (an atomic
// rename) ended it and later hand edits were never seen.
func TestWatcherSeesInPlaceRewriteAfterAtomicSave(t *testing.T) {
	dir := isolateDiskStateLayers(t)
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML("boot-org", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := loadForDiskStateTest(t, path)
	reloaded := startTestWatcher(t, path)

	cfg.Project.Org = "saved-org"
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// The save's own events must not reload; wait past the debounce.
	select {
	case org := <-reloaded:
		t.Fatalf("own save reloaded %q", org)
	case <-time.After(2 * debounceDelay):
	}

	// `cat new > file`: truncate and write the same inode.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(minimalValidYAML("in-place-org", "ghp_tok")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	waitForReloadedOrg(t, reloaded, "in-place-org")
}

func TestWatcherSeesRenameOntoConfigFile(t *testing.T) {
	dir := isolateDiskStateLayers(t)
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML("boot-org", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := startTestWatcher(t, path)

	for _, org := range []string{"renamed-org-1", "renamed-org-2"} {
		tmp := filepath.Join(dir, "hive.yaml.new")
		if err := os.WriteFile(tmp, []byte(minimalValidYAML(org, "ghp_tok")), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
		waitForReloadedOrg(t, reloaded, org)
	}
}

// An edit made a moment after a save (inside the debounce window of the
// save's own events) is still reloaded.
func TestWatcherDoesNotSwallowEditRightAfterSave(t *testing.T) {
	dir := isolateDiskStateLayers(t)
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML("boot-org", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := loadForDiskStateTest(t, path)
	reloaded := startTestWatcher(t, path)

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.WriteFile(path, []byte(minimalValidYAML("quick-edit-org", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForReloadedOrg(t, reloaded, "quick-edit-org")
}

func TestWatcherIgnoresOtherFilesInDirectory(t *testing.T) {
	dir := isolateDiskStateLayers(t)
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML("boot-org", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := startTestWatcher(t, path)
	if err := os.WriteFile(filepath.Join(dir, "governor-state.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case org := <-reloaded:
		t.Fatalf("unrelated file reloaded %q", org)
	case <-time.After(2 * debounceDelay):
	}
}

// The dashboard overlay is watched too (Start adds it on Kubernetes).
func TestWatcherSeesEditToAlsoWatchedFile(t *testing.T) {
	dir := isolateDiskStateLayers(t)
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML("boot-org", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}
	overlayDir := t.TempDir()
	overlay := filepath.Join(overlayDir, "hive.yaml.dashboard")

	reloaded := make(chan string, 16)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	w := NewWatcher(path, func(cfg *Config) { reloaded <- cfg.Project.Org }, logger)
	w.AlsoWatch(overlay)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.Start(ctx)
	<-w.ready

	// The test is outside Kubernetes, so the overlay's content is not
	// merged; the edit only has to trigger a reload of the config.
	if err := os.WriteFile(overlay, []byte("project: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForReloadedOrg(t, reloaded, "boot-org")
}

// Trigger reloads without any file event, for a save that refused to
// overwrite an edit the watcher missed.
func TestWatcherTriggerReloads(t *testing.T) {
	dir := isolateDiskStateLayers(t)
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML("triggered-org", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := make(chan string, 1)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	w := NewWatcher(path, func(cfg *Config) { reloaded <- cfg.Project.Org }, logger)
	w.Trigger()
	waitForReloadedOrg(t, reloaded, "triggered-org")
}
