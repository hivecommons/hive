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
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
)

const (
	// SourceTag marks every drained entry; `source: cli-memory` is also
	// written as frontmatter so the dashboard filter and the tag agree. The
	// knowledge package keys the agent-facing draft exclusion on it.
	SourceTag = knowledge.AutoMemoryTag
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
	// flood the vault; a file with facts over the cap is spooled (when a
	// spool is configured) and drained on the next kick.
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
// drain), Skipped the facts over the per-drain cap, Spooled the memory files
// copied to the spool because not all of their facts were stored.
type Result struct {
	Store   string
	Drained int
	Dupes   int
	Skipped int
	Spooled int
	Errors  []error
}

// Drainer writes draft entries into Dir. Store, when set, is reindexed after
// a drain that changed the vault so the Primer and the dashboard see the new
// drafts without waiting for the index refresh. Layer defaults to personal.
//
// SpoolDir, when set, keeps a copy of every memory file whose facts did not
// all reach the vault (the vault is unwritable, or the per-drain cap was
// reached) under SpoolDir/<agent>/, so clear_on_kick wiping the store right
// after the drain does not lose them. The agent's spool is retried first on
// its next drain and each spooled file is removed once all its facts are
// stored.
type Drainer struct {
	Dir      string
	Store    *knowledge.FileStore
	Layer    knowledge.LayerType
	SpoolDir string
	Now      func() time.Time

	// mu serialises drains: two agents reaching the same fact at once must
	// not race on the entry's tag rewrite or its spool directory.
	mu sync.Mutex
}

// memFile is one memory file to drain. path is the file in the agent's
// store (recorded as source_path); spoolDir is set when the content is
// being retried from the spool.
type memFile struct {
	cli, path, repo string
	spoolDir        string
	// store is the opened source the file is read through; nil for a file
	// being retried from the spool.
	store *memoryStore
}

// drainRun is the state of one Drain call.
type drainRun struct {
	d       *Drainer
	req     Request
	res     *Result
	seen    map[string]string // memhash → vault entry path
	changed bool
	now     func() time.Time
	layer   knowledge.LayerType
}

// Drain reads every source, converts its facts to draft entries and writes
// the ones whose hash is not yet in the vault. Entries are always written
// with `state: draft`; nothing in this package can produce another state.
// A fact already in the vault, in any state, is never rewritten: it only
// gains this agent's agent:<name> tag, so a fact an operator deprecated or
// superseded stays that way however often a CLI writes it again.
func (d *Drainer) Drain(req Request) Result {
	if d == nil || d.Dir == "" {
		return Result{Errors: []error{errors.New("cli-memory drain: no target directory")}}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	res := Result{Store: d.Dir}
	r := &drainRun{d: d, req: req, res: &res, now: time.Now, layer: d.Layer}
	if d.Now != nil {
		r.now = d.Now
	}
	if r.layer == "" {
		r.layer = knowledge.LayerPersonal
	}
	var files []memFile
	for _, src := range req.Sources {
		paths, err := listMemoryFiles(src.Path)
		if err != nil {
			res.Errors = append(res.Errors, err)
			continue
		}
		if len(paths) == 0 {
			continue
		}
		store, err := openMemoryStore(src.Path)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("cli-memory drain: opening %s: %w", src.Path, err))
			continue
		}
		defer store.Close()
		repo := repoFromClaudeSlug(src.Path, req.Repos)
		for _, p := range paths {
			files = append(files, memFile{cli: src.CLI, path: p, repo: repo, store: store})
		}
	}
	seen, err := r.prepareVault()
	if err != nil {
		res.Errors = append(res.Errors, err)
		for _, f := range files {
			r.spool(f, nil)
		}
		return res
	}
	r.seen = seen
	for _, f := range r.spooled() {
		content, err := os.ReadFile(filepath.Join(f.spoolDir, spoolContentFile))
		if err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("cli-memory drain: reading spooled %s: %w", f.spoolDir, err))
			continue
		}
		if r.drainFile(f, content) {
			if err := os.RemoveAll(f.spoolDir); err != nil {
				res.Errors = append(res.Errors, fmt.Errorf("cli-memory drain: removing spooled %s: %w", f.spoolDir, err))
			}
		}
	}
	for _, f := range files {
		content, err := f.store.read(f.path)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("cli-memory drain: reading %s: %w", f.path, err))
			continue
		}
		if !r.drainFile(f, content) {
			r.spool(f, content)
		}
	}
	if r.changed && d.Store != nil {
		d.Store.Reindex()
	}
	return res
}

func (r *drainRun) prepareVault() (map[string]string, error) {
	if err := os.MkdirAll(r.d.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("cli-memory drain: creating %s: %w", r.d.Dir, err)
	}
	return existingHashes(r.d.Dir)
}

// drainFile stores every fact in content and reports whether all of them
// are now in the vault (written now, or already there).
func (r *drainRun) drainFile(f memFile, content []byte) bool {
	complete := true
	for _, fact := range ParseFor(f.cli, string(content)) {
		h := Hash(fact.Body)
		if existing, ok := r.seen[h]; ok {
			r.res.Dupes++
			tagged, err := addAgentTag(existing, r.req.Agent)
			if err != nil {
				r.res.Errors = append(r.res.Errors, err)
			}
			r.changed = r.changed || tagged
			continue
		}
		if r.res.Drained >= maxFactsPerDrain {
			r.res.Skipped++
			complete = false
			continue
		}
		path := filepath.Join(r.d.Dir, EntryFilename(h))
		entry := renderEntry(fact, h, entryMeta{
			agent: r.req.Agent, cli: f.cli, repo: f.repo, path: f.path, layer: r.layer, at: r.now().UTC(),
		})
		if err := writeAtomic(path, entry); err != nil {
			r.res.Errors = append(r.res.Errors, err)
			complete = false
			continue
		}
		r.seen[h] = path
		r.res.Drained++
		r.changed = true
	}
	return complete
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

// existingHashes maps every memhash already recorded in dir to the entry
// that carries it, whatever the entry's current state or file name, so an
// entry an operator approved, deprecated or renamed is still a duplicate and
// is never re-drafted.
func existingHashes(dir string) (map[string]string, error) {
	seen := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("cli-memory drain: listing %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if h := hashFromFrontmatter(p); h != "" {
			seen[h] = p
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

// listMemoryFiles is memoryFiles behind a seam, so a test can hand Drain
// and TruncateDrained a listing the store no longer matches.
var listMemoryFiles = memoryFiles

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
