// Package codeindex provides an offline, immutable inverted index of tracked
// text in registered repository checkouts. Build a replacement snapshot after
// checkout updates; searches never invoke GitHub code search or read the disk.
package codeindex

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	maxFileBytes = 1 << 20
	maxLineBytes = 4096
)

// maxRepoBytes is a variable so tests can exercise the budget cutoff.
var maxRepoBytes = 64 << 20

// Repo is one explicitly registered checkout; Name is its GitHub owner/name.
type Repo struct {
	Name string
	Root string
}

// Match is a file-level result. Terms contains the exact indexed terms shared
// with the query; Line and Text identify the strongest matching source line.
type Match struct {
	Repo, Path, Revision string
	Line                 int
	Text                 string
	Terms                []string
	Doc                  bool
}

// URL links to the indexed revision rather than a moving default branch.
func (m Match) URL() string {
	parts := strings.Split(m.Path, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return "https://github.com/" + m.Repo + "/blob/" + m.Revision + "/" + strings.Join(parts, "/") + fmt.Sprintf("#L%d", m.Line)
}

type document struct {
	match Match
	lines []string
}

// Index is safe for concurrent searches. It holds no credentials or open files.
type Index struct {
	docs     []document
	postings map[string][]int
}

// tokenRE preserves identifiers, dotted config keys and --flags as whole terms,
// and also indexes their components for ordinary prose queries.
var tokenRE = regexp.MustCompile(`--[A-Za-z][A-Za-z0-9_-]*|[A-Za-z_][A-Za-z0-9_]*(?:[.-][A-Za-z0-9_]+)*`)
var componentRE = regexp.MustCompile(`[a-z0-9]+`)

func tokens(text string) map[string]bool {
	out := map[string]bool{}
	for _, token := range tokenRE.FindAllString(strings.ToLower(text), -1) {
		out[token] = true
		for _, part := range componentRE.FindAllString(token, -1) {
			out[part] = true
		}
	}
	return out
}

// Build indexes all supplied repos, not just the repo of a later issue. Only
// tracked regular files are read; symlinks, binary files, generated dependency
// directories and files over 1 MiB are skipped. Each repo has a 64 MiB text
// budget. Errors are reported per repo so callers can log incomplete coverage;
// cancellation returns no snapshot. A partial snapshot must not be used as
// proof that a feature is absent.
func Build(ctx context.Context, repos []Repo) (*Index, map[string]error) {
	idx := &Index{postings: map[string][]int{}}
	errs := map[string]error{}
	repos = append([]Repo(nil), repos...)
	sort.Slice(repos, func(i, j int) bool { return repos[i].Name < repos[j].Name })
	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			errs[repo.Name] = err
			return nil, errs
		}
		if err := idx.addRepo(ctx, repo); err != nil {
			errs[repo.Name] = err
		}
	}
	if ctx.Err() != nil {
		return nil, errs
	}
	return idx, errs
}

func (idx *Index) addRepo(ctx context.Context, repo Repo) error {
	git := func(args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "git", append([]string{"-C", repo.Root}, args...)...).Output()
	}
	head, err := git("rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("checkout revision: %w", err)
	}
	paths, err := git("ls-files", "--cached", "-z")
	if err != nil {
		return fmt.Errorf("tracked files: %w", err)
	}
	root, err := filepath.EvalSymlinks(repo.Root)
	if err != nil {
		return err
	}
	names := strings.Split(string(paths), "\x00")
	sort.Strings(names)
	remaining := maxRepoBytes
	var firstErr error
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "" || excluded(name) {
			continue
		}
		path := filepath.Join(repo.Root, filepath.FromSlash(name))
		info, err := os.Lstat(path)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", name, err)
			}
			continue
		}
		if !info.Mode().IsRegular() || info.Size() > maxFileBytes {
			continue
		}
		// Git may track a symlinked parent directory after a checkout has been
		// modified. Resolve and contain the entire path, not only its leaf.
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		f, err := os.Open(resolved)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", name, err)
			}
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
		f.Close()
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", name, err)
			}
			continue
		}
		if len(data) > maxFileBytes || bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
			continue
		}
		remaining -= len(data)
		if remaining < 0 {
			return fmt.Errorf("text budget exceeded (%d bytes)", maxRepoBytes)
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if len(line) > maxLineBytes {
				cut := maxLineBytes
				for cut > 0 && !utf8.RuneStart(line[cut]) {
					cut--
				}
				lines[i] = line[:cut]
			}
		}
		id := len(idx.docs)
		idx.docs = append(idx.docs, document{match: Match{Repo: repo.Name, Path: name, Revision: strings.TrimSpace(string(head)), Doc: isDoc(name)}, lines: lines})
		for token := range tokens(strings.Join(lines, "\n")) {
			idx.postings[token] = append(idx.postings[token], id)
		}
	}
	return firstErr
}

func excluded(path string) bool {
	for _, part := range strings.Split(path, "/") {
		switch part {
		case ".git", "vendor", "node_modules", "dist", "build":
			return true
		}
	}
	return false
}

func isDoc(path string) bool {
	for _, part := range strings.Split(strings.ToLower(path), "/") {
		if part == "docs" || strings.HasPrefix(part, "readme") || strings.HasPrefix(part, "changelog") {
			return true
		}
	}
	return false
}

// Search returns at most limit files ordered by shared terms, then repo/path.
// Terms are matched exactly (case-insensitively), so --foo does not hit
// --foobar. Pass both identifiers and prose terms; no two-identifier cap is
// imposed. The caller applies its confidence and filing policies separately.
func (idx *Index) Search(ctx context.Context, terms []string, limit int) ([]Match, error) {
	if idx == nil || limit <= 0 {
		return nil, ctx.Err()
	}
	seen := map[string]bool{}
	hits := map[int][]string{}
	for _, term := range terms {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		term = strings.ToLower(strings.TrimSpace(term))
		if term == "" || seen[term] {
			continue
		}
		seen[term] = true
		for _, id := range idx.postings[term] {
			hits[id] = append(hits[id], term)
		}
	}
	ids := make([]int, 0, len(hits))
	for id := range hits {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if len(hits[ids[i]]) != len(hits[ids[j]]) {
			return len(hits[ids[i]]) > len(hits[ids[j]])
		}
		return ids[i] < ids[j]
	})
	if len(ids) > limit {
		ids = ids[:limit]
	}
	out := make([]Match, 0, len(ids))
	for _, id := range ids {
		doc := idx.docs[id]
		m := doc.match
		m.Terms = append([]string(nil), hits[id]...)
		sort.Strings(m.Terms)
		best := -1
		for n, line := range doc.lines {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			words := tokens(line)
			score := 0
			for _, term := range m.Terms {
				if words[term] {
					score++
				}
			}
			if score > best {
				best, m.Line, m.Text = score, n+1, strings.TrimSpace(line)
			}
		}
		out = append(out, m)
	}
	return out, nil
}
