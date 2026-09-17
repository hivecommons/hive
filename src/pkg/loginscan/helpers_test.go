package loginscan

import (
	"io"
	"log/slog"
)

// testLogger discards detector output: these tests assert on decisions and on
// the manager/notifier calls, not on log text.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
