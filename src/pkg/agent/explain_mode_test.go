package agent

import (
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/config"
)

// Explain mode (#3887) lets an operator ask an agent to say WHY it did what it
// did, without relaxing the instruction that it must act rather than narrate.
// These tests pin both halves: the precedence that decides whether it is on,
// and the properties of the injected prompt that keep it from becoming a
// licence to reply with prose.

func TestResolveExplainMode_Precedence(t *testing.T) {
	tests := []struct {
		name      string
		agentMode string
		hiveEnv   string
		envSet    bool
		want      string
	}{
		{
			name: "default is off",
			want: config.ExplainModeOff,
		},
		{
			name:      "per-agent brief",
			agentMode: config.ExplainModeBrief,
			want:      config.ExplainModeBrief,
		},
		{
			name:      "per-agent full",
			agentMode: config.ExplainModeFull,
			want:      config.ExplainModeFull,
		},
		{
			name:    "unset agent inherits hive default",
			hiveEnv: config.ExplainModeBrief,
			envSet:  true,
			want:    config.ExplainModeBrief,
		},
		{
			// The whole reason ExplainMode is tri-state: an agent explicitly set
			// to "off" must stay off when an operator turns explanation on
			// fleet-wide, or the per-agent opt-out is meaningless.
			name:      "explicit off beats hive default",
			agentMode: config.ExplainModeOff,
			hiveEnv:   config.ExplainModeFull,
			envSet:    true,
			want:      config.ExplainModeOff,
		},
		{
			name:      "per-agent value beats hive default",
			agentMode: config.ExplainModeFull,
			hiveEnv:   config.ExplainModeBrief,
			envSet:    true,
			want:      config.ExplainModeFull,
		},
		{
			// A typo must degrade to today's behavior, never to a mode nobody
			// asked for.
			name:      "invalid agent value falls back to off",
			agentMode: "verbose",
			want:      config.ExplainModeOff,
		},
		{
			name:    "invalid hive default falls back to off",
			hiveEnv: "yes-please",
			envSet:  true,
			want:    config.ExplainModeOff,
		},
		{
			name:    "surrounding whitespace in hive default is tolerated",
			hiveEnv: "  full  ",
			envSet:  true,
			want:    config.ExplainModeFull,
		},
		{
			name:    "empty hive default is off",
			hiveEnv: "",
			envSet:  true,
			want:    config.ExplainModeOff,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envSet {
				t.Setenv(config.ExplainModeEnvVar, tt.hiveEnv)
			} else {
				// Setenv registers cleanup, so this also isolates the case from
				// an operator's own HIVE_EXPLAIN_MODE in the test environment.
				t.Setenv(config.ExplainModeEnvVar, "")
			}
			got := resolveExplainMode(config.AgentConfig{ExplainMode: tt.agentMode})
			if got != tt.want {
				t.Errorf("resolveExplainMode(agent=%q, env=%q) = %q, want %q",
					tt.agentMode, tt.hiveEnv, got, tt.want)
			}
		})
	}
}

func TestExplainKickSuffix_OffProducesNothing(t *testing.T) {
	for _, mode := range []string{config.ExplainModeOff, "", "bogus"} {
		if got := explainKickSuffix(mode); got != "" {
			t.Errorf("explainKickSuffix(%q) = %q, want empty", mode, got)
		}
	}
}

// The suffix exists to make agents explainable; if it ever stops naming the
// marker the log filter greps for, the two halves of the feature silently stop
// agreeing and the "explain only" view goes blank.
func TestExplainKickSuffix_NamesTheLogMarker(t *testing.T) {
	for _, mode := range []string{config.ExplainModeBrief, config.ExplainModeFull} {
		suffix := explainKickSuffix(mode)
		if suffix == "" {
			t.Fatalf("explainKickSuffix(%q) is empty", mode)
		}
		if !strings.Contains(suffix, config.ExplainLinePrefix) {
			t.Errorf("explainKickSuffix(%q) never mentions %q, so the agent has no way to know how to tag explanation",
				mode, config.ExplainLinePrefix)
		}
	}
}

// The terseness instruction this feature relaxes has a rationale: agents told
// to explain themselves narrate INSTEAD of acting (see
// inferenceKickActionSuffix). The suffix must keep restating that acting is
// still required, or enabling explain mode reintroduces the failure the
// terseness rule was written to prevent.
func TestExplainKickSuffix_PreservesActionRequirement(t *testing.T) {
	for _, mode := range []string{config.ExplainModeBrief, config.ExplainModeFull} {
		suffix := strings.ToLower(explainKickSuffix(mode))
		if !strings.Contains(suffix, "failure") {
			t.Errorf("explainKickSuffix(%q) does not name an explain-only response as a failure", mode)
		}
		if !strings.Contains(suffix, "tool execution is still the requirement") {
			t.Errorf("explainKickSuffix(%q) does not restate that tool execution is still required", mode)
		}
	}
}

