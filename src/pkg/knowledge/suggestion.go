package knowledge

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Agent-suggested knowledge updates (#11105).
//
// A suggestion never changes what other agents see on its own. It is rendered
// as a change to a repository's carried knowledge directory (.hive/wiki by
// default) and travels in a normal pull request. The suggested content only
// becomes approved knowledge when a human merges that PR and the repository's
// knowledge is ingested from the reviewed branch; closing the PR rejects it.

// DefaultSuggestionDir is the repository-carried knowledge directory that
// suggestions are written into.
const DefaultSuggestionDir = ".hive/wiki"

// SuggestionAction is the kind of change an agent proposes.
type SuggestionAction string

const (
	// SuggestAdd proposes a new entry.
	SuggestAdd SuggestionAction = "add"
	// SuggestUpdate rewrites an existing entry in place.
	SuggestUpdate SuggestionAction = "update"
	// SuggestReplace proposes a new entry that supersedes an existing one.
	SuggestReplace SuggestionAction = "replace"
	// SuggestDeprecate marks an existing entry as out of date.
	SuggestDeprecate SuggestionAction = "deprecate"
)

// AllSuggestionActions lists every valid suggestion action.
var AllSuggestionActions = []SuggestionAction{SuggestAdd, SuggestUpdate, SuggestReplace, SuggestDeprecate}

// ParseSuggestionAction validates s (case-insensitive) as a suggestion action.
func ParseSuggestionAction(s string) (SuggestionAction, error) {
	a := SuggestionAction(strings.ToLower(strings.TrimSpace(s)))
	for _, valid := range AllSuggestionActions {
		if a == valid {
			return a, nil
		}
	}
	return "", fmt.Errorf("invalid suggestion action %q (want add, update, replace or deprecate)", s)
}

// Suggestion is an agent-proposed knowledge change awaiting human approval.
type Suggestion struct {
	Action SuggestionAction `json:"action"`
	Title  string           `json:"title,omitempty"`
	Body   string           `json:"body,omitempty"`
	Type   FactType         `json:"type,omitempty"`
	// State is the lifecycle state the entry should carry once the PR is
	// merged (draft, approved or deprecated). Superseded is expressed with
	// SuggestReplace so the replacement is always linked.
	State LifecycleState `json:"state,omitempty"`
	Repo  string         `json:"repo,omitempty"`
	Layer LayerType      `json:"layer,omitempty"`
	Tags  []string       `json:"tags,omitempty"`
	// Source cites where the knowledge came from: a PR/issue URL or an
	// owner/repo#N reference.
	Source string `json:"source"`
	Reason string `json:"reason"`
	// Target names the existing entry (path under the knowledge dir, without
	// .md) that update, replace or deprecate acts on.
	Target string `json:"target,omitempty"`
	// Related holds dedupe hints: existing entries that look like the same
	// knowledge, for the reviewer to check before merging.
	Related     []string  `json:"related,omitempty"`
	SuggestedBy string    `json:"suggested_by,omitempty"`
	SuggestedAt time.Time `json:"suggested_at"`
}

var (
	suggestionRepoRe   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	suggestionSourceRe = regexp.MustCompile(`^(https?://\S+|([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)?#[0-9]+)$`)
)

// Normalize trims fields and fills defaults: approved for new content,
// deprecated for SuggestDeprecate, and a cleaned, de-duplicated tag list.
func (s *Suggestion) Normalize() {
	s.Action = SuggestionAction(strings.ToLower(strings.TrimSpace(string(s.Action))))
	s.Title = strings.TrimSpace(s.Title)
	s.Body = strings.TrimSpace(s.Body)
	s.Type = FactType(strings.ToLower(strings.TrimSpace(string(s.Type))))
	s.State = LifecycleState(strings.ToLower(strings.TrimSpace(string(s.State))))
	s.Repo = strings.TrimSpace(s.Repo)
	s.Layer = LayerType(strings.ToLower(strings.TrimSpace(string(s.Layer))))
	s.Source = strings.TrimSpace(s.Source)
	s.Reason = strings.TrimSpace(s.Reason)
	s.Target = strings.Trim(strings.TrimSuffix(strings.TrimSpace(s.Target), ".md"), "/")
	s.SuggestedBy = strings.TrimSpace(s.SuggestedBy)
	if s.State == "" {
		if s.Action == SuggestDeprecate {
			s.State = StateDeprecated
		} else {
			s.State = StateApproved
		}
	}
	seen := map[string]bool{}
	tags := make([]string, 0, len(s.Tags))
	for _, t := range s.Tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		tags = append(tags, t)
	}
	s.Tags = tags
}

