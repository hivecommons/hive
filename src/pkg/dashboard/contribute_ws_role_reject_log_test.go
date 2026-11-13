package dashboard

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// A client retrying a forbidden role must log once per (user, role), at Info.
func TestLogRoleClaimRejectedDedupes(t *testing.T) {
	var buf bytes.Buffer
	h := &ContributeWSHub{logger: slog.New(slog.NewTextHandler(&buf, nil))}
	for i := 0; i < 50; i++ {
		h.logRoleClaimRejected("alice", "advisor", "scanner", "nope")
	}
	h.logRoleClaimRejected("alice", "advisor", "quality", "nope")
	out := buf.String()
	if n := strings.Count(out, "agent role claim rejected"); n != 2 {
		t.Fatalf("got %d log lines, want 2:\n%s", n, out)
	}
	if strings.Contains(out, "level=WARN") {
		t.Fatalf("expected rejection must not log at WARN:\n%s", out)
	}
}
