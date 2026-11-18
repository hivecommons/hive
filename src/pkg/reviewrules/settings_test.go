package reviewrules

import (
	"reflect"
	"testing"
)

func TestClassifierExtraPhrasesAndDisabledSignals(t *testing.T) {
	c := NewClassifier([]string{"  house   style ", "C++", "", "TODO."}, []string{"Authoritative", "no-such-signal"})
	tests := []struct {
		name    string
		body    string
		kind    Kind
		signals []string
	}{
		{"extra phrase", "This is our house style for tables.", KindRule, []string{SignalExtraPhrase}},
		{"extra phrase, other whitespace and case", "HOUSE\n style applies", KindRule, []string{SignalExtraPhrase}},
		{"extra phrase inside a word", "warehouse styles differ", KindCorrection, nil},
		{"punctuation phrase", "Prefer C++ here.", KindRule, []string{SignalExtraPhrase}},
		{"disabled signal", "That registry is authoritative for names.", KindCorrection, nil},
		{"other built-ins still match", "The lockfile is the source of truth.", KindRule, []string{SignalSourceOfTruth}},
		{"built-in before extra", "Going forward, follow the house style.", KindRule, []string{SignalGoingForward, SignalExtraPhrase}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := c.Classify(tc.body)
			if got.Kind != tc.kind || !reflect.DeepEqual(got.Signals, tc.signals) {
				t.Fatalf("Classify(%q) = %+v, want %q %v", tc.body, got, tc.kind, tc.signals)
			}
		})
	}
	if got := c.ExtractRuleText("Thanks. Use the house style for tables. Typo on line 3."); got != "Use the house style for tables" {
		t.Fatalf("ExtractRuleText = %q", got)
	}
}

func TestClassifierZeroValueIsTheBuiltInTable(t *testing.T) {
	if got, want := (Classifier{}).Classify(maintainersComment), Classify(maintainersComment); !reflect.DeepEqual(got, want) {
		t.Fatalf("zero Classifier = %+v, package Classify = %+v", got, want)
	}
	for _, name := range SignalNames() {
		if !IsSignal(name) {
			t.Fatalf("IsSignal(%q) = false", name)
		}
	}
	if IsSignal(SignalExtraPhrase) {
		t.Fatal("the extra-phrase signal is not a built-in and cannot be disabled")
	}
}

func TestValidExtraPhrase(t *testing.T) {
	long := make([]byte, MaxExtraPhraseRunes+1)
	for i := range long {
		long[i] = 'a'
	}
	for _, p := range []string{"", "   ", string(long)} {
		if ValidExtraPhrase(p) == nil {
			t.Fatalf("ValidExtraPhrase(%q) = nil, want an error", p)
		}
	}
	if err := ValidExtraPhrase("house style"); err != nil {
		t.Fatal(err)
	}
}

func TestEnabledWithEnvWinsOverFile(t *testing.T) {
	off, on := false, true
	tests := []struct {
		env  string
		file *bool
		want bool
	}{
		{"", nil, true},
		{"", &off, false},
		{"", &on, true},
		{"off", &on, false},
		{"1", &off, true},
	}
	for _, tc := range tests {
		t.Setenv(DisableEnvVar, tc.env)
		if got := EnabledWith(tc.file); got != tc.want {
			t.Fatalf("env %q file %v: EnabledWith = %v, want %v", tc.env, tc.file, got, tc.want)
		}
	}
}

func TestConfigWithTrustedAssociations(t *testing.T) {
	t.Setenv(TrustedAssociationsEnvVar, "")
	if got := ConfigWith(Settings{}).Trusted; !reflect.DeepEqual(got, ParseAssociations(DefaultTrustedAssociations)) {
		t.Fatalf("default trusted = %v", got)
	}
	if got := ConfigWith(Settings{TrustedAssociations: []string{" owner", ""}}).Trusted; !reflect.DeepEqual(got, map[string]bool{"OWNER": true}) {
		t.Fatalf("file trusted = %v", got)
	}
	if got := ConfigWith(Settings{TrustedAssociations: []string{" "}}).Trusted; !reflect.DeepEqual(got, ParseAssociations(DefaultTrustedAssociations)) {
		t.Fatalf("blank file list should fall back to the default, got %v", got)
	}
	t.Setenv(TrustedAssociationsEnvVar, "MEMBER,CONTRIBUTOR")
	if got := ConfigWith(Settings{TrustedAssociations: []string{"OWNER"}}).Trusted; !reflect.DeepEqual(got, map[string]bool{"MEMBER": true, "CONTRIBUTOR": true}) {
		t.Fatalf("env should win, got %v", got)
	}
	cfg := ConfigWith(Settings{ExtraPhrases: []string{"house style"}, DisabledSignals: []string{SignalNever}})
	if !reflect.DeepEqual(cfg.ExtraPhrases, []string{"house style"}) || !reflect.DeepEqual(cfg.DisabledSignals, []string{SignalNever}) {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestSetConfigAppliesOnTheNextCapture(t *testing.T) {
	s, _, _ := openStore(t, Config{})
	body := "Follow the house style for tables."
	if res := s.CaptureComment(cmt("inline:1", "OWNER", body), t0); res.Outcome != OutcomeNotRule {
		t.Fatalf("before: %+v", res)
	}
	s.SetConfig(Config{ExtraPhrases: []string{"house style"}, Trusted: map[string]bool{"CONTRIBUTOR": true}})
	res := s.CaptureComment(cmt("inline:2", "CONTRIBUTOR", body), t0)
	if res.Outcome != OutcomeCaptured || res.Rule == nil || res.Rule.Text != "Follow the house style for tables." {
		t.Fatalf("after: %+v", res)
	}
	if !reflect.DeepEqual(res.Signals, []string{SignalExtraPhrase}) {
		t.Fatalf("signals = %v", res.Signals)
	}
	s.SetConfig(Config{DisabledSignals: []string{SignalSourceOfTruth}})
	if res := s.CaptureComment(cmt("inline:3", "OWNER", "The lockfile is the source of truth."), t0); res.Outcome != OutcomeNotRule {
		t.Fatalf("disabled signal still captured: %+v", res)
	}
}
