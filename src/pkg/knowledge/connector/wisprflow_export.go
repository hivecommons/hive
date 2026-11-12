package connector

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// wisprExportMaxFileBytes caps one export file read.
const wisprExportMaxFileBytes = 64 << 20

// wisprFilenameTimeRe pulls a date (and optional time) from an exported
// markdown filename such as 2026-11-11-1405-slack.md or 2026-11-11T14-05-09.md.
var wisprFilenameTimeRe = regexp.MustCompile(`(\d{4}-\d{2}-\d{2})(?:[T _-](\d{2})[-:h]?(\d{2})(?:[-:m]?(\d{2}))?)?`)

// readWisprExportDir walks an export folder (as written by `wispr export`,
// wispr-flow-exporter or a hand-made dump) and returns the dictations newer
// than since. Supported files: .json (array or wrapped object), .ndjson /
// .jsonl (one object per line), .csv (header row) and .md (one dictation per
// file, optional YAML front matter). Unreadable files are reported together
// as one error while the readable ones are still returned.
func readWisprExportDir(dir string, since time.Time, limit int) ([]wisprDictation, error) {
	var out []wisprDictation
	var problems []string
	seen := map[string]bool{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", path, walkErr))
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path != dir && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") {
			return nil
		}
		rows, err := readWisprExportFile(dir, path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", filepath.Base(path), err))
			return nil
		}
		for _, r := range rows {
			if r.At.IsZero() || !r.At.After(since) || seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			out = append(out, r)
			if len(out) >= limit {
				return fs.SkipAll
			}
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	if len(problems) > 0 {
		return out, fmt.Errorf("wisprflow: %d export file(s) skipped: %s", len(problems), strings.Join(problems, "; "))
	}
	return out, nil
}

// readWisprExportFile dispatches on extension; unknown extensions are ignored.
func readWisprExportFile(root, path string) ([]wisprDictation, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".json", ".ndjson", ".jsonl", ".csv", ".md", ".markdown", ".txt":
	default:
		return nil, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > wisprExportMaxFileBytes {
		return nil, fmt.Errorf("file larger than %d bytes", wisprExportMaxFileBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	switch ext {
	case ".json":
		return decodeWisprJSON(data)
	case ".ndjson", ".jsonl":
		return decodeWisprNDJSON(data)
	case ".csv":
		return decodeWisprCSV(data)
	default:
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = filepath.Base(path)
		}
		d, ok := decodeWisprMarkdown(rel, data, info.ModTime())
		if !ok {
			return nil, nil
		}
		return []wisprDictation{d}, nil
	}
}

func decodeWisprNDJSON(data []byte) ([]wisprDictation, error) {
	var out []wisprDictation
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 0, 64<<10), wisprExportMaxFileBytes)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(text), &m); err != nil {
			return out, fmt.Errorf("line %d: %v", line, err)
		}
		if d, ok := wisprFromMap(m); ok {
			out = append(out, d)
		}
	}
	return out, sc.Err()
}

// decodeWisprCSV maps header names onto the same keys wisprFromMap accepts.
func decodeWisprCSV(data []byte) ([]wisprDictation, error) {
	r := csv.NewReader(strings.NewReader(string(data)))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	header, err := r.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, err
	}
	for i := range header {
		header[i] = strings.TrimSpace(strings.TrimPrefix(header[i], "\ufeff"))
	}
	var out []wisprDictation
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, err
		}
		m := make(map[string]any, len(header))
		for i, h := range header {
			if i < len(rec) && h != "" {
				m[h] = rec[i]
			}
		}
		if d, ok := wisprFromMap(m); ok {
			out = append(out, d)
		}
	}
	return out, nil
}

// decodeWisprMarkdown reads one dictation per markdown file. Front matter
// keys id, timestamp/date/created, app and url are honoured; otherwise the
// id is the relative path and the time comes from the filename or mtime.
func decodeWisprMarkdown(rel string, data []byte, modTime time.Time) (wisprDictation, bool) {
	body := strings.ReplaceAll(string(data), "\r\n", "\n")
	meta := map[string]string{}
	if strings.HasPrefix(body, "---\n") {
		if end := strings.Index(body[4:], "\n---"); end >= 0 {
			for _, line := range strings.Split(body[4:4+end], "\n") {
				k, v, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				meta[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"'`)
			}
			body = body[4+end+4:]
		}
	}
	body = strings.TrimSpace(body)
	// Drop a leading heading: the title is derived from the text.
	if strings.HasPrefix(body, "# ") {
		if i := strings.IndexByte(body, '\n'); i >= 0 {
			body = strings.TrimSpace(body[i+1:])
		} else {
			body = ""
		}
	}
	d := wisprDictation{
		Text:     body,
		App:      meta["app"],
		URL:      meta["url"],
		Language: meta["language"],
	}
	d.ID = meta["id"]
	if d.ID == "" {
		d.ID = meta["transcriptentityid"]
	}
	if d.ID == "" {
		d.ID = strings.TrimSuffix(filepath.ToSlash(rel), filepath.Ext(rel))
	}
	for _, k := range []string{"timestamp", "dictated", "date", "created", "created_at", "createdat"} {
		if v := meta[k]; v != "" {
			d.At = wisprParseTime(v)
			if d.At.IsZero() {
				if t, ok := wisprTimeFromName(v); ok {
					d.At = t
				}
			}
			break
		}
	}
	if d.At.IsZero() {
		if t, ok := wisprTimeFromName(filepath.Base(rel)); ok {
			d.At = t
		} else {
			d.At = modTime.UTC()
		}
	}
	if d.Text == "" {
		return d, false
	}
	return d, true
}

// wisprTimeFromName extracts a UTC time from a filename-style date stamp.
func wisprTimeFromName(name string) (time.Time, bool) {
	m := wisprFilenameTimeRe.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	stamp := m[1]
	layout := "2006-01-02"
	if m[2] != "" {
		stamp += " " + m[2] + ":" + m[3]
		layout = "2006-01-02 15:04"
		if m[4] != "" {
			stamp += ":" + m[4]
			layout = "2006-01-02 15:04:05"
		}
	}
	t, err := time.Parse(layout, stamp)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}
