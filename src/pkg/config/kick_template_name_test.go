package config

import (
	"strings"
	"testing"
)

// hivecommons/hive#7390: kick_template is a bare file name the scheduler joins
// under the policy directories. A path would read outside them; it is refused
// at load and at overlay-reject time. Whether the name RESOLVES is the
// scheduler's softer question (warned, shown in the editor), not a load error
// — a template can legitimately be created later by the prompt editor.
func TestValidateKickTemplateName(t *testing.T) {
	for _, ok := range []string{"", "  ", "scanner-holdgated.md", "review.md", "scanner-CLAUDE.md", "my template.md"} {
		if err := ValidateKickTemplateName(ok); err != nil {
			t.Errorf("%q must be accepted, got %v", ok, err)
		}
	}
	for _, bad := range []string{"../../etc/passwd", "sub/dir.md", `dir\file.md`, "..", "a/../b.md"} {
		err := ValidateKickTemplateName(bad)
		if err == nil || !strings.Contains(err.Error(), "must be a bare file name") {
			t.Errorf("%q must be refused with the bare-file-name message, got %v", bad, err)
		}
	}
}

func TestValidateAgentOverlay_RejectsKickTemplatePath(t *testing.T) {
	c := &Config{Governor: GovernorConfig{}}
	if err := c.validateAgentOverlay("scanner", AgentConfig{Backend: "claude", KickTemplate: "../hive.yaml"}); err == nil || !strings.Contains(err.Error(), "kick_template") {
		t.Errorf("overlay with a path kick_template must be rejected, got %v", err)
	}
	if err := c.validateAgentOverlay("scanner", AgentConfig{Backend: "claude", KickTemplate: "review.md"}); err != nil {
		t.Errorf("a bare (even dangling) name is not an overlay error: %v", err)
	}
}
