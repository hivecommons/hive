package inbox

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// excerptRunes bounds the list excerpt.
const excerptRunes = 240

// FactSource lists items of one connector type by reading the facts that
// type's connectors wrote under <ConnectorsDir>/<layer>/. It needs nothing
// from the vendor: any connector whose facts carry `source`/`source_id`
// front matter is an Inbox source.
type FactSource struct {
	// ConnectorsDir is <knowledge base>/connectors.
	ConnectorsDir string
	// SourceType is the connector type (plaud, wisprflow, ...).
	SourceType string
}

// Type implements Source.
func (f *FactSource) Type() string { return f.SourceType }

// List implements Source: newest first.
func (f *FactSource) List(ctx context.Context) ([]Item, error) {
	layers, err := os.ReadDir(f.ConnectorsDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Item
	for _, layer := range layers {
		if !layer.IsDir() {
			continue
		}
		dir := filepath.Join(f.ConnectorsDir, layer.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, fi := range files {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			if fi.IsDir() || !strings.HasSuffix(fi.Name(), ".md") || !strings.HasPrefix(fi.Name(), f.SourceType+"-") {
				continue
			}
			path := filepath.Join(dir, fi.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			fm, body := splitFrontMatter(string(data))
			if fm["source"] != f.SourceType || fm["source_id"] == "" || fm["status"] == "deprecated" {
				continue
			}
			out = append(out, itemFromFact(fm, body, layer.Name(), path))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].RecordedAt.Equal(out[j].RecordedAt) {
			return out[i].RecordedAt.After(out[j].RecordedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func itemFromFact(fm map[string]string, body, layer, path string) Item {
	it := Item{
		Source:    fm["source"],
		Connector: fm["connector"],
		ID:        fm["source_id"],
		Title:     fm["title"],
		FactSlug:  strings.TrimSuffix(filepath.Base(path), ".md"),
		FactPath:  path,
		Layer:     layer,
		Excerpt:   excerpt(body),
	}
	if it.Title == "" {
		it.Title = it.ID
	}
	sections := splitSections(body)
	it.Summary = truncateRunes(sections["summary"], SummaryRunes)
	it.HasTranscript = sections["transcript"] != ""
	it.Pending = it.Summary == "" && !it.HasTranscript
	it.FactState = factState(fm, it.Pending)
	for _, key := range []string{"attr_recorded_at", "synthesized"} {
		if t, err := time.Parse(time.RFC3339, fm[key]); err == nil {
			it.RecordedAt = t.UTC()
			break
		}
	}
	if ms, err := strconv.ParseInt(fm["attr_duration_ms"], 10, 64); err == nil {
		it.DurationMS = ms
	}
	if tags := fm["attr_tags"]; tags != "" {
		for _, t := range strings.Split(tags, ",") {
			if t = strings.TrimSpace(t); t != "" {
				it.Tags = append(it.Tags, t)
			}
		}
	}
	return it
}

// Fetch implements Source by re-reading the fact and splitting the body
// into the connector's "## Summary" / "## Transcript" sections.
func (f *FactSource) Fetch(_ context.Context, it Item) (Content, error) {
	data, err := os.ReadFile(it.FactPath)
	if err != nil {
		return Content{}, err
	}
	fm, body := splitFrontMatter(string(data))
	c := Content{
		Title:      fm["title"],
		Markdown:   strings.TrimSpace(body),
		Provenance: f.SourceType + "://recordings/" + it.ID,
	}
	if c.Title == "" {
		c.Title = it.Title
	}
	sections := splitSections(body)
	c.Summary, c.Transcript, c.Highlights = sections["summary"], sections["transcript"], sections["highlights"]
	c.Pending = c.Summary == "" && c.Transcript == ""
	c.machineFact = connector.FactContentState(fm) == connector.FactContentMachine
	return c, nil
}

// splitFrontMatter mirrors the connector fact layout: a `---` block of
// `key: value` lines, then the markdown body.
func splitFrontMatter(content string) (map[string]string, string) {
	out := map[string]string{}
	if !strings.HasPrefix(content, "---\n") {
		return out, content
	}
	end := strings.Index(content[4:], "\n---")
	if end < 0 {
		return out, content
	}
	for _, l := range strings.Split(content[4:4+end], "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), "\"'")
	}
	rest := content[4+end+4:]
	if i := strings.Index(rest, "\n"); i >= 0 {
		rest = rest[i+1:]
	} else {
		rest = ""
	}
	return out, rest
}

// sectionKeys are the "## …" headings splitSections collects, keyed by the
// heading's first word (lower-cased): "## Summary (Plaud)" → summary.
var sectionKeys = []string{"summary", "transcript", "highlights"}

// splitSections returns the text under each known "## …" heading, keyed by
// sectionKeys. Headings may carry a suffix such as "(Plaud)"; unknown
// headings end the current section.
func splitSections(body string) map[string]string {
	builders := map[string]*strings.Builder{}
	var cur *strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "## ") {
			h := strings.ToLower(line[3:])
			cur = nil
			for _, key := range sectionKeys {
				if strings.HasPrefix(h, key) {
					if builders[key] == nil {
						builders[key] = &strings.Builder{}
					}
					cur = builders[key]
					break
				}
			}
			continue
		}
		if cur != nil {
			cur.WriteString(line)
			cur.WriteByte('\n')
		}
	}
	out := make(map[string]string, len(sectionKeys))
	for _, key := range sectionKeys {
		if b := builders[key]; b != nil {
			out[key] = strings.TrimSpace(b.String())
		}
	}
	return out
}

// truncateRunes bounds s to n runes with an ellipsis.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n])) + "…"
}

// excerptHidden reports the lines a fact carries for agents and provenance,
// not for reading: the untrusted-data envelope around a machine transcript
// (its open and close markers and the preamble) and the transcript's
// provenance line. The stored fact keeps them; the excerpt shows only text.
func excerptHidden(line string) bool {
	return strings.HasPrefix(line, "<"+UntrustedTag) || strings.HasPrefix(line, "</"+UntrustedTag) ||
		line == untrustedPreamble || isMachineProvenance(line)
}

// excerpt returns the first prose lines of body, bounded to excerptRunes.
// Headings, bold labels and the connector's "- **Recorded:**"/"- **Duration:**"
// metadata bullets are skipped (the Inbox shows those fields itself).
// The machine-transcript envelope and provenance line are skipped too.
func excerpt(body string) string {
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "**") || strings.HasPrefix(line, "- **") || excerptHidden(line) {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(line)
		if utf8.RuneCountInString(b.String()) >= excerptRunes {
			break
		}
	}
	return truncateRunes(b.String(), excerptRunes)
}
