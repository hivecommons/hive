package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/evidence/complexity"
)

// ingestEvidence walks EvidenceRoot once, indexing review bundles
// (<owner>/<repo>/<n>/<head>.json, including <n>.uid<N> fallback dirs) and
// complexity snapshots (<owner>/<repo>/complexity/<YYYY-MM>.json). A file
// whose mtime and size are unchanged is not re-read; a re-read file whose
// updated_at (captured_at for snapshots) is unchanged is not re-indexed.
func (i *Indexer) ingestEvidence(ctx context.Context, r *rebuild) error {
	root := i.EvidenceRoot
	if root == "" {
		return nil
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root && errors.Is(err, os.ErrNotExist) {
				// A missing root is not "every bundle was removed": keep
				// what was indexed until it is back.
				i.markSeenUnder(r, root)
				return nil
			}
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if d != nil && d.IsDir() && path != root {
				// Unreadable subtree: keep what was indexed from it.
				i.markSeenUnder(r, path)
				return filepath.SkipDir
			}
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil || rel == "." {
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if d.IsDir() {
			if len(parts) == 3 && parts[2] == evidence.IssueSeriesDirName {
				return filepath.SkipDir
			}
			if len(parts) >= 4 {
				return filepath.SkipDir
			}
			return nil
		}
		if len(parts) != 4 || filepath.Ext(path) != ".json" {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			i.markSeen(r, fileKey(path))
			return nil
		}
		repo := parts[0] + "/" + parts[1]
		if parts[2] == complexity.DirName {
			i.ingestComplexityFile(r, path, repo, info)
		} else {
			i.ingestBundleFile(r, path, repo, info)
		}
		return nil
	})
	return err
}

func fileKey(path string) string { return "file:" + path }

// markSeenUnder keeps every known file source under dir.
func (i *Indexer) markSeenUnder(r *rebuild, dir string) {
	prefix := fileKey(dir) + string(filepath.Separator)
	for key := range i.entries {
		if strings.HasPrefix(key, prefix) {
			r.seen[key] = true
		}
	}
}

// statUnchanged reports whether a file source is unchanged on disk since the
// last pass, refreshing nothing.
func (i *Indexer) statUnchanged(r *rebuild, key string, info fs.FileInfo) bool {
	prev := i.entries[key]
	if prev != nil && prev.mod.Equal(info.ModTime()) && prev.size == info.Size() {
		r.seen[key] = true
		r.stats.Unchanged++
		return true
	}
	return false
}

// restat records a file's new mtime/size when its content stamp is unchanged.
func (i *Indexer) restat(r *rebuild, key string, info fs.FileInfo) {
	prev := i.entries[key]
	if _, ok := r.touched[key]; !ok {
		r.touched[key] = prev
	}
	c := *prev
	c.mod, c.size = info.ModTime(), info.Size()
	i.entries[key] = &c
}

func (i *Indexer) ingestBundleFile(r *rebuild, path, repo string, info fs.FileInfo) {
	key := fileKey(path)
	if i.statUnchanged(r, key, info) {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		i.markSeen(r, key)
		return
	}
	var b evidence.Bundle
	if json.Unmarshal(data, &b) != nil || b.SchemaVersion != evidence.SchemaVersion || b.Number <= 0 || b.HeadSHA == "" {
		r.stats.Invalid++
		i.markSeen(r, key)
		return
	}
	if b.Repo == "" {
		b.Repo = repo
	}
	at := firstNonZero(b.UpdatedAt, b.CreatedAt, info.ModTime())
	if i.unchanged(r, key, timeStamp(at)) {
		i.restat(r, key, info)
		return
	}
	i.put(r, key, &sourceEntry{mod: info.ModTime(), size: info.Size(), stamp: timeStamp(at), bundle: healthBundle(&b)}, bundleDocs(&b, at))
}

