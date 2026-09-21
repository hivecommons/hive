package github

// Error-path pin for openTokenAccessLog: when the durable log cannot be
// opened, the error must propagate so PrepareTokenAccessAudit can warn —
// a silently nil log would drop the whole token-access trail.

import (
	"path/filepath"
	"testing"
)

func TestOpenTokenAccessLogFailsWhenDirMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "token-access.log")
	f, err := openTokenAccessLog(path)
	if err == nil {
		_ = f.Close()
		t.Fatal("expected error opening log in a missing directory")
	}
	if f != nil {
		t.Fatal("file handle must be nil on error")
	}
}
