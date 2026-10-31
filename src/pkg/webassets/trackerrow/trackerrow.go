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

// RequiredGlobals lists the STAGE_TRACKER_* constants the assets read as free
// globals. A host must define each with the spoke's value: TestRequiredGlobals
// keeps this list equal to what the assets reference, and
// TestTrackerRowRequiredGlobalsDefinedBySpoke in pkg/dashboard fails when the
// spoke stops defining one, so the hub can assert against it when it embeds
// the assets.
var RequiredGlobals = []string{
	"STAGE_TRACKER_ISSUE_DECISION_COLUMN",
	"STAGE_TRACKER_ISSUE_NEEDS_DECISION",
	"STAGE_TRACKER_MERGE_COLUMN",
	"STAGE_TRACKER_NEEDS_DECISION",
	"STAGE_TRACKER_PATH",
	"STAGE_TRACKER_POSSIBLY_EXISTS",
	"STAGE_TRACKER_PRE_MERGE_SOAK",
	"STAGE_TRACKER_PR_COLUMN",
	"STAGE_TRACKER_TERMINAL",
	"STAGE_TRACKER_TRIAGE_NEEDED",
}
