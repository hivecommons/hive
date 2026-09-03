package turn

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/toolapprove"
)

// fakeToken is a credential-shaped string matching logscrub's GitHub token
// pattern. It is not a real credential.
const fakeToken = "ghp_FAKE0000000000000000000000000000abcd"

func secretBearingEnvelope() SessionEnvelope {
	env := SessionEnvelope{
		SessionID: "sess-scrub-1",
		Agent:     toolapprove.AgentIdentity{Name: "scrubber"},
		ACMMLevel: 4,
		Variables: map[string]string{"gh_auth": "token " + fakeToken},
		PendingApprovals: []PendingApproval{
			{
				ToolCall: ToolCall{
					ID:        "c9",
					Name:      "bash",
					Arguments: `{"command":"export GH_TOKEN=` + fakeToken + `"}`,
				},
				Verdict: toolapprove.Verdict{
					Decision:  toolapprove.DecisionOperatorApprove,
					Rationale: "uses " + fakeToken,
				},
			},
		},
	}
	env.AddMessage(RoleUser, "my token is "+fakeToken+", please use it")
	env.AddToolCallMessage("calling tool", []ToolCall{
		{ID: "c1", Name: "bash", Arguments: `{"command":"echo ` + fakeToken + `"}`},
	})
	env.AddToolResultMessage("c1", "bash", fakeToken, "leaked "+fakeToken)
	return env
}

// TestToJSONScrubsSecretsOnPersist asserts the scrub-on-persist invariant from
// the #4002 maintainer review: the serialized (durable, handoff-able) form of
// the conversation must never contain credential-shaped substrings, in any
// content-bearing field.
func TestToJSONScrubsSecretsOnPersist(t *testing.T) {
	env := secretBearingEnvelope()

	// Positive control: the fixture genuinely carries the secret in raw form.
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("raw marshal: %v", err)
	}
	if !strings.Contains(string(raw), fakeToken) {
		t.Fatal("positive control failed: fixture does not contain the secret; the scrub assertions below would pass vacuously")
	}

	for name, serialize := range map[string]func() ([]byte, error){
		"ToJSON":       env.ToJSON,
		"ToPrettyJSON": env.ToPrettyJSON,
	} {
		payload, err := serialize()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(string(payload), fakeToken) {
			t.Errorf("%s: persisted envelope contains unscrubbed secret", name)
		}
		if !strings.Contains(string(payload), "[REDACTED]") {
			t.Errorf("%s: expected redaction marker in persisted envelope", name)
		}
	}

	// The scrubbed payload must still round-trip as a valid envelope.
	payload, err := env.ToJSON()
	if err != nil {
		t.Fatalf("ToJSON: %v", err)
	}
	restored, err := ParseEnvelope(payload)
	if err != nil {
		t.Fatalf("ParseEnvelope on scrubbed payload: %v", err)
	}
	if restored.SessionID != env.SessionID || len(restored.Messages) != len(env.Messages) {
		t.Errorf("scrubbed round-trip lost structure: %+v", restored)
	}
}

// TestCloneDoesNotScrub asserts that in-memory state used by Step is left
// intact: scrubbing applies only at the persistence boundary.
func TestCloneDoesNotScrub(t *testing.T) {
	env := secretBearingEnvelope()
	clone := env.Clone()
	found := false
	for _, m := range clone.Messages {
		if strings.Contains(m.Content, fakeToken) {
			found = true
		}
	}
	if !found {
		t.Error("Clone scrubbed message content; in-memory state must be unmodified")
	}
	if !strings.Contains(clone.Variables["gh_auth"], fakeToken) {
		t.Error("Clone scrubbed variables; in-memory state must be unmodified")
	}
}
