package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParallelSessionsDeclaration(t *testing.T) {
	for _, tc := range []struct{ input, want int }{{-1, 0}, {0, 0}, {1, 1}, {3, 3}, {64, 64}, {1000, 64}} {
		caps := (ContributorCapabilities{ParallelSessions: tc.input}).Sanitized()
		if caps.ParallelSessions != tc.want {
			t.Fatalf("Sanitized(%d) = %d, want %d", tc.input, caps.ParallelSessions, tc.want)
		}
	}
	var msg WSMessage
	if err := json.Unmarshal([]byte(`{"type":"auth_response","capabilities":{"parallel_sessions":3}}`), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Capabilities == nil || msg.Capabilities.Sanitized().ParallelSessions != 3 {
		t.Fatalf("declaration lost: %+v", msg.Capabilities)
	}
	body, err := json.Marshal(ContributorCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "parallel_sessions") {
		t.Fatalf("legacy wire shape changed: %s", body)
	}
	// Declaration alone must not advertise server-side fix-pr dispatch support.
	for _, cap := range serverCapabilities() {
		if cap == "fix-pr" {
			t.Fatal("unfinished fix-pr dispatch advertised")
		}
	}
}
