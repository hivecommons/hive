package knowledge

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func newTestGraphStore(t *testing.T) *GraphStore {
	t.Helper()
	dir := t.TempDir()
	gs, err := NewGraphStore(filepath.Join(dir, "test.db"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gs.Close() })
	return gs
}

func TestGraphStore_AddAndQuery(t *testing.T) {
	gs := newTestGraphStore(t)

	if err := gs.AddTriple(Triple{Subject: "auth-flow", Predicate: PredicateRelatedTo, Object: "oauth-scopes"}); err != nil {
		t.Fatal(err)
	}
	if err := gs.AddTriple(Triple{Subject: "auth-flow", Predicate: PredicateRelatedTo, Object: "token-refresh"}); err != nil {
		t.Fatal(err)
	}
	if err := gs.AddTriple(Triple{Subject: "deploy-patterns", Predicate: PredicateSupersedes, Object: "old-deploy"}); err != nil {
		t.Fatal(err)
	}

	if gs.Count() != 3 {
		t.Fatalf("expected 3 triples, got %d", gs.Count())
	}

	out := gs.Outgoing("auth-flow")
	if len(out) != 2 {
		t.Fatalf("expected 2 outgoing from auth-flow, got %d", len(out))
	}

	out = gs.Outgoing("auth-flow", PredicateRelatedTo)
	if len(out) != 2 {
		t.Fatalf("expected 2 related_to from auth-flow, got %d", len(out))
	}

	out = gs.Outgoing("auth-flow", PredicateSupersedes)
	if len(out) != 0 {
		t.Fatalf("expected 0 supersedes from auth-flow, got %d", len(out))
	}

	in := gs.Incoming("oauth-scopes")
	if len(in) != 1 || in[0].Subject != "auth-flow" {
		t.Fatalf("expected 1 incoming to oauth-scopes from auth-flow, got %+v", in)
	}
}

func TestGraphStore_RemoveTriple(t *testing.T) {
	gs := newTestGraphStore(t)

	triple := Triple{Subject: "a", Predicate: PredicateRelatedTo, Object: "b"}
	gs.AddTriple(triple)
	if gs.Count() != 1 {
		t.Fatal("expected 1")
	}

	gs.RemoveTriple(triple)
	if gs.Count() != 0 {
		t.Fatalf("expected 0 after remove, got %d", gs.Count())
	}

	if out := gs.Outgoing("a"); len(out) != 0 {
		t.Fatal("expected empty outgoing after remove")
	}
	if in := gs.Incoming("b"); len(in) != 0 {
		t.Fatal("expected empty incoming after remove")
	}
}

func TestGraphStore_Neighbors(t *testing.T) {
	gs := newTestGraphStore(t)

	gs.AddTriple(Triple{Subject: "a", Predicate: PredicateRelatedTo, Object: "b"})
	gs.AddTriple(Triple{Subject: "b", Predicate: PredicateRelatedTo, Object: "c"})
	gs.AddTriple(Triple{Subject: "c", Predicate: PredicateRelatedTo, Object: "d"})

	n1 := gs.Neighbors("a", 1)
	slugs := neighborSlugs(n1, "a")
	sort.Strings(slugs)
	if len(slugs) != 1 || slugs[0] != "b" {
		t.Fatalf("depth 1 from a: expected [b], got %v", slugs)
	}

	n2 := gs.Neighbors("a", 2)
	slugs2 := neighborSlugs(n2, "a")
	sort.Strings(slugs2)
	if len(slugs2) != 2 || slugs2[0] != "b" || slugs2[1] != "c" {
		t.Fatalf("depth 2 from a: expected [b c], got %v", slugs2)
	}
}

func neighborSlugs(triples []Triple, origin string) []string {
	seen := map[string]bool{origin: true}
	for _, t := range triples {
		if !seen[t.Subject] {
			seen[t.Subject] = true
		}
		if !seen[t.Object] {
			seen[t.Object] = true
		}
	}
	delete(seen, origin)
	var result []string
	for s := range seen {
		result = append(result, s)
	}
	return result
}

func TestGraphStore_AddTriples(t *testing.T) {
	gs := newTestGraphStore(t)

	triples := []Triple{
		{Subject: "x", Predicate: PredicateRelatedTo, Object: "y"},
		{Subject: "y", Predicate: PredicateDerivedFrom, Object: "z"},
	}
	if err := gs.AddTriples(triples); err != nil {
		t.Fatal(err)
	}
	if gs.Count() != 2 {
		t.Fatalf("expected 2 triples, got %d", gs.Count())
	}

	all := gs.AllTriples()
	if len(all) != 2 {
		t.Fatalf("expected 2 from AllTriples, got %d", len(all))
	}
}

