package search

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const fileMagic = "hive-search-index v1\n"

type fileImage struct {
	Repo string     `json:"repo"`
	Docs []Document `json:"docs"`
}

// indexPath maps owner/name to <dir>/<owner>/<name>.idx. Repos are validated
// before reaching here, so neither component can escape dir.
func indexPath(dir, repo string) string {
	owner, name, _ := strings.Cut(repo, "/")
	return filepath.Join(dir, owner, name+".idx")
}

func encode(repo string, docs []Document) ([]byte, error) {
	body, err := json.Marshal(fileImage{Repo: repo, Docs: docs})
	if err != nil {
		return nil, err
	}
	return append([]byte(fileMagic), body...), nil
}

// decode rejects anything it did not write itself, including a file for a
// different repo or documents that no longer validate.
func decode(repo string, data []byte) (*shard, error) {
	body, ok := bytes.CutPrefix(data, []byte(fileMagic))
	if !ok {
		return nil, fmt.Errorf("%w: bad header", ErrCorruptIndex)
	}
	var img fileImage
	if err := json.Unmarshal(body, &img); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptIndex, err)
	}
	if img.Repo != repo {
		return nil, fmt.Errorf("%w: file is for %q", ErrCorruptIndex, img.Repo)
	}
	seen := map[string]bool{}
	for _, d := range img.Docs {
		if err := d.validate(); err != nil || d.Repo != repo || seen[d.ID] {
			return nil, fmt.Errorf("%w: invalid document %q", ErrCorruptIndex, d.ID)
		}
		seen[d.ID] = true
	}
	return buildShard(img.Docs, int64(len(data))), nil
}

// readShard returns (nil, nil) when no index file exists.
func readShard(dir, repo string) (*shard, error) {
	data, err := os.ReadFile(indexPath(dir, repo))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decode(repo, data)
}

// writeAtomic writes a sibling temp file, syncs it and renames it over path,
// so readers (and a crash) see either the old or the new index, never a mix.
func writeAtomic(path string, data []byte) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// diskRepos lists repos with an index file under dir.
func diskRepos(dir string) map[string]int64 {
	out := map[string]int64{}
	paths, _ := filepath.Glob(filepath.Join(dir, "*", "*.idx"))
	for _, p := range paths {
		repo := filepath.Base(filepath.Dir(p)) + "/" + strings.TrimSuffix(filepath.Base(p), ".idx")
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() || !validRepo(repo) {
			continue
		}
		out[repo] = info.Size()
	}
	return out
}
