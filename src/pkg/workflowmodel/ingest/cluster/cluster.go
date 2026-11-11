// Package cluster ingests Tekton resources from a live Kubernetes context
// (hivecommons/hive#17620, RFC phase 6). It lists Pipelines, Tasks, the
// latest PipelineRuns per Pipeline and Tekton Triggers resources through
// kubectl, serialises every object to YAML under a stable virtual path and
// hands those files to the existing file-based Tekton ingester, so there is
// exactly one Tekton parser. Run outcomes are observations, not inferences:
// they are reported alongside the model and recorded as annotations, never
// as inferred lifecycle elements.
package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/ingest"
	"github.com/hivecommons/hive/pkg/workflowmodel/tekton"
)

// Caps. A cluster is not a repository: without them an all-namespaces
// fetch against a busy CI cluster would pull thousands of PipelineRuns.
const (
	// DefaultRunsPerPipeline is how many of the newest PipelineRuns per
	// Pipeline are kept when Source.RunsPerPipeline is zero.
	DefaultRunsPerPipeline = 3
	// MaxRunsPerPipeline bounds Source.RunsPerPipeline.
	MaxRunsPerPipeline = 20
	// MaxObjects is the most objects (after run trimming) one fetch keeps.
	MaxObjects = 500
	// MaxNamespaces is the most namespaces an all-namespaces fetch may span.
	MaxNamespaces = 25
	// MaxTotalBytes caps the YAML handed to the ingester.
	MaxTotalBytes = 8 << 20
)

// Findings this package adds on top of the Tekton ingester's.
const (
	CodeTriggersCRDMissing = "triggers-crd-missing"
	CodeNamespaceCapped    = "namespaces-capped"
	CodeRunsTrimmed        = "runs-trimmed"
	CodeObservedRun        = "observed-run-status"
)

// AnnotationObservedRun is the model annotation prefix under which the last
// observed PipelineRun status of each pipeline is recorded
// (<prefix><pipeline id> = <status>). It is an observation, not an
// inference, so it never carries Confidence.
const AnnotationObservedRun = "workflow.hivecommons.dev/observed-run/"

// AnnotationSource records the cluster source label on the model.
const AnnotationSource = "workflow.hivecommons.dev/source"

// Errors a caller can distinguish. The API maps them to clear responses.
var (
	// ErrNoCRD means the cluster has no tekton.dev Pipeline resource type.
	ErrNoCRD = errors.New("tekton is not installed in this cluster (no pipelines.tekton.dev resource)")
	// ErrForbidden means RBAC denied the list.
	ErrForbidden = errors.New("kubectl was denied: the hive's credentials cannot list Tekton resources")
	// ErrNoContext means the named context is not in the kubeconfig.
	ErrNoContext = errors.New("kube context not found")
	// ErrNamespaceRequired means neither Namespace nor AllNamespaces was set.
	ErrNamespaceRequired = errors.New("a namespace is required (or set allNamespaces)")
	// ErrTooLarge means a cap was exceeded.
	ErrTooLarge = errors.New("cluster fetch exceeds the ingest size cap")
	// ErrNoRunner means no kubectl runner was supplied.
	ErrNoRunner = errors.New("no kubectl runner")
)

// Source names what to fetch.
type Source struct {
	// Context is the kubeconfig context; empty uses kubectl's current one.
	Context string `json:"context,omitempty"`
	// Namespace scopes the list. Required unless AllNamespaces is set.
	Namespace string `json:"namespace,omitempty"`
	// AllNamespaces lists every namespace, capped at MaxNamespaces.
	AllNamespaces bool `json:"allNamespaces,omitempty"`
	// Selector is a label selector applied to every list.
	Selector string `json:"selector,omitempty"`
	// RunsPerPipeline is how many newest runs per pipeline to keep
	// (0 = DefaultRunsPerPipeline, capped at MaxRunsPerPipeline).
	RunsPerPipeline int `json:"runsPerPipeline,omitempty"`
}

// String is the source label recorded on the model: cluster:<ctx>/<ns>.
func (s Source) String() string {
	ctx := s.Context
	if ctx == "" {
		ctx = "current-context"
	}
	ns := s.Namespace
	if s.AllNamespaces {
		ns = "*"
	}
	return "cluster:" + ctx + "/" + ns
}

// Runner executes kubectl with args and returns stdout. Implementations
// must include stderr in the error so the failure can be classified.
type Runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// RunnerFunc adapts a function to Runner.
type RunnerFunc func(ctx context.Context, args ...string) ([]byte, error)

