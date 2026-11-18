package workflowmodel

import (
	"errors"
	"strings"
)

// Tekton PipelineRun observation config (hivecommons/hive#18356, #18646).
//
// A Tekton pipeline in the model may name the cluster, namespace and label
// selector whose PipelineRuns are its post-merge runs. Everything is
// explicit: hive never discovers clusters, namespaces or PipelineRuns on
// its own, so a pipeline without this block is simply not observed.

// Default revision sources, in the order they are tried.
const (
	// DefaultTektonRevisionAnnotation is the Pipelines-as-Code annotation
	// carrying the commit a PipelineRun was started for.
	DefaultTektonRevisionAnnotation = "pipelinesascode.tekton.dev/sha"
	// DefaultTektonRevisionParam is the conventional PipelineRun param
	// carrying the revision when the annotation is absent.
	DefaultTektonRevisionParam = "revision"
)

// TektonObserve says where a Tekton pipeline's PipelineRuns live and how
// to read the revision each one built or deployed.
type TektonObserve struct {
	// Context is the kubeconfig context to read from. Exactly one of
	// Context and InCluster is set.
	Context string `json:"context,omitempty"`
	// InCluster reads with the hive pod's own ServiceAccount.
	InCluster bool `json:"inCluster,omitempty"`
	// Namespace holds the PipelineRuns; required.
	Namespace string `json:"namespace"`
	// LabelSelector picks this pipeline's PipelineRuns (for example
	// "tekton.dev/pipeline=release"); required, so an unrelated run is
	// never attributed.
	LabelSelector string `json:"labelSelector"`
	// Column is the pipeline/environment column the runs are attributed
	// to; empty means the pipeline id.
	Column string `json:"column,omitempty"`
	// RevisionAnnotation names the annotation tried first; empty means
	// DefaultTektonRevisionAnnotation.
	RevisionAnnotation string `json:"revisionAnnotation,omitempty"`
	// RevisionParam names the spec.params entry tried next; empty means
	// DefaultTektonRevisionParam.
	RevisionParam string `json:"revisionParam,omitempty"`
}

// Annotation is the revision annotation in effect.
func (o TektonObserve) Annotation() string {
	if o.RevisionAnnotation != "" {
		return o.RevisionAnnotation
	}
	return DefaultTektonRevisionAnnotation
}

// Param is the revision param in effect.
func (o TektonObserve) Param() string {
	if o.RevisionParam != "" {
		return o.RevisionParam
	}
	return DefaultTektonRevisionParam
}

// ColumnFor is the column the runs land in for pipeline id.
func (o TektonObserve) ColumnFor(pipelineID string) string {
	if o.Column != "" {
		return o.Column
	}
	return pipelineID
}

// Validate checks the config is explicit enough to observe without
// guessing.
func (o TektonObserve) Validate() error {
	var errs []string
	switch {
	case o.InCluster && strings.TrimSpace(o.Context) != "":
		errs = append(errs, "set either context or inCluster, not both")
	case !o.InCluster && strings.TrimSpace(o.Context) == "":
		errs = append(errs, "one of context or inCluster is required")
	}
	if strings.TrimSpace(o.Namespace) == "" {
		errs = append(errs, "namespace is required")
	}
	if strings.TrimSpace(o.LabelSelector) == "" {
		errs = append(errs, "labelSelector is required")
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.New("tekton observe: " + strings.Join(errs, "; "))
}
