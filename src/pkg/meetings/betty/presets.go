package betty

import (
	"sort"

	"github.com/hivecommons/hive/pkg/config"
)

// The preset table itself lives in config (config.MeetingsPersonaPresets)
// because config resolves the effective persona and cannot import this
// package (betty imports knowledge, which imports config). These helpers
// are the listing the settings API uses (hivecommons/hive#18396).

// Presets returns the built-in personas sorted by ID.
func Presets() []config.MeetingsPersonaPreset {
	out := make([]config.MeetingsPersonaPreset, 0, len(config.MeetingsPersonaPresets))
	for _, p := range config.MeetingsPersonaPresets {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Preset returns one built-in persona by ID.
func Preset(id string) (config.MeetingsPersonaPreset, bool) {
	p, ok := config.MeetingsPersonaPresets[id]
	return p, ok
}
