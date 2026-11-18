package reviewrules

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// tagUnsafe matches characters a frontmatter tag cannot carry.
var tagUnsafe = regexp.MustCompile(`[^A-Za-z0-9._/:-]+`)

func tagValue(s string) string { return strings.Trim(tagUnsafe.ReplaceAllString(s, "-"), "-") }

func (s *Store) entryPath(r *Rule) string { return filepath.Join(s.vault, r.Slug()+".md") }

// entryState reads the lifecycle state of a rule's vault entry. found is false
// when the entry does not exist.
func (s *Store) entryState(r *Rule) (state string, found bool) {
	b, err := os.ReadFile(s.entryPath(r))
	if err != nil {
		return "", false
	}
	content := string(b)
	if !strings.HasPrefix(content, "---\n") {
		return "approved", true
	}
	end := strings.Index(content[4:], "\n---")
	if end < 0 {
		return "approved", true
	}
	for _, line := range strings.Split(content[4:4+end], "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "state:"); ok {
			return strings.ToLower(strings.Trim(strings.TrimSpace(v), `"'`)), true
		}
	}
	return "approved", true
}

// entryStateFor maps a rule status to the lifecycle state of its entry.
func entryStateFor(r *Rule) string {
	switch r.Status {
	case StatusPending:
		return "draft"
	case StatusRetracted:
		return "deprecated"
	case StatusMerged:
		return "superseded"
	}
	return "approved"
}

// lanes returns the lane list of the rule's entry scope, empty for scopes that
// are not tied to a lane.
func (r *Rule) lanes() []string {
	switch r.Scope {
	case ScopeAgent, ScopeWorkflow:
		return []string{r.AppliesTo}
	case ScopeRepo:
		if r.Lane != "" {
			return []string{r.Lane}
		}
	}
	return nil
}

// Describe says, in words, where the rule applies.
func (r *Rule) Describe() string {
	switch r.Scope {
	case ScopeWorkflow:
		return "the `" + r.AppliesTo + "` workflow"
	case ScopeRepo:
		return "work on `" + r.AppliesTo + "`"
	case ScopeGlobal:
		return "all agents' work"
	}
	return "the `" + r.AppliesTo + "` agent's future work"
}

func firstRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}

// writeEntry writes the rule's knowledge entry. The frontmatter is the shape
// pkg/knowledge's FileStore indexes: the scope block decides which kicks the
// entry may reach, the `pin` tag makes the primer carry it whatever the kick's
// keywords are, and `source` is the review comment the rule came from.
func (s *Store) writeEntry(r *Rule) error {
	if err := os.MkdirAll(s.vault, 0o755); err != nil {
		return err
	}
	var b strings.Builder
	title := fmt.Sprintf("%s %s", r.ID, firstRunes(r.Text, titleRunes))
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %s\n", title)
	b.WriteString("type: reviewer-rule\nlayer: project\n")
	fmt.Fprintf(&b, "state: %s\n", entryStateFor(r))
	fmt.Fprintf(&b, "confidence: %.2f\n", r.Confidence)
	tags := []string{"review-rule", strings.ToLower(r.ID)}
	if r.Scope != ScopeRepo {
		tags = append(tags, "pin")
	} else if r.AppliesTo != "" {
		tags = append(tags, "repo:"+tagValue(r.AppliesTo))
	}
	if len(r.Provenance) > 0 {
		tags = append(tags, "reviewer:"+tagValue(r.Provenance[0].Reviewer))
	}
	fmt.Fprintf(&b, "tags: [%s]\n", strings.Join(tags, ", "))
	if len(r.Provenance) > 0 {
		fmt.Fprintf(&b, "source: %s\n", strings.Join(strings.Fields(r.Provenance[0].CommentURL), ""))
	}
	fmt.Fprintf(&b, "author: %s\n", vaultAuthor)
	if r.Status == StatusMerged {
		fmt.Fprintf(&b, "superseded_by: rule-%s\n", strings.ToLower(r.MergedInto))
	}
	fmt.Fprintf(&b, "synthesized: %s\n", r.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"))
	if lanes := r.lanes(); len(lanes) > 0 {
		clean := make([]string, 0, len(lanes))
		for _, l := range lanes {
			clean = append(clean, tagValue(l))
		}
		fmt.Fprintf(&b, "scope:\n  lanes: [%s]\n", strings.Join(clean, ", "))
	}
	b.WriteString("---\n\n")
	fmt.Fprintf(&b, "Rule %s, stated by a reviewer for %s: %s\n\n", r.ID, r.Describe(), r.Text)
	b.WriteString("Provenance:\n")
	for _, p := range r.Provenance {
		fmt.Fprintf(&b, "- PR %s, comment %s, reviewer `%s` (%s), recorded %s by %s\n",
			p.PRURL, p.CommentURL, p.Reviewer, strings.ToLower(p.Association), p.At.UTC().Format("2006-01-02"), p.Source)
	}
	fmt.Fprintf(&b, "\nTo retract: `hivectl knowledge state %s deprecated`.\n", r.Slug())

	path := s.entryPath(r)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