// Terse mode must be suspended for the explanation (a caveman-compressed
// explanation is useless to a human debugging) but ONLY there — the agent's
// real output must keep the compression the operator configured.
func TestExplainKickSuffix_ScopesTerseSuspensionToExplainLines(t *testing.T) {
	suffix := explainKickSuffix(config.ExplainModeBrief)
	lower := strings.ToLower(suffix)
	if !strings.Contains(lower, "terse-mode output rules are suspended") {
		t.Fatal("suffix does not suspend terse mode for the explanation")
	}
	if !strings.Contains(lower, "lines only") {
		t.Error("suffix suspends terse mode without scoping it to EXPLAIN lines, which would decompress all agent output")
	}
	if !strings.Contains(lower, "keep every other line unchanged") {
		t.Error("suffix does not tell the agent to leave its ordinary output alone")
	}
}

func TestExplainKickSuffix_FullExtendsBrief(t *testing.T) {
	brief := explainKickSuffix(config.ExplainModeBrief)
	full := explainKickSuffix(config.ExplainModeFull)
	if !strings.HasPrefix(full, brief) {
		t.Error("full mode should build on brief, not replace it — otherwise full silently drops the per-call reasons")
	}
	if len(full) <= len(brief) {
		t.Error("full mode adds nothing over brief")
	}
	if !strings.Contains(strings.ToLower(full), "after the work, never instead of it") {
		t.Error("full mode's closing block is not pinned to come after the work")
	}
}

func TestKickMessageWithSuffixes_OffIsByteIdentical(t *testing.T) {
	const kick = "Do the thing."
	got := kickMessageWithSuffixes(kick, false, config.ExplainModeOff)
	if got != kick {
		t.Errorf("explain off altered the kick: %q", got)
	}
}

func TestKickMessageWithSuffixes_AppendsExplainBlock(t *testing.T) {
	const kick = "Do the thing."
	got := kickMessageWithSuffixes(kick, false, config.ExplainModeBrief)
	if !strings.HasPrefix(got, kick) {
		t.Error("original kick text was not preserved at the head of the message")
	}
	if !strings.Contains(got, explainKickSuffixBrief) {
		t.Error("explain suffix missing from composed kick")
	}
	if strings.Contains(got, inferenceKickActionSuffix) {
		t.Error("non-inference backend must not receive the inference action suffix")
	}
}

// Ordering is load-bearing on inference backends, where both suffixes apply:
// the model must read "EXECUTE, DO NOT NARRATE" before it reads the explain
// block, so the latter qualifies the former instead of appearing to revoke it.
func TestKickMessageWithSuffixes_ActionSuffixPrecedesExplain(t *testing.T) {
	got := kickMessageWithSuffixes("Do the thing.", true, config.ExplainModeFull)

	action := strings.Index(got, inferenceKickActionSuffix)
	explain := strings.Index(got, explainKickSuffixBrief) // full starts with brief
	if action < 0 {
		t.Fatal("inference action suffix missing")
	}
	if explain < 0 {
		t.Fatal("explain suffix missing")
	}
	if action > explain {
		t.Error("explain suffix precedes the action-forcing suffix; the model would read permission to explain before the requirement to act")
	}
}

func TestAgentEnvPairs_ExportsResolvedExplainMode(t *testing.T) {
	tests := []struct {
		name      string
		agentMode string
		hiveEnv   string
		want      string
	}{
		{name: "off by default", want: config.ExplainModeOff},
		{name: "per-agent", agentMode: config.ExplainModeFull, want: config.ExplainModeFull},
		{name: "inherited", hiveEnv: config.ExplainModeBrief, want: config.ExplainModeBrief},
		// The env var carries the RESOLVED answer, so an agent's own scripts
		// never have to re-implement the precedence rules.
		{name: "resolved not raw", agentMode: "nonsense", want: config.ExplainModeOff},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(config.ExplainModeEnvVar, tt.hiveEnv)
			ap := &AgentProcess{
				Name:   "scanner",
				Config: config.AgentConfig{Backend: "claude", Model: "sonnet", ExplainMode: tt.agentMode},
			}
			var got string
			var found bool
			for _, p := range testEnvPairs(ap) {
				if p.Key == config.ExplainModeEnvVar {
					got, found = p.Value, true
					if p.Secret {
						t.Error("explain mode is not a secret and must not ride the secret-only path")
					}
				}
			}
			if !found {
				t.Fatalf("%s missing from agent env", config.ExplainModeEnvVar)
			}
			if got != tt.want {
				t.Errorf("%s = %q, want %q", config.ExplainModeEnvVar, got, tt.want)
			}
		})
	}
}
