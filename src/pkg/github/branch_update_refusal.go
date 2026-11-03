package github

import (
	"errors"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// BranchUpdateFailure is the class of a refused update-branch call. The
// automerge sweep and the hold guard's keep-mergeable update both read it.
type BranchUpdateFailure int

const (
	BranchUpdateOther BranchUpdateFailure = iota
	BranchUpdateConflict
	BranchUpdateUpToDate
	BranchUpdateForkNotEditable
	BranchUpdateWorkflowPermission
	BranchUpdateForkWorkflowUpdate
)

// ClassifyBranchUpdateError separates the update-branch failures GitHub
// reports with overlapping statuses: 422 covers conflicts and an already
// current branch, while a 422 naming maintainer edits (or a 403 on a fork PR)
// means the app cannot write to the fork head. A refusal to update a workflow
// file means the base changed .github/workflows/: on a same-repo branch the
// token lacked the workflows permission, while a fork head is a repository the
// App is not installed on, so no grant on the base repository lets it push
// there (projectbluefin/utah#1001). Either way no retry with the same token can
// succeed, so a person must update the branch. Any other 403 on a same-repo
// branch is a plain permission or rate-limit failure. An unrecognised 422
// stays a conflict.
func ClassifyBranchUpdateError(err error, fork bool) BranchUpdateFailure {
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil {
		return BranchUpdateOther
	}
	status := ghErr.Response.StatusCode
	if (status == http.StatusForbidden || status == http.StatusUnprocessableEntity) && WorkflowPermissionRefusalPattern.MatchString(ghErr.Message) {
		if fork {
			return BranchUpdateForkWorkflowUpdate
		}
		return BranchUpdateWorkflowPermission
	}
	msg := strings.ToLower(ghErr.Message)
	switch status {
	case http.StatusForbidden:
		if fork {
			return BranchUpdateForkNotEditable
		}
		// TODO(hivecommons/hive#14589): a same-repo 403 "Resource not
		// accessible by integration" means the App lacks contents: write
		// and should mark the lane unauthorized (step: update-branch);
		// a rate-limit 403 must not. Wire through markUnauthorized once
		// the two are told apart here (isResourceNotAccessible).
	case http.StatusUnprocessableEntity:
		switch {
		case strings.Contains(msg, "maintainer") || strings.Contains(msg, "permission") || strings.Contains(msg, "not authorized"):
			return BranchUpdateForkNotEditable
		case strings.Contains(msg, "merge conflict"):
			return BranchUpdateConflict
		case strings.Contains(msg, "up to date") || strings.Contains(msg, "up-to-date") || strings.Contains(msg, "no new commits"):
			return BranchUpdateUpToDate
		}
		return BranchUpdateConflict
	}
	return BranchUpdateOther
}
