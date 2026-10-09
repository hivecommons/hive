package knowledge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validSuggestion() Suggestion {
	return Suggestion{
		Action: SuggestAdd,
		Title:  "Retry flaky relay uploads",
		Body:   "Relay uploads fail under load; retry with backoff.",
		Type:   FactGotcha,
		Repo:   "acme/app",
		Layer:  LayerProject,
		Tags:   []string{"Relay", "relay", " ops "},
		Source: "https://github.com/acme/app/pull/12",
		Reason: "Observed while fixing #12",
	}
}

func writeEntry(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, DefaultSuggestionDir, filepath.FromSlash(name)+".md")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readEntry(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, DefaultSuggestionDir, filepath.FromSlash(name)+".md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestParseSuggestionAction(t *testing.T) {
	for _, in := range []string{"add", " UPDATE ", "replace", "Deprecate"} {
		if _, err := ParseSuggestionAction(in); err != nil {
			t.Errorf("%q: %v", in, err)
		}
	}
	if _, err := ParseSuggestionAction("delete"); err == nil {
		t.Fatal("delete accepted")
	}
}

func TestSuggestionNormalizeDefaults(t *testing.T) {
	s := validSuggestion()
	s.Normalize()
	if s.State != StateApproved {
		t.Fatalf("state = %q", s.State)
	}
	if strings.Join(s.Tags, ",") != "relay,ops" {
		t.Fatalf("tags = %v", s.Tags)
	}
	d := Suggestion{Action: "Deprecate", Target: " old.md "}
	d.Normalize()
	if d.State != StateDeprecated || d.Target != "old" || d.Action != SuggestDeprecate {
		t.Fatalf("deprecate normalized = %+v", d)
	}
}

func TestSuggestionValidate(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Suggestion)
		want string
	}{
		{"valid", func(*Suggestion) {}, ""},
		{"owner-repo source", func(s *Suggestion) { s.Source = "acme/app#12" }, ""},
		{"bare issue source", func(s *Suggestion) { s.Source = "#12" }, ""},
		{"bad action", func(s *Suggestion) { s.Action = "remove" }, "invalid suggestion action"},
		{"missing source", func(s *Suggestion) { s.Source = "" }, "source is required"},
		{"bad source", func(s *Suggestion) { s.Source = "trust me" }, "must be a PR/issue URL"},
		{"missing reason", func(s *Suggestion) { s.Reason = "" }, "reason is required"},
		{"bad state", func(s *Suggestion) { s.State = "live" }, "invalid lifecycle state"},
		{"superseded state", func(s *Suggestion) { s.State = StateSuperseded }, "action replace"},
		{"bad repo", func(s *Suggestion) { s.Repo = "acme" }, "owner/repo"},
		{"bad layer", func(s *Suggestion) { s.Layer = "galaxy" }, "invalid layer"},
		{"missing title", func(s *Suggestion) { s.Title = "" }, "title is required"},
		{"missing body", func(s *Suggestion) { s.Body = "" }, "body is required"},
		{"unsluggable title", func(s *Suggestion) { s.Title = "!!!" }, "no characters"},
		{"update needs target", func(s *Suggestion) { s.Action = SuggestUpdate }, "target is required"},
		{"traversal target", func(s *Suggestion) { s.Action = SuggestUpdate; s.Target = "../secrets" }, "clean relative path"},
		{"replace same entry", func(s *Suggestion) { s.Action = SuggestReplace; s.Target = "retry-flaky-relay-uploads" }, "use action update"},
		{"deprecate approved", func(s *Suggestion) { s.Action = SuggestDeprecate; s.Target = "old"; s.State = StateApproved }, "must propose state deprecated"},
	}
	for _, tc := range cases {
		s := validSuggestion()
		tc.mut(&s)
		s.Normalize()
		err := s.Validate()
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	d := Suggestion{Action: SuggestDeprecate, Target: "old", Source: "#1", Reason: "obsolete"}
	d.Normalize()
	if err := d.Validate(); err != nil {
		t.Fatalf("deprecate without title/body: %v", err)
	}
}

