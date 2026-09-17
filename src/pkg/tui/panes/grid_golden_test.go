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
	"regexp"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/hivecommons/hive/pkg/tui"
	"github.com/hivecommons/hive/pkg/tui/client"
)

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

	// Size the model before bubbletea starts so its first View() is already
	// the golden frame. NewTestModel also queues the same WindowSizeMsg for the
	// renderer, but relying on that message left a scheduling race where the
	// renderer could flush the unsized splash frame first.
	m := tui.New()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(100, 30))

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	tm.WaitFinished(t, teatest.WithFinalTimeout(5*time.Second))

	out, err := io.ReadAll(tm.FinalOutput(t, teatest.WithFinalTimeout(5*time.Second)))
	if err != nil {
		t.Fatalf("read final output: %v", err)
	}
	requireGolden(t, lastFrame(out), filepath.Join("testdata", "grid.golden"))
}

// repaintBoundary matches the cursor-up sequence bubbletea's standard renderer
// emits to rewind over the previous frame before repainting it.
var repaintBoundary = regexp.MustCompile(`\x1b\[[0-9]+A`)

// terminalPrologue matches the private-mode sequences bubbletea writes once at
// startup (hide cursor, enable bracketed paste). They precede the first frame
// and are never re-emitted, so they must be carried across when superseded
// frames are dropped.
var terminalPrologue = regexp.MustCompile(`\A(?:\x1b\[\?[0-9]+[hl])+`)

// lastFrame reduces a captured teatest byte stream to the prologue plus the
// FINAL rendered frame, discarding any earlier frames the renderer repainted
// over.
//
// Why this exists (#7264): the golden used to be compared against the raw
// stream, whose LENGTH is timing-dependent. The model is pre-sized before
// bubbletea starts, but teatest.WithInitialTermSize queues a second
// WindowSizeMsg through the program, and the standard renderer does an
// unconditional full repaint on a window-size message -- it drops its line
// cache -- even when the content is byte-identical. Whether the quit key is
// processed before or after that queued repaint is a scheduling race, so a
// loaded CI shard captured two copies of the same frame and failed a test on
// a PR that never touched this package.
//
// Dropping WithInitialTermSize would be the narrower fix, but it would
// reintroduce the race it was added to solve (the renderer flushing the
// unsized splash frame first), so the assertion is made frame-based instead:
// however many repaints occur, only the frame the user would actually be
// looking at is compared. The frame CONTENT was never the flaky part.
//
// A single-frame stream contains no repaint boundary, so this is a no-op on
// one -- which keeps the committed golden and this function idempotent with
// respect to each other.
func lastFrame(stream []byte) []byte {
	boundaries := repaintBoundary.FindAllIndex(stream, -1)
	if len(boundaries) == 0 {
		return stream
	}
	last := boundaries[len(boundaries)-1]
	prologue := terminalPrologue.Find(stream)
	out := make([]byte, 0, len(prologue)+len(stream)-last[1])
	out = append(out, prologue...)
	return append(out, stream[last[1]:]...)
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