// Run calls f.
func (f RunnerFunc) Run(ctx context.Context, args ...string) ([]byte, error) { return f(ctx, args...) }

// RunStatus is the last observed outcome of one pipeline.
type RunStatus struct {
	Pipeline  string `json:"pipeline"`
	Namespace string `json:"namespace"`
	Run       string `json:"run"`
	// Status is Succeeded, Running, Unknown, or the Tekton reason (Failed,
	// Cancelled, …) when the Succeeded condition is false.
	Status    string `json:"status"`
	Completed string `json:"completed,omitempty"`
}

// Fetched is the raw harvest of one Fetch.
type Fetched struct {
	Files      []tekton.File
	Runs       []RunStatus
	Namespaces []string
	Findings   []workflowmodel.Finding
	Objects    int
}

const kindPipelineRun = "PipelineRun"

// resources names the kubectl resource types fetched, in order. Pipelines
// are required (their absence means no Tekton); the rest are optional.
var resources = []struct {
	plural   string
	kind     string
	required bool
}{
	{"pipelines.tekton.dev", "Pipeline", true},
	{"tasks.tekton.dev", "Task", false},
	{"pipelineruns.tekton.dev", kindPipelineRun, false},
	{"eventlisteners.triggers.tekton.dev", "EventListener", false},
	{"triggertemplates.triggers.tekton.dev", "TriggerTemplate", false},
	{"triggerbindings.triggers.tekton.dev", "TriggerBinding", false},
	{"triggers.triggers.tekton.dev", "Trigger", false},
}

// Fetch lists the Tekton resources src names through runner and returns
// them as virtual files cluster/<ns>/<kind>/<name>.yaml plus the latest run
// status per pipeline.
func Fetch(ctx context.Context, runner Runner, src Source) (*Fetched, error) {
	if runner == nil {
		return nil, ErrNoRunner
	}
	if strings.TrimSpace(src.Namespace) == "" && !src.AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	keep := src.RunsPerPipeline
	if keep <= 0 {
		keep = DefaultRunsPerPipeline
	}
	if keep > MaxRunsPerPipeline {
		keep = MaxRunsPerPipeline
	}
	out := &Fetched{}
	var runs []map[string]any
	nsSeen := map[string]bool{}
	capped := false
	for _, r := range resources {
		items, err := list(ctx, runner, src, r.plural)
		if err != nil {
			if errors.Is(err, ErrNoCRD) && !r.required {
				if strings.Contains(r.plural, "triggers.tekton.dev") {
					out.Findings = append(out.Findings, workflowmodel.Finding{Path: "cluster", Code: CodeTriggersCRDMissing, Detail: r.plural + " not installed", Severity: workflowmodel.SeverityUnsupported})
				}
				continue
			}
			return nil, err
		}
		for _, it := range items {
			ns := stringAt(mapAt(it, "metadata"), "namespace")
			if !nsSeen[ns] {
				if src.AllNamespaces && len(nsSeen) >= MaxNamespaces {
					capped = true
					continue
				}
				nsSeen[ns] = true
			}
			if r.kind == kindPipelineRun {
				runs = append(runs, it)
				continue
			}
			if err := out.add(it, r.kind); err != nil {
				return nil, err
			}
		}
	}
	if capped {
		out.Findings = append(out.Findings, workflowmodel.Finding{Path: "cluster", Code: CodeNamespaceCapped, Detail: fmt.Sprintf("only the first %d namespaces were read", MaxNamespaces), Severity: workflowmodel.SeverityWarning})
	}
	kept, statuses, trimmed := trimRuns(runs, keep, nsSeen)
	if trimmed > 0 {
		out.Findings = append(out.Findings, workflowmodel.Finding{Path: "cluster", Code: CodeRunsTrimmed, Detail: fmt.Sprintf("%d older PipelineRuns skipped; newest %d per pipeline kept", trimmed, keep), Severity: workflowmodel.SeverityWarning})
	}
	for _, it := range kept {
		if err := out.add(it, kindPipelineRun); err != nil {
			return nil, err
		}
	}
	out.Runs = statuses
	total := 0
	for _, f := range out.Files {
		total += len(f.Data)
	}
	if total > MaxTotalBytes {
		return nil, fmt.Errorf("%w: %d bytes of YAML", ErrTooLarge, total)
	}
	for ns := range nsSeen {
		out.Namespaces = append(out.Namespaces, ns)
	}
	sort.Strings(out.Namespaces)
	sort.SliceStable(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	return out, nil
}

// add serialises one object into the harvest, enforcing MaxObjects.
func (f *Fetched) add(obj map[string]any, kind string) error {
	f.Objects++
	if f.Objects > MaxObjects {
		return fmt.Errorf("%w: more than %d objects", ErrTooLarge, MaxObjects)
	}
	file, err := fileFor(obj, kind)
	if err != nil {
		return err
	}
	f.Files = append(f.Files, file)
	return nil
}

// list runs one kubectl get and returns the items.
func list(ctx context.Context, runner Runner, src Source, plural string) ([]map[string]any, error) {
	var args []string
	if src.Context != "" {
		args = append(args, "--context", src.Context)
	}
	args = append(args, "get", plural, "-o", "json")
	if src.AllNamespaces {
		args = append(args, "--all-namespaces")
	} else {
		args = append(args, "-n", src.Namespace)
	}
	if src.Selector != "" {
		args = append(args, "-l", src.Selector)
	}
	raw, err := runner.Run(ctx, args...)
	if err != nil {
		return nil, classify(err, plural, src.Context)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("kubectl get %s: decode: %w", plural, err)
	}
	var items []map[string]any
	for _, it := range sliceAt(doc, "items") {
		if m, ok := it.(map[string]any); ok {
			items = append(items, m)
		}
	}
	return items, nil
}

// classify maps kubectl's stderr to the package errors.
func classify(err error, plural, kubeContext string) error {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "doesn't have a resource type"), strings.Contains(msg, "could not find the requested resource"), strings.Contains(msg, "no matches for kind"):
		return fmt.Errorf("%w (%s)", ErrNoCRD, plural)
	case strings.Contains(msg, "forbidden"), strings.Contains(msg, "unauthorized"):
		return fmt.Errorf("%w: %v", ErrForbidden, err)
	case strings.Contains(msg, "context") && (strings.Contains(msg, "does not exist") || strings.Contains(msg, "not found")):
		return fmt.Errorf("%w: %q", ErrNoContext, kubeContext)
	}
	return fmt.Errorf("kubectl get %s: %w", plural, err)
}