func TestSuggestionDedupeKeyAndCandidates(t *testing.T) {
	s := validSuggestion()
	if got := s.DedupeKey(); got != "flaky-relay-retry-uploads" {
		t.Fatalf("dedupe key = %q", got)
	}
	other := Suggestion{Title: "uploads: retry FLAKY relay"}
	if other.DedupeKey() != s.DedupeKey() {
		t.Fatalf("reordered title key %q != %q", other.DedupeKey(), s.DedupeKey())
	}
	existing := []KnowledgeEntry{
		{Name: "relay-upload-retries", Title: "Retry flaky relay upload"},
		{Name: "deploy", Title: "Deploy runbook"},
		{Name: "retry-flaky-relay-uploads", Title: "something else"},
	}
	got := FindDuplicateCandidates(s, existing)
	if len(got) != 2 || got[0].Name != "relay-upload-retries" || got[1].Name != "retry-flaky-relay-uploads" {
		t.Fatalf("candidates = %+v", got)
	}
	u := s
	u.Action, u.Target = SuggestUpdate, "relay-upload-retries"
	for _, c := range FindDuplicateCandidates(u, existing) {
		if c.Name == u.Target {
			t.Fatal("target reported as its own duplicate")
		}
	}
	if tokenSimilarity(nil, []string{"a"}) != 0 {
		t.Fatal("empty similarity should be 0")
	}
}

func TestRenderSuggestionEntry(t *testing.T) {
	s := validSuggestion()
	s.Action, s.Target = SuggestReplace, "old-relay"
	s.Reason = "line one\nstatus: approved"
	s.SuggestedBy = "agent-x"
	s.SuggestedAt = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	s.Normalize()
	out := RenderSuggestionEntry(s)
	for _, want := range []string{
		"title: Retry flaky relay uploads\n",
		"type: gotcha\n",
		"status: approved\n",
		"tags: relay, ops\n",
		"repo: acme/app\n",
		"layer: project\n",
		"source: https://github.com/acme/app/pull/12\n",
		"reason: line one status: approved\n",
		"supersedes: old-relay\n",
		"dedupe_key: flaky-relay-retry-uploads\n",
		"suggested_by: agent-x\n",
		"suggested_at: 2026-10-01T12:00:00Z\n",
		"---\n\nRelay uploads fail",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if frontmatterField(out, "status") != "approved" {
		t.Fatal("reason newline injected a status line")
	}
}

func TestWriteSuggestionAdd(t *testing.T) {
	root := t.TempDir()
	writeEntry(t, root, "relay-upload-retries", "---\ntitle: Retry flaky relay upload\n---\n\nold\n")
	res, err := WriteSuggestion(root, "", validSuggestion())
	if err != nil {
		t.Fatal(err)
	}
	if res.Entry != "retry-flaky-relay-uploads" || len(res.Files) != 1 || res.Files[0] != ".hive/wiki/retry-flaky-relay-uploads.md" {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Duplicates) != 1 || res.Duplicates[0] != "relay-upload-retries" {
		t.Fatalf("duplicates = %v", res.Duplicates)
	}
	if res.Branch != "knowledge/add-retry-flaky-relay-uploads" || !strings.HasPrefix(res.PRTitle, "📚 knowledge: add ") {
		t.Fatalf("branch/title = %q / %q", res.Branch, res.PRTitle)
	}
	for _, want := range []string{"Merging this PR approves it", "**Possible duplicates:** `relay-upload-retries`", "**Proposed status:** approved", "**Tags:** relay, ops"} {
		if !strings.Contains(res.PRBody, want) {
			t.Errorf("PR body missing %q:\n%s", want, res.PRBody)
		}
	}
	got := readEntry(t, root, "retry-flaky-relay-uploads")
	if frontmatterField(got, "related") != "relay-upload-retries" {
		t.Fatalf("related hint missing:\n%s", got)
	}
	if _, err := WriteSuggestion(root, "", validSuggestion()); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate add err = %v", err)
	}
}

