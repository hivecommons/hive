// The full-frame golden test lives HERE, next to the panes, because the
// design doc's testing convention (src/docs/design/tui.md §"Testing
// convention") puts rendering goldens under src/pkg/tui/panes/testdata/ and
// the T3 acceptance criteria name panes/testdata/grid.golden specifically.
// The frame itself is composed by pkg/tui, reached through its exported New —
// an external test package, so no import cycle.
//
// Regenerate after a DELIBERATE layout change with:
//
//	cd src && go test ./pkg/tui/panes/... -update
//
// and review the regenerated file in the diff like any other change — a
// golden file updated without reading it asserts nothing.
package panes_test

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/hivecommons/hive/pkg/tui"
	"github.com/hivecommons/hive/pkg/tui/client"
)

// splashFrame is the bytes bubbletea's renderer flushes for the bare
// pre-size line (see pkg/tui.splash: "Hive TUI (q to quit)"): a leading
// carriage return (written once, for what the renderer sees as the FIRST
// render), the line itself, and an erase-to-end-of-line. It deliberately
// excludes the carriage return that follows: that one belongs to the NEXT
// frame's own repaint — every frame after the very first supplies its own
// leading \r to return the cursor to column 0 — so leaving it in place
// means the sized grid frame that follows an extra splash flush keeps
// exactly the \r it would have wanted to write for itself either way.
const splashFrame = "\rHive TUI (q to quit)\x1b[K"

// TestGridGolden pins the complete 100x30 frame — header, all four bordered
// stub panes with the top-left one focused, and the footer — byte for byte.
// The size is pinned explicitly per the design doc: golden files are
// width-sensitive terminal output, and a test that inherited a default size
// would produce a diff on someone else's machine.
func TestGridGolden(t *testing.T) {
	// Pin the dashboard at a closed port. T12 made the app poll on startup, so
	// an unpinned golden would render whatever a dashboard on localhost:3001
	// returned — and a hive developer's machine is exactly where one is running.
	// A refused connection produces a swallowed fetch error and no visible
	// change, which is what keeps this frame the same everywhere.
	t.Setenv(client.BaseURLEnv, "http://127.0.0.1:1")

	tm := teatest.NewTestModel(t, tui.New(), teatest.WithInitialTermSize(100, 30))

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	tm.WaitFinished(t, teatest.WithFinalTimeout(5*time.Second))

	out, err := io.ReadAll(tm.FinalOutput(t, teatest.WithFinalTimeout(5*time.Second)))
	if err != nil {
		t.Fatalf("read final output: %v", err)
	}
	requireGolden(t, stripSplashRace(out), filepath.Join("testdata", "grid.golden"))
}

// stripSplashRace removes an extra splash-frame flush from the captured
// output, if one is present.
//
// WHY THIS EXISTS. bubbletea renders the model's pristine (unsized) View()
// once, synchronously, before its event loop starts draining Send'd
// messages (see (*tea.Program).Run: renderer.write(model.View()) happens
// before input is subscribed). This package's View() falls back to the bare
// splash line until width/height are set (pkg/tui/app.go). The renderer
// then flushes its buffer on its own ~16ms ticker, independently of when
// Update actually processes the WindowSizeMsg that NewTestModel queues
// immediately: if that ticker fires before WindowSizeMsg is processed, the
// splash gets flushed to the wire as ITS OWN frame before the sized grid
// frame overwrites it in place.
//
// That extra flush cannot be avoided by waiting longer before sending "q":
// the model's FINAL state is always sized (WindowSizeMsg is queued, and
// therefore processed, strictly before "q" in every run), but the bytes of
// an already-flushed transient frame are already committed to the captured
// stream and no later wait un-writes them. The race is in scheduling
// between bubbletea's internal renderer goroutine and its event-loop
// goroutine — outside this test's control — so the fix normalizes the
// captured stream instead of trying to dodge the race: an extra splash
// flush is a scheduling artifact of the FIRST frame, never a layout change
// (a layout change would alter the SIZED frame the golden actually pins),
// so it is safe to strip and every other byte is compared unchanged.
func stripSplashRace(out []byte) []byte {
	return bytes.ReplaceAll(out, []byte(splashFrame), nil)
}

// requireGolden is golden.RequireEqual with the file name fixed to the path
// the T3 acceptance criteria specify (testdata/grid.golden) instead of the
// package's tb.Name()-derived default, which would be TestGridGolden.golden.
// It honours the SAME -update flag: teatest's import of x/exp/golden has
// already registered it, so the documented regeneration command
// (`go test ./pkg/tui/panes/... -update`) drives this test too, and no second
// flag definition can collide with the panes' future per-pane goldens.
func requireGolden(t *testing.T, out []byte, path string) {
	t.Helper()
	if f := flag.Lookup("update"); f != nil {
		if getter, ok := f.Value.(flag.Getter); ok {
			if update, ok := getter.Get().(bool); ok && update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, out, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden file (regenerate with -update): %v", err)
	}
	if !bytes.Equal(out, want) {
		t.Fatalf("output does not match %s (regenerate with -update after a DELIBERATE layout change and review the diff)\ngot %d bytes, want %d", path, len(out), len(want))
	}
}
