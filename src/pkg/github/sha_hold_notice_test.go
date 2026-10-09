package github

import (
	"strings"
	"testing"
)

// TestSHAHoldCommentPointsAtImageFriendlySources: most spokes run from an
// image with no checkout, so the notice must name the dashboard version string
// and the version_read admin-MCP tool, not only git rev-parse (#11220).
func TestSHAHoldCommentPointsAtImageFriendlySources(t *testing.T) {
	for _, want := range []string{"git rev-parse --short HEAD", "dashboard", "`version_read` admin-MCP tool", shaHoldLegacyNoticeSentence} {
		if !strings.Contains(shaHoldComment, want) {
			t.Fatalf("shaHoldComment missing %q:\n%s", want, shaHoldComment)
		}
	}
}