func TestGraphStore_SyncFromFileStore(t *testing.T) {
	dir := t.TempDir()

	content := `---
title: Auth Pattern
confidence: 0.9
related: [oauth-scopes, token-refresh]
tags: [auth]
---
Check OAuth scopes before API calls. See [[session-mgmt]] for details.
`
	if err := os.WriteFile(filepath.Join(dir, "auth-pattern.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	content2 := `---
title: OAuth Scopes
confidence: 0.85
tags: [auth]
---
Always verify scopes.
`
	if err := os.WriteFile(filepath.Join(dir, "oauth-scopes.md"), []byte(content2), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := NewFileStore(dir, "test", slog.Default())
	if err != nil {
		t.Fatal(err)
	}

	gs := newTestGraphStore(t)
	n, err := gs.SyncFromFileStore(store)
	if err != nil {
		t.Fatal(err)
	}

	if n != 4 {
		t.Fatalf("expected 4 triples (2 from related + 1 from wikilink + 1 shared_tag for auth), got %d", n)
	}

	out := gs.Outgoing("auth-pattern", PredicateRelatedTo)
	if len(out) != 3 {
		t.Fatalf("expected 3 outgoing from auth-pattern, got %d", len(out))
	}

	objects := make([]string, len(out))
	for i, o := range out {
		objects[i] = o.Object
	}
	sort.Strings(objects)
	expected := []string{"oauth-scopes", "session-mgmt", "token-refresh"}
	for i, e := range expected {
		if objects[i] != e {
			t.Fatalf("expected object %d = %s, got %s", i, e, objects[i])
		}
	}
}

func TestGraphStore_BuildGraphData(t *testing.T) {
	dir := t.TempDir()
	content := `---
title: Test Fact
confidence: 0.75
related: [other-fact]
tags: [test]
---
A test fact body.
`
	os.WriteFile(filepath.Join(dir, "test-fact.md"), []byte(content), 0o644)
	os.WriteFile(filepath.Join(dir, "other-fact.md"), []byte("---\ntitle: Other\n---\nOther body."), 0o644)

	store, _ := NewFileStore(dir, "test", slog.Default())
	gs := newTestGraphStore(t)
	gs.SyncFromFileStore(store)

	data := gs.BuildGraphData([]*FileStore{store}, "", 1)
	if len(data.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(data.Nodes))
	}
	if len(data.Edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(data.Edges))
	}
}

func TestParseObsidianFile_Confidence(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		wantConf   float64
		wantTitle  string
	}{
		{
			name:      "with confidence",
			content:   "---\ntitle: Test\nconfidence: 0.95\n---\nBody text.",
			wantConf:  0.95,
			wantTitle: "Test",
		},
		{
			name:      "no confidence defaults to -1",
			content:   "---\ntitle: NoConf\n---\nBody text.",
			wantConf:  -1.0,
			wantTitle: "NoConf",
		},
		{
			name:      "no frontmatter defaults to -1",
			content:   "# Plain Title\nBody text.",
			wantConf:  -1.0,
			wantTitle: "Plain Title",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			title, _, _, conf, _ := parseObsidianFile(tt.content, "fallback")
			if title != tt.wantTitle {
				t.Errorf("title = %q, want %q", title, tt.wantTitle)
			}
			if conf != tt.wantConf {
				t.Errorf("confidence = %f, want %f", conf, tt.wantConf)
			}
		})
	}
}

func TestParseObsidianFile_Related(t *testing.T) {
	content := `---
title: Test
related: [slug-a, slug-b]
---
Body with [[wikilink-c]] and [[slug-a]] duplicate.
`
	_, _, _, _, related := parseObsidianFile(content, "fallback")

	sort.Strings(related)
	expected := []string{"slug-a", "slug-b", "wikilink-c"}
	if len(related) != len(expected) {
		t.Fatalf("expected %d related, got %d: %v", len(expected), len(related), related)
	}
	for i, e := range expected {
		if related[i] != e {
			t.Errorf("related[%d] = %q, want %q", i, related[i], e)
		}
	}
}

func TestEffectiveConfidence(t *testing.T) {
	if v := effectiveConfidence(-1); v != defaultFactConfidence {
		t.Errorf("expected default %f for sentinel, got %f", defaultFactConfidence, v)
	}
	if v := effectiveConfidence(0.42); v != 0.42 {
		t.Errorf("expected 0.42, got %f", v)
	}
	if v := effectiveConfidence(0); v != 0 {
		t.Errorf("expected 0.0 for zero confidence, got %f", v)
	}
}
