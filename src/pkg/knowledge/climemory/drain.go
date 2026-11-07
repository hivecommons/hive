package climemory

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
)

const (
	// SourceTag marks every drained entry; `source: cli-memory` is also
	// written as frontmatter so the dashboard filter and the tag agree.
	SourceTag = "cli-memory"
	// HashKey is the frontmatter key (and tag prefix) carrying the SHA-256 of
	// the normalised fact body; it is what makes repeated drains idempotent.
	HashKey = "memhash"
	// filePrefix + the first hashFilenameHex hex digits of the hash names the
	// entry file, so the same fact drained twice targets the same path.
	filePrefix      = "cli-memory-"
	hashFilenameHex = 16
	// maxMemoryFileBytes skips runaway memory files; a CLI note that large is
	// a transcript, not a fact.
	maxMemoryFileBytes = 512 * 1024
	// maxFactsPerDrain caps one drain so a pathological memory store cannot
	// flood the vault; the rest is drained on the next kick (the memory file
	// is not touched by this package).
	maxFactsPerDrain = 200
	// frontmatterScanBytes bounds how much of an existing entry is read when
	// collecting the hashes already in the vault.
	frontmatterScanBytes = 4 * 1024
)

// Source is one memory store to drain: a file (GEMINI.md, MEMORY.md) or a
// directory walked for *.md (a Claude memory dir, a Codex memories* dir).
type Source struct {
	CLI  string
	Path string
}

// Request describes one drain. Repos are the "org/repo" names the agent
// serves; a Claude project slug that ends in one of them stamps that repo on
// the entries drained from it.
type Request struct {
	Agent   string
	Repos   []string
	Sources []Source
}

// Result reports what the drain did. Drained counts entries written, Dupes
// the facts whose hash was already in the vault (or repeated within this
// drain), Skipped the facts over the per-drain cap.
type Result struct {
	Store   string
	Drained int
	Dupes   int
	Skipped int
	Errors  []error
}

// Drainer writes draft entries into Dir. Store, when set, is reindexed after
// a drain that wrote something so the Primer and the dashboard see the new
// drafts without waiting for the index refresh. Layer defaults to personal.
type Drainer struct {
	Dir   string
	Store *knowledge.FileStore
	Layer knowledge.LayerType
	Now   func() time.Time
}

// Drain reads every source, converts its facts to draft entries and writes
// the ones whose hash is not yet in the vault. Entries are always written
// with `state: draft`; nothing in this package can produce another state.
func (d *Drainer) Drain(req Request) Result {
	res := Result{Store: d.Dir}
	if d == nil || d.Dir == "" {
		res.Errors = append(res.Errors, errors.New("cli-memory drain: no target directory"))
		return res
	}
	if err := os.MkdirAll(d.Dir, 0o755); err != nil {
		res.Errors = append(res.Errors, fmt.Errorf("cli-memory drain: creating %s: %w", d.Dir, err))
		return res
	}
	seen, err := existingHashes(d.Dir)
	if err != nil {
		res.Errors = append(res.Errors, err)
		return res
	}
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	layer := d.Layer
	if layer == "" {
		layer = knowledge.LayerPersonal
	}
	for _, src := range req.Sources {
		files, err := memoryFiles(src.Path)
		if err != nil {
			res.Errors = append(res.Errors, err)
			continue
		}
		repo := repoFromClaudeSlug(src.Path, req.Repos)
		for _, file := range files {
			content, err := os.ReadFile(file)
			if err != nil {
				res.Errors = append(res.Errors, fmt.Errorf("cli-memory drain: reading %s: %w", file, err))
				continue
			}
			for _, fact := range ParseFor(src.CLI, string(content)) {
				h := Hash(fact.Body)
				if seen[h] {
					res.Dupes++
					continue
				}
				if res.Drained >= maxFactsPerDrain {
					res.Skipped++
					continue
				}
				entry := renderEntry(fact, h, entryMeta{
					agent: req.Agent, cli: src.CLI, repo: repo, path: file, layer: layer, at: now().UTC(),
				})
				if err := writeAtomic(filepath.Join(d.Dir, EntryFilename(h)), entry); err != nil {
					res.Errors = append(res.Errors, err)
					continue
				}
				seen[h] = true
				res.Drained++
			}
		}
	}
	if res.Drained > 0 && d.Store != nil {
		d.Store.Reindex()
	}
	return res
}

// Hash is the SHA-256 of the normalised fact body, hex encoded.
func Hash(body string) string {
	sum := sha256.Sum256([]byte(normalise(body)))
	return hex.EncodeToString(sum[:])
}

// EntryFilename is the vault file name for a fact with the given hash.
func EntryFilename(hash string) string {
	if len(hash) > hashFilenameHex {
		hash = hash[:hashFilenameHex]
	}
	return filePrefix + hash + ".md"
}

type entryMeta struct {
	agent, cli, repo, path string
	layer                  knowledge.LayerType
	at                     time.Time
}

