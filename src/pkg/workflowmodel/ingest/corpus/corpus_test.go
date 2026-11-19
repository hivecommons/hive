package corpus

import (
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestProjectsAndRepos(t *testing.T) {
	ps := Projects()
	if len(ps) < 200 {
		t.Fatalf("expected the full CNCF corpus, got %d projects", len(ps))
	}
	if !sort.SliceIsSorted(ps, func(i, j int) bool {
		return strings.ToLower(ps[i].Name) < strings.ToLower(ps[j].Name)
	}) {
		t.Fatal("projects not sorted by name")
	}
	repos := Repos()
	if !sort.StringsAreSorted(repos) {
		t.Fatal("repos not sorted")
	}
	seen := map[string]bool{}
	for _, r := range repos {
		if r != strings.ToLower(r) || !strings.Contains(r, "/") {
			t.Fatalf("bad repo entry %q", r)
		}
		if seen[r] {
			t.Fatalf("duplicate repo %q", r)
		}
		seen[r] = true
	}
	if !seen["project-akri/akri"] {
		t.Fatal("expected project-akri/akri in corpus")
	}
	m := Maturity()
	if m["project-akri/akri"] != "sandbox" {
		t.Fatalf("maturity = %q", m["project-akri/akri"])
	}
	if !seen["cncf/techdocs"] {
		t.Fatal("expected the extra.json repo cncf/techdocs in Repos()")
	}
	xs := Extras()
	if len(xs) == 0 || xs[0].Repos[0] != "cncf/techdocs" || xs[0].Why == "" {
		t.Fatalf("extras = %+v", xs)
	}
	xs[0].Repos[0] = "tampered"
	if Extras()[0].Repos[0] == "tampered" {
		t.Fatal("Extras leaked internal slice")
	}
	if _, ok := m["cncf/techdocs"]; ok {
		t.Fatal("extras must not carry a maturity")
	}
	// Projects() hands out copies.
	ps[0].Repos[0] = "tampered"
	if Projects()[0].Repos[0] == "tampered" {
		t.Fatal("Projects leaked internal slice")
	}
}

func TestLoadInvalid(t *testing.T) {
	reset := func() { loadOnce = sync.Once{}; loaded, loadedExtra, loadErr = nil, nil, nil }
	oldCorpus, oldExtra := corpusJSON, extraJSON
	t.Cleanup(func() { corpusJSON, extraJSON = oldCorpus, oldExtra; reset() })
	expectPanic := func(t *testing.T, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic on invalid embedded corpus")
			}
		}()
		f()
	}
	t.Run("corpus.json", func(t *testing.T) {
		reset()
		corpusJSON = []byte("{not json")
		expectPanic(t, func() { Projects() })
	})
	t.Run("extra.json", func(t *testing.T) {
		reset()
		corpusJSON = oldCorpus
		extraJSON = []byte("{not json")
		expectPanic(t, func() { Extras() })
	})
}