// Validate checks that the suggestion carries everything a reviewer needs:
// a citation, a reason, a valid proposed lifecycle state and, for changes to
// existing knowledge, the entry they target.
func (s Suggestion) Validate() error {
	if _, err := ParseSuggestionAction(string(s.Action)); err != nil {
		return err
	}
	if s.Source == "" {
		return fmt.Errorf("source is required (PR/issue URL or owner/repo#N)")
	}
	if !suggestionSourceRe.MatchString(s.Source) {
		return fmt.Errorf("source %q must be a PR/issue URL or an owner/repo#N reference", s.Source)
	}
	if s.Reason == "" {
		return fmt.Errorf("reason is required")
	}
	st, err := ParseLifecycleState(string(s.State))
	if err != nil {
		return err
	}
	if st == StateSuperseded {
		return fmt.Errorf("propose superseded knowledge with action replace so the replacement is linked")
	}
	if s.Action == SuggestDeprecate && st != StateDeprecated {
		return fmt.Errorf("deprecate suggestions must propose state deprecated, not %s", st)
	}
	if s.Repo != "" && (!suggestionRepoRe.MatchString(s.Repo) || strings.Contains(s.Repo, "..")) {
		return fmt.Errorf("repo %q must look like owner/repo", s.Repo)
	}
	if s.Layer != "" && s.Layer.Precedence() == 99 {
		return fmt.Errorf("invalid layer %q (want personal, project, org or community)", s.Layer)
	}
	if s.Action != SuggestAdd {
		if s.Target == "" {
			return fmt.Errorf("target is required for %s suggestions", s.Action)
		}
		if err := validEntryName(s.Target); err != nil {
			return fmt.Errorf("target: %w", err)
		}
	}
	if s.Action != SuggestDeprecate {
		if s.Title == "" {
			return fmt.Errorf("title is required for %s suggestions", s.Action)
		}
		if s.Body == "" {
			return fmt.Errorf("body is required for %s suggestions", s.Action)
		}
		if s.Name() == "" {
			return fmt.Errorf("title %q has no characters usable in an entry name", s.Title)
		}
	}
	if s.Action == SuggestReplace && s.Name() == s.Target {
		return fmt.Errorf("replacement title maps to the target entry %q; use action update to edit it in place", s.Target)
	}
	return nil
}

// Name is the entry the suggestion writes: the target for update and
// deprecate, otherwise a slug derived from the title.
func (s Suggestion) Name() string {
	if s.Action == SuggestUpdate || s.Action == SuggestDeprecate {
		return s.Target
	}
	return slugify(s.Title)
}

// DedupeKey is an order-insensitive normalisation of the title used to spot
// suggestions that restate existing knowledge.
func (s Suggestion) DedupeKey() string {
	return strings.Join(titleTokens(s.Title), "-")
}

func titleTokens(title string) []string {
	seen := map[string]bool{}
	var out []string
	for _, tok := range strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		if len(tok) < 2 || seen[tok] {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
	}
	sort.Strings(out)
	return out
}

func validEntryName(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\\n\r") ||
		path.Clean(name) != name || strings.HasPrefix(name, "..") || strings.Contains(name, "/../") || strings.HasSuffix(name, "/..") {
		return fmt.Errorf("entry name %q must be a clean relative path under the knowledge dir", name)
	}
	return nil
}

// KnowledgeEntry is an existing file in a repository's knowledge directory.
type KnowledgeEntry struct {
	Name  string `json:"name"`
	Title string `json:"title"`
}

// FindDuplicateCandidates returns existing entries that look like the same
// knowledge as s: the same entry name, the same dedupe key, or a title whose
// tokens overlap by at least half. The suggestion's own target is excluded.
func FindDuplicateCandidates(s Suggestion, existing []KnowledgeEntry) []KnowledgeEntry {
	want := titleTokens(s.Title)
	name := s.Name()
	var out []KnowledgeEntry
	for _, e := range existing {
		if e.Name == s.Target && s.Action != SuggestAdd {
			continue
		}
		if e.Name == name || tokenSimilarity(want, titleTokens(e.Title)) >= 0.5 {
			out = append(out, e)
		}
	}
	return out
}

