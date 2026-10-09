package connector

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// Fact lifecycle values written to the `status:` front-matter key.
const (
	StatusActive     = "active"
	StatusDeprecated = "deprecated"
)

// DefaultMaxPageBytes caps the markdown body of a single synced page.
const DefaultMaxPageBytes = 1 << 20

// maxSlugIDLen bounds the source-id part of a slug so filenames stay well
// under filesystem limits.
const maxSlugIDLen = 80

// Slug returns the deterministic vault slug `<type>-<name>-<source_id>`.
// The source id is lower-cased and reduced to [a-z0-9-]; when that loses
// information (or the id is very long) a short hash of the raw id is appended
// so two upstream ids never collapse onto the same fact.
func Slug(typ, name, sourceID string) string {
	id := slugPart(sourceID)
	if id != sourceID || len(id) > maxSlugIDLen {
		if len(id) > maxSlugIDLen {
			id = strings.TrimRight(id[:maxSlugIDLen], "-")
		}
		sum := sha256.Sum256([]byte(sourceID))
		h := hex.EncodeToString(sum[:])[:8]
		if id == "" {
			id = h
		} else {
			id = id + "-" + h
		}
	}
	return slugPrefix(typ, name) + id
}

func slugPrefix(typ, name string) string {
	return slugPart(typ) + "-" + slugPart(name) + "-"
}

func slugPart(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// FactWriter writes connector pages into a vault directory as markdown facts.
type FactWriter struct {
	Dir          string
	MaxPageBytes int
	Now          func() time.Time
}

func (w *FactWriter) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *FactWriter) path(slug string) string {
	return filepath.Join(w.Dir, slug+".md")
}

// Write renders p as a fact. It returns the slug and whether the file
// changed; an unchanged page (ignoring sync timestamps) is not rewritten.
func (w *FactWriter) Write(cfg ConnectorConfig, p Page) (string, bool, error) {
	if strings.TrimSpace(p.ID) == "" {
		return "", false, fmt.Errorf("page has empty ID")
	}
	slug := Slug(cfg.Type, cfg.Name, p.ID)
	status := StatusActive
	if p.Archived {
		status = StatusDeprecated
	}
	content := w.render(cfg, p, slug, status)
	path := w.path(slug)
	if old, err := os.ReadFile(path); err == nil && stripVolatile(string(old)) == stripVolatile(content) {
		return slug, false, nil
	}
	if err := writeAtomic(path, content); err != nil {
		return slug, false, err
	}
	return slug, true, nil
}

func (w *FactWriter) render(cfg ConnectorConfig, p Page, slug, status string) string {
	now := w.now()
	title := p.Title
	if strings.TrimSpace(title) == "" {
		title = p.ID
	}
	synthesized := now
	if !p.UpdatedAt.IsZero() {
		synthesized = p.UpdatedAt.UTC()
	}

	var buf strings.Builder
	buf.WriteString("---\n")
	fmt.Fprintf(&buf, "title: %s\n", knowledge.SanitizeFrontmatterValue(title))
	fmt.Fprintf(&buf, "type: %s\n", knowledge.FactReference)
	fmt.Fprintf(&buf, "layer: %s\n", knowledge.SanitizeFrontmatterValue(cfg.Layer))
	fmt.Fprintf(&buf, "status: %s\n", status)
	fmt.Fprintf(&buf, "tags: [%s]\n", knowledge.SanitizeFrontmatterList([]string{"connector", cfg.Type, cfg.Name}))
	fmt.Fprintf(&buf, "source: %s\n", knowledge.SanitizeFrontmatterValue(cfg.Type))
	fmt.Fprintf(&buf, "connector: %s\n", knowledge.SanitizeFrontmatterValue(cfg.Name))
	fmt.Fprintf(&buf, "source_id: %s\n", knowledge.SanitizeFrontmatterValue(p.ID))
	if p.URL != "" {
		fmt.Fprintf(&buf, "source_url: %s\n", knowledge.SanitizeFrontmatterValue(p.URL))
	}
	if len(p.Path) > 0 {
		fmt.Fprintf(&buf, "source_path: %s\n", knowledge.SanitizeFrontmatterValue(strings.Join(p.Path, " / ")))
	}
	for _, a := range sortedAttrs(p.Attrs) {
		fmt.Fprintf(&buf, "attr_%s: %s\n", a[0], knowledge.SanitizeFrontmatterValue(a[1]))
	}
	fmt.Fprintf(&buf, "synthesized: %s\n", synthesized.Format(time.RFC3339))
	fmt.Fprintf(&buf, "synced_at: %s\n", now.Format(time.RFC3339))
	buf.WriteString("---\n\n")
	buf.WriteString(w.capBody(p.Markdown))
	buf.WriteString("\n")
	return buf.String()
}

