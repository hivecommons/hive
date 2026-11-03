package dashboard

import (
	"strings"
	"testing"
)

// jsRollUpgradeFuncs returns the roll-upgrade helpers (#15788) the shared
// version renderers now call, plus their pacing constants, for node harnesses
// that assemble the renderer function by function.
func jsRollUpgradeFuncs(t *testing.T, html string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("const VERSION_ROLL_POLL_MS = 2000;\nconst VERSION_ROLL_TOAST_MS = 15000;\n")
	for _, name := range []string{
		"versionRollPhaseActive", "versionRollRecordFromEvent", "versionSameRollTarget", "versionAdoptServerRollProgress",
		"versionRollComplete", "versionRollOutcome", "versionRollReconcile", "versionRollNotify", "versionRollWho",
		"versionRollTitle", "versionRollStartupLabel", "versionRollStatusText", "versionRollStatusHTML",
		"versionRollNavbarStatus", "versionRollReloadDeferred", "versionUpgradeTitle",
	} {
		b.WriteString(jsFunc(t, html, name))
		b.WriteByte('\n')
	}
	return b.String()
}