func tokenSimilarity(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	set := make(map[string]bool, len(a))
	for _, t := range a {
		set[t] = true
	}
	inter := 0
	for _, t := range b {
		if set[t] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

// ReadKnowledgeEntries lists the markdown entries under dir. A missing
// directory yields no entries.
func ReadKnowledgeEntries(dir string) ([]KnowledgeEntry, error) {
	var out []KnowledgeEntry
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && p == dir {
				return fs.SkipAll
			}
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() || !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(filepath.ToSlash(rel), filepath.Ext(rel))
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		title := frontmatterField(string(data), "title")
		if title == "" {
			title = strings.ReplaceAll(path.Base(name), "-", " ")
		}
		out = append(out, KnowledgeEntry{Name: name, Title: title})
		return nil
	})
	return out, err
}

func frontmatterField(content, key string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(content, "---\n") {
		return ""
	}
	for _, line := range strings.Split(content[4:], "\n") {
		if strings.TrimSpace(line) == "---" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), key) {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// RenderSuggestionEntry renders the entry file a suggestion adds or rewrites,
// using flat `key: value` front matter that repository knowledge ingestion
// reads (status, title, tags) plus the provenance a reviewer needs.
func RenderSuggestionEntry(s Suggestion) string {
	var b strings.Builder
	b.WriteString("---\n")
	line := func(k, v string) {
		if v = sanitizeFrontmatterValue(v); v != "" {
			fmt.Fprintf(&b, "%s: %s\n", k, v)
		}
	}
	line("title", s.Title)
	line("type", string(s.Type))
	line("status", string(s.State))
	line("tags", sanitizeFrontmatterList(s.Tags))
	line("repo", s.Repo)
	line("layer", string(s.Layer))
	line("source", s.Source)
	line("reason", s.Reason)
	if s.Action == SuggestReplace {
		line("supersedes", s.Target)
	}
	line("related", sanitizeFrontmatterList(s.Related))
	line("dedupe_key", s.DedupeKey())
	line("suggested_by", s.SuggestedBy)
	if !s.SuggestedAt.IsZero() {
		line("suggested_at", s.SuggestedAt.UTC().Format(time.RFC3339))
	}
	b.WriteString("---\n\n")
	b.WriteString(s.Body)
	b.WriteString("\n")
	return b.String()
}

// markTargetEntry rewrites the front matter of the entry a replace or
// deprecate suggestion acts on, leaving its body and original provenance
// untouched; the status change carries its own source and reason.
func markTargetEntry(content string, s Suggestion) string {
	kv := map[string]string{"status_source": s.Source, "status_reason": s.Reason}
	switch s.Action {
	case SuggestReplace:
		kv["status"] = string(StateSuperseded)
		kv["superseded_by"] = s.Name()
	case SuggestDeprecate:
		kv["status"] = string(StateDeprecated)
	}
	return upsertFrontmatter(strings.ReplaceAll(content, "\r\n", "\n"), kv)
}

// SuggestionResult describes the files a suggestion changed and the pull
// request that should carry them for human approval.
type SuggestionResult struct {
	Entry      string   `json:"entry"`
	Files      []string `json:"files"`
	Duplicates []string `json:"duplicates,omitempty"`
	Branch     string   `json:"branch"`
	PRTitle    string   `json:"pr_title"`
	PRBody     string   `json:"pr_body"`
}

// WriteSuggestion validates s, records dedupe hints from the existing entries
// in repoRoot/dir, writes the proposed change into the working tree and
// returns the PR that should carry it. It never touches approved knowledge
// outside the working tree: approval is the human merge of that PR.
func WriteSuggestion(repoRoot, dir string, s Suggestion) (SuggestionResult, error) {
	s.Normalize()
	if err := s.Validate(); err != nil {
		return SuggestionResult{}, err
	}
	dir = strings.Trim(filepath.ToSlash(strings.TrimSpace(dir)), "/")
	if dir == "" {
		dir = DefaultSuggestionDir
	}
	if err := validEntryName(dir); err != nil {
		return SuggestionResult{}, fmt.Errorf("knowledge dir: %w", err)
	}
	root := filepath.Join(repoRoot, filepath.FromSlash(dir))
	existing, err := ReadKnowledgeEntries(root)
	if err != nil {
		return SuggestionResult{}, fmt.Errorf("reading %s: %w", dir, err)
	}
	exists := map[string]bool{}
	for _, e := range existing {
		exists[e.Name] = true
	}
	name := s.Name()
	switch s.Action {
	case SuggestAdd:
		if exists[name] {
			return SuggestionResult{}, fmt.Errorf("entry %s already exists; suggest update or replace with --target %s", name, name)
		}
	case SuggestReplace:
		if exists[name] {
			return SuggestionResult{}, fmt.Errorf("replacement entry %s already exists", name)
		}
		fallthrough
	default:
		if !exists[s.Target] {
			return SuggestionResult{}, fmt.Errorf("target entry %s not found under %s", s.Target, dir)
		}
	}
	var dupes []string
	for _, e := range FindDuplicateCandidates(s, existing) {
		dupes = append(dupes, e.Name)
	}
	s.Related = append(s.Related, dupes...)

	var files []string
	write := func(entry, content string) error {
		p := filepath.Join(root, filepath.FromSlash(entry)+".md")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		tmp := p + ".tmp"
		if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, p); err != nil {
			return err
		}
		files = append(files, path.Join(dir, entry+".md"))
		return nil
	}
	if s.Action == SuggestReplace || s.Action == SuggestDeprecate {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(s.Target)+".md"))
		if err != nil {
			return SuggestionResult{}, fmt.Errorf("reading target %s: %w", s.Target, err)
		}
		if err := write(s.Target, markTargetEntry(string(data), s)); err != nil {
			return SuggestionResult{}, fmt.Errorf("writing target %s: %w", s.Target, err)
		}
	}
	if s.Action != SuggestDeprecate {
		if err := write(name, RenderSuggestionEntry(s)); err != nil {
			return SuggestionResult{}, fmt.Errorf("writing entry %s: %w", name, err)
		}
	}
	sort.Strings(files)
	return SuggestionResult{
		Entry:      name,
		Files:      files,
		Duplicates: dupes,
		Branch:     "knowledge/" + string(s.Action) + "-" + strings.ReplaceAll(name, "/", "-"),
		PRTitle:    suggestionPRTitle(s),
		PRBody:     SuggestionPRBody(s, files, dupes),
	}, nil
}

