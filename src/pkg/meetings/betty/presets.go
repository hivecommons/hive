package betty

import (
	"github.com/hivecommons/hive/pkg/config"
)

// The preset table itself lives in config (config.MeetingsPersonaPresets)
// because config resolves the effective persona and cannot import this
// package (betty imports knowledge, which imports config). These helpers
// re-export the listing the settings API uses (hivecommons/hive#18396).

// Presets returns the built-in personas in the settings tab's order, the
// default (Betty) first.
func Presets() []config.MeetingsPersonaPreset {
	return config.OrderedMeetingsPersonaPresets()
}

// Preset returns one built-in persona by ID.
func Preset(id string) (config.MeetingsPersonaPreset, bool) {
	p, ok := config.MeetingsPersonaPresets[id]
	return p, ok
}
