package tekton

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// File is one source file handed to Ingest.
type File struct {
	Path string
	Data []byte
}

// Result is what Ingest produces: pipelines, the sources they came from,
// and how completely the sources were understood.
type Result struct {
	Pipelines []workflowmodel.Pipeline
	Sources   []workflowmodel.Source
	Coverage  workflowmodel.Coverage
}

// document is one parsed YAML mapping with the path it came from.
type document struct {
	path string
	doc  map[string]any
}

// pipelineRun is a PipelineRun that references a Pipeline by name, held
// until every Pipeline has been collected.
type pipelineRun struct {
	path string
	doc  map[string]any
	ref  string
}

// ingest holds the state of one Ingest call.
type ingest struct {
	result    Result
	findings  []workflowmodel.Finding
	pipelines []*workflowmodel.Pipeline
	// byName indexes Pipeline resources (not embedded runs) by
	// metadata.name; the first declaration wins.
	byName map[string]*workflowmodel.Pipeline
	runs   []pipelineRun
	// webhooks are Pipeline names a TriggerTemplate instantiates.
	webhooks []string
}

// Ingest reads every file, collects every Tekton document and emits one
// Pipeline per Pipeline resource and per PipelineRun with an embedded
// pipelineSpec. PipelineRuns that reference a Pipeline by name add their
// Pipelines-as-Code triggers to it; TriggerTemplates that instantiate a
// Pipeline add a webhook trigger. It never fails on a bad file: problems
// become Findings. Output is deterministic: pipelines sorted by (Path,
// Name), findings by (Path, Node, Code, Detail).
func Ingest(files []File) Result {
	in := &ingest{byName: map[string]*workflowmodel.Pipeline{}}
	in.result.Coverage.Files = len(files)
	for _, f := range files {
		in.file(f)
	}
	in.linkRuns()
	in.linkWebhooks()
	in.finish()
	return in.result
}

func (in *ingest) find(path, node, code, detail string, sev workflowmodel.Severity) {
	in.findings = append(in.findings, workflowmodel.Finding{Path: path, Node: node, Code: code, Detail: detail, Severity: sev})
}

// file parses one file and dispatches its Tekton documents.
func (in *ingest) file(f File) {
	in.result.Sources = append(in.result.Sources, workflowmodel.Source{System: workflowmodel.SystemTekton, Path: f.Path, SHA256: workflowmodel.SHA256Hex(f.Data)})
	docs, err := SplitDocuments(f.Data)
	if err != nil {
		in.find(f.Path, "", CodeYAML, err.Error(), workflowmodel.SeverityUnknown)
		return
	}
	var tekton []document
	for _, d := range docs {
		if IsTektonDocument(d) {
			tekton = append(tekton, document{path: f.Path, doc: d})
		}
	}
	if len(tekton) == 0 {
		in.find(f.Path, "", CodeNotTekton, "no tekton.dev documents", workflowmodel.SeverityUnknown)
		return
	}
	in.result.Coverage.Parsed++

	// Pipeline ids only carry the resource name when the file emits more
	// than one pipeline, so count first.
	emitted := 0
	for _, d := range tekton {
		if emitsPipeline(d.doc) {
			emitted++
		}
	}
	taskDefs := 0
	for _, d := range tekton {
		kind := stringAt(d.doc, "kind")
		switch kind {
		case kindPipeline:
			in.pipeline(d, emitted > 1, true)
		case kindPipelineRun:
			in.pipelineRun(d, emitted > 1)
		case kindTask, kindClusterTask, kindStepAction, kindTaskRun:
			taskDefs++
		case kindEventListener, kindTriggerTemplate, kindTriggerBinding, kindTrigger:
			in.find(f.Path, "", CodeTektonTriggers, kind+" "+metadataName(d.doc), workflowmodel.SeverityUnsupported)
			if kind == kindTriggerTemplate {
				in.webhooks = append(in.webhooks, templatePipelineRefs(d.doc)...)
			}
		default:
			in.find(f.Path, "", CodeUnknownKind, kind+" "+metadataName(d.doc), workflowmodel.SeverityUnknown)
		}
	}
	if taskDefs > 0 {
		in.find(f.Path, "", CodeTaskDefinitions, fmt.Sprintf("%d task/stepaction documents", taskDefs), workflowmodel.SeverityWarning)
	}
}

// emitsPipeline reports whether doc becomes a Pipeline of its own.
func emitsPipeline(doc map[string]any) bool {
	switch stringAt(doc, "kind") {
	case kindPipeline:
		return true
	case kindPipelineRun:
		return mapAt(mapAt(doc, "spec"), "pipelineSpec") != nil
	}
	return false
}

// pipeline emits a Pipeline from a Pipeline resource or an embedded
// pipelineSpec. register says whether PipelineRuns may reference it by
// name.
func (in *ingest) pipeline(d document, qualify, register bool) *workflowmodel.Pipeline {
	name := metadataName(d.doc)
	id := workflowmodel.PipelineID(workflowmodel.SystemTekton, d.path)
	if qualify {
		id += "-" + workflowmodel.Slug(name)
	}
	spec := mapAt(d.doc, "spec")
	if stringAt(d.doc, "kind") == kindPipelineRun {
		spec = mapAt(spec, "pipelineSpec")
	}
	p, findings := buildPipeline(d.path, id, name, spec)
	in.findings = append(in.findings, findings...)
	in.pipelines = append(in.pipelines, &p)
	if register {
		if _, dup := in.byName[name]; !dup {
			in.byName[name] = &p
		}
	}
	return &p
}