// fileFor serialises one object to a virtual file. Server-side noise
// (managedFields, resourceVersion, uid) is dropped so re-ingesting an
// unchanged cluster hashes the same; status is kept on PipelineRuns only.
func fileFor(obj map[string]any, kind string) (tekton.File, error) {
	md := mapAt(obj, "metadata")
	ns := stringAt(md, "namespace")
	if ns == "" {
		ns = "_cluster"
	}
	name := stringAt(md, "name")
	if name == "" {
		name = strings.TrimSuffix(stringAt(md, "generateName"), "-")
	}
	clean := make(map[string]any, len(obj))
	for k, v := range obj {
		clean[k] = v
	}
	cmd := make(map[string]any, len(md))
	for k, v := range md {
		switch k {
		case "managedFields", "resourceVersion", "uid", "selfLink":
			continue
		}
		cmd[k] = v
	}
	clean["metadata"] = cmd
	if kind != kindPipelineRun {
		delete(clean, "status")
	}
	data, err := yaml.Marshal(clean)
	if err != nil {
		return tekton.File{}, fmt.Errorf("%s/%s %s: %w", ns, kind, name, err)
	}
	return tekton.File{Path: fmt.Sprintf("cluster/%s/%s/%s.yaml", ns, strings.ToLower(kind), workflowmodel.Slug(name)), Data: data}, nil
}

// trimRuns keeps the newest keep runs per (namespace, pipeline) and
// reports the latest status of each. Runs in namespaces beyond the cap are
// dropped.
func trimRuns(runs []map[string]any, keep int, nsSeen map[string]bool) ([]map[string]any, []RunStatus, int) {
	type keyed struct {
		when string
		run  map[string]any
	}
	groups := map[string][]keyed{}
	for _, r := range runs {
		ns := stringAt(mapAt(r, "metadata"), "namespace")
		if !nsSeen[ns] {
			continue
		}
		key := ns + "/" + runPipelineName(r)
		groups[key] = append(groups[key], keyed{when: runTime(r), run: r})
	}
	order := make([]string, 0, len(groups))
	for k := range groups {
		order = append(order, k)
	}
	sort.Strings(order)
	var kept []map[string]any
	var statuses []RunStatus
	trimmed := 0
	for _, key := range order {
		g := groups[key]
		sort.SliceStable(g, func(i, j int) bool { return g[i].when > g[j].when })
		if len(g) > keep {
			trimmed += len(g) - keep
			g = g[:keep]
		}
		for _, k := range g {
			kept = append(kept, k.run)
		}
		latest := g[0].run
		md := mapAt(latest, "metadata")
		statuses = append(statuses, RunStatus{
			Pipeline:  runPipelineName(latest),
			Namespace: stringAt(md, "namespace"),
			Run:       stringAt(md, "name"),
			Status:    runOutcome(latest),
			Completed: stringAt(mapAt(latest, "status"), "completionTime"),
		})
	}
	return kept, statuses, trimmed
}

