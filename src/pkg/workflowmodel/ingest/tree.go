package ingest

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/hivecommons/hive/pkg/workflowmodel/githubactions"
	"github.com/hivecommons/hive/pkg/workflowmodel/tekton"
)

// MaxTreeFiles bounds how many CI files ReadTree returns from one
// checkout; a monorepo past this is reported, not read to exhaustion.
const MaxTreeFiles = 2000

// tektonDirs are the directories Tekton resources conventionally live in
// (the same list the survey collector used, so a spoke ingest reads what
// the corpus read).
var tektonDirs = []string{".tekton/", "tekton/", "pipelines/", ".pipelines/", "openshift/ci/", "ci/tekton/"}

// skipDirs are never descended into.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, "testdata": true, "third_party": true}

// ReadTree walks a checkout and returns the files the ingest reads, keyed
// by repo-relative slash path: every GitHub Actions workflow, every YAML
// under a Tekton directory, any other YAML whose name hints at a pipeline
// and whose content looks like a Tekton document, and the process
// convention files IsIntakePath names (issue and PR templates,
// CONTRIBUTING, CODEOWNERS/OWNERS, label and settings files). Files over
// MaxFileBytes are returned too (Repository reports them). The result is
// what Repository and Lifecycle take.
func ReadTree(root string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || len(files) >= MaxTreeFiles {
			return nil
		}
		data, keep, err := selectTreeFile(rel, func() ([]byte, error) { return os.ReadFile(p) })
		if err != nil {
			return err
		}
		if keep {
			files[rel] = data
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// Tarball streaming limits: ReadTarball keeps at most MaxTarballFileBytes
// per matched file and MaxTarballKeptBytes across all kept files, so a
// monorepo's source tree never has to fit anywhere — only its CI files do.
const (
	MaxTarballFileBytes = 1 << 20
	MaxTarballKeptBytes = 32 << 20
)

// ErrTarballKeptTooLarge is returned when the CI files alone exceed
// MaxTarballKeptBytes.
var ErrTarballKeptTooLarge = errors.New("ingest: pipeline files exceed size cap")

// ReadTarball streams a gzipped tar archive (GitHub's /tarball shape: one
// wrapping top-level directory, a leading pax_global_header) and returns the
// same selection ReadTree makes from a checkout, without extracting
// anything to disk. Entries outside the selection are discarded as they
// stream, so the archive's total size is unbounded; a matched file over
// MaxTarballFileBytes is skipped (and Repository never sees it), and the
// kept set is capped at MaxTarballKeptBytes.
func ReadTarball(r io.Reader) (map[string][]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("ingest: tarball gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	var kept int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return nil, fmt.Errorf("ingest: tarball: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		rel, ok := tarballRelPath(hdr.Name)
		if !ok || len(files) >= MaxTreeFiles || hdr.Size > MaxTarballFileBytes {
			continue
		}
		data, keep, err := selectTreeFile(rel, func() ([]byte, error) {
			return io.ReadAll(io.LimitReader(tr, MaxTarballFileBytes+1))
		})
		if err != nil {
			return nil, fmt.Errorf("ingest: tarball %s: %w", rel, err)
		}
		if !keep || int64(len(data)) > MaxTarballFileBytes {
			continue
		}
		kept += int64(len(data))
		if kept > MaxTarballKeptBytes {
			return nil, ErrTarballKeptTooLarge
		}
		files[rel] = data
	}
}

// tarballRelPath strips the wrapping top-level directory from a GitHub
// tarball entry and rejects anything under a skipDirs component or that
// escapes the tree.
func tarballRelPath(name string) (string, bool) {
	name = strings.TrimPrefix(filepath.ToSlash(name), "./")
	_, rel, ok := strings.Cut(name, "/")
	if !ok || rel == "" || strings.HasSuffix(rel, "/") {
		return "", false
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." || skipDirs[seg] {
			return "", false
		}
	}
	return rel, true
}

// selectTreeFile applies the ReadTree selection to one repo-relative path,
// calling read only when the path is a candidate; keep reports whether
// the returned data belongs in the result.
func selectTreeFile(rel string, read func() ([]byte, error)) (data []byte, keep bool, err error) {
	switch {
	case IsIntakePath(rel):
	case !isYAMLPath(rel):
		return nil, false, nil
	case githubactions.IsWorkflowPath(rel), underTektonDir(rel):
	case hintsPipeline(rel):
		data, err := read()
		if err != nil {
			return nil, false, err
		}
		return data, tekton.LooksTekton(data), nil
	default:
		return nil, false, nil
	}
	data, err = read()
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func underTektonDir(rel string) bool {
	for _, d := range tektonDirs {
		if strings.HasPrefix(rel, d) {
			return true
		}
	}
	return false
}

func hintsPipeline(rel string) bool {
	base := strings.ToLower(filepath.Base(rel))
	return strings.Contains(base, "pipeline") || strings.Contains(base, "tekton")
}
