package climemory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Spool layout: SpoolDir/<agent>/<key>/{memory.md, source.json}, where key
// is derived from the CLI and the store path so re-spooling the same file
// overwrites its earlier copy instead of piling up.
const (
	spoolContentFile = "memory.md"
	spoolMetaFile    = "source.json"
	spoolKeyHex      = 16
)

// spoolMeta records what a spooled copy is needed to drain it like the
// original: the parser to use and the provenance stamped on its entries.
type spoolMeta struct {
	CLI        string `json:"cli"`
	SourcePath string `json:"source_path"`
	Repo       string `json:"repo,omitempty"`
}

// agentSpoolDir is SpoolDir/<agent>, or "" when no spool is configured or
// the agent name is not safe as a path component.
func (r *drainRun) agentSpoolDir() string {
	if r.d.SpoolDir == "" || !agentNameRe.MatchString(r.req.Agent) {
		return ""
	}
	return filepath.Join(r.d.SpoolDir, r.req.Agent)
}

// spool keeps a copy of f (content, or the file itself when content is nil)
// in the agent's spool. A file already being retried from the spool stays
// where it is.
func (r *drainRun) spool(f memFile, content []byte) {
	dir := r.agentSpoolDir()
	if dir == "" || f.spoolDir != "" {
		return
	}
	if content == nil {
		var err error
		if content, err = f.store.read(f.path); err != nil {
			r.res.Errors = append(r.res.Errors, fmt.Errorf("cli-memory drain: reading %s to spool: %w", f.path, err))
			return
		}
	}
	sum := sha256.Sum256([]byte(f.cli + "\x00" + f.path))
	entryDir := filepath.Join(dir, hex.EncodeToString(sum[:])[:spoolKeyHex])
	meta, err := json.Marshal(spoolMeta{CLI: f.cli, SourcePath: f.path, Repo: f.repo})
	if err == nil {
		err = os.MkdirAll(entryDir, 0o700)
	}
	if err == nil {
		err = writeFilePrivate(filepath.Join(entryDir, spoolContentFile), content)
	}
	if err == nil {
		// The metadata goes last: a spool entry without it is never
		// retried, so a half-written copy cannot be drained.
		err = writeFilePrivate(filepath.Join(entryDir, spoolMetaFile), meta)
	}
	if err != nil {
		r.res.Errors = append(r.res.Errors, fmt.Errorf("cli-memory drain: spooling %s: %w", f.path, err))
		return
	}
	r.res.Spooled++
}

// spooled lists the agent's spooled files, oldest key order first. An entry
// whose metadata is unreadable is reported and left for the operator.
func (r *drainRun) spooled() []memFile {
	dir := r.agentSpoolDir()
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			r.res.Errors = append(r.res.Errors, fmt.Errorf("cli-memory drain: listing spool %s: %w", dir, err))
		}
		return nil
	}
	var out []memFile
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		entryDir := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(filepath.Join(entryDir, spoolMetaFile))
		if err != nil {
			if !os.IsNotExist(err) {
				r.res.Errors = append(r.res.Errors, fmt.Errorf("cli-memory drain: reading spool %s: %w", entryDir, err))
			}
			continue
		}
		var m spoolMeta
		if err := json.Unmarshal(data, &m); err != nil || (m.CLI != CLIClaude && m.CLI != CLICodex && m.CLI != CLIGemini) {
			r.res.Errors = append(r.res.Errors, fmt.Errorf("cli-memory drain: spool entry %s has invalid metadata", entryDir))
			continue
		}
		out = append(out, memFile{cli: m.CLI, path: m.SourcePath, repo: m.Repo, spoolDir: entryDir})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].spoolDir < out[j].spoolDir })
	return out
}

func writeFilePrivate(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// addAgentTag adds agent:<agent> to the tags of the vault entry at path
// and reports whether it changed the file. It edits only the inline `tags:`
// list of the frontmatter (adding one when absent) and never touches the
// state, so a fact under review keeps its state whichever agent writes it.
func addAgentTag(path, agent string) (bool, error) {
	if path == "" || !agentNameRe.MatchString(agent) {
		return false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("cli-memory drain: reading %s: %w", path, err)
	}
	content := string(data)
	const open = "---\n"
	if !strings.HasPrefix(content, open) {
		return false, nil
	}
	end := strings.Index(content[len(open):], "\n---")
	if end < 0 {
		return false, nil
	}
	fm := content[len(open) : len(open)+end]
	rest := content[len(open)+end:]
	tag := "agent:" + agent
	lines := strings.Split(fm, "\n")
	found := false
	for i, line := range lines {
		v, ok := strings.CutPrefix(line, "tags:")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if !strings.HasPrefix(v, "[") || !strings.HasSuffix(v, "]") {
			return false, fmt.Errorf("cli-memory drain: %s: tags is not an inline list; %s not added", path, tag)
		}
		var items []string
		for _, it := range strings.Split(v[1:len(v)-1], ",") {
			it = strings.Trim(strings.TrimSpace(it), "\"'")
			if it == tag {
				return false, nil
			}
			if it != "" {
				items = append(items, it)
			}
		}
		lines[i] = "tags: [" + list(append(items, tag)) + "]"
		found = true
		break
	}
	if !found {
		lines = append(lines, "tags: ["+tag+"]")
	}
	if err := writeAtomic(path, open+strings.Join(lines, "\n")+rest); err != nil {
		return false, err
	}
	return true, nil
}
