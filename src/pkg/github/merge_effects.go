package github

import (
	"errors"
	"net/http"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
)

// MergeAPIErrorNotApplied wraps a definite GitHub refusal of a pull request
// merge (403, 405, 409, 422) in effects.NotApplied so the mutation journal
// records NotApplied instead of an uncertain outcome. Other errors (5xx,
// transport failures) are returned unchanged and stay uncertain.
func MergeAPIErrorNotApplied(err error) error {
	if err == nil {
		return nil
	}
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil {
		return err
	}
	switch ghErr.Response.StatusCode {
	case http.StatusMethodNotAllowed, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusForbidden:
		return effects.NotApplied(err)
	}
	return err
}