// renderEntry writes the same hand-rolled frontmatter the vault writers use
// (FileStore.WriteFacts, BeadSynthesizer.writeFactToVault) so the entry
// indexes like any other fact, plus the drain provenance keys.
func renderEntry(f Fact, hash string, m entryMeta) string {
	tags := []string{SourceTag, "cli:" + m.cli, HashKey + ":" + hash}
	if m.agent != "" {
		tags = append(tags, "agent:"+m.agent)
	}
	if m.repo != "" {
		tags = append(tags, "repo:"+m.repo)
	}
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %s\n", scalar(f.Title()))
	fmt.Fprintf(&b, "type: %s\n", knowledge.FactReference)
	fmt.Fprintf(&b, "layer: %s\n", scalar(string(m.layer)))
	fmt.Fprintf(&b, "state: %s\n", knowledge.StateDraft)
	fmt.Fprintf(&b, "source: %s\n", SourceTag)
	fmt.Fprintf(&b, "cli: %s\n", scalar(m.cli))
	if m.agent != "" {
		fmt.Fprintf(&b, "agent: %s\n", scalar(m.agent))
	}
	if m.repo != "" {
		fmt.Fprintf(&b, "repo: %s\n", scalar(m.repo))
	}
	fmt.Fprintf(&b, "source_path: %s\n", scalar(m.path))
	fmt.Fprintf(&b, "%s: %s\n", HashKey, hash)
	fmt.Fprintf(&b, "tags: [%s]\n", list(tags))
	fmt.Fprintf(&b, "drained: %s\n", m.at.Format(time.RFC3339))
	b.WriteString("---\n\n")
	b.WriteString(f.Body)
	b.WriteString("\n")
	return b.String()
}

// scalar makes s safe as a one-line frontmatter value: the vault parser
// ends the block at the first "\n---" and matches keys by line prefix, so a
// newline inside a value could forge keys (#7688).
func scalar(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

func list(items []string) string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		if v := strings.TrimSpace(strings.ReplaceAll(scalar(it), ",", " ")); v != "" {
			out = append(out, v)
		}
	}
	return strings.Join(out, ", ")
}

// existingHashes collects every memhash already recorded in dir, whatever
// the entry's current state or file name, so an entry an operator approved,
// deprecated or renamed is still a duplicate and is never re-drafted.
func existingHashes(dir string) (map[string]bool, error) {
	seen := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("cli-memory drain: listing %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if h := hashFromFrontmatter(filepath.Join(dir, e.Name())); h != "" {
			seen[h] = true
		}
	}
	return seen, nil
}

func hashFromFrontmatter(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(bufio.NewReaderSize(f, frontmatterScanBytes))
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if first {
			first = false
			if line != "---" {
				return ""
			}
			continue
		}
		if line == "---" {
			return ""
		}
		if v, ok := strings.CutPrefix(line, HashKey+":"); ok {
			return strings.Trim(strings.TrimSpace(v), "\"'")
		}
	}
	return ""
}

// memoryFiles lists the markdown files under path (or path itself when it
// is a file), skipping temp files, hidden entries, symlinks and oversized
// files. A missing path is not an error: the CLI simply wrote nothing.
func memoryFiles(path string) ([]string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("cli-memory drain: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// A symlinked store points outside the agent's own home (a shared
		// or bridged tree); draining it would attribute another agent's
		// memory to this one.
		return nil, nil
	}
	if !info.IsDir() {
		if info.Size() > maxMemoryFileBytes || !strings.HasSuffix(path, ".md") {
			return nil, nil
		}
		return []string{path}, nil
	}
	var files []string
	walkErr := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if p != path && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".tmp.md") {
			return nil
		}
		fi, err := d.Info()
		if err != nil || fi.Mode()&os.ModeSymlink != 0 || fi.Size() > maxMemoryFileBytes {
			return nil
		}
		files = append(files, p)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("cli-memory drain: walking %s: %w", path, walkErr)
	}
	sort.Strings(files)
	return files, nil
}

// repoFromClaudeSlug derives "org/repo" from a Claude project slug. Claude
// keys auto-memory by cwd with path separators turned into "-", so a memory
// dir ~/.claude/projects/-data-agents-scanner-hivecommons-hive/memory maps to
// hivecommons/hive when that repo is one the agent serves. Other layouts
// (Codex, Gemini) carry no project and yield "".
func repoFromClaudeSlug(path string, repos []string) string {
	parts := strings.Split(filepath.ToSlash(path), "/")
	slug := ""
	for i, p := range parts {
		if p == "projects" && i+1 < len(parts) {
			slug = parts[i+1]
			break
		}
	}
	if slug == "" {
		return ""
	}
	slug = strings.ToLower(slug)
	for _, repo := range repos {
		if repo == "" {
			continue
		}
		suffix := strings.ToLower(strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(repo))
		if strings.HasSuffix(slug, "-"+suffix) || slug == suffix {
			return repo
		}
	}
	return ""
}

func writeAtomic(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("cli-memory drain: writing %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("cli-memory drain: renaming %s: %w", path, err)
	}
	return nil
}