// bundleDocs maps one evidence bundle to a bundle document plus one document
// per verdict finding and per sentinel hit.
func bundleDocs(b *evidence.Bundle, at time.Time) []Document {
	base := fmt.Sprintf("evidence/%d/%s", b.Number, b.HeadSHA)
	url := fmt.Sprintf("https://github.com/%s/pull/%d", b.Repo, b.Number)
	head := b.HeadSHA
	if len(head) > 12 {
		head = head[:12]
	}

	verdicts := append([]evidence.Verdict(nil), b.Verdicts...)
	sort.SliceStable(verdicts, func(x, y int) bool { return verdicts[x].RecordedAt.Before(verdicts[y].RecordedAt) })
	var text []string
	fields := map[string]string{
		"number":      strconv.Itoa(b.Number),
		"head":        b.HeadSHA,
		"author":      b.Author.Login,
		"author.kind": b.Author.Kind,
		"ci":          b.CI.Status,
	}
	if len(verdicts) > 0 {
		fields["verdict"] = verdicts[len(verdicts)-1].Verdict
	}
	if b.Outcome != nil {
		fields["outcome"] = b.Outcome.State
	}
	var docs []Document
	for vi, v := range verdicts {
		text = append(text, strings.TrimSpace(v.Perspective+": "+v.Verdict))
		for fi, f := range v.Findings {
			loc := f.Path
			if f.Line > 0 {
				loc = fmt.Sprintf("%s:%d", f.Path, f.Line)
			}
			docs = append(docs, Document{
				ID: fmt.Sprintf("%s/finding/%d-%d", base, vi, fi), Repo: b.Repo, Kind: KindFinding,
				At: firstNonZero(v.RecordedAt, at), URL: url,
				Title: firstLine(f.Summary), Text: strings.TrimSpace(loc + "\n" + f.Summary),
				Fields: map[string]string{"verdict": v.Verdict, "perspective": v.Perspective, "severity": f.Severity,
					"path": f.Path, "number": strconv.Itoa(b.Number)},
			})
		}
	}
	for si, s := range b.Sentinel {
		text = append(text, "sentinel "+s.Rule+": "+s.Summary)
		docs = append(docs, Document{
			ID: fmt.Sprintf("%s/sentinel/%d", base, si), Repo: b.Repo, Kind: KindSentinel, At: at, URL: url,
			Title: "sentinel " + s.Rule, Text: strings.TrimSpace(s.Summary + "\n" + strings.Join(s.Paths, "\n")),
			Fields: map[string]string{"sentinel.rule": s.Rule, "number": strconv.Itoa(b.Number)},
		})
	}
	for _, c := range b.CI.Checks {
		if c.Conclusion != "" && c.Conclusion != "success" && c.Conclusion != "skipped" && c.Conclusion != "neutral" {
			text = append(text, "check "+c.Name+": "+c.Conclusion)
		}
	}
	for _, a := range b.HumanActions {
		text = append(text, strings.TrimSpace(a.Kind+" "+a.Actor+" "+a.Detail))
	}
	bundle := Document{
		ID: base, Repo: b.Repo, Kind: KindEvidence, At: at, URL: url,
		Title:  fmt.Sprintf("%s#%d review evidence @ %s", b.Repo, b.Number, head),
		Text:   strings.Join(text, "\n"),
		Fields: fields,
	}
	return append([]Document{bundle}, docs...)
}

func (i *Indexer) ingestComplexityFile(r *rebuild, path, repo string, info fs.FileInfo) {
	key := fileKey(path)
	if i.statUnchanged(r, key, info) {
		return
	}
	s, err := complexity.Load(path)
	if err != nil {
		i.markSeen(r, key)
		return
	}
	month := strings.TrimSuffix(filepath.Base(path), ".json")
	if s.Month == "" {
		s.Month = month
	}
	if s.Repo == "" {
		s.Repo = repo
	}
	monthStart, _ := time.Parse("2006-01", s.Month)
	at := firstNonZero(s.CapturedAt, monthStart, info.ModTime())
	if i.unchanged(r, key, timeStamp(at)) {
		i.restat(r, key, info)
		return
	}
	text := []string{fmt.Sprintf("functions %d, max cyclomatic %d, over threshold %d, static warnings %d",
		s.Functions, s.MaxCyclo, s.OverThreshold, s.Warnings)}
	for _, h := range s.Hotspots {
		text = append(text, fmt.Sprintf("%s %s %d", h.File, h.Name, h.Cyclo))
	}
	doc := Document{
		ID: "complexity/" + s.Month, Repo: s.Repo, Kind: KindComplexity, At: at, URL: repoURL(s.Repo),
		Title: fmt.Sprintf("%s complexity %s", s.Repo, s.Month), Text: strings.Join(text, "\n"),
		Fields: map[string]string{"month": s.Month},
	}
	i.put(r, key, &sourceEntry{mod: info.ModTime(), size: info.Size(), stamp: timeStamp(at)}, []Document{doc})
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if line, _, ok := strings.Cut(s, "\n"); ok {
		return strings.TrimSpace(line)
	}
	return s
}
