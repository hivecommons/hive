package dashboard

import (
	"bytes"
	"errors"
	"log/slog"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestOverviewEncodeLogLevels14696(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	slogWarnOverviewEncode(&net.OpError{Op: "write", Err: os.NewSyscallError("write", syscall.EPIPE)})
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("client disconnect must not log WARN, got:\n%s", buf.String())
	}

	buf.Reset()
	slogWarnOverviewEncode(errors.New("boom"))
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("genuine encode failure should log WARN, got:\n%s", buf.String())
	}
}
