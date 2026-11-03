package policies

import (
	"bytes"
	"io/fs"
	"path"
	"strings"
	"testing"
)

// Every shipped policy that carries the three-of-a-class rule (#13428) must
// count a class across every repository the lane serves (#13680) and treat a
// ruling on a gate proposal as the written policy for that class, so a
// declined proposal ends the class instead of restarting per-finding filing
// (#13679). The fragments are shared by the long and the short
// (scanner-automerge) wording, so no lane drifts to its own phrasing.
func TestThreeOfAClassPoliciesHonourGateProposalRulings(t *testing.T) {
	t.Parallel()

	marker := []byte("**Three of a class means the gate is missing.**")
	required := []struct {
		fragment string
		why      string
	}{
		{"every other repository in `$HIVE_REPOS`", "the class count must follow the lane across repositories (#13680)"},
		{"search every repository in `$HIVE_REPOS` for a `Gate proposal:` on", "one proposal per class across the hive, not one per repository (#13680)"},
		{"A ruling on a proposal is the written policy for its class", "a ruling must bind the class in every repository the proposal lists (#13679, #13680)"},
		{"Resume individual findings in a declined class only if the ruling", "a decline ends the class unless the ruling asks for individual findings (#13679)"},
		{"An accepted proposal", "an accepted ruling must reach repositories the proposal did not list, or the owner is asked again per repository"},
		{"also settles the class in every other repository in `$HIVE_REPOS`", "an accepted ruling must reach repositories the proposal did not list, or the owner is asked again per repository"},
		{"Decision taken by hive: applying the ruling on <owner/repo#n>", "work filed on an accepted ruling must say which ruling it applies"},
	}
	forbidden := []struct {
		fragment string
		why      string
	}{
		{"only if the proposal is declined", "resuming on any decline restarts the stream the proposal stopped (#13679)"},
		{"already filed in this repository (", "a per-repository lookback never trips on a multi-repo hive (#13680)"},
		{"Check what you already filed in this repository (", "a per-repository lookback never trips on a multi-repo hive (#13680)"},
	}

	entries, err := fs.ReadDir(DefaultPolicies, "defaults")
	if err != nil {
		t.Fatalf("read embedded defaults: %v", err)
	}
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := DefaultPolicies.ReadFile(path.Join("defaults", e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if !bytes.Contains(data, marker) {
			continue
		}
		checked++
		for _, r := range required {
			if !bytes.Contains(data, []byte(r.fragment)) {
				t.Errorf("%s lacks %q: %s", e.Name(), r.fragment, r.why)
			}
		}
		for _, f := range forbidden {
			if bytes.Contains(data, []byte(f.fragment)) {
				t.Errorf("%s still contains %q: %s", e.Name(), f.fragment, f.why)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no policy carries the three-of-a-class rule; the guard would pass vacuously")
	}
}
