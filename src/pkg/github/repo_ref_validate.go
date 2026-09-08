package github

import (
	"fmt"
	"strings"
)

// Agent policy prompts show repository arguments as templates, e.g.
// pkg/policies/defaults/sec-check-holdgated.md:
//
//	gh issue create --repo "<org>/<target-repo>" \
//
// An agent that copies the line verbatim instead of substituting produces a
// request naming the literal repo `<org>/<target-repo>`. Observed live: hive
// dutifully called GitHub with it and got
//
//	POST https://api.github.com/repos/%3Corg%3E/%3Ctarget-repo%3E/labels: 404
//
// once per label plus a dedupe lookup and a create, then retried the whole
// request on the watcher's backoff for up to the 24h give-up horizon. The
// request can never succeed — the repo does not and cannot exist — so every
// one of those calls is waste charged against the App installation's rate
// limit, which request_retry.go's own comment records as having taken a whole
// org dark for an hour at a time.
//
// GitHub owner and repository names are limited to ASCII letters, digits,
// hyphen, underscore and period, so an unsubstituted placeholder is cheap to
// reject with certainty rather than heuristics.
func validRepoNameComponent(s string) bool {
	if s == "" {
		return false
	}
	// "." and ".." are made entirely of legal characters but are path
	// segments, not names: they would traverse the URL rather than address a
	// repository. Rejected explicitly because the charset check below cannot
	// catch them.
	if s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// validateRepoRef rejects a repo reference that GitHub could never accept.
// It takes the ALREADY-SPLIT owner and name so it validates exactly what is
// about to be put in the URL path, rather than re-parsing and risking a
// different split than the caller used.
//
// The returned error is deliberately explicit about the placeholder case,
// because that is the failure operators will actually see and the fix is in an
// agent's output rather than in hive's configuration.
func validateRepoRef(owner, name string) error {
	if !validRepoNameComponent(owner) || !validRepoNameComponent(name) {
		ref := owner + "/" + name
		if strings.ContainsAny(ref, "<>") {
			return fmt.Errorf("refusing to call GitHub with repo %q: this looks like an unsubstituted prompt placeholder, not a repository — the agent copied a template literally instead of naming a repo from $HIVE_REPOS", ref)
		}
		return fmt.Errorf("refusing to call GitHub with repo %q: not a valid owner/name (allowed: letters, digits, '-', '_', '.')", ref)
	}
	return nil
}
