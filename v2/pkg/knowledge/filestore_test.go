package knowledge

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func fileStoreTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// --- NewFileStore ---

func TestNewFileStore_ValidDir(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir, "test-vault", fileStoreTestLogger())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if fs.Name() != "test-vault" {
		t.Errorf("Name() = %q, want %q", fs.Name(), "test-vault")
	}
	if fs.RootDir() != dir {
		t.Errorf("RootDir() = %q, want %q", fs.RootDir(), dir)
	}
}

func TestNewFileStore_NonexistentDir(t *testing.T) {
	_, err := NewFileStore("/nonexistent/path/xyz", "bad", fileStoreTestLogger())
	if err == nil {
		t.Fatal("expected error for nonexistent dir")
	}
}

func TestNewFileStore_FileNotDir(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "filestore-test")
	if err := os.WriteFile(filePath, []byte("not a directory"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := NewFileStore(filePath, "bad", fileStoreTestLogger())
	if err == nil {
		t.Fatal("expected error for file path")
	}
}

// --- parseObsidianFile ---

func TestParseObsidianFile_PlainMarkdown(t *testing.T) {
	content := "# My Title\n\nSome body text here."
	title, body, tags, confidence, related := parseObsidianFile(content, "fallback")

	if title != "My Title" {
		t.Errorf("title = %q, want %q", title, "My Title")
	}
	if body != "Some body text here." {
		t.Errorf("body = %q, want %q", body, "Some body text here.")
	}
	if len(tags) != 0 {
		t.Errorf("tags = %v, want empty", tags)
	}
	if confidence != -1.0 {
		t.Errorf("confidence = %f, want -1.0 (sentinel)", confidence)
	}
	if len(related) != 0 {
		t.Errorf("related = %v, want empty", related)
	}
}

func TestParseObsidianFile_WithFrontmatter(t *testing.T) {
	content := `---
title: "Frontmatter Title"
confidence: 0.9
tags: [security, auth]
related: [slug-a, slug-b]
---
Body after frontmatter.`

	title, body, tags, confidence, related := parseObsidianFile(content, "fallback")

	if title != "Frontmatter Title" {
		t.Errorf("title = %q, want %q", title, "Frontmatter Title")
	}
	if body != "Body after frontmatter." {
		t.Errorf("body = %q, want %q", body, "Body after frontmatter.")
	}
	if len(tags) != 2 || tags[0] != "security" || tags[1] != "auth" {
		t.Errorf("tags = %v, want [security, auth]", tags)
	}
	if confidence != 0.9 {
		t.Errorf("confidence = %f, want 0.9", confidence)
	}
	if len(related) != 2 || related[0] != "slug-a" || related[1] != "slug-b" {
		t.Errorf("related = %v, want [slug-a, slug-b]", related)
	}
}

func TestParseObsidianFile_InlineTagsAndWikilinks(t *testing.T) {
	content := "Some text #inline-tag and a [[linked-page]] reference."
	_, body, tags, _, related := parseObsidianFile(content, "fallback")

	if body != content {
		t.Errorf("body = %q, want original content", body)
	}
	foundTag := false
	for _, tag := range tags {
		if tag == "inline-tag" {
			foundTag = true
		}
	}
	if !foundTag {
		t.Errorf("tags = %v, want to contain 'inline-tag'", tags)
	}
	foundRelated := false
	for _, r := range related {
		if r == "linked-page" {
			foundRelated = true
		}
	}
	if !foundRelated {
		t.Errorf("related = %v, want to contain 'linked-page'", related)
	}
}

func TestParseObsidianFile_FallbackTitle(t *testing.T) {
	content := "No heading here, just text."
	title, _, _, _, _ := parseObsidianFile(content, "my-fallback")
	if title != "my-fallback" {
		t.Errorf("title = %q, want %q", title, "my-fallback")
	}
}

// --- effectiveConfidence ---

func TestFileStoreEffectiveConfidence(t *testing.T) {
	if got := effectiveConfidence(0.7); got != 0.7 {
		t.Errorf("effectiveConfidence(0.7) = %f, want 0.7", got)
	}
	if got := effectiveConfidence(-1.0); got != defaultFactConfidence {
		t.Errorf("effectiveConfidence(-1) = %f, want %f", got, defaultFactConfidence)
	}
}

// --- containsTag ---

func TestContainsTag(t *testing.T) {
	tags := []string{"Security", "auth", "CI"}
	if !containsTag(tags, "security") {
		t.Error("expected case-insensitive match for 'security'")
	}
	if !containsTag(tags, "AUTH") {
		t.Error("expected case-insensitive match for 'AUTH'")
	}
	if containsTag(tags, "missing") {
		t.Error("expected no match for 'missing'")
	}
	if containsTag(nil, "anything") {
		t.Error("expected no match on nil slice")
	}
}

// --- termMatchBonus ---

func TestTermMatchBonus(t *testing.T) {
	p := filePage{
		Title: "Auth Module Overview",
		Tags:  []string{"security", "auth"},
	}
	bonus := termMatchBonus(p, []string{"auth"})
	if bonus <= 0 {
		t.Errorf("expected positive bonus for title+tag match, got %f", bonus)
	}

	noMatch := termMatchBonus(p, []string{"unrelated"})
	if noMatch != 0 {
		t.Errorf("expected zero bonus for no match, got %f", noMatch)
	}
}

// --- makeSigmaVec ---

func TestMakeSigmaVec(t *testing.T) {
	v := makeSigmaVec(4, 1.5)
	if len(v) != 4 {
		t.Fatalf("len = %d, want 4", len(v))
	}
	for i, val := range v {
		if val != 1.5 {
			t.Errorf("v[%d] = %f, want 1.5", i, val)
		}
	}
}

// --- Reindex + Search + ReadPage + ListPages + Stats ---

func writeMarkdown(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestFileStore_ReindexAndSearch(t *testing.T) {
	dir := t.TempDir()
	writeMarkdown(t, dir, "proxy-security.md", `---
title: "Proxy Security"
tags: [security, proxy]
confidence: 0.95
---
The proxy handles TLS interception and token injection.`)

	writeMarkdown(t, dir, "knowledge-search.md", `# Knowledge Search
This document covers search algorithms and ranking.`)

	fs, err := NewFileStore(dir, "test", fileStoreTestLogger())
	if err != nil {
		t.Fatal(err)
	}

	// Stats
	stats := fs.Stats()
	if stats.TotalPages != 2 {
		t.Errorf("TotalPages = %d, want 2", stats.TotalPages)
	}
	if stats.Name != "test" {
		t.Errorf("Name = %q, want %q", stats.Name, "test")
	}

	// Search
	results := fs.Search("proxy security TLS", 10)
	if len(results) == 0 {
		t.Fatal("expected search results for 'proxy security TLS'")
	}
	// The proxy-security page should rank highest
	if results[0].Slug != "proxy-security" {
		t.Errorf("top result slug = %q, want %q", results[0].Slug, "proxy-security")
	}

	// Search with empty query
	empty := fs.Search("", 10)
	if len(empty) != 0 {
		t.Errorf("expected no results for empty query, got %d", len(empty))
	}

	// Search with zero limit defaults to 20 and should still return matches.
	defaultLimitResults := fs.Search("proxy", 0)
	if len(defaultLimitResults) == 0 {
		t.Fatal("expected default-limit search to return matches")
	}
	if len(defaultLimitResults) > 20 {
		t.Fatalf("default-limit search returned %d results, want at most 20", len(defaultLimitResults))
	}
}

func TestFileStore_ReadPageDetailed(t *testing.T) {
	dir := t.TempDir()
	writeMarkdown(t, dir, "test-page.md", `---
title: "Test Page"
confidence: 0.85
---
Test body content.`)

	fs, err := NewFileStore(dir, "test", fileStoreTestLogger())
	if err != nil {
		t.Fatal(err)
	}

	fact, err := fs.ReadPage("test-page")
	if err != nil {
		t.Fatalf("ReadPage error: %v", err)
	}
	if fact.Title != "Test Page" {
		t.Errorf("Title = %q, want %q", fact.Title, "Test Page")
	}
	if fact.Confidence != 0.85 {
		t.Errorf("Confidence = %f, want 0.85", fact.Confidence)
	}

	// Missing page
	_, err = fs.ReadPage("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent page")
	}
}

func TestFileStore_ListPages(t *testing.T) {
	dir := t.TempDir()
	writeMarkdown(t, dir, "alpha.md", `---
title: "Alpha"
tags: [security]
---
Alpha body.`)
	writeMarkdown(t, dir, "beta.md", `---
title: "Beta"
tags: [testing]
---
Beta body.`)
	writeMarkdown(t, dir, "gamma.md", `---
title: "Gamma"
tags: [security, testing]
---
Gamma body.`)

	fs, err := NewFileStore(dir, "test", fileStoreTestLogger())
	if err != nil {
		t.Fatal(err)
	}

	// Unfiltered
	all := fs.ListPages("")
	if len(all) != 3 {
		t.Errorf("ListPages('') = %d pages, want 3", len(all))
	}

	// Filter by tag
	sec := fs.ListPages("security")
	if len(sec) != 2 {
		t.Errorf("ListPages('security') = %d, want 2", len(sec))
	}

	// Case-insensitive tag filter
	secUpper := fs.ListPages("SECURITY")
	if len(secUpper) != 2 {
		t.Errorf("ListPages('SECURITY') = %d, want 2 (case-insensitive)", len(secUpper))
	}

	// Nonexistent tag
	none := fs.ListPages("nonexistent-tag")
	if len(none) != 0 {
		t.Errorf("ListPages('nonexistent-tag') = %d, want 0", len(none))
	}

	// Verify sorted by title
	if len(all) >= 2 && all[0].Title > all[1].Title {
		t.Errorf("ListPages not sorted: %q > %q", all[0].Title, all[1].Title)
	}
}

// --- Access counts ---

func TestFileStore_AccessCounts(t *testing.T) {
	dir := t.TempDir()
	writeMarkdown(t, dir, "test.md", "# Test\nBody.")

	fs, err := NewFileStore(dir, "test", fileStoreTestLogger())
	if err != nil {
		t.Fatal(err)
	}

	if fs.AccessCount("test") != 0 {
		t.Errorf("initial access count = %d, want 0", fs.AccessCount("test"))
	}

	fs.RecordAccess("test")
	fs.RecordAccess("test")
	fs.RecordAccess("test")

	if fs.AccessCount("test") != 3 {
		t.Errorf("access count = %d, want 3", fs.AccessCount("test"))
	}
}

func TestFileStore_AccessCountPersistence(t *testing.T) {
	dir := t.TempDir()
	writeMarkdown(t, dir, "page.md", "# Page\nContent.")

	fs1, err := NewFileStore(dir, "test", fileStoreTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	fs1.RecordAccess("page")
	fs1.RecordAccess("page")
	fs1.persistAccessCounts()

	// Verify JSON was written
	data, err := os.ReadFile(filepath.Join(dir, accessCountsFile))
	if err != nil {
		t.Fatalf("access counts file not written: %v", err)
	}
	var counts map[string]int
	if err := json.Unmarshal(data, &counts); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if counts["page"] != 2 {
		t.Errorf("persisted count = %d, want 2", counts["page"])
	}

	// Create new store — should load persisted counts
	fs2, err := NewFileStore(dir, "test", fileStoreTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	if fs2.AccessCount("page") != 2 {
		t.Errorf("loaded access count = %d, want 2", fs2.AccessCount("page"))
	}
}

func TestFileStore_CorruptedAccessCounts(t *testing.T) {
	dir := t.TempDir()
	writeMarkdown(t, dir, "page.md", "# Page\nContent.")

	// Write corrupted JSON
	if err := os.WriteFile(filepath.Join(dir, accessCountsFile), []byte("not-json{{{"), 0644); err != nil {
		t.Fatal(err)
	}

	// Should not error — just reset to empty
	fs, err := NewFileStore(dir, "test", fileStoreTestLogger())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if fs.AccessCount("page") != 0 {
		t.Errorf("access count after corruption = %d, want 0", fs.AccessCount("page"))
	}
}

// --- SetName ---

func TestFileStore_SetName(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir, "original", fileStoreTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	fs.SetName("renamed")
	if fs.Name() != "renamed" {
		t.Errorf("Name() = %q, want %q", fs.Name(), "renamed")
	}
}

// --- Reindex skips hidden dirs and non-markdown ---

func TestFileStore_SkipsHiddenDirsAndNonMarkdown(t *testing.T) {
	dir := t.TempDir()

	// Hidden directory
	hiddenDir := filepath.Join(dir, ".obsidian")
	if err := os.MkdirAll(hiddenDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hiddenDir, "config.md"), []byte("# Hidden"), 0644); err != nil {
		t.Fatal(err)
	}

	// Non-markdown file
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("plain text"), 0644); err != nil {
		t.Fatal(err)
	}

	// Valid markdown
	writeMarkdown(t, dir, "valid.md", "# Valid\nContent.")

	fs, err := NewFileStore(dir, "test", fileStoreTestLogger())
	if err != nil {
		t.Fatal(err)
	}

	stats := fs.Stats()
	if stats.TotalPages != 1 {
		t.Errorf("TotalPages = %d, want 1 (should skip hidden + non-md)", stats.TotalPages)
	}
}

// --- Subdirectory slug paths ---

func TestFileStore_SubdirectorySlugs(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "security")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	writeMarkdown(t, dir, filepath.Join("security", "ssrf.md"), "# SSRF Guard\nContent about SSRF.")

	fs, err := NewFileStore(dir, "test", fileStoreTestLogger())
	if err != nil {
		t.Fatal(err)
	}

	fact, err := fs.ReadPage("security/ssrf")
	if err != nil {
		t.Fatalf("ReadPage('security/ssrf') error: %v", err)
	}
	if fact.Title != "SSRF Guard" {
		t.Errorf("Title = %q, want %q", fact.Title, "SSRF Guard")
	}
}

