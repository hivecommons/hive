package wire

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docAnchors lists the heading anchors of a Markdown file as GitHub
// renders them: lower case, punctuation dropped, spaces to dashes.
func docAnchors(t *testing.T, path string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	drop := regexp.MustCompile(`[^a-z0-9 _-]`)
	out := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "#") {
			continue
		}
		h := strings.ToLower(strings.TrimSpace(strings.TrimLeft(line, "#")))
		out[strings.ReplaceAll(drop.ReplaceAllString(h, ""), " ", "-")] = true
	}
	return out
}

// TestHelpAnchorsAreInTheDocs (hivecommons/hive#18744): the link an SMTP
// refusal and the Inbox give for a reason lands on a heading of the docs
// section, for every reason that has a fix.
func TestHelpAnchorsAreInTheDocs(t *testing.T) {
	anchors := docAnchors(t, filepath.Join("..", "..", "..", "docs", "inbox-voice-sources.md"))
	if !anchors[helpPageAnchor] {
		t.Fatalf("the docs have no section %q", helpPageAnchor)
	}
	reasons := HelpReasons()
	if len(reasons) == 0 {
		t.Fatal("no help reasons")
	}
	for _, r := range reasons {
		if !anchors[HelpAnchor(r)] {
			t.Errorf("reason %s: the docs have no heading with the anchor %q", r, HelpAnchor(r))
		}
		if !strings.HasPrefix(HelpURL(r), HelpPageURL+"#") || Fix(r) == "" {
			t.Errorf("reason %s: url %q fix %q", r, HelpURL(r), Fix(r))
		}
	}
	// Every refusal category a hive or the receiver records has help.
	for _, r := range []string{ReasonEmpty, ReasonUnsupported, ReasonContentMismatch, ReasonAttachmentTooLarge, ReasonTooManyAttachments,
		ReasonMessageTooLarge, ReasonMalformed, ReasonHiveVolume, ReasonHiveRefused, ReasonMailInOff, ReasonHiveStorageFull,
		ReasonUnknownMailbox, ReasonNotAuthenticated, ReasonNotOnAllowlist} {
		if Fix(r) == "" {
			t.Errorf("reason %s has no fix", r)
		}
	}
}

// TestHelpLines: a refusal's extra lines are the fix, the help page and the
// report page; an unknown reason still links the section and the report
// page. No line is long enough to be folded by a mail server.
func TestHelpLines(t *testing.T) {
	const maxReplyLine = 400
	for _, r := range append(HelpReasons(), "made_up") {
		lines := HelpLines(r)
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "What this means: "+HelpURL(r)) || !strings.Contains(joined, "Report a problem: "+ReportURL) {
			t.Errorf("%s: %q", r, lines)
		}
		if (Fix(r) != "") != strings.Contains(joined, "To try again: ") {
			t.Errorf("%s: fix line %q", r, lines)
		}
		for _, l := range lines {
			if len(l) > maxReplyLine || strings.ContainsAny(l, "\r\n") {
				t.Errorf("%s: line %q", r, l)
			}
		}
	}
	if got := HelpURL("made_up"); got != HelpPageURL+"#"+helpPageAnchor {
		t.Errorf("unknown reason links %q", got)
	}
}
