// Package jsonextract is the Hive-side name for the JSON-object extraction
// helper. The helper lives in the Crux ring as pkg/crux/jsonextract because
// the runtime's trajectory classifier parses model output with it; this
// package re-exports it so pkg/intent, pkg/ioscan and pkg/retro keep their
// import path: Crux owns, Hive aliases (hivecommons/hive#13536).
package jsonextract

import cruxjsonextract "github.com/hivecommons/hive/crux/jsonextract"

// Object is pkg/crux/jsonextract.Object.
var Object = cruxjsonextract.Object
