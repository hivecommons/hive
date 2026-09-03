package panes_test

import (
	"io"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/hivecommons/hive/pkg/tui"
	"github.com/hivecommons/hive/pkg/tui/client"
)

// TestTooSmallGolden pins the complete below-minimum frame (T24) byte for
// byte: the message, centred, and nothing else — no header, no borders, no
// footer.
//
// It sits beside the grid golden and shares its machinery for the same
// reasons. The size is pinned because golden files are width-sensitive
// terminal output; 50x12 is comfortably under the 60x20 floor in both
// dimensions and wide enough that the 40-cell message fits on one line, so the
// file stays readable in a diff. The dashboard is pinned at a closed port so
// the poll T12 issues on startup cannot make this frame depend on whether a
// hive is running on the developer's machine.
//
// Regenerate exactly like the grid golden:
//
//	cd src && go test ./pkg/tui/panes/... -update
func TestTooSmallGolden(t *testing.T) {
	t.Setenv(client.BaseURLEnv, "http://127.0.0.1:1")

	tm := teatest.NewTestModel(t, tui.New(), teatest.WithInitialTermSize(50, 12))

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	tm.WaitFinished(t, teatest.WithFinalTimeout(5*time.Second))

	out, err := io.ReadAll(tm.FinalOutput(t, teatest.WithFinalTimeout(5*time.Second)))
	if err != nil {
		t.Fatalf("read final output: %v", err)
	}
	// Same splash-frame normalization as the grid golden: the pre-size frame
	// can be flushed as its own frame depending on how bubbletea's renderer
	// ticker interleaves with its event loop. See stripSplashRace.
	requireGolden(t, stripSplashRace(out), filepath.Join("testdata", "too_small.golden"))
}
