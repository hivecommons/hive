package dashboard

import (
	"errors"
	"net/http"
	"strings"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// handleRepoIssueRelease lets an authenticated dashboard operator execute the
// same live-validated typed release action as `/hive approve`. Tag and target
// are resolved from the current trusted issue owner-step, never from the body.
func (s *Server) handleRepoIssueRelease(w http.ResponseWriter, r *http.Request) {
	owner, repoName, number, ok := s.parseRepoItemPath(w, r)
	if !ok {
		return
	}
	allowed, status, msg := s.canToggleRepoHold(r, owner, repoName)
	if !allowed {
		jsonError(w, msg, status)
		return
	}
	if s.deps == nil || s.deps.GHClient == nil || s.deps.Config == nil {
		jsonError(w, "release action is not configured", http.StatusServiceUnavailable)
		return
	}
	repo := owner + "/" + repoName
	// Same order as the comment path: a label-driven repo refuses `/hive
	// approve` before the release executor runs, so the button refuses too.
	if s.deps.Config.RepoLabelDriven(repo) {
		jsonError(w, "this repository is label-driven: Hive refuses /hive approve here, so it does not execute releases from the dashboard either", http.StatusConflict)
		return
	}
	if !s.deps.Config.RepoReleaseOnApprove(repo) {
		jsonError(w, "release_on_approve is not enabled for this repository", http.StatusConflict)
		return
	}

	event, err := s.deps.GHClient.ApproveIssueRelease(r.Context(), repo, number, approvalOperator(r), s.deps.Config.RepoReleaseVersionSource(repo))
	if err != nil {
		var refusal *ghpkg.IssueReleaseRefusalError
		if errors.As(err, &refusal) {
			jsonError(w, refusal.Reason, http.StatusConflict)
			return
		}
		// The release write may have succeeded before posting the issue URL
		// failed. Report the committed result instead of inviting a retry.
		if event.ReleaseURL == "" {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
	}

	minStatusSeq := s.refreshAfterMutationSeq()
	response := map[string]any{
		"ok":           true,
		"repo":         repo,
		"number":       number,
		"tag":          event.ReleaseTag,
		"url":          event.ReleaseURL,
		"minStatusSeq": minStatusSeq,
	}
	if err != nil {
		response["warning"] = strings.TrimSpace(err.Error())
	}
	jsonResponse(w, response)
}
