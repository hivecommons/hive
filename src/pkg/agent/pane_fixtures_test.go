package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// paneFixturesDir points at the SHARED golden fixtures also read by
// bin/contributor-relay.test.js (kubestellar/hive#6427). Reading the same
// files from both languages, instead of maintaining two independently
// written fixture sets, is what stops the JS and Go pane-tail/classifier
// implementations from silently drifting apart again the way paneTail (JS)
// and paneTail (Go) already had: JS kept a blank-including tail alongside a
// second `paneTailNonBlank` variant, while Go's paneTail always filtered
// blanks — the direct root cause of kubestellar/hive#6413.
//
// The path is relative to this package directory (src/pkg/agent), three
// levels up to the repo root, then into bin/testdata/pane-fixtures.
const paneFixturesDir = "../../../bin/testdata/pane-fixtures"

// paneFixtureSidecar mirrors the <name>.json sidecar written alongside each
// <name>.pane.txt / <name>.tail.txt pair. Only the fields this test actually
// asserts on are declared; the JS test reads the same files and asserts on a
// broader set (getCLIState, classifyTmuxPane, …) that has no Go equivalent.
type paneFixtureSidecar struct {
	Backend   string `json:"backend"`
	TailLines int    `json:"tailLines"`
	Expect    struct {
		PaneShowsBlockingPrompt     *bool `json:"paneShowsBlockingPrompt"`
		PaneShowsTransientAPIError  *bool `json:"paneShowsTransientAPIError"`
		PaneShowsUnretryableAPIErr  *bool `json:"paneShowsUnretryableAPIError"`
		PaneShowsLoginRequiredError *bool `json:"paneShowsLoginRequiredError"`
	} `json:"expect"`
	Note string `json:"note"`
}

// paneFixtureNames lists the fixtures by reading every *.pane.txt in
// paneFixturesDir, mirroring loadPaneFixtures() in contributor-relay.test.js.
func paneFixtureNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(paneFixturesDir)
	if err != nil {
		t.Skipf("shared pane fixtures directory %s is absent or unreadable (%v); skipping cross-language fixture checks", paneFixturesDir, err)
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".pane.txt") {
			names = append(names, strings.TrimSuffix(e.Name(), ".pane.txt"))
		}
	}
	if len(names) == 0 {
		t.Skipf("no *.pane.txt fixtures found under %s; skipping cross-language fixture checks", paneFixturesDir)
	}
	return names
}

func loadPaneFixture(t *testing.T, name string) (pane, wantTail string, sidecar paneFixtureSidecar) {
	t.Helper()
	paneBytes, err := os.ReadFile(filepath.Join(paneFixturesDir, name+".pane.txt"))
	if err != nil {
		t.Fatalf("reading %s.pane.txt: %v", name, err)
	}
	tailBytes, err := os.ReadFile(filepath.Join(paneFixturesDir, name+".tail.txt"))
	if err != nil {
		t.Fatalf("reading %s.tail.txt: %v", name, err)
	}
	jsonBytes, err := os.ReadFile(filepath.Join(paneFixturesDir, name+".json"))
	if err != nil {
		t.Fatalf("reading %s.json: %v", name, err)
	}
	if err := json.Unmarshal(jsonBytes, &sidecar); err != nil {
		t.Fatalf("parsing %s.json: %v", name, err)
	}
	return string(paneBytes), string(tailBytes), sidecar
}

// TestPaneTail_MatchesSharedGoldenFixtures is the core contract this issue
// asks for: Go's paneTail (manager.go) must return byte-for-byte the same
// last-n-non-blank-lines window as JS's paneTail (contributor-relay.sh) does
// over the identical capture, for every shared fixture — including the
// #6413 shapes (an inline-rendering CLI whose real content sits far above a
// pane's blank-padded bottom, and a stale marker buried above newer non-blank
// output).
func TestPaneTail_MatchesSharedGoldenFixtures(t *testing.T) {
	for _, name := range paneFixtureNames(t) {
		name := name
		t.Run(name, func(t *testing.T) {
			pane, wantTail, sidecar := loadPaneFixture(t, name)
			got := paneTail(pane, sidecar.TailLines)
			if got != wantTail {
				t.Errorf("paneTail(pane, %d) diverged from the golden tail.txt shared with the JS test — the exact regression this fixture set exists to catch\nnote: %s\ngot:\n%s\nwant:\n%s",
					sidecar.TailLines, sidecar.Note, got, wantTail)
			}
		})
	}
}

// TestPaneClassifiers_AgreeWithSharedGoldenFixtures exercises the Go
// classifiers whose JS counterpart shares both a name and an intent —
// PaneShowsBlockingPrompt vs. classifyBlockedOnHumanReason's onboarding
// gates, and paneShowsTransientAPIError, which is defined identically in
// both languages (same tail-lines constant: transientAPIErrorTailLines == 12
// == JS's TRANSIENT_API_ERROR_TAIL_LINES) — against the sidecar's expected
// verdict, so a fixture that gets ONE cross-language answer in JS cannot
// silently get a DIFFERENT one in Go.
func TestPaneClassifiers_AgreeWithSharedGoldenFixtures(t *testing.T) {
	for _, name := range paneFixtureNames(t) {
		name := name
		t.Run(name, func(t *testing.T) {
			pane, _, sidecar := loadPaneFixture(t, name)

			if want := sidecar.Expect.PaneShowsBlockingPrompt; want != nil {
				got := PaneShowsBlockingPrompt(sidecar.Backend, pane)
				if got != *want {
					t.Errorf("PaneShowsBlockingPrompt(%q, pane) = %v, want %v\nnote: %s", sidecar.Backend, got, *want, sidecar.Note)
				}
			}

			if want := sidecar.Expect.PaneShowsTransientAPIError; want != nil {
				tail := paneTail(pane, transientAPIErrorTailLines)
				got := paneShowsTransientAPIError(strings.Split(tail, "\n"))
				if got != *want {
					t.Errorf("paneShowsTransientAPIError(paneTail(pane, %d)) = %v, want %v\nnote: %s", transientAPIErrorTailLines, got, *want, sidecar.Note)
				}
			}
		})
	}
}