// pipelineRun handles a PipelineRun: embedded specs become pipelines
// carrying the run's own triggers; references are linked after every
// Pipeline is known.
func (in *ingest) pipelineRun(d document, qualify bool) {
	spec := mapAt(d.doc, "spec")
	if mapAt(spec, "pipelineSpec") != nil {
		p := in.pipeline(d, qualify, false)
		addTriggers(p, pacTriggers(d.doc))
		return
	}
	ref := mapAt(spec, "pipelineRef")
	in.runs = append(in.runs, pipelineRun{path: d.path, doc: d.doc, ref: pipelineRefName(ref)})
}

// pipelineRefName renders a pipelineRef: the name, or resolver plus its
// identifying params when resolved remotely.
func pipelineRefName(ref map[string]any) string {
	if name := stringAt(ref, "name"); name != "" {
		return name
	}
	resolver := stringAt(ref, "resolver")
	if resolver == "" {
		return ""
	}
	var parts []string
	for _, p := range mapsAt(ref, "params") {
		if contains(resolverParamNames, stringAt(p, "name")) {
			if v := stringAt(p, "value"); v != "" {
				parts = append(parts, v)
			}
		}
	}
	if len(parts) == 0 {
		return resolver
	}
	return resolver + ":" + strings.Join(parts, "/")
}

// linkRuns attaches each referencing PipelineRun's triggers to its
// Pipeline, or records that the Pipeline lives elsewhere.
func (in *ingest) linkRuns() {
	for _, r := range in.runs {
		p, ok := in.byName[r.ref]
		if !ok {
			detail := "pipelineRef " + r.ref + " not in ingested files"
			if r.ref == "" {
				detail = "PipelineRun has neither pipelineRef nor pipelineSpec"
			}
			in.find(r.path, "", CodePipelineRefExternal, detail, workflowmodel.SeverityWarning)
			continue
		}
		addTriggers(p, pacTriggers(r.doc))
	}
}

// linkWebhooks adds a webhook trigger to every Pipeline a TriggerTemplate
// instantiates.
func (in *ingest) linkWebhooks() {
	for _, name := range in.webhooks {
		p, ok := in.byName[name]
		if !ok {
			continue
		}
		addTriggers(p, []workflowmodel.Trigger{{Event: eventWebhook}})
	}
}

// templatePipelineRefs lists the Pipeline names a TriggerTemplate's
// resourcetemplates instantiate through pipelineRef.name.
func templatePipelineRefs(doc map[string]any) []string {
	var out []string
	for _, rt := range mapsAt(mapAt(doc, "spec"), "resourcetemplates") {
		if stringAt(rt, "kind") != kindPipelineRun {
			continue
		}
		if name := stringAt(mapAt(mapAt(rt, "spec"), "pipelineRef"), "name"); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// addTriggers appends ts to p.Triggers, skipping any trigger whose full
// value (event and every list, order-insensitive) is already present, so
// many PipelineRuns of one Pipeline do not repeat the same trigger.
// First-seen order is preserved.
func addTriggers(p *workflowmodel.Pipeline, ts []workflowmodel.Trigger) {
	seen := make(map[string]bool, len(p.Triggers)+len(ts))
	for _, t := range p.Triggers {
		seen[triggerKey(t)] = true
	}
	for _, t := range ts {
		k := triggerKey(t)
		if seen[k] {
			continue
		}
		seen[k] = true
		p.Triggers = append(p.Triggers, t)
	}
}

// triggerKey is a canonical string for a trigger: the event followed by
// each list sorted, so list order does not make two equal triggers differ.
func triggerKey(t workflowmodel.Trigger) string {
	var b strings.Builder
	b.WriteString(t.Event)
	for _, list := range [][]string{t.Types, t.Branches, t.Tags, t.Paths, t.Schedule} {
		sorted := append([]string(nil), list...)
		sort.Strings(sorted)
		b.WriteString("\x1f")
		b.WriteString(strings.Join(sorted, "\x1e"))
	}
	return b.String()
}

func hasEvent(ts []workflowmodel.Trigger, event string) bool {
	for _, t := range ts {
		if t.Event == event {
			return true
		}
	}
	return false
}

// finish derives roles, flags trigger-less pipelines and sorts output.
func (in *ingest) finish() {
	for _, p := range in.pipelines {
		p.Role = roleFor(p.Triggers)
		if len(p.Triggers) == 0 {
			in.find(p.Path, "", CodeNoTrigger, "pipeline "+p.Name+" has no PipelineRun or trigger in the ingested files", workflowmodel.SeverityWarning)
		}
		in.result.Pipelines = append(in.result.Pipelines, *p)
	}
	sort.SliceStable(in.result.Pipelines, func(i, j int) bool {
		a, b := in.result.Pipelines[i], in.result.Pipelines[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Name < b.Name
	})
	sort.SliceStable(in.findings, func(i, j int) bool {
		a, b := in.findings[i], in.findings[j]
		switch {
		case a.Path != b.Path:
			return a.Path < b.Path
		case a.Node != b.Node:
			return a.Node < b.Node
		case a.Code != b.Code:
			return a.Code < b.Code
		}
		return a.Detail < b.Detail
	})
	in.result.Coverage.Findings = in.findings
}
