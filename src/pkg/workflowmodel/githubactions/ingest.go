// Package githubactions ingests GitHub Actions workflow files into
// workflowmodel pipelines. It reads the YAML as a node tree so job and
// trigger order follow the file, never a map iteration, and it never
// rejects a repository because one file is malformed: bad files are counted
// in Coverage and reported as findings.
package githubactions

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"gopkg.in/yaml.v3"
)

// Finding codes this ingester records.
const (
	// CodeYAMLParse is a file that is not valid YAML.
	CodeYAMLParse = "yaml-parse"
	// CodeNotAWorkflow is valid YAML that has neither `on` nor `jobs`.
	CodeNotAWorkflow = "not-a-workflow"
	// CodeReusableOnly is a workflow triggered only by workflow_call.
	CodeReusableOnly = "reusable-only"
	// CodeEnvironmentOnPR is a PR-gating job that targets an environment.
	CodeEnvironmentOnPR = "environment-on-pr"
	// CodeNeedsUnresolved is a need naming no job; the need is dropped.
	CodeNeedsUnresolved = "needs-unresolved"
	// CodeNeedsCycle is a cycle in needs; all levels are reset to zero.
	CodeNeedsCycle = "needs-cycle"
	// CodeConcurrencyExpression is a cancel-in-progress that is an expression.
	CodeConcurrencyExpression = "concurrency-expression"
	// CodeJobContainer is a job with container or services, which are not modelled.
	CodeJobContainer = "job-container"
	// CodeSecretsInherit is a reusable job passing `secrets: inherit`.
	CodeSecretsInherit = "secrets-inherit"
	// CodeJobEmpty is a job with no steps and no uses.
	CodeJobEmpty = "job-empty"
	// CodeNoJobs is a workflow whose jobs map is missing or empty.
	CodeNoJobs = "no-jobs"
)

// Event names the role derivation looks at.
const (
	EventPush              = "push"
	EventPullRequest       = "pull_request"
	EventPullRequestTarget = "pull_request_target"
	EventMergeGroup        = "merge_group"
	EventRelease           = "release"
	EventSchedule          = "schedule"
	EventWorkflowDispatch  = "workflow_dispatch"
	EventWorkflowCall      = "workflow_call"
	EventWorkflowRun       = "workflow_run"
	EventDeployment        = "deployment"
	EventDeploymentStatus  = "deployment_status"
)

// Workflow file keys.
const (
	keyOn               = "on"
	keyName             = "name"
	keyJobs             = "jobs"
	keyConcurrency      = "concurrency"
	keyGroup            = "group"
	keyCancelInProgress = "cancel-in-progress"
	keyTypes            = "types"
	keyBranches         = "branches"
	keyBranchesIgnore   = "branches-ignore"
	keyTags             = "tags"
	keyTagsIgnore       = "tags-ignore"
	keyPaths            = "paths"
	keyPathsIgnore      = "paths-ignore"
	keyCron             = "cron"
	keyWorkflows        = "workflows"
	keyUses             = "uses"
	keyNeeds            = "needs"
	keyEnvironment      = "environment"
	keyIf               = "if"
	keyRunsOn           = "runs-on"
	keyLabels           = "labels"
	keyStrategy         = "strategy"
	keyMatrix           = "matrix"
	keyInclude          = "include"
	keyExclude          = "exclude"
	keyContainer        = "container"
	keyServices         = "services"
	keySecrets          = "secrets"
	keySteps            = "steps"
	secretsInherit      = "inherit"
)

// Workflow path layout.
const (
	workflowsDir = ".github/workflows/"
	extYML       = ".yml"
	extYAML      = ".yaml"
)

// negatePrefix marks an ignore-list entry when folded into its positive list.
const negatePrefix = "!"

// YAML tags and literals the parser distinguishes.
const (
	tagBool      = "!!bool"
	tagNull      = "!!null"
	literalTrue  = "true"
	literalFalse = "false"
)

// expressionOpen starts a GitHub Actions expression.
const expressionOpen = "${{"

