// Package ingest turns a repository's CI definition files into one bound
// workflow model: GitHub Actions workflows and Tekton resources are read by
// their ingesters, then composed into the hive-default process graph.
package ingest

import (
	"path"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/githubactions"
	"github.com/hivecommons/hive/pkg/workflowmodel/tekton"
)

// MaxFileBytes is the largest file the ingest reads; bigger ones are
// generated manifests, not process definitions, and are reported.
const MaxFileBytes = 2 << 20

// CodeFileTooLarge is the finding for a skipped oversized file.
const CodeFileTooLarge = "file-too-large"

// Repository ingests files (repo-relative slash paths to contents) for
// scope. Files that are neither GitHub Actions workflows nor Tekton
// documents are ignored. Output is deterministic for a given input.
func Repository(scope workflowmodel.Scope, files map[string][]byte) *workflowmodel.Model {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var gha []githubactions.File
	var tkn []tekton.File
	var findings []workflowmodel.Finding
	for _, p := range paths {
		data := files[p]
		if len(data) > MaxFileBytes {
			findings = append(findings, workflowmodel.Finding{Path: p, Code: CodeFileTooLarge, Severity: workflowmodel.SeverityWarning})
			continue
		}
		switch {
		case githubactions.IsWorkflowPath(p):
			gha = append(gha, githubactions.File{Path: p, Data: data})
		case isYAMLPath(p) && tekton.LooksTekton(data):
			tkn = append(tkn, tekton.File{Path: p, Data: data})
		}
	}
	g := githubactions.Ingest(gha)
	t := tekton.Ingest(tkn)
	pipelines := append(append([]workflowmodel.Pipeline(nil), g.Pipelines...), t.Pipelines...)
	sources := append(append([]workflowmodel.Source(nil), g.Sources...), t.Sources...)
	cov := workflowmodel.Coverage{
		Files:    g.Coverage.Files + t.Coverage.Files + len(findings),
		Parsed:   g.Coverage.Parsed + t.Coverage.Parsed,
		Findings: append(append(append([]workflowmodel.Finding(nil), g.Coverage.Findings...), t.Coverage.Findings...), findings...),
	}
	return workflowmodel.Bind(scope, pipelines, sources, cov)
}

func isYAMLPath(p string) bool {
	ext := strings.ToLower(path.Ext(p))
	return ext == ".yml" || ext == ".yaml"
}
