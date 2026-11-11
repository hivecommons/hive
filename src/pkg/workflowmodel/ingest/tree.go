package ingest

import (
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
		switch {
		case IsIntakePath(rel):
		case !isYAMLPath(rel):
			return nil
		case githubactions.IsWorkflowPath(rel), underTektonDir(rel):
		case hintsPipeline(rel):
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if tekton.LooksTekton(data) {
				files[rel] = data
			}
			return nil
		default:
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[rel] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
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
