package agent

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
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
			// hiveDefault "" is the no-resolver path (tests / bare setups):
			// resolveExplainMode still reads the env var itself, so these cases
			// pin the pre-#4712 behavior.
			got := resolveExplainMode(config.AgentConfig{ExplainMode: tt.agentMode}, "")
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

// ── Explanation must not be mistaken for execution ───────────────────────────
//
// The prose-only watchdog (checkInferenceKickStall) disarms when
// countToolMarkers rises after a kick. toolSummaryRe matches ENGLISH PHRASES —
// "read 3 files", "running 2 shell commands" — because that is how the CLI
// renders its own collapsed tool summaries. An agent in explain mode is asked
// to state in plain English what it is about to do, so it writes those exact
// phrases as narration.
//
// Left unhandled, explain mode silently disables the one check that catches a
// model answering a kick with a plan instead of running it — on precisely the
// agents an operator turned explanation ON to debug.

func TestCountToolMarkers_ExplanationIsNotExecution(t *testing.T) {
	// No tool ran here. Every phrase that looks like execution is narration on
	// an EXPLAIN line, and both spellings toolSummaryRe accepts are present.
	proseOnly := "" +
		"EXPLAIN: I am reading 3 files under pkg/agent to find the kick handler.\n" +
		"EXPLAIN: Then running 2 shell commands to confirm the build still passes.\n" +
		"  EXPLAIN: indented, because the CLI indents assistant output.\n" +
		"Here is the plan I suggest you run.\n"

	if n := countToolMarkers(proseOnly); n != 0 {
		t.Errorf("countToolMarkers = %d on a pane with NO tool execution — the prose-only "+
			"watchdog would disarm and the action nudge would never fire (#3887)", n)
	}
}

// A quoted tool glyph inside an explanation is the agent DESCRIBING a call, not
// making one — which is why the whole line is dropped rather than just the
// prefix.
func TestCountToolMarkers_QuotedGlyphInExplanationDoesNotCount(t *testing.T) {
	if n := countToolMarkers("EXPLAIN: next I will run ⏺ Bash( to list the dir.\n"); n != 0 {
		t.Errorf("countToolMarkers = %d for a tool glyph quoted inside an explanation, want 0", n)
	}
}

// The positive control, and the property that actually matters: stripping
// explanation must not cost the watchdog a REAL marker sitting beside it.
func TestCountToolMarkers_RealToolsStillCountAlongsideExplanation(t *testing.T) {
	mixed := "" +
		"EXPLAIN: reading the manager to find where kicks are delivered.\n" +
		"⏺ Read(v2/pkg/agent/manager.go)\n" +
		"  ⎿  read 1 file\n" +
		"EXPLAIN: now running the tests to see the failure.\n" +
		"⏺ Bash(go test ./pkg/agent/)\n"

	got := countToolMarkers(mixed)
	// ⏺ Read( + ⎿ + ⏺ Bash( — the "read 1 file" summary rides on the ⎿ line and
	// is counted by toolSummaryRe as well, which is existing behaviour.
	if got < 3 {
		t.Errorf("countToolMarkers = %d with real tool calls present, want >= 3 — "+
			"stripping explanation must not swallow genuine markers", got)
	}
}

// Explain mode off must leave pane analysis byte-identical to before the
// feature existed: the fast path returns the input untouched.
func TestStripExplainLines_NoExplanationIsUnchanged(t *testing.T) {
	pane := "⏺ Bash(ls)\n  ⎿  ran 1 shell command\nnormal output\n"
	if got := stripExplainLines(pane); got != pane {
		t.Errorf("a pane with no explanation was modified:\n got %q\nwant %q", got, pane)
	}
	if strings.Contains(pane, config.ExplainLinePrefix) {
		t.Fatal("fixture accidentally contains the explain prefix — test is not proving the fast path")
	}
}