// Sentinel errors ParseWorkflow wraps.
var (
	// ErrYAML is returned when the file is not valid YAML.
	ErrYAML = errors.New("yaml parse error")
	// ErrNotAWorkflow is returned when the document is not a workflow.
	ErrNotAWorkflow = errors.New("not a workflow")
)

// File is one workflow file read from a repository.
type File struct {
	Path string
	Data []byte
}

// Result of ingesting a repository's workflows.
type Result struct {
	Pipelines []workflowmodel.Pipeline
	Sources   []workflowmodel.Source
	Coverage  workflowmodel.Coverage
}

// Ingest parses every file as a GitHub Actions workflow. It never returns
// an error for a bad file: the file is counted in Coverage.Files but not
// Coverage.Parsed and a Finding (code "yaml-parse" or "not-a-workflow",
// severity unknown) is recorded. Output is deterministic: pipelines sorted
// by Path, nodes in job-declaration order, findings sorted by (Path, Node,
// Code).
func Ingest(files []File) Result {
	sorted := make([]File, len(files))
	copy(sorted, files)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	var res Result
	for _, f := range sorted {
		res.Sources = append(res.Sources, workflowmodel.Source{
			System: workflowmodel.SystemGitHubActions,
			Path:   f.Path,
			SHA256: workflowmodel.SHA256Hex(f.Data),
		})
		res.Coverage.Files++
		p, findings, err := ParseWorkflow(f.Path, f.Data)
		if err != nil {
			code := CodeNotAWorkflow
			if errors.Is(err, ErrYAML) {
				code = CodeYAMLParse
			}
			res.Coverage.Findings = append(res.Coverage.Findings, workflowmodel.Finding{
				Path: f.Path, Code: code, Detail: err.Error(), Severity: workflowmodel.SeverityUnknown,
			})
			continue
		}
		res.Coverage.Parsed++
		res.Pipelines = append(res.Pipelines, p)
		res.Coverage.Findings = append(res.Coverage.Findings, findings...)
	}
	sort.SliceStable(res.Pipelines, func(i, j int) bool { return res.Pipelines[i].Path < res.Pipelines[j].Path })
	sortFindings(res.Coverage.Findings)
	return res
}

// IsWorkflowPath reports whether path is `.github/workflows/<name>.yml|.yaml`
// (top level only).
func IsWorkflowPath(p string) bool {
	rest, ok := strings.CutPrefix(p, workflowsDir)
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return false
	}
	ext := path.Ext(rest)
	return (ext == extYML || ext == extYAML) && len(rest) > len(ext)
}

// ParseWorkflow parses one workflow file into a Pipeline; returns findings
// for constructs it recognised but did not model.
func ParseWorkflow(p string, data []byte) (workflowmodel.Pipeline, []workflowmodel.Finding, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return workflowmodel.Pipeline{}, nil, fmt.Errorf("%w: %v", ErrYAML, err)
	}
	root := documentRoot(&doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return workflowmodel.Pipeline{}, nil, fmt.Errorf("%w: document is not a mapping", ErrNotAWorkflow)
	}
	top := entries(root)
	onNode, hasOn := top[keyOn]
	jobsNode, hasJobs := top[keyJobs]
	if !hasOn && !hasJobs {
		return workflowmodel.Pipeline{}, nil, fmt.Errorf("%w: neither on nor jobs present", ErrNotAWorkflow)
	}

	pl := workflowmodel.Pipeline{
		ID:     workflowmodel.PipelineID(workflowmodel.SystemGitHubActions, p),
		Name:   baseName(p),
		System: workflowmodel.SystemGitHubActions,
		Path:   p,
	}
	if n := scalar(top[keyName]); n != "" {
		pl.Name = n
	}

	w := &walker{path: p}
	pl.Triggers = w.triggers(onNode)
	pl.Nodes = w.jobs(jobsNode)
	pl.Levels = w.levels(pl.Nodes)
	pl.Role = w.role(pl.Triggers, pl.Nodes)
	pl.Concurrency = w.concurrency(top[keyConcurrency])
	sortFindings(w.findings)
	return pl, w.findings, nil
}

