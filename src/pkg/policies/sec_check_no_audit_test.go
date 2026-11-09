package policies

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// secCheckNoAuditLine is the stand-down line a sec-check turn that audited no
// repository must end with. pkg/agent's kick-outcome classifier records a
// turn whose last screen starts a line with "STAND DOWN" as blocked, so this
// exact form is what keeps an un-run audit from counting as a finished one.
const secCheckNoAuditLine = "`STAND DOWN — no repository audited: <what stopped you>`"

// TestSecCheckPoliciesReportNoAuditAsStandDown pins the knuckle failure of
// 2026-11-08: sec-check audited nothing (no checkout, GitHub reads refused),
// summarised "security posture remains unassessed", and the turn was recorded
// as an ordinary ended kick. Both copies of both sec-check templates must tell
// the agent to end such a turn on the stand-down line instead.
//
// Fails on v6 tip, where neither template mentions the no-audit case.
func TestSecCheckPoliciesReportNoAuditAsStandDown(t *testing.T) {
	for _, name := range []string{"sec-check-full.md", "sec-check-holdgated.md"} {
		t.Run(name, func(t *testing.T) {
			embedded, err := DefaultPolicies.ReadFile("defaults/" + name)
			if err != nil {
				t.Fatalf("read embedded %s: %v", name, err)
			}
			source, err := os.ReadFile(filepath.Join(policySourceDir, name))
			if err != nil {
				t.Fatalf("read source %s: %v", name, err)
			}
			for tree, body := range map[string]string{"embedded": string(embedded), "source": string(source)} {
				if !strings.Contains(body, "If you audited no repository this turn") {
					t.Errorf("%s %s does not cover a turn that audited no repository", tree, name)
				}
				if !strings.Contains(body, secCheckNoAuditLine) {
					t.Errorf("%s %s does not give the stand-down line %s", tree, name, secCheckNoAuditLine)
				}
			}
		})
	}
}
