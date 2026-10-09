package knowledge

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCodeMapFixture(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func codeMapFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeCodeMapFixture(t, root, map[string]string{
		"go.mod":             "module example.com/demo\n\ngo 1.22\n",
		"Makefile":           "all:\n\tgo build ./...\n",
		".github/CODEOWNERS": "# owners\n/pkg/widget/ @alice @bob\n*.md @docs-team\n",
		"cmd/demo/main.go":   "package main\n\nfunc main() {}\n",
		"pkg/widget/widget.go": `package widget

// Widget is public.
type Widget struct{}

// Plugin is an extension point.
type Plugin interface{ Name() string }

type hidden struct{}

const MaxWidgets = 3

var defaultWidget = Widget{}

func NewWidget() *Widget { return &Widget{} }

func RegisterPlugin(p Plugin) {}

func helper() {}

func (w *Widget) Method() {}
`,
		"pkg/widget/widget_test.go":      "package widget\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n",
		"pkg/widget/testdata/sample.txt": "fixture\n",
		"scripts/deploy.sh":              "#!/bin/sh\necho deploy\n",
		"plugins/hello/hello.py":         "def hello():\n    return 1\n",
		"web/app.ts":                     "export const x = 1;\n",
		"web/app.test.ts":                "test('x', () => {});\n",
		"node_modules/dep/index.js":      "module.exports = {};\n",
		"vendor/lib/lib.go":              "package lib\n\nfunc Vendored() {}\n",
	})
	return root
}

func codeMapSection(t *testing.T, cm *CodeMap, title string) []string {
	t.Helper()
	for _, s := range cm.Sections {
		if s.Title == title {
			return s.Entries
		}
	}
	t.Fatalf("section %q missing", title)
	return nil
}

func entriesContain(entries []string, sub string) bool {
	for _, e := range entries {
		if strings.Contains(e, sub) {
			return true
		}
	}
	return false
}

func fixedNow() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

