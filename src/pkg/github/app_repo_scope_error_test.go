package github

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func TestIsRepoScopeMintError(t *testing.T) {
	ghErr := func(code int) error {
		return fmt.Errorf("creating scoped token for tier contributor: %w",
			&gh.ErrorResponse{Response: &http.Response{StatusCode: code}, Message: "x"})
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"422 renamed/removed repo", ghErr(http.StatusUnprocessableEntity), true},
		{"404", ghErr(http.StatusNotFound), true},
		{"500", ghErr(http.StatusInternalServerError), false},
		{"401 bad app key", ghErr(http.StatusUnauthorized), false},
		{"non-github error", errors.New("dial tcp: timeout"), false},
		{"nil", nil, false},
		{"ErrorResponse without response", &gh.ErrorResponse{}, false},
	}
	for _, tc := range cases {
		if got := IsRepoScopeMintError(tc.err); got != tc.want {
			t.Errorf("%s: IsRepoScopeMintError = %v, want %v", tc.name, got, tc.want)
		}
	}
}
