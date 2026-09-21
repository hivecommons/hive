package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The standby floor has no write API, and this file is the reason that is a
// property rather than a hope.
//
// src/docs/design/standby-contributors.md opens with the one thing the whole
// design exists to protect: an owner watching a stuck queue must not be nudged
// into lowering `min_model_capability` until something qualifies. Three
// mechanisms carry it, and the first is that the floor is readable everywhere
// and writable only by editing hive.yaml — no dashboard control, no PUT field,
// no chat command. The design is explicit about why that is the *strongest* of
// the three: "A rule about what the UI says can be violated by a well-meant
// copy change; an absent endpoint cannot."
//
// So the guard is structural. Every route, template and surface outside
// pkg/config is scanned for the field, in both the Go spelling and the wire
// spelling. S4 adds the paused-lane tile that renders the floor; this test is
// what makes the tile a reader. If a later phase legitimately needs to name
// the wire key on a read-only path, the fix is to widen this test with a
// documented exception — deliberately, in review — not to quietly add a
// setter.

// standbyFloorGoField / standbyFloorWireKey are the two spellings a writer
// would have to use. The Go field is what an assignment targets; the wire key
// is what a JSON request body or an HTML form control would have to name.
const (
	standbyFloorGoField = "MinModelCapability"
	standbyFloorWireKey = "min_model_capability"
)

// standbyFloorScanRoots are the trees a surface could live in, relative to
// this package. src/ is the Go module (every route, CLI and TUI surface);
// dashboard/ is the browser client the hub serves.
var standbyFloorScanRoots = []string{"../..", "../../../dashboard"}

// standbyFloorReaderExceptions names paths permitted to mention the field.
// pkg/config OWNS it — the schema, the defaults pass and the validator are the
// only writers by design — and this test file names both spellings itself.
var standbyFloorReaderExceptions = []string{
	"pkg/config/",
}

func TestStandbyFloorHasNoWriterOutsideConfig(t *testing.T) {
	var offenders []string
	scanned := 0

	for _, root := range standbyFloorScanRoots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// A missing tree is a checkout-shape problem, not a finding;
				// the "did this scan anything" assertion below catches a scan
				// that walked nothing at all.
				return nil
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "node_modules", "vendor", "testdata":
					return filepath.SkipDir
				}
				return nil
			}
			if !standbyFloorScannableFile(path) {
				return nil
			}
			rel := filepath.ToSlash(path)
			for _, skip := range standbyFloorReaderExceptions {
				if strings.Contains(rel, skip) {
					return nil
				}
			}
			data, readErr := os.ReadFile(path) // #nosec G304 -- path comes from walking a fixed in-repo tree
			if readErr != nil {
				return nil
			}
			scanned++
			body := string(data)
			if strings.Contains(body, standbyFloorWireKey) {
				offenders = append(offenders, rel+": mentions the wire key "+standbyFloorWireKey)
			}
			if line, ok := standbyFloorAssignment(body); ok {
				offenders = append(offenders, rel+": assigns "+standbyFloorGoField+" — "+line)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}

	// A guard that scanned nothing passes for the wrong reason.
	if scanned < 100 {
		t.Fatalf("scanned only %d files; the scan roots %v did not resolve to the repository trees", scanned, standbyFloorScanRoots)
	}

	if len(offenders) > 0 {
		t.Fatalf("the standby model floor must stay readable-only outside pkg/config — lowering it is an edit to hive.yaml, never a control:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// The guard must not be vacuous: pkg/config really does own the field, so the
// exception it carves out is load-bearing. If this fails, the field was
// renamed and the scan above is looking for a string that no longer exists.
func TestStandbyFloorIsOwnedByConfig(t *testing.T) {
	data, err := os.ReadFile("standby.go")
	if err != nil {
		t.Fatalf("reading standby.go: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, standbyFloorGoField) {
		t.Errorf("pkg/config/standby.go does not mention %s; the no-writer guard is scanning for a dead string", standbyFloorGoField)
	}
	if !strings.Contains(body, standbyFloorWireKey) {
		t.Errorf("pkg/config/standby.go does not mention %s; the no-writer guard is scanning for a dead string", standbyFloorWireKey)
	}
	// And the defaults pass is the writer the exception exists for.
	if _, ok := standbyFloorAssignment(body); !ok {
		t.Errorf("pkg/config/standby.go assigns %s nowhere; the config path is supposed to be the ONLY writer", standbyFloorGoField)
	}
}

func standbyFloorScannableFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go", ".js", ".mjs", ".ts", ".html", ".htm", ".json":
		return true
	}
	return false
}

// standbyFloorAssignment reports whether body contains an assignment to the
// floor field, and returns the offending line. It looks for the field followed
// by "=" (but not "==" or "!="), which is what a setter on any surface would
// have to write.
func standbyFloorAssignment(body string) (string, bool) {
	for _, line := range strings.Split(body, "\n") {
		idx := strings.Index(line, standbyFloorGoField)
		if idx < 0 {
			continue
		}
		rest := strings.TrimSpace(line[idx+len(standbyFloorGoField):])
		if !strings.HasPrefix(rest, "=") && !strings.HasPrefix(rest, ":=") {
			continue
		}
		if strings.HasPrefix(rest, "==") {
			continue
		}
		return strings.TrimSpace(line), true
	}
	return "", false
}
