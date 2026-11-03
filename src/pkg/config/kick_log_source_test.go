package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestNormalizeKickLogSource(t *testing.T) {
	for _, tc := range []struct {
		in    string
		want  string
		valid bool
	}{
		{"", KickLogSourceScrollback, false},
		{"scrollback", KickLogSourceScrollback, true},
		{"transcript", KickLogSourceTranscript, true},
		{"auto", KickLogSourceAuto, true},
		{" Transcript ", KickLogSourceTranscript, true},
		{"pipe-pane", KickLogSourceScrollback, false},
	} {
		if got := NormalizeKickLogSource(tc.in); got != tc.want {
			t.Errorf("NormalizeKickLogSource(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got := ValidKickLogSource(tc.in); got != tc.valid {
			t.Errorf("ValidKickLogSource(%q) = %v, want %v", tc.in, got, tc.valid)
		}
	}
	if got := KickLogSources(); len(got) != 3 || got[0] != KickLogSourceScrollback {
		t.Errorf("KickLogSources() = %v", got)
	}
}

func TestLoggingConfig_ResolveKickLogSource(t *testing.T) {
	for _, tc := range []struct {
		name       string
		env        string
		cfg        string
		want       string
		wantOrigin string
	}{
		{"default", "", "", KickLogSourceScrollback, KickLogSourceOriginDefault},
		{"config scrollback", "", "scrollback", KickLogSourceScrollback, KickLogSourceOriginConfig},
		{"config transcript", "", "transcript", KickLogSourceTranscript, KickLogSourceOriginConfig},
		{"config auto", "", "AUTO", KickLogSourceAuto, KickLogSourceOriginConfig},
		{"config unknown falls back", "", "pipe-pane", KickLogSourceScrollback, KickLogSourceOriginDefault},
		{"env wins over config", "transcript", "scrollback", KickLogSourceTranscript, KickLogSourceOriginEnv},
		{"env wins over unset config", "auto", "", KickLogSourceAuto, KickLogSourceOriginEnv},
		{"unknown env ignored", "bogus", "transcript", KickLogSourceTranscript, KickLogSourceOriginConfig},
		{"blank env ignored", "  ", "auto", KickLogSourceAuto, KickLogSourceOriginConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(KickLogSourceEnvVar, tc.env)
			l := LoggingConfig{KickLogSource: tc.cfg}
			if got := l.ResolveKickLogSource(); got != tc.want {
				t.Errorf("ResolveKickLogSource() = %q, want %q", got, tc.want)
			}
			if got := l.KickLogSourceOrigin(); got != tc.wantOrigin {
				t.Errorf("KickLogSourceOrigin() = %q, want %q", got, tc.wantOrigin)
			}
			wantEnv := ""
			if tc.wantOrigin == KickLogSourceOriginEnv {
				wantEnv = tc.want
			}
			if got := KickLogSourceEnvOverride(); got != wantEnv {
				t.Errorf("KickLogSourceEnvOverride() = %q, want %q", got, wantEnv)
			}
		})
	}
}

func TestLoggingConfig_KickLogSourceYAMLRoundTrip(t *testing.T) {
	t.Setenv(KickLogSourceEnvVar, "")
	var cfg Config
	if err := yaml.Unmarshal([]byte("governor:\n  logging:\n    kick_log_source: transcript\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.applyDefaults()
	if cfg.Governor.Logging.KickLogSource != KickLogSourceTranscript {
		t.Fatalf("kick_log_source = %q, want transcript", cfg.Governor.Logging.KickLogSource)
	}
	if got := cfg.Governor.Logging.ResolveKickLogSource(); got != KickLogSourceTranscript {
		t.Errorf("ResolveKickLogSource() = %q, want transcript", got)
	}
}
