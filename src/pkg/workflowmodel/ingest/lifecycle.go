package ingest

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
)

// LifecycleVersion is the spec.meta.version every ingested lifecycle
// model starts at; the store and marketplace assign later versions.
const LifecycleVersion = "1"

// EventPipelineSucceeded moves work along the ingested post-merge chain.
const EventPipelineSucceeded = "pipeline-succeeded"

// Lifecycle ingests files for scope and returns a model with the RFC's
// lifecycle half filled in so that it compiles: the hive-default lifecycle
// (states, columns, builtin derivation, roles, gates, events) plus the
// repository's pipelines bound where they run — PR-verify pipelines on the
// CI / Review states, and the post-merge chain (main CI, release, deploys,
// done) as new columns after Merged. Everything the ingest added is
// origin ingested, confidence inferred (the compiler warns per element)
// and the model's provenance is ingested-unreviewed until a human edits it.
//
// The intake half (RFC phase 5) then reads the tree's issue and
// pull-request templates, CONTRIBUTING, CODEOWNERS/OWNERS and label
// files, plus branch rules carried at BranchRulesPath, and adds intake
// and triage states, roles, gates and checks; see intake.go. A tree with
// none of those inputs yields exactly the pipeline-only model.
//
// The process half (Repository) is unchanged; this composes on top of it.
func Lifecycle(scope workflowmodel.Scope, files map[string][]byte) *workflowmodel.Model {
	return LifecycleOptions(scope, files, Options{BranchRules: BranchRulesFromFiles(files)})
}

// LifecycleOptions is Lifecycle with the branch rules passed explicitly
// (the forge API reading, when the caller had a client).
func LifecycleOptions(scope workflowmodel.Scope, files map[string][]byte, opts Options) *workflowmodel.Model {
	base := Repository(scope, files)
	m := hivedefault.Generate()
	m.Metadata.Name = base.Metadata.Name
	m.Metadata.Annotations = base.Metadata.Annotations
	m.Spec.Scope = scope
	m.Spec.Meta.Version = LifecycleVersion
	m.Spec.Meta.Author = ""
	m.Spec.Meta.Description = fmt.Sprintf("Lifecycle ingested from %s: hive-default stages with the repository's pipelines bound", scope.Repository)
	m.Spec.Meta.Source = ingestSource(base)
	m.Spec.Meta.Provenance = workflowmodel.ProvenanceIngestedUnreviewed
	m.Spec.Pipelines = base.Spec.Pipelines
	m.Spec.Sources = base.Spec.Sources
	m.Spec.Coverage = base.Spec.Coverage
	bindPipelines(m, base)
	applyIntake(m, readIntake(files), opts.BranchRules)
	return m
}

// bindPipelines binds the process half's pipelines onto the lifecycle:
// PR-verify pipelines on the CI / Review states, the post-merge chain as
// new columns after Merged.
func bindPipelines(m, base *workflowmodel.Model) {
	if len(base.Spec.Pipelines) == 0 {
		return
	}

	cite := citations(base.Spec.Pipelines)

	// PR-verify pipelines run on every head in the CI / Review column.
	if ci := base.FindState(workflowmodel.StateCI); ci != nil && len(ci.Pipelines) > 0 {
		for i := range m.Spec.States {
			s := &m.Spec.States[i]
			if s.Lifecycle.Column == hivedefault.ColumnCIReview {
				s.Pipelines = append([]string(nil), ci.Pipelines...)
				s.Lifecycle.Citations = cite(ci.Pipelines)
			}
		}
	}

	// The post-merge chain: every ingested state after Merged, in order.
	var post []workflowmodel.State
	for _, s := range base.Spec.States {
		if s.Origin == workflowmodel.OriginIngested && s.ID != workflowmodel.StateCI {
			post = append(post, s)
		}
	}
	if len(post) == 0 {
		return
	}
	for _, g := range base.Spec.Guards {
		if g.Kind == workflowmodel.GuardEnvironmentApproval {
			m.Spec.Guards = append(m.Spec.Guards, g)
		}
	}
	m.Spec.Events = append(m.Spec.Events, workflowmodel.Event{
		ID: EventPipelineSucceeded, Label: "Post-merge pipeline succeeded",
		Fact:       workflowmodel.Predicate{Fact: "pipeline.status", Op: workflowmodel.OpIs, Value: "green"},
		Confidence: workflowmodel.ConfidenceInferred,
	})
	prev := hivedefault.StateMerged
	for _, s := range post {
		st := s
		st.Lifecycle = workflowmodel.StateLifecycle{Column: st.ID, Kinds: []workflowmodel.ItemKind{workflowmodel.KindPR}, Confidence: workflowmodel.ConfidenceInferred, Citations: cite(st.Pipelines)}
		col := workflowmodel.Column{ID: st.ID, Label: st.Label, States: []string{st.ID}, Terminal: st.Type == workflowmodel.StateFinal}
		if st.ID == workflowmodel.StateDone {
			col.Hint = "Every post-merge pipeline has run"
		} else {
			col.Hint = "Runs after merge: " + strings.Join(st.Pipelines, ", ")
		}
		m.Spec.States = append(m.Spec.States, st)
		m.Spec.Columns = append(m.Spec.Columns, col)
		m.Spec.Transitions = append(m.Spec.Transitions, workflowmodel.Transition{From: prev, To: st.ID, On: EventPipelineSucceeded, GuardRef: st.GuardRef, Note: "ingested post-merge chain"})
		prev = st.ID
	}
}

// ingestSource is the meta source for the systems the pipelines came from.
func ingestSource(m *workflowmodel.Model) workflowmodel.MetaSource {
	var gha, tkn bool
	for _, p := range m.Spec.Pipelines {
		if p.System == workflowmodel.SystemTekton {
			tkn = true
		} else {
			gha = true
		}
	}
	switch {
	case gha && !tkn:
		return workflowmodel.SourceGitHubActions
	case tkn && !gha:
		return workflowmodel.SourceTekton
	case gha && tkn:
		return workflowmodel.SourceGitHubActions + "+" + workflowmodel.SourceTekton
	}
	return ""
}

// citations returns a lookup from pipeline ids to the files they came from.
func citations(pipelines []workflowmodel.Pipeline) func(ids []string) []workflowmodel.Citation {
	paths := map[string]string{}
	for _, p := range pipelines {
		paths[p.ID] = p.Path
	}
	return func(ids []string) []workflowmodel.Citation {
		seen := map[string]bool{}
		var out []workflowmodel.Citation
		for _, id := range ids {
			p, ok := paths[id]
			if !ok || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, workflowmodel.Citation{Path: p, Text: id})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
		return out
	}
}
