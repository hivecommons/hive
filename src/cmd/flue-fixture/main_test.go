package main

import (
	"bytes"
	"context"
	"flag"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/extwork/flue/fixture"
)

const (
	workflowDir  = "../../pkg/extwork/flue/testdata/flue-fixture"
	startTimeout = 5 * time.Second
	childEnv     = "FLUE_FIXTURE_MAIN_CHILD"
)

// syncBuffer is a bytes.Buffer safe for a writer goroutine and a reading
// test; run writes the address line from inside the fixture.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRunPrintsAddress drives run in-process: the fixture starts, prints its
// address line, and exits cleanly when the context ends.
func TestRunPrintsAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var out, errOut syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"-workflow", workflowDir, "-state", t.TempDir()}, &out, &errOut, func() (context.Context, context.CancelFunc) { return ctx, cancel })
	}()
	deadline := time.Now().Add(startTimeout)
	for !strings.Contains(out.String(), fixture.AddrLinePrefix) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code %d: %s", code, errOut.String())
		}
	case <-time.After(startTimeout):
		t.Fatal("run did not return after cancel")
	}
	if !strings.Contains(out.String(), fixture.AddrLinePrefix+"http://127.0.0.1:") {
		t.Fatalf("no address line in %q", out.String())
	}
	if code := run(nil, &out, &errOut, signalContext); code != 2 {
		t.Fatalf("missing -workflow exit code = %d", code)
	}
}

// TestMainBinaryExits runs the real main through a helper process: it must
// start, print its address, and shut down on SIGINT.
func TestMainBinaryExits(t *testing.T) {
	if os.Getenv(childEnv) == "1" {
		// The test runner consumed its own flags; what follows "--" is the
		// fixture's argv.
		os.Args = append([]string{os.Args[0]}, flag.Args()...)
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainBinaryExits$", "--", "-workflow", workflowDir)
	cmd.Env = append(os.Environ(), childEnv+"=1")
	cmd.WaitDelay = startTimeout
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 256)
	n, _ := stdout.Read(buf)
	if !strings.Contains(string(buf[:n]), fixture.AddrLinePrefix) {
		_ = cmd.Process.Kill()
		t.Fatalf("child did not print an address: %q", buf[:n])
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child exit: %v", err)
	}
}