// runPipelineName is the Pipeline a run executes: pipelineRef.name, or
// the run's own name for an embedded pipelineSpec.
func runPipelineName(run map[string]any) string {
	if name := stringAt(mapAt(mapAt(run, "spec"), "pipelineRef"), "name"); name != "" {
		return name
	}
	md := mapAt(run, "metadata")
	if name := stringAt(md, "name"); name != "" {
		return name
	}
	return strings.TrimSuffix(stringAt(md, "generateName"), "-")
}

// runTime orders runs: completion, then start, then creation.
func runTime(run map[string]any) string {
	st := mapAt(run, "status")
	for _, k := range []string{"completionTime", "startTime"} {
		if v := stringAt(st, k); v != "" {
			return v
		}
	}
	return stringAt(mapAt(run, "metadata"), "creationTimestamp")
}

// runOutcome reads the Succeeded condition.
func runOutcome(run map[string]any) string {
	for _, c := range mapsAt(mapAt(run, "status"), "conditions") {
		if stringAt(c, "type") != "Succeeded" {
			continue
		}
		switch stringAt(c, "status") {
		case "True":
			return "Succeeded"
		case "False":
			if reason := stringAt(c, "reason"); reason != "" {
				return reason
			}
			return "Failed"
		default:
			return "Running"
		}
	}
	return "Unknown"
}

// Result is a cluster ingest: the model plus what was observed.
type Result struct {
	Model *workflowmodel.Model `json:"model"`
	Runs  []RunStatus          `json:"runs"`
	// Files is how many virtual files were handed to the Tekton ingester.
	Files      int      `json:"files"`
	Namespaces []string `json:"namespaces"`
	Source     string   `json:"source"`
}

// Ingest fetches src and composes the lifecycle model for scope exactly as
// a repository ingest would (ingest.Lifecycle over the virtual files). The
// latest run outcome per pipeline is recorded as an observed-run annotation
// and a finding; it is never an inferred element.
func Ingest(ctx context.Context, runner Runner, src Source, scope workflowmodel.Scope) (*Result, error) {
	fetched, err := Fetch(ctx, runner, src)
	if err != nil {
		return nil, err
	}
	files := make(map[string][]byte, len(fetched.Files))
	for _, f := range fetched.Files {
		files[f.Path] = f.Data
	}
	m := ingest.Lifecycle(scope, files)
	m.Spec.Meta.Source = workflowmodel.SourceTekton
	m.Spec.Meta.Description = fmt.Sprintf("Lifecycle ingested from %s: hive-default stages with the cluster's Tekton pipelines bound", src.String())
	m.Spec.Coverage.Findings = append(m.Spec.Coverage.Findings, fetched.Findings...)
	if m.Metadata.Annotations == nil {
		m.Metadata.Annotations = map[string]string{}
	}
	m.Metadata.Annotations[AnnotationSource] = src.String()
	byName := map[string]string{}
	for _, p := range m.Spec.Pipelines {
		byName[p.Name] = p.ID
	}
	for _, r := range fetched.Runs {
		id, ok := byName[r.Pipeline]
		if !ok {
			continue
		}
		m.Metadata.Annotations[AnnotationObservedRun+id] = r.Status
		m.Spec.Coverage.Findings = append(m.Spec.Coverage.Findings, workflowmodel.Finding{
			Path: fmt.Sprintf("cluster/%s/pipelinerun/%s.yaml", r.Namespace, workflowmodel.Slug(r.Run)), Node: id,
			Code: CodeObservedRun, Detail: "last run " + r.Status, Severity: workflowmodel.SeverityWarning,
		})
	}
	return &Result{Model: m, Runs: fetched.Runs, Files: len(fetched.Files), Namespaces: fetched.Namespaces, Source: src.String()}, nil
}

func stringAt(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func mapAt(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	sub, _ := m[key].(map[string]any)
	return sub
}

func sliceAt(m map[string]any, key string) []any {
	if m == nil {
		return nil
	}
	s, _ := m[key].([]any)
	return s
}

func mapsAt(m map[string]any, key string) []map[string]any {
	items := sliceAt(m, key)
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if im, ok := it.(map[string]any); ok {
			out = append(out, im)
		}
	}
	return out
}
