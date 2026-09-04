package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
)

// captureStdout runs fn while capturing os.Stdout, returning what was written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

// reloadStore re-opens the beads store at dir, returning a handle that reflects
// what is actually on disk right now.
//
// Every cmdX function opens its OWN store through openStore(), mutates it, and
// persists. A *beads.Store handle created earlier in a test holds the in-memory
// snapshot taken at NewStore time, and List/Ready read that map without ever
// re-reading the file — so asserting through the original handle observes the
// PRE-command state and reports a mutation that did happen as if it had not.
// Four tests here did exactly that and failed from the day they were added;
// nothing caught it because CI runs `go test ./pkg/...` and never ./cmd/....
//
// Re-opening is also the more faithful assertion: the real `bd` CLI is a
// separate process per invocation, so reading persisted state is what an
// operator actually observes.
func reloadStore(t *testing.T, dir string) *beads.Store {
	t.Helper()
	s, err := beads.NewStore(dir)
	if err != nil {
		t.Fatalf("reopening beads store at %s: %v", dir, err)
	}
	return s
}

// --- resolveDir ---

func TestResolveDirBDDIROverride(t *testing.T) {
	t.Setenv("BD_DIR", "/custom/beads")
	if got := resolveDir(); got != "/custom/beads" {
		t.Errorf("resolveDir() = %q; want /custom/beads", got)
	}
}

func TestResolveDirFallbackCwd(t *testing.T) {
	t.Setenv("BD_DIR", "")
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig)

	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	got := resolveDir()
	want, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("resolveDir() = %q; want %q (cwd fallback)", got, want)
	}
}

// --- ensureSlice ---

func TestEnsureSliceNil(t *testing.T) {
	result := ensureSlice(nil)
	if result == nil {
		t.Error("ensureSlice(nil) returned nil; want empty slice")
	}
	if len(result) != 0 {
		t.Errorf("ensureSlice(nil) len = %d; want 0", len(result))
	}
}

func TestEnsureSliceNonNil(t *testing.T) {
	input := []*beads.Bead{{ID: "abc"}}
	result := ensureSlice(input)
	if len(result) != 1 || result[0].ID != "abc" {
		t.Errorf("ensureSlice passthrough failed: got %v", result)
	}
}

// --- urlToName ---

func TestURLToName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"https://example.com/docs/guide.md", "guide"},
		{"https://example.com/docs/guide.html", "guide"},
		{"https://example.com/", "example.com"},
		{"https://example.com", "example.com"},
		{"not-a-url", "not-a-url"},
		{"https://example.com/path/to/readme.txt", "readme"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := urlToName(tt.input)
			if got != tt.want {
				t.Errorf("urlToName(%q) = %q; want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- dashboardURL ---

func TestDashboardURLDefault(t *testing.T) {
	t.Setenv("BD_DASHBOARD_URL", "")
	got := dashboardURL()
	if got != defaultDashboardURL {
		t.Errorf("dashboardURL() = %q; want %q", got, defaultDashboardURL)
	}
}

func TestDashboardURLOverride(t *testing.T) {
	t.Setenv("BD_DASHBOARD_URL", "http://custom:9000/")
	got := dashboardURL()
	if got != "http://custom:9000" {
		t.Errorf("dashboardURL() = %q; want http://custom:9000 (trailing slash trimmed)", got)
	}
}

// --- command handler tests ---

func TestCmdCreateJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BD_DIR", dir)

	out := captureStdout(t, func() {
		cmdCreate([]string{
			"--title", "test bead",
			"--type", "advisory",
			"--priority", "1",
			"--actor", "quality",
			"--external-ref", "pkg/foo.go",
		})
	})

	var b beads.Bead
	if err := json.Unmarshal([]byte(out), &b); err != nil {
		t.Fatalf("cmdCreate output is not valid JSON: %v\noutput: %s", err, out)
	}
	if b.Title != "test bead" {
		t.Errorf("title = %q; want 'test bead'", b.Title)
	}
	if b.Type != beads.TypeAdvisory {
		t.Errorf("type = %q; want advisory", b.Type)
	}
	if b.Priority != beads.PriorityHigh {
		t.Errorf("priority = %d; want 1", b.Priority)
	}
	if b.Actor != "quality" {
		t.Errorf("actor = %q; want quality", b.Actor)
	}
	if b.ExternalRef != "pkg/foo.go" {
		t.Errorf("external_ref = %q; want pkg/foo.go", b.ExternalRef)
	}
}

func TestCmdListJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BD_DIR", dir)

	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create("list-me", beads.TypeTask, beads.PriorityMedium, "quality", "")
	if err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdList([]string{"--json"})
	})

	var items []beads.Bead
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatalf("cmdList --json output is not valid JSON: %v\noutput: %s", err, out)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(items))
	}
	if items[0].ID != created.ID {
		t.Errorf("listed ID = %q; want %q", items[0].ID, created.ID)
	}
	if items[0].Title != "list-me" {
		t.Errorf("listed title = %q; want 'list-me'", items[0].Title)
	}
}

func TestCmdReadyJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BD_DIR", dir)

	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Create("open-bead", beads.TypeTask, beads.PriorityMedium, "quality", "")
	if err != nil {
		t.Fatal(err)
	}
	closed, err := store.Create("closed-bead", beads.TypeTask, beads.PriorityLow, "quality", "")
	if err != nil {
		t.Fatal(err)
	}
	store.Close(closed.ID)

	out := captureStdout(t, func() {
		cmdReady([]string{"--json"})
	})

	var items []beads.Bead
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatalf("cmdReady --json: %v\noutput: %s", err, out)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 ready bead, got %d", len(items))
	}
	if items[0].Title != "open-bead" {
		t.Errorf("ready bead title = %q; want 'open-bead'", items[0].Title)
	}
}

func TestCmdCloseAndReopenViaUpdate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BD_DIR", dir)

	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	b, err := store.Create("closable", beads.TypeAdvisory, beads.PriorityHigh, "quality", "")
	if err != nil {
		t.Fatal(err)
	}

	captureStdout(t, func() {
		cmdClose([]string{b.ID})
	})

	// Verify closed via list, re-read from disk (cmdClose used its own store).
	items := reloadStore(t, dir).List(beads.ListFilter{})
	found := false
	for _, item := range items {
		if item.ID == b.ID {
			if item.Status != beads.StatusClosed {
				t.Errorf("after Close, status = %q; want closed", item.Status)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("closed bead not found in list")
	}

	// Reopen via cmdUpdate.
	captureStdout(t, func() {
		cmdUpdate([]string{b.ID, "--status", "open"})
	})

	items = reloadStore(t, dir).List(beads.ListFilter{})
	found = false
	for _, item := range items {
		if item.ID == b.ID {
			if item.Status != beads.StatusOpen {
				t.Errorf("after reopen, status = %q; want open", item.Status)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("reopened bead not found in list")
	}
}

func TestCmdUpdateMetadata(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BD_DIR", dir)

	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	b, err := store.Create("meta-test", beads.TypeTask, beads.PriorityMedium, "quality", "")
	if err != nil {
		t.Fatal(err)
	}

	captureStdout(t, func() {
		cmdUpdate([]string{b.ID, "--set-metadata", "finding_type=coverage-gap"})
	})

	items := reloadStore(t, dir).List(beads.ListFilter{})
	found := false
	for _, item := range items {
		if item.ID == b.ID {
			if item.Meta("finding_type") != "coverage-gap" {
				t.Errorf("metadata finding_type = %q; want coverage-gap", item.Meta("finding_type"))
			}
			found = true
		}
	}
	if !found {
		t.Fatal("bead not found after SetMetadata")
	}

	captureStdout(t, func() {
		cmdUpdate([]string{b.ID, "--unset-metadata", "finding_type"})
	})

	items = reloadStore(t, dir).List(beads.ListFilter{})
	found = false
	for _, item := range items {
		if item.ID == b.ID {
			if item.Meta("finding_type") != "" {
				t.Errorf("after unset, metadata finding_type = %q; want empty", item.Meta("finding_type"))
			}
			found = true
		}
	}
	if !found {
		t.Fatal("bead not found after UnsetMetadata")
	}
}

func TestCmdUpdateClaim(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BD_DIR", dir)

	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	b, err := store.Create("claim-me", beads.TypeTask, beads.PriorityMedium, "quality", "")
	if err != nil {
		t.Fatal(err)
	}

	captureStdout(t, func() {
		cmdUpdate([]string{b.ID, "--claim"})
	})

	items := reloadStore(t, dir).List(beads.ListFilter{})
	found := false
	for _, item := range items {
		if item.ID == b.ID {
			if item.Status != beads.StatusInProgress {
				t.Errorf("after claim, status = %q; want in_progress", item.Status)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("bead not found after claim")
	}
}

func TestCmdReset(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BD_DIR", dir)

	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if _, err := store.Create("bead", beads.TypeTask, beads.PriorityMedium, "quality", ""); err != nil {
			t.Fatal(err)
		}
	}

	captureStdout(t, func() {
		cmdReset([]string{"--reason", "test reset"})
	})

	ready := reloadStore(t, dir).Ready("")
	if len(ready) != 0 {
		t.Errorf("after reset, Ready() returned %d beads; want 0", len(ready))
	}
}

func TestCmdRemember(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BD_DIR", dir)

	out := captureStdout(t, func() {
		cmdRemember([]string{"important", "fact", "here"})
	})

	if out == "" {
		t.Fatal("cmdRemember produced no output")
	}

	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	items := store.List(beads.ListFilter{})
	if len(items) != 1 {
		t.Fatalf("expected 1 bead after remember, got %d", len(items))
	}
	if items[0].Title != "important fact here" {
		t.Errorf("remembered title = %q; want 'important fact here'", items[0].Title)
	}
	if items[0].Type != beads.TypeAdvisory {
		t.Errorf("remembered type = %q; want advisory", items[0].Type)
	}
}

func TestCmdListTable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BD_DIR", dir)

	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	store.Create("table-bead", beads.TypeTask, beads.PriorityMedium, "quality", "")

	out := captureStdout(t, func() {
		cmdList([]string{})
	})

	if out == "" {
		t.Fatal("cmdList table produced no output")
	}
	if !bytes.Contains([]byte(out), []byte("table-bead")) {
		t.Errorf("table output missing bead title: %s", out)
	}
	if !bytes.Contains([]byte(out), []byte("1 bead(s)")) {
		t.Errorf("table output missing count: %s", out)
	}
}

func TestCmdListEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BD_DIR", dir)

	// Init the store so it exists.
	beads.NewStore(dir)

	out := captureStdout(t, func() {
		cmdList([]string{})
	})

	if !bytes.Contains([]byte(out), []byte("No beads found")) {
		t.Errorf("empty list should say 'No beads found'; got: %s", out)
	}
}

// --- regex pattern tests ---

func TestAgentDirPattern(t *testing.T) {
	tests := []struct {
		path  string
		match bool
	}{
		{"/home/dev/quality-beads", true},
		{"/data/beads/quality", true},
		{"/data/beads/sec-check", true},
		{"/home/dev/quality", false},
		{"/data/agents/quality", false},
		{"/tmp/beads", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := agentDirPattern.MatchString(tt.path)
			if got != tt.match {
				t.Errorf("agentDirPattern.MatchString(%q) = %v; want %v", tt.path, got, tt.match)
			}
		})
	}
}

func TestAgentWorkdirPattern(t *testing.T) {
	tests := []struct {
		path      string
		match     bool
		agentName string
	}{
		{"/data/agents/quality", true, "quality"},
		{"/data/agents/sec-check", true, "sec-check"},
		{"/data/agents/", false, ""},
		{"/data/beads/quality", false, ""},
		{"/home/dev/quality", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			m := agentWorkdirPattern.FindStringSubmatch(tt.path)
			if tt.match && m == nil {
				t.Errorf("agentWorkdirPattern did not match %q", tt.path)
			} else if !tt.match && m != nil {
				t.Errorf("agentWorkdirPattern matched %q unexpectedly", tt.path)
			}
			if tt.match && m != nil && m[1] != tt.agentName {
				t.Errorf("captured agent name = %q; want %q", m[1], tt.agentName)
			}
		})
	}
}
