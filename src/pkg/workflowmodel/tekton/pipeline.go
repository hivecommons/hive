package tekton

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// Finding codes the ingest emits.
const (
	CodeYAML                = "yaml-parse"
	CodeNotTekton           = "not-tekton"
	CodeUnknownKind         = "tekton-kind"
	CodeTaskDefinitions     = "task-definitions"
	CodeTektonTriggers      = "tekton-triggers"
	CodePipelineRefExternal = "pipelineref-external"
	CodeNoTrigger           = "no-trigger"
	CodeFinally             = "finally"
	CodeRunAfterUnresolved  = "runafter-unresolved"
	CodeRunAfterCycle       = "runafter-cycle"
	CodeDuplicateTask       = "duplicate-task"
	CodeTaskRefBundle       = "taskref-bundle"
	CodePipelineResources   = "pipeline-resources"
	CodeOnError             = "on-error"
	CodeCustomTask          = "custom-task"
)

// refInline is the Ref of a task whose taskSpec is embedded.
const refInline = "inline"

// resultRefPattern matches $(tasks.<name>.results.<r>) and the other
// $(tasks.<name>.…) forms that imply an ordering dependency.
var resultRefPattern = regexp.MustCompile(`\$\(tasks\.([A-Za-z0-9_-]+)\.`)

// resolverParamNames are the resolver params, in order, that identify what
// a resolver-based taskRef points at.
var resolverParamNames = []string{"name", "url", "pathInRepo"}

// builder accumulates one pipeline's nodes and findings.
type builder struct {
	path     string
	findings []workflowmodel.Finding
}

func (b *builder) find(node, code, detail string, sev workflowmodel.Severity) {
	b.findings = append(b.findings, workflowmodel.Finding{Path: b.path, Node: node, Code: code, Detail: detail, Severity: sev})
}

// buildPipeline turns a pipelineSpec mapping into a Pipeline with nodes
// levelled. Findings about the spec are returned alongside.
func buildPipeline(path, id, name string, spec map[string]any) (workflowmodel.Pipeline, []workflowmodel.Finding) {
	b := &builder{path: path}
	p := workflowmodel.Pipeline{ID: id, Name: name, System: workflowmodel.SystemTekton, Path: path, Nodes: []workflowmodel.Node{}}

	if _, ok := spec["resources"]; ok {
		b.find("", CodePipelineResources, "spec.resources (PipelineResources) are not modelled", workflowmodel.SeverityUnsupported)
	}

	seen := map[string]bool{}
	var taskNames []string
	for _, t := range mapsAt(spec, "tasks") {
		n := b.node(t, workflowmodel.NodeTask)
		if n.ID == "" {
			continue
		}
		if seen[n.ID] {
			b.find(n.ID, CodeDuplicateTask, "task name declared more than once", workflowmodel.SeverityUnknown)
			continue
		}
		seen[n.ID] = true
		taskNames = append(taskNames, n.ID)
		p.Nodes = append(p.Nodes, n)
	}
	finally := mapsAt(spec, "finally")
	if len(finally) > 0 {
		b.find("", CodeFinally, "finally tasks run regardless of outcome", workflowmodel.SeverityWarning)
	}
	for _, t := range finally {
		n := b.node(t, workflowmodel.NodeFinally)
		if n.ID == "" {
			continue
		}
		if seen[n.ID] {
			b.find(n.ID, CodeDuplicateTask, "task name declared more than once", workflowmodel.SeverityUnknown)
			continue
		}
		seen[n.ID] = true
		n.Needs = append([]string(nil), taskNames...)
		p.Nodes = append(p.Nodes, n)
	}

	b.resolveNeeds(p.Nodes)
	levels, err := workflowmodel.Levels(p.Nodes)
	if err != nil {
		// resolveNeeds removed every unknown need and every back edge, so
		// Levels cannot fail; record it rather than drop the pipeline if
		// it somehow does.
		b.find("", CodeRunAfterCycle, err.Error(), workflowmodel.SeverityUnknown)
	}
	p.Levels = levels
	return p, b.findings
}

// node maps one spec.tasks or spec.finally entry.
func (b *builder) node(t map[string]any, kind workflowmodel.NodeKind) workflowmodel.Node {
	name := stringAt(t, "name")
	n := workflowmodel.Node{ID: name, Name: name, Kind: kind}
	n.Ref = b.taskRef(name, t)
	if _, ok := t["onError"]; ok {
		b.find(name, CodeOnError, "onError is not modelled", workflowmodel.SeverityWarning)
	}
	n.Needs = implicitNeeds(name, stringsAt(t, "runAfter"), t)
	n.When = whenClauses(t)
	n.Matrix = matrixSummary(mapAt(t, "matrix"))
	return n
}

