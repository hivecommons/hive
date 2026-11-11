// Package tekton ingests Tekton Pipeline and PipelineRun definitions into
// workflowmodel pipelines.
//
// Files may hold several YAML documents or a Kubernetes List; every
// document whose apiVersion is in the tekton.dev family is considered.
// Pipeline resources and PipelineRuns with an embedded pipelineSpec become
// one Pipeline each; PipelineRuns that reference a Pipeline by name
// contribute their Pipelines-as-Code triggers to it; Tekton Triggers
// documents contribute a webhook trigger. Everything the ingest recognises
// but cannot model is recorded as a Finding rather than an error.
package tekton
