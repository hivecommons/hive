// Package trackerrow is the tracker's pure row-presentation JavaScript, shared
// so the spoke dashboard and the hub can both embed the same bytes
// (hivecommons/hive#16062, part of #15965). It depends on nothing in
// pkg/dashboard: the hub embeds it directly.
//
// The spoke does not read this package at runtime. Its copies of these files
// live at pkg/dashboard/static-src/js/<name>, where manifest.txt concatenates
// them into static/index.html (the generator cannot reach outside
// static-src/). TestTrackerRowSharedAssetCompat in pkg/dashboard fails when
// the two copies drift, so edit both together.
//
// The files are plain function declarations with no side effects. A host page
// supplies what they call: the STAGE_TRACKER_* stage constants
// (tracker-words-core.js and agent-now.js on the spoke) and esc(). The anchor
// calls stageTrackerUnderRepair and stageTrackerMergeRefusalFromRaw only when
// they are defined.
package trackerrow

import "embed"

// FS holds the asset files named in Files.
//
//go:embed *.js
var FS embed.FS

// Files lists the asset files in the order a page loads them.
var Files = []string{
	// stageTrackerAnchor: which strip column a stage lights.
	"tracker-row-anchor.js",
	// stageTrackerSplitTitleTag / stageTrackerTitleHtml: the escaped, text-only title.
	"tracker-row-title.js",
}

// Global is one STAGE_TRACKER_* constant the assets read as a free global,
// with the spoke's JavaScript literal for it.
type Global struct {
	Name string
	// JS is the spoke's literal for the constant, verbatim as it appears in
	// pkg/dashboard/static-src/js (agent-now.js, tracker-words-core.js).
	JS string
}

// Decl returns the `const NAME = JS;` declaration a host page must carry.
func (g Global) Decl() string {
	return "const " + g.Name + " = " + g.JS + ";"
}

// RequiredGlobals lists the STAGE_TRACKER_* constants the assets read as free
// globals, each pinned to the spoke's value (hivecommons/hive#16068). A host
// must define every one with exactly that value: TestRequiredGlobals keeps
// the names equal to what the assets reference, and
// TestTrackerRowRequiredGlobalsDefinedBySpoke in pkg/dashboard fails when the
// spoke's declaration differs from Decl(), so the hub can assert its own
// definitions against this list when it embeds the assets.
var RequiredGlobals = []Global{
	{Name: "STAGE_TRACKER_ISSUE_DECISION_COLUMN", JS: "10"},
	{Name: "STAGE_TRACKER_ISSUE_NEEDS_DECISION", JS: "45"},
	{Name: "STAGE_TRACKER_MERGE_COLUMN", JS: "80"},
	{Name: "STAGE_TRACKER_NEEDS_DECISION", JS: "85"},
	{Name: "STAGE_TRACKER_PATH", JS: "[0, 10, 20, 30, 50, 80]"},
	{Name: "STAGE_TRACKER_POSSIBLY_EXISTS", JS: "17"},
	{Name: "STAGE_TRACKER_PRE_MERGE_SOAK", JS: "95"},
	{Name: "STAGE_TRACKER_PR_COLUMN", JS: "30"},
	{Name: "STAGE_TRACKER_TERMINAL", JS: "110"},
	{Name: "STAGE_TRACKER_TRIAGE_NEEDED", JS: "15"},
}