// #4712: the hive-wide default used to live ONLY in HIVE_EXPLAIN_MODE, which is
// set on the deployment. A hosted spoke owner has no deployment-env access, so
// they went looking for the setting and found nothing. governor.explain_mode
// puts it in config (and therefore in the dashboard form); these tests pin the
// precedence between the two and the per-agent opt-out that must survive both.
func TestResolveExplainMode_GovernorDefault(t *testing.T) {
	tests := []struct {
		name        string
		agentMode   string
		hiveDefault string
		env         string
		want        string
	}{
		{
			name:        "governor default applies to an unset agent",
			hiveDefault: config.ExplainModeBrief,
			want:        config.ExplainModeBrief,
		},
		{
			// The whole point of the tri-state: an agent that opted out stays
			// out even when the operator turns explanation on hive-wide.
			name:        "explicit per-agent off beats the governor default",
			agentMode:   config.ExplainModeOff,
			hiveDefault: config.ExplainModeFull,
			want:        config.ExplainModeOff,
		},
		{
			name:        "per-agent value beats the governor default",
			agentMode:   config.ExplainModeBrief,
			hiveDefault: config.ExplainModeFull,
			want:        config.ExplainModeBrief,
		},
		{
			// Config wins over the environment — the resolver has already
			// applied that precedence, so what arrives here is final.
			name:        "governor default beats the env var",
			hiveDefault: config.ExplainModeFull,
			env:         config.ExplainModeBrief,
			want:        config.ExplainModeFull,
		},
		{
			// No resolver wired / no governor default set: the env var is still
			// honoured, so hives that already set it keep working.
			name: "env var still applies when no governor default is set",
			env:  config.ExplainModeBrief,
			want: config.ExplainModeBrief,
		},
		{
			name:        "unknown governor default degrades to off",
			hiveDefault: "verbose",
			want:        config.ExplainModeOff,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(config.ExplainModeEnvVar, tt.env)
			got := resolveExplainMode(config.AgentConfig{ExplainMode: tt.agentMode}, tt.hiveDefault)
			if got != tt.want {
				t.Errorf("resolveExplainMode(agent=%q, hiveDefault=%q, env=%q) = %q, want %q",
					tt.agentMode, tt.hiveDefault, tt.env, got, tt.want)
			}
		})
	}
}

// The resolver is injected, so a Manager without one (tests, bare setups) must
// keep resolving rather than panicking or reporting a mode.
func TestManagerExplainModeDefault_UninjectedIsEmpty(t *testing.T) {
	m := &Manager{}
	if got := m.explainModeDefault(); got != "" {
		t.Errorf("explainModeDefault() with no resolver = %q, want empty", got)
	}
	m.SetExplainModeDefaultResolver(func() string { return config.ExplainModeFull })
	if got := m.explainModeDefault(); got != config.ExplainModeFull {
		t.Errorf("explainModeDefault() = %q, want %q", got, config.ExplainModeFull)
	}
	// A nil fn clears it: the env-only fallback must be restorable.
	m.SetExplainModeDefaultResolver(nil)
	if got := m.explainModeDefault(); got != "" {
		t.Errorf("explainModeDefault() after clear = %q, want empty", got)
	}
}

// The resolver is read on every kick, not cached at boot, so an operator who
// turns explanation on from the dashboard sees it on the next kick.
func TestManagerExplainModeDefault_ReadsLiveValue(t *testing.T) {
	m := &Manager{}
	current := config.ExplainModeOff
	m.SetExplainModeDefaultResolver(func() string { return current })
	if got := m.explainModeDefault(); got != config.ExplainModeOff {
		t.Fatalf("explainModeDefault() = %q, want %q", got, config.ExplainModeOff)
	}
	current = config.ExplainModeBrief
	if got := m.explainModeDefault(); got != config.ExplainModeBrief {
		t.Errorf("explainModeDefault() after config change = %q, want %q", got, config.ExplainModeBrief)
	}
}
