package config

import (
	"strings"
	"testing"
)

func TestParseMode(t *testing.T) {
	cases := []struct {
		in   string
		want Mode
		err  bool
	}{
		{"", ModeInProcess, false},
		{"inprocess", ModeInProcess, false},
		{" InProcess ", ModeInProcess, false},
		{"sidecar", ModeSidecar, false},
		{"SIDECAR", ModeSidecar, false},
		{"remote", "", true},
	}
	for _, tc := range cases {
		got, err := ParseMode(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("ParseMode(%q) = %q, want error", tc.in, got)
			} else if !strings.Contains(err.Error(), "inprocess or sidecar") {
				t.Errorf("ParseMode(%q) error %q should name the accepted modes", tc.in, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestModesListsEveryConstant(t *testing.T) {
	want := map[Mode]bool{ModeInProcess: true, ModeSidecar: true}
	for _, m := range Modes() {
		delete(want, m)
	}
	if len(want) != 0 {
		t.Fatalf("Modes() omits %v", want)
	}
	if DefaultMode != ModeInProcess {
		t.Fatalf("DefaultMode = %q, want inprocess until the sidecar flips (#14138 stage 6)", DefaultMode)
	}
}

func TestRuntimeConfigValidate(t *testing.T) {
	var zero RuntimeConfig
	if err := zero.Validate(); err != nil || zero.Mode != ModeInProcess {
		t.Fatalf("zero RuntimeConfig: %+v, %v; want inprocess", zero, err)
	}
	if zero.Server != "" {
		t.Fatalf("in-process Validate must not fill Server, got %q", zero.Server)
	}

	t.Setenv(ServeSocketEnvVar, "/run/test/crux.sock")
	side := RuntimeConfig{Mode: " Sidecar "}
	if err := side.Validate(); err != nil {
		t.Fatal(err)
	}
	if side.Mode != ModeSidecar || side.Server != "/run/test/crux.sock" {
		t.Fatalf("sidecar Validate = %+v; want normalised mode and default socket", side)
	}

	explicit := RuntimeConfig{Mode: ModeSidecar, Server: "127.0.0.1:9", Token: "t"}
	if err := explicit.Validate(); err != nil || explicit.Server != "127.0.0.1:9" {
		t.Fatalf("explicit server must be kept: %+v, %v", explicit, err)
	}

	bad := RuntimeConfig{Mode: "nope"}
	if err := bad.Validate(); err == nil {
		t.Fatal("unknown mode must fail Validate")
	}
}

func TestRuntimeConfigFromEnv(t *testing.T) {
	t.Setenv(ModeEnvVar, "sidecar")
	t.Setenv(ServerEnvVar, "unix:///tmp/x.sock")
	t.Setenv(ServeTokenEnvVar, "secret")
	got := RuntimeConfigFromEnv()
	want := RuntimeConfig{Mode: "sidecar", Server: "unix:///tmp/x.sock", Token: "secret"}
	if got != want {
		t.Fatalf("RuntimeConfigFromEnv() = %+v, want %+v", got, want)
	}
	t.Setenv(ModeEnvVar, "")
	if got := RuntimeConfigFromEnv(); got.Mode != "" {
		t.Fatalf("unset %s must read as empty (defaulted by Validate), got %q", ModeEnvVar, got.Mode)
	}
}

func TestResolveReasoningEffort(t *testing.T) {
	cases := []struct{ backend, model, configured, want string }{
		{"agy", "gemini-pro", "", AgyDefaultEffort},
		{"agy", "gemini-pro-high", "", "high"},
		{"agy", "gemini-pro", "medium", "medium"},
		{"agy", "gemini-pro", "xhigh", AgyDefaultEffort},
		{"agy", "", "high", ""},
		{"codex", "o3", "xhigh", "xhigh"},
		{"codex", "o3", "", ""},
		{"omp", "m", "high", "high"},
		{ClaudeBackend, "opus", ClaudeEffortHigh, ClaudeEffortHigh},
		{ClaudeBackend, "opus", "bogus", ""},
		{"copilot", "gpt", "high", ""},
	}
	for _, tc := range cases {
		if got := ResolveReasoningEffort(tc.backend, tc.model, tc.configured); got != tc.want {
			t.Errorf("ResolveReasoningEffort(%q, %q, %q) = %q, want %q", tc.backend, tc.model, tc.configured, got, tc.want)
		}
	}
	if AgyLaunchEffort("x", "low") != "low" || AgyLaunchEffort("x-medium", "") != "medium" {
		t.Fatal("AgyLaunchEffort: configured effort must win, then the model suffix")
	}
}
