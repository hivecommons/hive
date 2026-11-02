// Package trackerview is the Activity Monitor's pure URL-view and
// display-playlist JavaScript (#15860 Part A: `am.*` state, `am.view` +
// `interval` playlists), shared so the hub Fleet activity page reads a display
// URL exactly as a spoke display does (hivecommons/hive#16064, part of #15965).
// It depends on nothing in pkg/dashboard: the hub embeds it directly.
//
// The spoke does not read this package at runtime. Every declaration in
// tracker-view.js is a verbatim copy of the spoke's own source (static-src
// tracker-words-stage.js, tracker-filter.js and tracker-filter-state.js, as
// concatenated into static/index.html). TestTrackerViewSharedAssetCompat in
// pkg/dashboard fails when a copy drifts, so a change starts on the spoke and
// is copied here; the hub never carries its own variant of these helpers.
//
// The file is plain declarations with no side effects. A host page supplies
// the STAGE_TRACKER_* vocabulary constants they read (listed in the file's
// header comment).
package trackerview

import "embed"

// FS holds the asset files named in Files.
//
//go:embed *.js
var FS embed.FS

// Files lists the asset files in the order a page loads them.
var Files = []string{
	// stageTrackerDefaultFilter / stageTrackerNormalizeFilter, the am.* URL
	// view (stageTrackerFilterFromUrl / stageTrackerViewUrl) and the display
	// playlist helpers (stageTrackerParsePlaylist and friends).
	"tracker-view.js",
}
