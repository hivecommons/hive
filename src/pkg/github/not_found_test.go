package github

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func TestIsNotFound(t *testing.T) {
	notFound := &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}}
	if !IsNotFound(notFound) {
		t.Error("IsNotFound(404) = false, want true")
	}
	if !IsNotFound(fmt.Errorf("getting file o/r/p: %w", notFound)) {
		t.Error("IsNotFound(wrapped 404) = false, want true")
	}
	forbidden := &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusForbidden}}
	if IsNotFound(forbidden) {
		t.Error("IsNotFound(403) = true, want false")
	}
	if IsNotFound(errors.New("404 Not Found")) {
		t.Error("IsNotFound(plain error text) = true, want false: only a typed API 404 counts")
	}
	if IsNotFound(nil) {
		t.Error("IsNotFound(nil) = true, want false")
	}
}
