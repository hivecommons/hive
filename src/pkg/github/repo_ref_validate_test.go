package github

import (
	"strings"
	"testing"
)

// The exact value observed live, from an agent copying
// pkg/policies/defaults/sec-check-holdgated.md's
// `gh issue create --repo "<org>/<target-repo>"` verbatim.
func TestValidateRepoRefRejectsUnsubstitutedPlaceholder(t *testing.T) {
	err := validateRepoRef("<org>", "<target-repo>")
	if err == nil {
		t.Fatal("placeholder repo accepted; hive would 404-loop against it for 24h")
	}
	if !strings.Contains(err.Error(), "placeholder") {
		t.Errorf("error should name the placeholder case so the operator knows the fix is in agent output, got: %v", err)
	}
}

func TestValidateRepoRefAcceptsRealRepos(t *testing.T) {
	for _, tc := range [][2]string{
		{"hanthor", "indiafoss-companion"},
		{"tuna-os", "hive"},
		{"kubestellar", "hive"},
		{"hanthor", "reilly.asia"},  // dots are legal
		{"tuna-os", "suite_common"}, // underscores are legal
		{"hanthor", ".github"},      // leading dot is a real repo name
	} {
		if err := validateRepoRef(tc[0], tc[1]); err != nil {
			t.Errorf("rejected real repo %s/%s: %v", tc[0], tc[1], err)
		}
	}
}

func TestValidateRepoRefRejectsEmptyAndPathTricks(t *testing.T) {
	for _, tc := range [][2]string{
		{"", "repo"},
		{"owner", ""},
		{"owner", "re/po"}, // a second slash would change the URL path
		{"..", "repo"},     // traversal
		{"owner", "re po"}, // whitespace
	} {
		if err := validateRepoRef(tc[0], tc[1]); err == nil {
			t.Errorf("accepted invalid repo ref %q/%q", tc[0], tc[1])
		}
	}
}

// CreateIssue must refuse before it makes any API call — the whole point is
// that a doomed request costs zero rate limit.
func TestCreateIssueRefusesPlaceholderRepoWithoutCallingGitHub(t *testing.T) {
	c := &Client{}
	if _, err := c.CreateIssue(t.Context(), "<org>/<target-repo>", "t", "b", nil); err == nil {
		t.Fatal("CreateIssue accepted a placeholder repo")
	}
}
