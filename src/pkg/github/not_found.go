package github

import "net/http"

// IsNotFound reports whether err is (or wraps) a GitHub API 404. Callers
// outside this package use it to tell "the file or directory does not exist"
// apart from a transient failure, without importing go-github themselves.
func IsNotFound(err error) bool {
	return githubStatusError(err, http.StatusNotFound)
}
