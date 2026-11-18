package reviewrules

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// hivecommons/hive#19138: operator-tunable capture policy.

func TestSignalVocabularyMatchesConfig(t *testing.T) {
	if got, want := strings.Join(config.ReviewFeedbackSignals, ","), strings.Join(SignalNames(), ","); got != want {
		t.Fatalf("config.ReviewFeedbackSignals = %s, classifier signals = %s", got, want)
	}
}

func TestResolve(t *testing.T) {
	off := false
	t.Run("defaults", func(t *testing.T) {
		t.Setenv(DisableEnvVar, "")
		t.Setenv(TrustedAssociationsEnvVar, "")
		r := Resolve(Settings{})
		if r.Policy.Off || r.EnabledFromEnv || r.TrustedFromEnv {
			t.Fatalf("resolved = %+v", r)
		}
		if !r.Policy.Trusted["OWNER"] || !r.Policy.Trusted["MEMBER"] || !r.Policy.Trusted["COLLABORATOR"] || r.Policy.Trusted["CONTRIBUTOR"] {
			t.Fatalf("trusted = %v", r.Policy.Trusted)
		}
	})
	t.Run("yaml applies without the env", func(t *testing.T) {
		t.Setenv(DisableEnvVar, "")
		t.Setenv(TrustedAssociationsEnvVar, "")
		r := Resolve(Settings{Enabled: &off, TrustedAssociations: []string{"OWNER"}, DisabledSignals: []string{"never"}})
		if !r.Policy.Off || len(r.Policy.Trusted) != 1 || !r.Policy.Trusted["OWNER"] || !r.Policy.DisabledSignals["never"] {
			t.Fatalf("resolved = %+v", r)
		}
	})
	t.Run("env still wins", func(t *testing.T) {
		t.Setenv(DisableEnvVar, "off")
		t.Setenv(TrustedAssociationsEnvVar, "MEMBER, contributor")
		on := true
		r := Resolve(Settings{Enabled: &on, TrustedAssociations: []string{"OWNER"}})
		if !r.Policy.Off || !r.EnabledFromEnv || !r.TrustedFromEnv || !r.Policy.Trusted["CONTRIBUTOR"] || r.Policy.Trusted["OWNER"] {
			t.Fatalf("resolved = %+v", r)
		}
	})
	t.Run("env on overrides yaml off", func(t *testing.T) {
		t.Setenv(DisableEnvVar, "1")
		t.Setenv(TrustedAssociationsEnvVar, "")
		if r := Resolve(Settings{Enabled: &off}); r.Policy.Off || !r.EnabledFromEnv {
			t.Fatalf("resolved = %+v", r)
		}
	})
}

func TestClassifyWithPolicy(t *testing.T) {
	extra := Resolve(Settings{ExtraPhrases: []string{"house rule", "ground truth"}}).Policy
	for _, tc := range []struct {
		name string
		body string
		p    Policy
		kind Kind
		sig  string
	}{
		{"extra phrase", "This is a House   Rule for us.", extra, KindRule, SignalExtraPhrase},
		{"extra phrase at the edges", "ground truth", extra, KindRule, SignalExtraPhrase},
		{"extra phrase needs word boundaries", "the groundtruthful answer", extra, KindCorrection, ""},
		{"extra phrase in a quote is ignored", "> house rule\nthe score is too high", extra, KindCorrection, ""},
		{"no extra phrases, no match", "This is a house rule.", Policy{}, KindCorrection, ""},
		{"phrase is not a pattern", "anything", Resolve(Settings{ExtraPhrases: []string{".*.*"}}).Policy, KindCorrection, ""},
		{"disabled signal", "Add that to the knowledge base.", Policy{DisabledSignals: map[string]bool{SignalKnowledgeBase: true}}, KindCorrection, ""},
		{"other signals survive", "Going forward, cite it. Add that to the knowledge base.", Policy{DisabledSignals: map[string]bool{SignalKnowledgeBase: true}}, KindRule, SignalGoingForward},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyWith(tc.body, tc.p)
			if got.Kind != tc.kind {
				t.Fatalf("kind = %q (%v), want %q", got.Kind, got.Signals, tc.kind)
			}
			if tc.sig != "" && (len(got.Signals) != 1 || got.Signals[0] != tc.sig) {
				t.Fatalf("signals = %v, want [%s]", got.Signals, tc.sig)
			}
		})
	}
}

func TestExtractRuleTextWithExtraPhrase(t *testing.T) {
	p := Policy{ExtraPhrases: []string{"house rule"}}
	got := ExtractRuleTextWith("Nice work overall. House rule: every analysis cites the scorecard. Thanks!", p)
	if !strings.Contains(got, "every analysis cites the scorecard") || strings.Contains(got, "Thanks") {
		t.Fatalf("text = %q", got)
	}
}

// An owner's edit applies to a running store: no reopen.
func TestStoreSetPolicyAppliesLive(t *testing.T) {
	s, _, _ := openStore(t, Config{})
	const phrase = "House rule: every analysis cites the scorecard."
	if r := s.CaptureComment(cmt("inline:1", "OWNER", phrase), t0); r.Outcome != OutcomeNotRule {
		t.Fatalf("before = %q", r.Outcome)
	}
	s.SetPolicy(Policy{Trusted: ParseAssociations("OWNER"), ExtraPhrases: []string{"house rule"}})
	r := s.CaptureComment(cmt("inline:2", "OWNER", phrase), t0)
	if r.Outcome != OutcomeCaptured || len(r.Signals) != 1 || r.Signals[0] != SignalExtraPhrase {
		t.Fatalf("after = %q %v", r.Outcome, r.Signals)
	}
	// Narrowing the trusted set queues a member's rule that was trusted before.
	if r := s.CaptureComment(cmt("inline:3", "MEMBER", "Going forward, cite the commit for every claim."), t0); r.Outcome != OutcomeQueued {
		t.Fatalf("member after narrowing = %q", r.Outcome)
	}
	// A disabled built-in signal stops matching at once.
	s.SetPolicy(Policy{Trusted: ParseAssociations("OWNER"), DisabledSignals: map[string]bool{SignalGoingForward: true}})
	if r := s.CaptureComment(cmt("inline:4", "OWNER", "Going forward, cite the commit for every claim, always."), t0); r.Outcome != OutcomeNotRule {
		t.Fatalf("disabled signal = %q", r.Outcome)
	}
}