// walker accumulates findings while mapping one workflow file.
type walker struct {
	path     string
	findings []workflowmodel.Finding
}

func (w *walker) find(node, code, detail string, sev workflowmodel.Severity) {
	w.findings = append(w.findings, workflowmodel.Finding{Path: w.path, Node: node, Code: code, Detail: detail, Severity: sev})
}

// triggers maps the `on` value (string, list or map) to Triggers.
func (w *walker) triggers(on *yaml.Node) []workflowmodel.Trigger {
	on = deref(on)
	if on == nil {
		return nil
	}
	switch on.Kind {
	case yaml.ScalarNode:
		if on.Tag == tagNull {
			return nil
		}
		return []workflowmodel.Trigger{{Event: on.Value}}
	case yaml.SequenceNode:
		var out []workflowmodel.Trigger
		for _, e := range strs(on) {
			out = append(out, workflowmodel.Trigger{Event: e})
		}
		return out
	case yaml.MappingNode:
		var out []workflowmodel.Trigger
		for _, kv := range pairs(on) {
			out = append(out, triggerFor(kv.key, kv.val))
		}
		return out
	}
	return nil
}

func triggerFor(event string, cfg *yaml.Node) workflowmodel.Trigger {
	t := workflowmodel.Trigger{Event: event}
	cfg = deref(cfg)
	if event == EventSchedule {
		if cfg != nil && cfg.Kind == yaml.SequenceNode {
			for _, item := range cfg.Content {
				if c := scalar(entries(deref(item))[keyCron]); c != "" {
					t.Schedule = append(t.Schedule, c)
				}
			}
		}
		return t
	}
	if cfg == nil || cfg.Kind != yaml.MappingNode {
		return t
	}
	m := entries(cfg)
	if event == EventWorkflowRun {
		t.Types = append(t.Types, strs(m[keyWorkflows])...)
	}
	t.Types = append(t.Types, strs(m[keyTypes])...)
	t.Branches = append(strs(m[keyBranches]), negate(strs(m[keyBranchesIgnore]))...)
	t.Tags = append(strs(m[keyTags]), negate(strs(m[keyTagsIgnore]))...)
	t.Paths = append(strs(m[keyPaths]), negate(strs(m[keyPathsIgnore]))...)
	return t
}

func negate(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, negatePrefix+s)
	}
	return out
}

// jobs maps the `jobs` mapping to Nodes in declaration order.
func (w *walker) jobs(jobs *yaml.Node) []workflowmodel.Node {
	jobs = deref(jobs)
	nodes := []workflowmodel.Node{}
	if jobs == nil || jobs.Kind != yaml.MappingNode || len(jobs.Content) == 0 {
		w.find("", CodeNoJobs, "jobs is missing or empty", workflowmodel.SeverityUnknown)
		return nodes
	}
	for _, kv := range pairs(jobs) {
		nodes = append(nodes, w.job(kv.key, kv.val))
	}
	return nodes
}

func (w *walker) job(id string, job *yaml.Node) workflowmodel.Node {
	n := workflowmodel.Node{ID: id, Kind: workflowmodel.NodeJob}
	job = deref(job)
	if job == nil || job.Kind != yaml.MappingNode {
		w.find(id, CodeJobEmpty, "job is not a mapping", workflowmodel.SeverityUnknown)
		return n
	}
	m := entries(job)
	n.Name = scalar(m[keyName])
	if uses := scalar(m[keyUses]); uses != "" {
		n.Kind = workflowmodel.NodeReusableWorkflow
		n.Ref = uses
	}
	n.Needs = strs(m[keyNeeds])
	n.Environment = environmentName(m[keyEnvironment])
	n.If = scalar(m[keyIf])
	n.RunsOn = runsOn(m[keyRunsOn])
	n.Matrix = matrix(entries(deref(m[keyStrategy]))[keyMatrix])

	if _, ok := m[keyContainer]; ok {
		w.find(id, CodeJobContainer, "container is not modelled", workflowmodel.SeverityUnsupported)
	} else if _, ok := m[keyServices]; ok {
		w.find(id, CodeJobContainer, "services are not modelled", workflowmodel.SeverityUnsupported)
	}
	if scalar(m[keySecrets]) == secretsInherit {
		w.find(id, CodeSecretsInherit, "reusable job inherits all secrets", workflowmodel.SeverityWarning)
	}
	steps := deref(m[keySteps])
	if n.Ref == "" && (steps == nil || steps.Kind != yaml.SequenceNode || len(steps.Content) == 0) {
		w.find(id, CodeJobEmpty, "job has no steps and no uses", workflowmodel.SeverityUnknown)
	}
	return n
}