func TestGenerateCodeMapSummarizesFixture(t *testing.T) {
	root := codeMapFixture(t)
	cm, err := GenerateCodeMap(context.Background(), CodeMapOptions{Repo: "acme/demo", Root: root, SourceSHA: "abc123", Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if cm.SourceSHA != "abc123" || cm.GeneratorVersion != CodeMapGeneratorVersion || !cm.GeneratedAt.Equal(fixedNow()) {
		t.Fatalf("unexpected metadata: %+v", cm)
	}

	pkgs := codeMapSection(t, cm, CodeMapSectionPackages)
	if !entriesContain(pkgs, "pkg/widget — go package widget (1 files, 1 tests)") {
		t.Errorf("packages missing widget: %v", pkgs)
	}
	if !entriesContain(pkgs, "web — typescript 2") {
		t.Errorf("packages missing web: %v", pkgs)
	}
	if entriesContain(pkgs, "node_modules") || entriesContain(pkgs, "vendor") {
		t.Errorf("dependency dirs must be skipped: %v", pkgs)
	}

	entry := codeMapSection(t, cm, CodeMapSectionEntryPoints)
	for _, want := range []string{"cmd/demo — go main package", "scripts/deploy.sh", "Makefile", "go.mod"} {
		if !entriesContain(entry, want) {
			t.Errorf("entry points missing %q: %v", want, entry)
		}
	}

	owners := codeMapSection(t, cm, CodeMapSectionOwnership)
	if !entriesContain(owners, "CODEOWNERS: /pkg/widget/ → @alice @bob") {
		t.Errorf("ownership missing CODEOWNERS rule: %v", owners)
	}

	apis := codeMapSection(t, cm, CodeMapSectionPublicAPIs)
	if !entriesContain(apis, "pkg/widget (widget): MaxWidgets, NewWidget, Plugin, RegisterPlugin, Widget") {
		t.Errorf("public APIs wrong: %v", apis)
	}
	for _, e := range apis {
		if strings.Contains(e, "hidden") || strings.Contains(e, "helper") || strings.Contains(e, "Method") || strings.Contains(e, "defaultWidget") {
			t.Errorf("unexported or method leaked into public APIs: %q", e)
		}
		if strings.HasPrefix(e, "cmd/demo") {
			t.Errorf("main package listed as public API: %q", e)
		}
	}

	tests := codeMapSection(t, cm, CodeMapSectionTestLayout)
	for _, want := range []string{"pkg/widget — 1 test files", "pkg/widget/testdata/ — test directory", "web — 1 test files"} {
		if !entriesContain(tests, want) {
			t.Errorf("test layout missing %q: %v", want, tests)
		}
	}

	ext := codeMapSection(t, cm, CodeMapSectionExtensionPoint)
	for _, want := range []string{"interface widget.Plugin", "registry func widget.RegisterPlugin", "plugins/ — plugin/extension directory"} {
		if !entriesContain(ext, want) {
			t.Errorf("extension points missing %q: %v", want, ext)
		}
	}

	body := cm.Body()
	if !strings.HasPrefix(body, "# Code map: acme/demo\n") || !strings.Contains(body, "`abc123`") {
		t.Errorf("unexpected body header:\n%s", body)
	}
	if strings.Contains(body, CodeMapTruncatedMarker) {
		t.Errorf("small fixture must not be truncated:\n%s", body)
	}
}

func TestGenerateCodeMapIsDeterministic(t *testing.T) {
	root := codeMapFixture(t)
	opts := CodeMapOptions{Repo: "acme/demo", Root: root, SourceSHA: "abc", Now: fixedNow}
	a, err := GenerateCodeMap(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateCodeMap(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if a.Body() != b.Body() || a.ContentHash() != b.ContentHash() {
		t.Fatal("generation is not deterministic")
	}
}

func TestCodeMapCapsTruncateDeterministically(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	for i := 0; i < 120; i++ {
		files[fmt.Sprintf("pkg/p%03d/p.go", i)] = fmt.Sprintf("package p%03d\n\nfunc Exported%03d() {}\n", i, i)
	}
	writeCodeMapFixture(t, root, files)

	caps := CodeMapCaps{MaxEntriesPerSection: 10, MaxSectionBytes: 600, MaxTotalBytes: 2000}
	cm, err := GenerateCodeMap(context.Background(), CodeMapOptions{Repo: "big", Root: root, SourceSHA: "s", Caps: caps})
	if err != nil {
		t.Fatal(err)
	}
	body := cm.Body()
	if len(body) > caps.MaxTotalBytes {
		t.Fatalf("body is %d bytes, cap %d", len(body), caps.MaxTotalBytes)
	}
	if !strings.Contains(body, CodeMapTruncatedMarker) {
		t.Fatalf("expected truncation marker:\n%s", body)
	}
	if !strings.Contains(body, "- pkg/p000 — go package p000") {
		t.Errorf("truncation must keep the first sorted entries:\n%s", body)
	}
	if strings.Contains(body, "pkg/p119 — go package") {
		t.Errorf("last entry should have been truncated:\n%s", body)
	}
	if cm.Body() != body {
		t.Error("truncated render is not deterministic")
	}

	tiny := CodeMapCaps{MaxEntriesPerSection: 1000, MaxSectionBytes: 1 << 20, MaxTotalBytes: 1}
	cm.Caps = tiny
	small := cm.Body()
	if len(small) > codeMapMinTotalBytes {
		t.Fatalf("total cap floor not enforced: %d bytes", len(small))
	}
	if !strings.Contains(small, CodeMapTruncatedMarker) {
		t.Errorf("expected truncation marker under tiny cap:\n%s", small)
	}
}

func TestCodeMapCapsLimitEntriesPerSection(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	for i := 0; i < 8; i++ {
		files[fmt.Sprintf("d%d/x.go", i)] = fmt.Sprintf("package d%d\n", i)
	}
	writeCodeMapFixture(t, root, files)
	cm, err := GenerateCodeMap(context.Background(), CodeMapOptions{Repo: "r", Root: root, Caps: CodeMapCaps{MaxEntriesPerSection: 3}})
	if err != nil {
		t.Fatal(err)
	}
	body := cm.Body()
	if !strings.Contains(body, CodeMapTruncatedMarker+" 5 more entries omitted") {
		t.Errorf("expected 5 omitted packages:\n%s", body)
	}
}

func TestCodeMapNeedsRegeneration(t *testing.T) {
	cur := CodeMapMeta{GeneratorVersion: CodeMapGeneratorVersion, SourceSHA: "aaa"}
	cases := []struct {
		name   string
		meta   CodeMapMeta
		exists bool
		head   string
		want   bool
	}{
		{"missing", CodeMapMeta{}, false, "aaa", true},
		{"same sha", cur, true, "aaa", false},
		{"new sha", cur, true, "bbb", true},
		{"unknown head", cur, true, "", true},
		{"old generator", CodeMapMeta{GeneratorVersion: "0", SourceSHA: "aaa"}, true, "aaa", true},
	}
	for _, tc := range cases {
		if got, reason := CodeMapNeedsRegeneration(tc.meta, tc.exists, tc.head); got != tc.want {
			t.Errorf("%s: got %v (%s), want %v", tc.name, got, reason, tc.want)
		}
	}
}

func TestWriteCodeMapFreshnessAndIdempotence(t *testing.T) {
	root := codeMapFixture(t)
	vault := t.TempDir()
	gen := func(sha string, now time.Time) *CodeMap {
		cm, err := GenerateCodeMap(context.Background(), CodeMapOptions{Repo: "acme/demo", Root: root, SourceSHA: sha, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		return cm
	}
	t1 := fixedNow()
	p, written, err := WriteCodeMap(vault, gen("sha1", t1), "")
	if err != nil || !written {
		t.Fatalf("first write: written=%v err=%v", written, err)
	}
	if p != CodeMapPath(vault, "acme/demo") {
		t.Fatalf("path = %s", p)
	}
	meta, ok, err := ReadCodeMapMeta(p)
	if err != nil || !ok {
		t.Fatalf("meta: ok=%v err=%v", ok, err)
	}
	if meta.SourceSHA != "sha1" || meta.GeneratorVersion != CodeMapGeneratorVersion || !meta.GeneratedAt.Equal(t1) || meta.Repo != "acme/demo" || meta.ContentHash == "" {
		t.Fatalf("unexpected meta: %+v", meta)
	}
	first, _ := os.ReadFile(p)

	_, written, err = WriteCodeMap(vault, gen("sha1", t1.Add(time.Hour)), "")
	if err != nil || written {
		t.Fatalf("identical map must not be rewritten: written=%v err=%v", written, err)
	}
	again, _ := os.ReadFile(p)
	if string(first) != string(again) {
		t.Fatal("file changed on idempotent write")
	}

	_, written, err = WriteCodeMap(vault, gen("sha2", t1.Add(2*time.Hour)), "")
	if err != nil || !written {
		t.Fatalf("new sha must be recorded: written=%v err=%v", written, err)
	}
	meta, _, _ = ReadCodeMapMeta(p)
	if meta.SourceSHA != "sha2" || !meta.GeneratedAt.Equal(t1) {
		t.Fatalf("sha-only change should keep generated_at: %+v", meta)
	}

	writeCodeMapFixture(t, root, map[string]string{"pkg/extra/extra.go": "package extra\n\nfunc New() {}\n"})
	t3 := t1.Add(3 * time.Hour)
	_, written, err = WriteCodeMap(vault, gen("sha3", t3), "")
	if err != nil || !written {
		t.Fatalf("content change must be written: written=%v err=%v", written, err)
	}
	meta, _, _ = ReadCodeMapMeta(p)
	if !meta.GeneratedAt.Equal(t3) {
		t.Fatalf("content change should refresh generated_at: %+v", meta)
	}
}

func TestCodeMapServedAsRepoScopedKnowledge(t *testing.T) {
	root := codeMapFixture(t)
	vault := t.TempDir()
	cm, err := GenerateCodeMap(context.Background(), CodeMapOptions{Repo: "acme/demo", Root: root, SourceSHA: "abc", Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteCodeMap(vault, cm, LayerProject); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStore(vault, "code-maps", schedTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	f, err := store.ReadPage(CodeMapSlug("acme/demo"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Title != "Code map: acme/demo" || f.Type != FactReference || f.Layer != LayerProject || f.EffectiveState() != StateApproved {
		t.Fatalf("unexpected fact: title=%q type=%s layer=%s state=%s", f.Title, f.Type, f.Layer, f.EffectiveState())
	}
	if FactRepo(*f) != "acme/demo" {
		t.Fatalf("repo scope = %q, tags %v", FactRepo(*f), f.Tags)
	}
	if !(TOCScope{Repos: []string{"acme/demo"}}).InScope(*f) {
		t.Error("code map should be in scope for its repo")
	}
	if (TOCScope{Repos: []string{"other/repo"}}).InScope(*f) {
		t.Error("code map must be out of scope for other repos")
	}
}

func TestCodeMapSchedulerRunOnce(t *testing.T) {
	root := codeMapFixture(t)
	vault := t.TempDir()
	reindexed := 0
	cfg := CodeMapConfig{Enabled: boolPtr(true), Repos: []CodeMapRepoConfig{{Name: "acme/demo", Path: root}}}
	s := NewCodeMapScheduler(cfg, vault, func() { reindexed++ }, schedTestLogger())

	res := s.RunOnce(context.Background())
	if len(res) != 1 || res[0].Err != nil || !res[0].Written {
		t.Fatalf("first run: %+v", res)
	}
	if reindexed != 1 {
		t.Fatalf("reindex calls = %d, want 1", reindexed)
	}
	// Without git the revision is unknown, so the map is regenerated but the
	// unchanged content is not rewritten and no reindex happens.
	res = s.RunOnce(context.Background())
	if len(res) != 1 || res[0].Err != nil || res[0].Written {
		t.Fatalf("second run should be a no-op write: %+v", res)
	}
	if reindexed != 1 {
		t.Fatalf("reindex calls = %d, want 1", reindexed)
	}

	disabled := NewCodeMapScheduler(CodeMapConfig{Repos: cfg.Repos}, t.TempDir(), nil, schedTestLogger())
	if got := disabled.RunOnce(context.Background()); got != nil {
		t.Fatalf("disabled scheduler ran: %+v", got)
	}

	bad := NewCodeMapScheduler(CodeMapConfig{Enabled: boolPtr(true), Repos: []CodeMapRepoConfig{{Name: "x"}}}, t.TempDir(), nil, schedTestLogger())
	if got := bad.RunOnce(context.Background()); len(got) != 1 || got[0].Err == nil {
		t.Fatalf("repo without path/url must error: %+v", got)
	}
}

func TestCodeMapSchedulerRegeneratesOnNewHead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := codeMapFixture(t)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	head1 := CodeMapHeadSHA(context.Background(), root)
	if head1 == "" {
		t.Fatal("HEAD not resolved")
	}

	vault := t.TempDir()
	cfg := CodeMapConfig{Enabled: boolPtr(true), Repos: []CodeMapRepoConfig{{Name: "acme/demo", Path: root}}}
	s := NewCodeMapScheduler(cfg, vault, nil, schedTestLogger())
	if res := s.RunOnce(context.Background()); res[0].Err != nil || !res[0].Written || res[0].SourceSHA != head1 {
		t.Fatalf("first run: %+v", res)
	}
	body, _ := os.ReadFile(CodeMapPath(vault, "acme/demo"))
	if !strings.Contains(string(body), "churn: pkg/widget") {
		t.Errorf("expected churn hotspot from git history:\n%s", body)
	}

	if res := s.RunOnce(context.Background()); res[0].Regenerated || res[0].Written {
		t.Fatalf("unchanged HEAD must not regenerate: %+v", res)
	}

	writeCodeMapFixture(t, root, map[string]string{"pkg/more/more.go": "package more\n\nfunc More() {}\n"})
	git("add", "-A")
	git("commit", "-q", "-m", "more")
	res := s.RunOnce(context.Background())
	if !res[0].Regenerated || !res[0].Written || res[0].SourceSHA == head1 {
		t.Fatalf("new HEAD must regenerate: %+v", res)
	}
	meta, _, _ := ReadCodeMapMeta(CodeMapPath(vault, "acme/demo"))
	if meta.SourceSHA != res[0].SourceSHA {
		t.Fatalf("stored sha %q, want %q", meta.SourceSHA, res[0].SourceSHA)
	}
}

func TestCodeMapRepoName(t *testing.T) {
	cases := map[string]CodeMapRepoConfig{
		"explicit":         {Name: "explicit", Path: "/x/y"},
		"hivecommons/hive": {URL: "https://github.com/hivecommons/hive.git"},
		"checkout":         {Path: "/srv/checkout/"},
	}
	for want, in := range cases {
		if got := CodeMapRepoName(in); got != want {
			t.Errorf("CodeMapRepoName(%+v) = %q, want %q", in, got, want)
		}
	}
}
