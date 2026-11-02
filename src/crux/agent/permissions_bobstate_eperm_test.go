package agent

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"testing"
)

// A permanent EPERM whose text embeds a changing /proc/self/fd/N path must log
// once at WARN (never ERROR) and demote repeats to DEBUG.
func TestBobStateChmodEPERMLoggedOnceAtWarn(t *testing.T) {
	resetPermWarnDedupe()
	t.Cleanup(resetPermWarnDedupe)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	for fd := 16; fd < 21; fd++ {
		chmod := func(os.FileMode) error {
			return &os.PathError{Op: "chmod", Path: fmt.Sprintf("/proc/self/fd/%d", fd), Err: syscall.EPERM}
		}
		fixBobStateDirGroupWrite("/x/.bob", 0o755|os.ModeDir, chmod, logger)
	}
	out := buf.String()
	if strings.Contains(out, "level=ERROR") {
		t.Errorf("must not log at ERROR:\n%s", out)
	}
	if n := strings.Count(out, "chmod bob state dir failed"); n != 1 {
		t.Errorf("want exactly 1 WARN, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "hint=") {
		t.Errorf("missing ownership hint:\n%s", out)
	}
}

func TestBobStateChmodSkippedWhenNotOwner(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can chmod any dir")
	}
	resetPermWarnDedupe()
	t.Cleanup(resetPermWarnDedupe)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	called := false
	chmod := func(os.FileMode) error { called = true; return nil }
	other := uint32(os.Geteuid()) + 1
	fixBobStateDirGroupWrite("/x/.bob", 0o755|os.ModeDir, chmod, logger, other)
	if called {
		t.Error("chmod attempted on a dir owned by another uid")
	}
	if strings.Contains(buf.String(), "level=ERROR") || !strings.Contains(buf.String(), "owner_uid") {
		t.Errorf("unexpected log:\n%s", buf.String())
	}
}
