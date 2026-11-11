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
	// Projects() hands out copies.
	ps[0].Repos[0] = "tampered"
	if Projects()[0].Repos[0] == "tampered" {
		t.Fatal("Projects leaked internal slice")
	}
}

func TestLoadInvalid(t *testing.T) {
	old := corpusJSON
	t.Cleanup(func() { corpusJSON = old; loadOnce = sync.Once{}; loaded, loadErr = nil, nil })
	loadOnce = sync.Once{}
	loaded, loadErr = nil, nil
	corpusJSON = []byte("{not json")
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on invalid embedded corpus")
		}
	}()
	Projects()
}
