package stageengine

// PostMergeFacts is a merged PR's observed post-merge pipeline status
// (hivecommons/hive#18645): the pipeline.status fact. A post-merge run
// observer (GitHub Actions: workflowmodel/githubactions.ObservePostMerge)
// resolves it under the range attribution rules of #18356.
type PostMergeFacts struct {
	// Status is green | red | running | unknown.
	Status string
	// Column is the post-merge column that decided Status ("" when every
	// column succeeded).
	Column string
	// Reason says why Status is not green; it is recorded, never guessed
	// around.
	Reason string
}

// pipelineStatus is the pipeline.status fact: unknown until an observer
// has reported on the PR.
func pipelineStatus(f Facts) string {
	if f.PR == nil || f.PR.PostMerge == nil {
		return "unknown"
	}
	switch s := f.PR.PostMerge.Status; s {
	case "green", "red", "running":
		return s
	}
	return "unknown"
}
