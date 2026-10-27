package hub

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestWarnGitHubFetchOnceDemotesRepeats(t *testing.T) {
	githubWarnMu.Lock()
	githubWarnLast = map[string]time.Time{}
	githubWarnMu.Unlock()
	now := time.Now()
	githubWarnNow = func() time.Time { return now }
	t.Cleanup(func() { githubWarnNow = time.Now })

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	for i := 0; i < 6; i++ {
		warnGitHubFetchOnce(logger, "k1", "boom", "status", 403)
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Fatalf("warns for one key = %d, want 1", n)
	}

	warnGitHubFetchOnce(logger, "k2", "boom", "status", 403)
	if n := strings.Count(buf.String(), "level=WARN"); n != 2 {
		t.Fatalf("distinct key should warn; total = %d, want 2", n)
	}

	now = now.Add(githubWarnInterval + time.Second)
	warnGitHubFetchOnce(logger, "k1", "boom", "status", 403)
	if n := strings.Count(buf.String(), "level=WARN"); n != 3 {
		t.Fatalf("warn after interval; total = %d, want 3", n)
	}
}