// sortedAttrs returns [key, value] pairs with keys reduced to [a-z0-9_],
// sorted by key. Keys that reduce to nothing are dropped; when two keys
// reduce to the same name the lexically first original key wins.
func sortedAttrs(attrs map[string]string) [][2]string {
	orig := make([]string, 0, len(attrs))
	for k := range attrs {
		orig = append(orig, k)
	}
	sort.Strings(orig)
	seen := map[string]bool{}
	out := make([][2]string, 0, len(orig))
	for _, k := range orig {
		key := strings.ReplaceAll(slugPart(k), "-", "_")
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, [2]string{key, attrs[k]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

func (w *FactWriter) capBody(md string) string {
	limit := w.MaxPageBytes
	if limit <= 0 {
		limit = DefaultMaxPageBytes
	}
	md = strings.TrimSpace(md)
	if len(md) <= limit {
		return md
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(md[cut]) {
		cut--
	}
	return md[:cut] + fmt.Sprintf("\n\n_(truncated by hive: page exceeds %d bytes)_", limit)
}

// Existing returns slug → status for every fact in Dir that belongs to the
// connector described by cfg. Ownership is confirmed from front-matter, not
// just the filename prefix, so connector `a` never claims `a-b`'s facts.
func (w *FactWriter) Existing(cfg ConnectorConfig) (map[string]string, error) {
	entries, err := os.ReadDir(w.Dir)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing vault: %w", err)
	}
	prefix := slugPrefix(cfg.Type, cfg.Name)
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(w.Dir, name))
		if err != nil {
			continue
		}
		fm := frontMatter(string(data))
		if fm["connector"] != cfg.Name || fm["source"] != cfg.Type {
			continue
		}
		out[strings.TrimSuffix(name, ".md")] = fm["status"]
	}
	return out, nil
}

// Deprecate marks the fact `status: deprecated` (a tombstone: the file stays
// so links resolve, but primers can skip it). It reports whether it changed.
func (w *FactWriter) Deprecate(slug string) (bool, error) {
	path := w.path(slug)
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("reading fact %s: %w", slug, err)
	}
	content := string(data)
	if !strings.HasPrefix(content, "---\n") {
		return false, fmt.Errorf("fact %s has no front-matter", slug)
	}
	end := strings.Index(content[4:], "\n---")
	if end < 0 {
		return false, fmt.Errorf("fact %s has unterminated front-matter", slug)
	}
	head, rest := content[4:4+end], content[4+end:]
	lines := strings.Split(head, "\n")
	changed, hasStatus := false, false
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "status:"):
			hasStatus = true
			if strings.TrimSpace(strings.TrimPrefix(l, "status:")) != StatusDeprecated {
				lines[i] = "status: " + StatusDeprecated
				changed = true
			}
		case strings.HasPrefix(l, "synced_at:"):
			lines[i] = "synced_at: " + w.now().Format(time.RFC3339)
		}
	}
	if !hasStatus {
		lines = append(lines, "status: "+StatusDeprecated)
		changed = true
	}
	if !changed {
		return false, nil
	}
	return true, writeAtomic(path, "---\n"+strings.Join(lines, "\n")+rest)
}

// frontMatter parses the simple `key: value` front-matter block.
func frontMatter(content string) map[string]string {
	out := map[string]string{}
	if !strings.HasPrefix(content, "---\n") {
		return out
	}
	end := strings.Index(content[4:], "\n---")
	if end < 0 {
		return out
	}
	for _, l := range strings.Split(content[4:4+end], "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), "\"'")
	}
	return out
}

// stripVolatile drops the timestamp lines that change on every sync so
// unchanged pages are not rewritten.
func stripVolatile(content string) string {
	lines := strings.Split(content, "\n")
	out := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(l, "synced_at:") || strings.HasPrefix(l, "synthesized:") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

func writeAtomic(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating vault dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing fact: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("renaming fact: %w", err)
	}
	return nil
}