func environmentName(n *yaml.Node) string {
	n = deref(n)
	if n == nil {
		return ""
	}
	if n.Kind == yaml.MappingNode {
		return scalar(entries(n)[keyName])
	}
	return scalar(n)
}

func runsOn(n *yaml.Node) []string {
	n = deref(n)
	if n == nil {
		return nil
	}
	if n.Kind == yaml.MappingNode {
		m := entries(n)
		return append(strs(m[keyGroup]), strs(m[keyLabels])...)
	}
	return strs(n)
}

func matrix(n *yaml.Node) *workflowmodel.Matrix {
	n = deref(n)
	if n == nil {
		return nil
	}
	if n.Kind == yaml.ScalarNode {
		return &workflowmodel.Matrix{Dynamic: isExpression(n.Value)}
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	mx := &workflowmodel.Matrix{}
	for _, kv := range pairs(n) {
		v := deref(kv.val)
		if v != nil && v.Kind == yaml.ScalarNode && isExpression(v.Value) {
			mx.Dynamic = true
		}
		switch kv.key {
		case keyInclude:
			mx.Include = seqLen(v)
		case keyExclude:
			mx.Exclude = seqLen(v)
		default:
			mx.Keys = append(mx.Keys, kv.key)
		}
	}
	sort.Strings(mx.Keys)
	return mx
}

func seqLen(n *yaml.Node) int {
	if n == nil || n.Kind != yaml.SequenceNode {
		return 0
	}
	return len(n.Content)
}

func isExpression(s string) bool { return strings.Contains(s, expressionOpen) }

// levels assigns topological levels, repairing unknown needs and cycles so
// the pipeline is always well-formed.
func (w *walker) levels(nodes []workflowmodel.Node) int {
	count, err := workflowmodel.Levels(nodes)
	if err == nil {
		return count
	}
	known := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		known[n.ID] = true
	}
	for i := range nodes {
		kept := nodes[i].Needs[:0]
		for _, need := range nodes[i].Needs {
			if known[need] {
				kept = append(kept, need)
				continue
			}
			w.find(nodes[i].ID, CodeNeedsUnresolved, fmt.Sprintf("needs unknown job %q", need), workflowmodel.SeverityUnknown)
		}
		if len(kept) == 0 {
			kept = nil
		}
		nodes[i].Needs = kept
	}
	count, err = workflowmodel.Levels(nodes)
	if err == nil {
		return count
	}
	w.find("", CodeNeedsCycle, err.Error(), workflowmodel.SeverityUnknown)
	for i := range nodes {
		nodes[i].Level = 0
	}
	if len(nodes) == 0 {
		return 0
	}
	return 1
}

