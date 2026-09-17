package main

import (
	"github.com/hivecommons/hive/pkg/loginscan"
)

// loginSightings is the login detector's process-scoped state. The governor
// cycle is a function rather than an object, so the consecutive-sighting counts
// have to outlive a single call; tests build their own tracker and pass it
// explicitly.
//
// The detector itself lives in pkg/loginscan (#7238 stage 4).
var loginSightings = loginscan.NewSightingTracker()
