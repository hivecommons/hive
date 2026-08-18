package dashboard

import (
	"strings"
	"testing"
)

// A gist has no stable raw URL across revisions, so a KEEP-LINKED import would
// silently pin to a snapshot that never updates again — the user believes the
// agent tracks the gist, and it does not. One-shot gist imports are fine and
// must keep working, so the guard has to reject exactly one combination.

// TestValidateImportURL_GistRejectedWhenKeepLinked is the regression: a gist URL
// with keepLinked=true must be refused, with a message that points at the fix.
func TestValidateImportURL_GistRejectedWhenKeepLinked(t *testing.T) {
	s := &Server{}
	err := s.validateImportURL("https://gist.github.com/someone/abc123", true)
	if err == nil {
		t.Fatal("a keep-linked gist import must be rejected — it would pin to a snapshot forever")
	}
	if !strings.Contains(err.Error(), "gist") {
		t.Errorf("error should name gists so the user knows what to change, got: %v", err)
	}
}

// TestValidateImportURL_GistAllowedOneShot is the positive control. Without it,
// a guard that rejected every gist would satisfy the test above while breaking
// a supported workflow.
func TestValidateImportURL_GistAllowedOneShot(t *testing.T) {
	s := &Server{}
	if err := s.validateImportURL("https://gist.github.com/someone/abc123", false); err != nil {
		t.Errorf("a one-shot gist import is supported and must be allowed, got: %v", err)
	}
}

// TestValidateImportURL_GistPathRequired: a bare gist host carries no document.
func TestValidateImportURL_GistPathRequired(t *testing.T) {
	s := &Server{}
	if err := s.validateImportURL("https://gist.github.com/", false); err == nil {
		t.Error("a gist URL with no path identifies no document and must be rejected")
	}
}

// TestValidateImportURL_NonGistKeepLinkedStillWorks proves the gist branch did
// not accidentally capture ordinary repo file URLs, which are the main
// keep-linked case.
func TestValidateImportURL_NonGistKeepLinkedStillWorks(t *testing.T) {
	s := &Server{}
	if err := s.validateImportURL("https://raw.githubusercontent.com/o/r/main/a.md", true); err != nil {
		t.Errorf("a raw GitHub file URL is the canonical keep-linked import, got: %v", err)
	}
}
