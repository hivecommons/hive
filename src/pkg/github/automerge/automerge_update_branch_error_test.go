package automerge

import (
	"errors"
	"net/http"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func TestClassifyUpdateBranchError(t *testing.T) {
	resp := func(status int, msg string) error {
		return &gh.ErrorResponse{Response: &http.Response{StatusCode: status}, Message: msg}
	}
	tests := []struct {
		name string
		err  error
		fork bool
		want updateBranchFailure
	}{
		{"merge conflict", resp(http.StatusUnprocessableEntity, "merge conflict between base and head"), false, updateBranchConflict},
		{"unrecognised 422 stays conflict", resp(http.StatusUnprocessableEntity, "Validation failed"), false, updateBranchConflict},
		{"already up to date", resp(http.StatusUnprocessableEntity, "There are no new commits on the base branch."), false, updateBranchUpToDate},
		{"already up to date legacy text", resp(http.StatusUnprocessableEntity, "branch is already up to date"), false, updateBranchUpToDate},
		{"403 on fork PR", resp(http.StatusForbidden, "Resource not accessible by integration"), true, updateBranchForkNotEditable},
		{"403 on same-repo PR", resp(http.StatusForbidden, "Resource not accessible by integration"), false, updateBranchOther},
		{"422 maintainer edits", resp(http.StatusUnprocessableEntity, "Maintainer edits are not allowed on this pull request"), false, updateBranchForkNotEditable},
		{"422 permission", resp(http.StatusUnprocessableEntity, "Insufficient permission to update head branch"), false, updateBranchForkNotEditable},
		{"server error", resp(http.StatusInternalServerError, "boom"), false, updateBranchOther},
		{"non-API error", errors.New("dial tcp"), false, updateBranchOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyUpdateBranchError(tt.err, tt.fork); got != tt.want {
				t.Fatalf("classifyUpdateBranchError() = %v, want %v", got, tt.want)
			}
		})
	}
}
