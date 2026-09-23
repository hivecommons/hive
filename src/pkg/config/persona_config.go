package config

// PersonaConfig holds operator settings for the per-user communication
// persona. It configures how persona LEARNING behaves; the persona record
// itself lives on the hub user record (pkg/persona) and shares no key with
// this or any autonomy configuration.
type PersonaConfig struct {
	Learning PersonaLearningConfig `yaml:"learning,omitempty" json:"learning,omitempty"`
}

// PersonaLearningConfig gates persona learning from observed behaviour
// (hivecommons/hive#8363). Default off: an absent block records no signals
// and proposes nothing. When on, the chat spine counts explicit signals per
// user and PROPOSES one-step persona changes the user must accept; it never
// silently mutates a persona and never touches what an agent may do.
type PersonaLearningConfig struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Threshold is the number of same-direction signals inside one window
	// that produces a suggestion. Zero means the persona package default (5).
	Threshold int `yaml:"threshold,omitempty" json:"threshold,omitempty"`
	// WindowDays bounds the evidence window. Zero means the default (7).
	WindowDays int `yaml:"window_days,omitempty" json:"window_days,omitempty"`
}