func TestFileStore_SkipsSymlinkedMarkdownOutsideRoot(t *testing.T) {
	dir := t.TempDir()
	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "secret.md")
	if err := os.WriteFile(outsidePath, []byte("# Outside Secret\nshould not be indexed"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsidePath, filepath.Join(dir, "linked-secret.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeMarkdown(t, dir, "inside.md", "# Inside\nindexed")

	fs, err := NewFileStore(dir, "test", fileStoreTestLogger())
	if err != nil {
		t.Fatal(err)
	}

	if stats := fs.Stats(); stats.TotalPages != 1 {
		t.Fatalf("TotalPages = %d, want only the in-root markdown page", stats.TotalPages)
	}
	if _, err := fs.ReadPage("linked-secret"); err == nil {
		t.Fatal("expected symlinked markdown outside root not to be readable")
	}
	if results := fs.Search("Outside Secret", 10); len(results) != 0 {
		t.Fatalf("search returned symlinked outside content: %#v", results)
	}
	if _, err := fs.ReadPage("../secret"); err == nil {
		t.Fatal("expected path-traversal-looking slug not to be readable")
	}
}

// --- Body snippet truncation ---

func TestFileStore_ListPagesSnippetTruncation(t *testing.T) {
	dir := t.TempDir()
	longBody := ""
	for i := 0; i < 300; i++ {
		longBody += "x"
	}
	writeMarkdown(t, dir, "long.md", "# Long\n"+longBody)

	fs, err := NewFileStore(dir, "test", fileStoreTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	pages := fs.ListPages("")
	if len(pages) != 1 {
		t.Fatalf("expected 1 page, got %d", len(pages))
	}
	if len([]rune(pages[0].Body)) > 210 {
		t.Errorf("body rune length = %d, expected truncation to ~200", len([]rune(pages[0].Body)))
	}
}
