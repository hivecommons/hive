package config

import (
	"testing"

	"github.com/hivecommons/hive/pkg/ioscan"
)

// TestIoscanConfig_IsEnabled covers the F11 tri-state default-on contract:
// a nil Enabled pointer (an omitted `enabled:` key, or a zero-valued
// IoscanConfig) means scanning is ON, while an explicit false opts out.
func TestIoscanConfig_IsEnabled(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name string
		in   *bool
		want bool
	}{
		{"nil defaults on", nil, true},
		{"explicit true", &on, true},
		{"explicit false opts out", &off, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := IoscanConfig{Enabled: tc.in}
			if got := c.IsEnabled(); got != tc.want {
				t.Fatalf("IsEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIoscanConfig_FailClosed(t *testing.T) {
	cases := []struct {
		name string
		mode string
		want bool
	}{
		{"empty defaults open", "", false},
		{"open", "open", false},
		{"closed", "closed", true},
		{"closed case insensitive", "CLOSED", true},
		{"unknown stays open", "strict", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := IoscanConfig{FailMode: tc.mode}
			if got := c.FailClosed(); got != tc.want {
				t.Fatalf("FailClosed() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIoscanConfig_CanariesEnabled covers the flipped default: canaries are ON
// now that CanaryRegistry.Scan is encoding-aware (#6701, #6720). A nil pointer
// (an omitted `canaries:` key, or a zero-valued IoscanConfig) means ON, an
// explicit true stays ON, and — the dangerous case a naive nil-means-true
// implementation swallows — an explicit false still opts OUT.
func TestIoscanConfig_CanariesEnabled(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name string
		in   *bool
		want bool
	}{
		{"nil defaults on", nil, true},
		{"explicit true", &on, true},
		{"explicit false opts out", &off, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := IoscanConfig{Canaries: tc.in}
			if got := c.CanariesEnabled(); got != tc.want {
				t.Fatalf("CanariesEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIoscanConfig_FailClosedAtLevel covers the level-conditional default: with
// no explicit fail_mode, the ACMM packs supply closed at L5/L6 (where agents can
// merge) and open at L1-L4. An explicit operator value overrides the level
// default in either direction, at every level.
func TestIoscanConfig_FailClosedAtLevel(t *testing.T) {
	open, closed := "open", "closed"
	cases := []struct {
		name  string
		mode  string
		level int
		want  bool
	}{
		{"default L1 open", "", 1, false},
		{"default L2 open", "", 2, false},
		{"default L3 open", "", 3, false},
		{"default L4 open", "", 4, false},
		{"default L5 closed", "", 5, true},
		{"default L6 closed", "", 6, true},
		{"default unknown level open", "", 0, false},

		{"override closed at L1", closed, 1, true},
		{"override closed at L4", closed, 4, true},
		{"override open at L5", open, 5, false},
		{"override open at L6", open, 6, false},
		{"override closed at L6 stays closed", closed, 6, true},
		{"override open at L1 stays open", open, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := IoscanConfig{FailMode: tc.mode}
			if got := c.FailClosedAtLevel(tc.level); got != tc.want {
				t.Fatalf("FailClosedAtLevel(%d) with mode %q = %v, want %v",
					tc.level, tc.mode, got, tc.want)
			}
		})
	}
}

func TestIoscanClassifierConfig_DefaultFollowsLevel(t *testing.T) {
	var c IoscanConfig
	for level, want := range map[int]bool{0: false, 1: false, 3: false, 4: true, 5: true, 6: true, 7: false} {
		if got := c.ClassifierEnabledAtLevel(level); got != want {
			t.Errorf("ClassifierEnabledAtLevel(%d) = %v, want %v", level, got, want)
		}
	}
	off := false
	c.Classifier.Enabled = &off
	if c.ClassifierEnabledAtLevel(5) {
		t.Fatal("explicit enabled: false must win at L5")
	}
	on := true
	c.Classifier.Enabled = &on
	if !c.ClassifierEnabledAtLevel(1) {
		t.Fatal("explicit enabled: true must win at L1")
	}
	disabled := false
	c.Enabled = &disabled
	if c.ClassifierEnabledAtLevel(5) {
		t.Fatal("ioscan.enabled: false must switch the classifier off too")
	}
}

func TestIoscanClassifierConfig_FailClosedAtLevel(t *testing.T) {
	cases := []struct {
		name, ioscanMode, classifierMode string
		level                            int
		want                             bool
	}{
		{"pack default L4 open", "", "", 4, false},
		{"pack default L5 closed", "", "", 5, true},
		{"pack default L6 closed", "", "", 6, true},
		{"ioscan open inherited", "open", "", 5, false},
		{"ioscan closed inherited", "closed", "", 1, true},
		{"classifier open wins", "closed", "open", 6, false},
		{"classifier closed wins", "open", "closed", 1, true},
		{"case-insensitive", "", "CLOSED", 2, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := IoscanConfig{FailMode: tc.ioscanMode, Classifier: IoscanClassifierConfig{FailMode: tc.classifierMode}}
			if got := c.ClassifierFailClosedAtLevel(tc.level); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIoscanClassifierConfig_CLIDefaults(t *testing.T) {
	var c IoscanClassifierConfig
	if c.CLIBackend() != "" {
		t.Fatalf("CLIBackend() = %q, want empty (HTTP path)", c.CLIBackend())
	}
	if got := c.CLIBudgetPerEvalOrDefault(); got != ioscan.DefaultCLIClassifierBudgetPerEval || ioscan.DefaultCLIClassifierBudgetPerEval != 8 {
		t.Fatalf("CLIBudgetPerEvalOrDefault() = %d, want 8", got)
	}
	if got := c.CLIDailyQuotaOrDefault(); got != ioscan.DefaultCLIClassifierDailyQuota || ioscan.DefaultCLIClassifierDailyQuota != 500 {
		t.Fatalf("CLIDailyQuotaOrDefault() = %d, want 500", got)
	}
	if ioscan.DefaultCLIClassifierBudgetPerEval >= ioscan.DefaultClassifierBudgetPerEval {
		t.Fatalf("CLI budget %d must be below the HTTP budget %d", ioscan.DefaultCLIClassifierBudgetPerEval, ioscan.DefaultClassifierBudgetPerEval)
	}

	c = IoscanClassifierConfig{Backend: "  claude ", CLIBudgetPerEval: 4, CLIDailyQuota: 50}
	if c.CLIBackend() != "claude" || c.CLIBudgetPerEvalOrDefault() != 4 || c.CLIDailyQuotaOrDefault() != 50 {
		t.Fatalf("explicit CLI values not honoured: %q %d %d", c.CLIBackend(), c.CLIBudgetPerEvalOrDefault(), c.CLIDailyQuotaOrDefault())
	}
}

func TestIoscanClassifierConfig_ValidateCLI(t *testing.T) {
	cases := []struct {
		name    string
		in      IoscanClassifierConfig
		wantErr bool
	}{
		{"zero values use defaults", IoscanClassifierConfig{}, false},
		{"explicit values", IoscanClassifierConfig{Backend: "claude", CLIBudgetPerEval: 8, CLIDailyQuota: 500}, false},
		{"budget at HTTP ceiling", IoscanClassifierConfig{CLIBudgetPerEval: ioscan.DefaultClassifierBudgetPerEval}, false},
		{"negative budget", IoscanClassifierConfig{CLIBudgetPerEval: -1}, true},
		{"budget above HTTP ceiling", IoscanClassifierConfig{CLIBudgetPerEval: ioscan.DefaultClassifierBudgetPerEval + 1}, true},
		{"negative quota", IoscanClassifierConfig{CLIDailyQuota: -1}, true},
		// Backend names are refused at boot with a log line, not at load.
		{"refused backend still loads", IoscanClassifierConfig{Backend: "copilot"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.in.ValidateCLI(); (err != nil) != tc.wantErr {
				t.Fatalf("ValidateCLI() err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