// role derives the pipeline role from its triggers; first rule wins.
func (w *walker) role(triggers []workflowmodel.Trigger, nodes []workflowmodel.Node) workflowmodel.Role {
	events := map[string]bool{}
	pushTags := false
	for _, t := range triggers {
		events[t.Event] = true
		if t.Event == EventPush {
			for _, tag := range t.Tags {
				if !strings.HasPrefix(tag, negatePrefix) {
					pushTags = true
				}
			}
		}
	}
	var envJobs []string
	for _, n := range nodes {
		if n.Environment != "" {
			envJobs = append(envJobs, n.ID)
		}
	}
	// onlyWithin reports whether every event is e or one of the manual
	// companions (workflow_dispatch, workflow_call), and e is present: a
	// nightly with a manual button is still scheduled.
	onlyWithin := func(e string) bool {
		if !events[e] {
			return false
		}
		for ev := range events {
			if ev != e && ev != EventWorkflowDispatch && ev != EventWorkflowCall {
				return false
			}
		}
		return true
	}
	only := func(e string) bool { return len(events) == 1 && events[e] }

	switch {
	case events[EventPullRequest] || events[EventPullRequestTarget] || events[EventMergeGroup]:
		for _, id := range envJobs {
			w.find(id, CodeEnvironmentOnPR, "PR-gating job targets an environment", workflowmodel.SeverityWarning)
		}
		return workflowmodel.RolePRVerify
	case (events[EventPush] && pushTags) || events[EventRelease]:
		return workflowmodel.RoleRelease
	case events[EventPush]:
		return workflowmodel.RolePushVerify
	case events[EventWorkflowDispatch] && len(envJobs) > 0:
		return workflowmodel.RoleDeploy
	case events[EventDeployment] || events[EventDeploymentStatus]:
		return workflowmodel.RoleDeploy
	case onlyWithin(EventSchedule):
		return workflowmodel.RoleScheduled
	case onlyWithin(EventWorkflowDispatch):
		return workflowmodel.RoleManual
	case only(EventWorkflowCall):
		w.find("", CodeReusableOnly, "called by other workflows", workflowmodel.SeverityWarning)
		return workflowmodel.RoleOther
	}
	return workflowmodel.RoleOther
}

// concurrency maps the top-level concurrency string or map.
func (w *walker) concurrency(n *yaml.Node) *workflowmodel.Concurrency {
	n = deref(n)
	if n == nil {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return &workflowmodel.Concurrency{Group: scalar(n)}
	}
	m := entries(n)
	c := &workflowmodel.Concurrency{Group: scalar(m[keyGroup])}
	cancel := deref(m[keyCancelInProgress])
	if cancel == nil || cancel.Kind != yaml.ScalarNode {
		return c
	}
	switch {
	case strings.EqualFold(cancel.Value, literalTrue):
		c.CancelInProgress = true
	case strings.EqualFold(cancel.Value, literalFalse) || cancel.Value == "":
		c.CancelInProgress = false
	default:
		w.find("", CodeConcurrencyExpression, fmt.Sprintf("cancel-in-progress is %q", cancel.Value), workflowmodel.SeverityWarning)
	}
	return c
}

// YAML node helpers. All of them resolve aliases and tolerate nil.

func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil
	}
	return deref(doc.Content[0])
}

func deref(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

type pair struct {
	key string
	val *yaml.Node
}

// pairs lists a mapping's entries in declaration order. A key YAML 1.1
// would read as the boolean `on` is normalised back to "on".
func pairs(n *yaml.Node) []pair {
	n = deref(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	out := make([]pair, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := deref(n.Content[i])
		key := k.Value
		if k.Tag == tagBool && strings.EqualFold(key, keyOn) {
			key = keyOn
		}
		out = append(out, pair{key: key, val: n.Content[i+1]})
	}
	return out
}

func entries(n *yaml.Node) map[string]*yaml.Node {
	ps := pairs(n)
	if ps == nil {
		return nil
	}
	m := make(map[string]*yaml.Node, len(ps))
	for _, p := range ps {
		if _, dup := m[p.key]; !dup {
			m[p.key] = p.val
		}
	}
	return m
}

func scalar(n *yaml.Node) string {
	n = deref(n)
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag == tagNull {
		return ""
	}
	return n.Value
}

// strs reads a scalar as a one-element list or a sequence of scalars.
func strs(n *yaml.Node) []string {
	n = deref(n)
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.ScalarNode:
		if s := scalar(n); s != "" {
			return []string{s}
		}
	case yaml.SequenceNode:
		var out []string
		for _, c := range n.Content {
			if s := scalar(c); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func baseName(p string) string {
	b := path.Base(p)
	return strings.TrimSuffix(b, path.Ext(b))
}

func sortFindings(fs []workflowmodel.Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Node != b.Node {
			return a.Node < b.Node
		}
		return a.Code < b.Code
	})
}
