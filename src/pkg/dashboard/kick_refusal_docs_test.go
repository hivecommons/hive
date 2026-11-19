package dashboard

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docsHeadingSlug mirrors GitHub's heading anchors for the plain headings the
// kick refusals page uses.
func docsHeadingSlug(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = regexp.MustCompile(`[^a-z0-9_\- ]`).ReplaceAllString(h, "")
	return strings.ReplaceAll(h, " ", "-")
}

func TestKickRefusalReasonsHaveDocsAnchors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "kick-refusals.md"))
	if err != nil {
		t.Fatalf("read docs page: %v", err)
	}
	slugs := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "#") {
			slugs[docsHeadingSlug(strings.TrimLeft(line, "# "))] = true
		}
	}
	for _, reason := range kickRefusalReasons {
		anchor, ok := kickRefusalAnchors[reason]
		if !ok || anchor == "" {
			t.Errorf("kick refusal reason %q has no docs anchor", reason)
			continue
		}
		if !slugs[anchor] {
			t.Errorf("reason %q anchor #%s is not a heading in src/docs/kick-refusals.md", reason, anchor)
		}
		if !strings.Contains(string(raw), "(#"+anchor+")") {
			t.Errorf("reason %q is missing from the docs reasons table", reason)
		}
		if got := kickRefusalDocsURL(reason); !strings.HasSuffix(got, "#"+anchor) {
			t.Errorf("kickRefusalDocsURL(%q) = %q", reason, got)
		}
	}
	for reason := range kickRefusalAnchors {
		found := false
		for _, r := range kickRefusalReasons {
			found = found || r == reason
		}
		if !found {
			t.Errorf("anchor for %q is not in kickRefusalReasons", reason)
		}
	}
}

func TestClassifyKickError(t *testing.T) {
	cases := map[string]string{
		"agent scanner cannot be kicked: it is paused (by budget)":                  kickRefusalPaused,
		"agent a blocked: inference (quota): 429; next provider probe in 2m0s":      kickRefusalProviderBackoff,
		"kick held after restart: agent a was restarted (x) while producing a turn": kickRefusalRestartHold,
		"kick cancelled by restart: agent a restarted while the kick was waiting":   kickRefusalRestartCancelled,
		"agent turn in progress: agent a kick delivery already in progress":         kickRefusalDeliveryBusy,
		"tmux session hive-a not found":                                             kickRefusalNoSession,
		"agent a cannot be kicked: it failed to start: boom":                        kickRefusalFailedStart,
		"agent a cannot be kicked: it is stopped":                                   kickRefusalNotRunning,
		"agent a sandbox image is not configured":                                   kickRefusalSandboxBlocked,
		"agent zzz not found": kickRefusalUnknownAgent,
	}
	for msg, want := range cases {
		if got := classifyKickError(msg); got != want {
			t.Errorf("classifyKickError(%q) = %q, want %q", msg, got, want)
		}
	}
	if got := classifyKickError("something unrelated"); got != "" {
		t.Errorf("unrelated message classified as %q", got)
	}
	if classifyKickRefusalFlag("deferred until startup completes") != kickRefusalStartupDeferred {
		t.Error("startup deferral not classified")
	}
	if classifyKickRefusalFlag("I'm declining to execute this") != kickRefusalAgentDeclined {
		t.Error("model refusal not classified")
	}
}