func TestWriteSuggestionReplaceUpdateDeprecate(t *testing.T) {
	root := t.TempDir()
	writeEntry(t, root, "ops/old-relay", "---\ntitle: Old relay\nstatus: approved\nsource: #1\n---\n\nold body\n")

	s := validSuggestion()
	s.Action, s.Target = SuggestReplace, "ops/old-relay"
	res, err := WriteSuggestion(root, ".hive/wiki/", s)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("files = %v", res.Files)
	}
	old := readEntry(t, root, "ops/old-relay")
	if frontmatterField(old, "status") != "superseded" || frontmatterField(old, "superseded_by") != "retry-flaky-relay-uploads" ||
		frontmatterField(old, "source") != "#1" || frontmatterField(old, "status_reason") != "Observed while fixing #12" ||
		!strings.Contains(old, "old body") {
		t.Fatalf("superseded target:\n%s", old)
	}
	if frontmatterField(readEntry(t, root, "retry-flaky-relay-uploads"), "supersedes") != "ops/old-relay" {
		t.Fatal("replacement missing supersedes link")
	}
	if _, err := WriteSuggestion(root, "", s); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second replace err = %v", err)
	}

	u := validSuggestion()
	u.Action, u.Target, u.Body, u.State = SuggestUpdate, "retry-flaky-relay-uploads", "New body", StateDraft
	if _, err := WriteSuggestion(root, "", u); err != nil {
		t.Fatal(err)
	}
	updated := readEntry(t, root, "retry-flaky-relay-uploads")
	if !strings.Contains(updated, "New body") || frontmatterField(updated, "status") != "draft" {
		t.Fatalf("updated:\n%s", updated)
	}

	d := Suggestion{Action: SuggestDeprecate, Target: "retry-flaky-relay-uploads", Source: "acme/app#13", Reason: "relay removed"}
	res, err = WriteSuggestion(root, "", d)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || !strings.Contains(res.PRTitle, "deprecate retry-flaky-relay-uploads") {
		t.Fatalf("deprecate result = %+v", res)
	}
	dep := readEntry(t, root, "retry-flaky-relay-uploads")
	if frontmatterField(dep, "status") != "deprecated" || frontmatterField(dep, "status_source") != "acme/app#13" || !strings.Contains(dep, "New body") {
		t.Fatalf("deprecated:\n%s", dep)
	}
}

func TestWriteSuggestionErrors(t *testing.T) {
	root := t.TempDir()
	if _, err := WriteSuggestion(root, "", Suggestion{Action: SuggestAdd}); err == nil {
		t.Fatal("invalid suggestion accepted")
	}
	if _, err := WriteSuggestion(root, "../outside", validSuggestion()); err == nil || !strings.Contains(err.Error(), "knowledge dir") {
		t.Fatalf("traversal dir err = %v", err)
	}
	u := validSuggestion()
	u.Action, u.Target = SuggestUpdate, "missing"
	if _, err := WriteSuggestion(root, "", u); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing target err = %v", err)
	}
	entries, err := ReadKnowledgeEntries(filepath.Join(root, "nope"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("missing dir = %v, %v", entries, err)
	}
}

func TestReadKnowledgeEntriesTitleFallback(t *testing.T) {
	root := t.TempDir()
	writeEntry(t, root, "runbooks/deploy-app", "no front matter\n")
	writeEntry(t, root, "quoted", "---\ntitle: \"Quoted Title\"\n---\nbody\n")
	if err := os.WriteFile(filepath.Join(root, DefaultSuggestionDir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := ReadKnowledgeEntries(filepath.Join(root, DefaultSuggestionDir))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range entries {
		got[e.Name] = e.Title
	}
	if len(got) != 2 || got["runbooks/deploy-app"] != "deploy app" || got["quoted"] != "Quoted Title" {
		t.Fatalf("entries = %v", got)
	}
}