// taskRef renders the task reference: a name, a resolver with its
// identifying params, or "inline" for an embedded taskSpec.
func (b *builder) taskRef(node string, t map[string]any) string {
	ref := mapAt(t, "taskRef")
	if ref == nil {
		if _, ok := t["taskSpec"]; ok {
			return refInline
		}
		return ""
	}
	if bundle := stringAt(ref, "bundle"); bundle != "" {
		b.find(node, CodeTaskRefBundle, "taskRef.bundle "+bundle+" is deprecated", workflowmodel.SeverityWarning)
	}
	if apiVersion := stringAt(ref, "apiVersion"); apiVersion != "" {
		if k := stringAt(ref, "kind"); k != kindTask && k != kindClusterTask {
			b.find(node, CodeCustomTask, fmt.Sprintf("custom task %s %s", apiVersion, k), workflowmodel.SeverityUnsupported)
		}
	}
	if name := stringAt(ref, "name"); name != "" {
		return name
	}
	resolver := stringAt(ref, "resolver")
	if resolver == "" {
		return ""
	}
	params := map[string]string{}
	for _, p := range mapsAt(ref, "params") {
		params[stringAt(p, "name")] = stringAt(p, "value")
	}
	parts := []string{}
	for _, k := range resolverParamNames {
		if v := params[k]; v != "" {
			parts = append(parts, v)
		}
	}
	if len(parts) == 0 {
		return resolver
	}
	return resolver + ":" + strings.Join(parts, "/")
}

// implicitNeeds merges runAfter with every task named by a
// $(tasks.<name>.…) reference in params, when, workspaces or matrix.
func implicitNeeds(self string, runAfter []string, t map[string]any) []string {
	set := map[string]bool{}
	for _, r := range runAfter {
		if r != "" && r != self {
			set[r] = true
		}
	}
	for _, key := range []string{"params", "when", "workspaces", "matrix"} {
		walkStrings(t[key], func(s string) {
			for _, m := range resultRefPattern.FindAllStringSubmatch(s, -1) {
				if m[1] != self {
					set[m[1]] = true
				}
			}
		})
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// whenClauses renders each when[] entry as "<input> <operator> [values]",
// or "cel: <expr>" for CEL entries.
func whenClauses(t map[string]any) []string {
	var out []string
	for _, w := range mapsAt(t, "when") {
		if cel := stringAt(w, "cel"); cel != "" {
			out = append(out, "cel: "+cel)
			continue
		}
		out = append(out, fmt.Sprintf("%s %s [%s]", stringAt(w, "input"), stringAt(w, "operator"), strings.Join(stringsAt(w, "values"), ", ")))
	}
	return out
}

// matrixSummary summarises matrix.params and matrix.include.
func matrixSummary(m map[string]any) *workflowmodel.Matrix {
	if m == nil {
		return nil
	}
	out := &workflowmodel.Matrix{Include: len(sliceAt(m, "include"))}
	for _, p := range mapsAt(m, "params") {
		if name := stringAt(p, "name"); name != "" {
			out.Keys = append(out.Keys, name)
		}
	}
	walkStrings(m, func(s string) {
		if strings.Contains(s, "$(") {
			out.Dynamic = true
		}
	})
	return out
}

// resolveNeeds drops needs that name no node and back edges that would
// close a cycle, recording a finding for each, so Levels succeeds.
func (b *builder) resolveNeeds(nodes []workflowmodel.Node) {
	idx := make(map[string]int, len(nodes))
	for i, n := range nodes {
		idx[n.ID] = i
	}
	for i := range nodes {
		kept := nodes[i].Needs[:0]
		for _, need := range nodes[i].Needs {
			if _, ok := idx[need]; ok {
				kept = append(kept, need)
				continue
			}
			b.find(nodes[i].ID, CodeRunAfterUnresolved, "depends on unknown task "+need, workflowmodel.SeverityUnknown)
		}
		nodes[i].Needs = nilIfEmpty(kept)
	}
	const unvisited, visiting, done = 0, 1, 2
	state := make([]int, len(nodes))
	var visit func(i int)
	visit = func(i int) {
		state[i] = visiting
		kept := nodes[i].Needs[:0]
		for _, need := range nodes[i].Needs {
			j := idx[need]
			switch state[j] {
			case visiting:
				b.find(nodes[i].ID, CodeRunAfterCycle, "dependency on "+need+" closes a cycle; dropped", workflowmodel.SeverityUnknown)
				continue
			case unvisited:
				visit(j)
			}
			kept = append(kept, need)
		}
		nodes[i].Needs = nilIfEmpty(kept)
		state[i] = done
	}
	for i := range nodes {
		if state[i] == unvisited {
			visit(i)
		}
	}
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}
