package config

import (
	"strings"
	"testing"
)

// ============================================================
// #5921 — an agent with `backend: copilot` but `launch_cmd: bob --model auto`
// was accepted silently: the hive launched Bob in the pane, waited for a
// Copilot prompt that could never appear, and relaunched the agent as "hung"
// forever. The contradiction must be rejected at config load/save time.
// ============================================================

func TestLaunchCmdDeclaredBackend(t *testing.T) {
	cases := []struct {
		cmd  string
		want string
	}{
		// The live #5921 config.
		{"bob --model auto", "bob"},
		{"copilot --allow-all --model claude-sonnet-4-6", "copilot"},
		{"claude --dangerously-skip-permissions", "claude"},
		// Absolute path and leading env assignments.
		{"/usr/bin/copilot --allow-all", "copilot"},
		{"FOO=bar BAZ=qux gemini --model pro", "gemini"},
		// The standard wrapper declares its backend explicitly.
		{"agent-launch.sh --backend copilot --model claude-sonnet-4-6", "copilot"},
		{"/usr/local/bin/agent-launch.sh --model x --backend goose", "goose"},
		{"agent-launch.sh --model x", ""}, // wrapper without --backend: no opinion
		// Unknown wrappers and empties: no opinion, never a mismatch.
		{"", ""},
		{"   ", ""},
		{"./my-custom-wrapper.sh --whatever", ""},
		{"python3 runner.py", ""},
	}
	for _, tc := range cases {
		if got := LaunchCmdDeclaredBackend(tc.cmd); got != tc.want {
			t.Errorf("LaunchCmdDeclaredBackend(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}

func TestValidateLaunchCmdBackend_RejectsContradiction(t *testing.T) {
	var g GovernorConfig
	err := g.ValidateLaunchCmdBackend("copilot", "bob --model auto")
	if err == nil {
		t.Fatal("backend copilot + launch_cmd bob accepted — the #5921 contradiction must be rejected")
	}
	msg := err.Error()
	for _, want := range []string{"copilot", "bob"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should name %q so the operator sees both halves of the contradiction", msg, want)
		}
	}
}

func TestValidateLaunchCmdBackend_AcceptsAgreementAndNoOpinion(t *testing.T) {
	var g GovernorConfig
	cases := []struct{ backend, cmd string }{
		{"copilot", ""},                        // no custom launch_cmd
		{"", "bob --model auto"},               // hive-default backend
		{"bob", "bob --model auto"},            // agreement
		{"copilot", "/usr/bin/copilot --allow-all --model x"},
		{"copilot", "agent-launch.sh --backend copilot --model x"},
		{"copilot", "./my-wrapper.sh --anything"}, // unknown wrapper: no opinion
		{"Bob", "bob --model auto"},               // case-insensitive agreement
	}
	for _, tc := range cases {
		if err := g.ValidateLaunchCmdBackend(tc.backend, tc.cmd); err != nil {
			t.Errorf("ValidateLaunchCmdBackend(%q, %q) = %v, want nil", tc.backend, tc.cmd, err)
		}
	}
}

func TestValidateLaunchCmdBackend_InferenceAndGateways(t *testing.T) {
	g := GovernorConfig{
		Gateways: []GatewayConfig{
			{Name: "ibm-granite-prod", Kind: GatewayKindWatsonx, Endpoint: "https://us-south.ml.cloud.ibm.com/ml/gateway"},
		},
	}
	// Inference backends and gateway names run the claude CLI, so a claude
	// launch_cmd agrees with them...
	for _, backend := range []string{"watsonx", "litellm", "ibm-granite-prod"} {
		if err := g.ValidateLaunchCmdBackend(backend, "claude --dangerously-skip-permissions"); err != nil {
			t.Errorf("ValidateLaunchCmdBackend(%q, claude ...) = %v, want nil (they launch the claude CLI)", backend, err)
		}
		// ...but a launch_cmd launching a DIFFERENT known CLI is a contradiction.
		if err := g.ValidateLaunchCmdBackend(backend, "bob --model auto"); err == nil {
			t.Errorf("ValidateLaunchCmdBackend(%q, bob ...) = nil, want an error", backend)
		}
	}
}

// The contradiction must be caught by full-config validation — the gate every
// load and config write goes through — not only by direct helper calls.
func TestConfigValidateRejectsBackendLaunchCmdMismatch(t *testing.T) {
	c := &Config{}
	c.Project.Org = "example"
	c.GitHub.Token = "x"
	c.Agents = map[string]AgentConfig{
		"supervisor": {
			Backend:   "copilot",
			LaunchCmd: "bob --model auto",
		},
	}
	err := c.validate()
	if err == nil {
		t.Fatal("validate() accepted backend copilot + launch_cmd bob (#5921)")
	}
	if !strings.Contains(err.Error(), "supervisor") {
		t.Errorf("error %q should name the offending agent", err)
	}

	// The same config with an agreeing launch_cmd must validate.
	c.Agents["supervisor"] = AgentConfig{
		Backend:   "copilot",
		LaunchCmd: "agent-launch.sh --backend copilot --model claude-sonnet-4-6",
	}
	if err := c.validate(); err != nil {
		t.Errorf("validate() = %v, want nil for an agreeing backend/launch_cmd", err)
	}
}