func suggestionPRTitle(s Suggestion) string {
	subject := s.Title
	if subject == "" {
		subject = s.Target
	}
	return fmt.Sprintf("📚 knowledge: %s %s", s.Action, sanitizeFrontmatterValue(subject))
}

// SuggestionPRBody renders the review description for a suggestion's PR.
func SuggestionPRBody(s Suggestion, files, dupes []string) string {
	var b strings.Builder
	b.WriteString("Agent-suggested knowledge update. Merging this PR approves it; closing it rejects it.\n\n")
	fmt.Fprintf(&b, "- **Action:** %s\n", s.Action)
	if s.Target != "" {
		fmt.Fprintf(&b, "- **Target:** `%s`\n", s.Target)
	}
	fmt.Fprintf(&b, "- **Proposed status:** %s\n", s.State)
	fmt.Fprintf(&b, "- **Source:** %s\n", s.Source)
	fmt.Fprintf(&b, "- **Reason:** %s\n", sanitizeFrontmatterValue(s.Reason))
	if s.Repo != "" {
		fmt.Fprintf(&b, "- **Repo:** %s\n", s.Repo)
	}
	if s.Layer != "" {
		fmt.Fprintf(&b, "- **Layer:** %s\n", s.Layer)
	}
	if len(s.Tags) > 0 {
		fmt.Fprintf(&b, "- **Tags:** %s\n", strings.Join(s.Tags, ", "))
	}
	if len(dupes) > 0 {
		fmt.Fprintf(&b, "- **Possible duplicates:** `%s` — check before merging\n", strings.Join(dupes, "`, `"))
	}
	b.WriteString("\nFiles:\n")
	for _, f := range files {
		fmt.Fprintf(&b, "- `%s`\n", f)
	}
	return b.String()
}
