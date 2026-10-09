package knowledge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLifecycleFact(t *testing.T, dir, slug, frontmatter, body string) {
	t.Helper()
	content := "---\n" + frontmatter + "---\n\n" + body
	if err := os.WriteFile(filepath.Join(dir, slug+".md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// lifecycleVault seeds a vault with one fact per lifecycle situation, all
// matching the query "deploy".
func lifecycleVault(t *testing.T) (string, *FileStore) {
	t.Helper()
	dir := t.TempDir()
	writeLifecycleFact(t, dir, "legacy", "title: Legacy deploy notes\ntype: gotcha\n", "Legacy deploy fact without a state.")
	writeLifecycleFact(t, dir, "current", "title: Current deploy flow\nstate: approved\n", "Current deploy fact.")
	writeLifecycleFact(t, dir, "retired", "title: Retired deploy flow\nstate: deprecated\n", "Deprecated deploy fact.")
	writeLifecycleFact(t, dir, "tombstone", "title: Tombstone deploy flow\nstatus: deprecated\n", "Connector tombstone deploy fact.")
	writeLifecycleFact(t, dir, "proposal", "title: Proposal deploy flow\nstate: draft\n", "Draft deploy fact.")
	writeLifecycleFact(t, dir, "old-deploy", "title: Old deploy flow\nsuperseded_by: current\n", "Superseded deploy fact.")
	store, err := NewFileStore(dir, "vault", fileStoreTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	return dir, store
}

func slugStates(facts []Fact) map[string]LifecycleState {
	out := make(map[string]LifecycleState, len(facts))
	for _, f := range facts {
		out[f.Slug] = f.State
	}
	return out
}

func TestParseLifecycleState(t *testing.T) {
	for _, in := range []string{"draft", "Approved", " deprecated ", "SUPERSEDED"} {
		if _, err := ParseLifecycleState(in); err != nil {
			t.Errorf("ParseLifecycleState(%q) error: %v", in, err)
		}
	}
	for _, in := range []string{"", "verified", "deprecaed"} {
		if _, err := ParseLifecycleState(in); err == nil {
			t.Errorf("ParseLifecycleState(%q) = nil error, want error", in)
		}
	}
	all, err := ParseLifecycleStates("all")
	if err != nil || len(all) != len(AllLifecycleStates) {
		t.Fatalf("ParseLifecycleStates(all) = %v, %v", all, err)
	}
	got, err := ParseLifecycleStates("deprecated, superseded")
	if err != nil || len(got) != 2 || got[0] != StateDeprecated || got[1] != StateSuperseded {
		t.Fatalf("ParseLifecycleStates = %v, %v", got, err)
	}
	if got, err := ParseLifecycleStates(""); err != nil || got != nil {
		t.Fatalf("ParseLifecycleStates(\"\") = %v, %v; want nil, nil", got, err)
	}
	if _, err := ParseLifecycleStates("approved,bogus"); err == nil {
		t.Fatal("expected error for unknown state")
	}
}

func TestLifecycle_LegacyFactsDefaultOnLoad(t *testing.T) {
	_, store := lifecycleVault(t)
	want := map[string]LifecycleState{
		"legacy":     StateApproved,
		"current":    StateApproved,
		"retired":    StateDeprecated,
		"tombstone":  StateDeprecated,
		"proposal":   StateDraft,
		"old-deploy": StateSuperseded,
	}
	got := slugStates(store.ListPages(""))
	for slug, st := range want {
		if got[slug] != st {
			t.Errorf("%s state = %q, want %q", slug, got[slug], st)
		}
	}
	f, err := store.ReadPage("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if f.State != StateApproved {
		t.Errorf("ReadPage legacy state = %q, want approved", f.State)
	}
	// A legacy Fact value with no state (e.g. from a remote wiki) is approved.
	if st := (Fact{Slug: "x"}).EffectiveState(); st != StateApproved {
		t.Errorf("zero Fact EffectiveState = %q, want approved", st)
	}
	if st := (Fact{Status: "verified"}).EffectiveState(); st != StateApproved {
		t.Errorf("verified status EffectiveState = %q, want approved", st)
	}
	if st := (Fact{State: "nonsense", Status: "deprecated"}).EffectiveState(); st != StateDeprecated {
		t.Errorf("invalid state falls back to status: got %q", st)
	}
}

func TestLifecycle_SetStateTransitions(t *testing.T) {
	dir, store := lifecycleVault(t)

	for _, st := range []LifecycleState{StateDraft, StateApproved, StateDeprecated, StateApproved} {
		if err := store.SetLifecycleState("legacy", st); err != nil {
			t.Fatalf("SetLifecycleState(%s): %v", st, err)
		}
		f, err := store.ReadPage("legacy")
		if err != nil {
			t.Fatal(err)
		}
		if f.State != st {
			t.Fatalf("after SetLifecycleState(%s) state = %q", st, f.State)
		}
	}

	data, err := os.ReadFile(filepath.Join(dir, "legacy.md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if strings.Count(content, "state:") != 1 || !strings.Contains(content, "state: approved") {
		t.Errorf("frontmatter not rewritten in place:\n%s", content)
	}
	if !strings.Contains(content, "title: Legacy deploy notes") || !strings.Contains(content, "type: gotcha") {
		t.Errorf("existing frontmatter lost:\n%s", content)
	}
	if !strings.HasSuffix(content, "Legacy deploy fact without a state.") {
		t.Errorf("body changed:\n%s", content)
	}

	if err := store.SetLifecycleState("legacy", "bogus"); err == nil {
		t.Error("expected error for invalid state")
	}
	if err := store.SetLifecycleState("legacy", StateSuperseded); err == nil {
		t.Error("expected error: superseded must go through Supersede")
	}
	if err := store.SetLifecycleState("missing", StateDraft); err == nil {
		t.Error("expected error for missing page")
	}

	// Restoring a superseded fact clears its superseded_by link.
	if err := store.SetLifecycleState("old-deploy", StateApproved); err != nil {
		t.Fatal(err)
	}
	f, err := store.ReadPage("old-deploy")
	if err != nil {
		t.Fatal(err)
	}
	if f.State != StateApproved || f.SupersededBy != "" {
		t.Errorf("restored fact = state %q superseded_by %q", f.State, f.SupersededBy)
	}
}

func TestLifecycle_SupersedeLinksBothFacts(t *testing.T) {
	_, store := lifecycleVault(t)

	if err := store.Supersede("legacy", "current"); err != nil {
		t.Fatal(err)
	}
	oldFact, err := store.ReadPage("legacy")
	if err != nil {
		t.Fatal(err)
	}
	newFact, err := store.ReadPage("current")
	if err != nil {
		t.Fatal(err)
	}
	if oldFact.State != StateSuperseded || oldFact.SupersededBy != "current" {
		t.Errorf("old fact = state %q superseded_by %q", oldFact.State, oldFact.SupersededBy)
	}
	if newFact.Supersedes != "legacy" || newFact.State != StateApproved {
		t.Errorf("new fact = state %q supersedes %q", newFact.State, newFact.Supersedes)
	}

	if err := store.Supersede("legacy", "legacy"); err == nil {
		t.Error("expected error for self-supersession")
	}
	if err := store.Supersede("legacy", "missing"); err == nil {
		t.Error("expected error for missing replacement")
	}
	if err := store.Supersede("missing", "current"); err == nil {
		t.Error("expected error for missing superseded fact")
	}
}

func TestLifecycle_FileWithoutFrontmatterGainsState(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plain.md"), []byte("# Plain note\n\nNo frontmatter here."), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStore(dir, "vault", fileStoreTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetLifecycleState("plain", StateDeprecated); err != nil {
		t.Fatal(err)
	}
	f, err := store.ReadPage("plain")
	if err != nil {
		t.Fatal(err)
	}
	if f.State != StateDeprecated || f.Title != "Plain note" || f.Body != "No frontmatter here." {
		t.Errorf("got state %q title %q body %q", f.State, f.Title, f.Body)
	}
}

func TestFilterByLifecycle_DefaultExcludesStaleContext(t *testing.T) {
	_, store := lifecycleVault(t)
	all := store.Search("deploy", 50)

	got := slugStates(FilterByLifecycle(all, nil))
	for _, slug := range []string{"legacy", "current"} {
		if _, ok := got[slug]; !ok {
			t.Errorf("approved fact %s missing from default search", slug)
		}
	}
	for _, slug := range []string{"retired", "tombstone", "proposal", "old-deploy"} {
		if _, ok := got[slug]; ok {
			t.Errorf("non-approved fact %s leaked into default search", slug)
		}
	}

	withStale := slugStates(FilterByLifecycle(all, []LifecycleState{StateDeprecated, StateSuperseded}))
	for _, slug := range []string{"legacy", "current", "retired", "tombstone", "old-deploy"} {
		if _, ok := withStale[slug]; !ok {
			t.Errorf("%s missing when deprecated+superseded included", slug)
		}
	}
	if _, ok := withStale["proposal"]; ok {
		t.Error("draft fact included without being requested")
	}

	if n := len(FilterByLifecycle(all, AllLifecycleStates)); n != len(all) {
		t.Errorf("include all kept %d of %d facts", n, len(all))
	}
}

func TestPrimer_ExcludesDeprecatedAndSupersededByDefault(t *testing.T) {
	_, store := lifecycleVault(t)

	p := NewPrimer(nil, PrimerConfig{MaxFacts: 50}, fileStoreTestLogger())
	p.AddFileStore("vault", store, LayerProject)
	primed := p.Prime(context.Background(), nil, []string{"deploy"})
	got := slugStates(primed.Facts)
	if _, ok := got["current"]; !ok {
		t.Fatalf("approved fact missing from primer: %v", got)
	}
	for _, slug := range []string{"retired", "tombstone", "proposal", "old-deploy"} {
		if _, ok := got[slug]; ok {
			t.Errorf("primer leaked %s fact %s", got[slug], slug)
		}
	}
	prompt := primed.FormatForPrompt()
	for _, title := range []string{"Retired deploy flow", "Tombstone deploy flow", "Old deploy flow", "Proposal deploy flow"} {
		if strings.Contains(prompt, title) {
			t.Errorf("prompt contains stale fact %q", title)
		}
	}

	opt := NewPrimer(nil, PrimerConfig{MaxFacts: 50, IncludeStates: []LifecycleState{StateDeprecated, StateSuperseded}}, fileStoreTestLogger())
	opt.AddFileStore("vault", store, LayerProject)
	got = slugStates(opt.Prime(context.Background(), nil, []string{"deploy"}).Facts)
	for _, slug := range []string{"retired", "tombstone", "old-deploy"} {
		if _, ok := got[slug]; !ok {
			t.Errorf("primer with IncludeStates missing %s", slug)
		}
	}
	if _, ok := got["proposal"]; ok {
		t.Error("primer included draft fact without being asked")
	}
}

func TestPrimer_GraphExpansionSkipsSupersededFacts(t *testing.T) {
	dir := t.TempDir()
	writeLifecycleFact(t, dir, "deploy-guide", "title: Deploy guide\n", "Deploy with helm.")
	writeLifecycleFact(t, dir, "stale-guide", "title: Stale guide\nstate: superseded\nsuperseded_by: deploy-guide\n", "Unrelated stale text.")
	store, err := NewFileStore(dir, "vault", fileStoreTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	gs := newTestGraphStore(t)
	if err := gs.AddTriple(Triple{Subject: "deploy-guide", Predicate: PredicateSupersedes, Object: "stale-guide"}); err != nil {
		t.Fatal(err)
	}

	p := NewPrimer(nil, PrimerConfig{MaxFacts: 50}, fileStoreTestLogger())
	p.AddFileStore("vault", store, LayerProject)
	p.SetGraphStore(gs)
	got := slugStates(p.Prime(context.Background(), nil, []string{"deploy"}).Facts)
	if _, ok := got["deploy-guide"]; !ok {
		t.Fatalf("approved fact missing: %v", got)
	}
	if _, ok := got["stale-guide"]; ok {
		t.Error("graph expansion pulled in a superseded fact")
	}
}

func TestKnowledgeAPI_UpdateFactLifecycleOnVault(t *testing.T) {
	api, vaultDir := apiWithVault(t)
	writeLifecycleFact(t, vaultDir, "a", "title: Fact A\n", "Body A.")
	writeLifecycleFact(t, vaultDir, "b", "title: Fact B\n", "Body B.")
	if err := api.ReindexVault(vaultDir); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := api.UpdateFact(ctx, "vault", "a", UpdateFactRequest{State: "deprecated"}); err != nil {
		t.Fatal(err)
	}
	f, err := api.VaultFact("a")
	if err != nil {
		t.Fatal(err)
	}
	if f.State != StateDeprecated {
		t.Errorf("state = %q, want deprecated", f.State)
	}

	if err := api.UpdateFact(ctx, "vault", "a", UpdateFactRequest{SupersededBy: "b"}); err != nil {
		t.Fatal(err)
	}
	a, _ := api.VaultFact("a")
	b, _ := api.VaultFact("b")
	if a == nil || b == nil || a.State != StateSuperseded || a.SupersededBy != "b" || b.Supersedes != "a" {
		t.Fatalf("supersession not linked: a=%+v b=%+v", a, b)
	}

	if err := api.UpdateFact(ctx, "vault", "a", UpdateFactRequest{State: "draft", SupersededBy: "b"}); err == nil {
		t.Error("expected error for superseded_by with non-superseded state")
	}
	if err := api.UpdateFact(ctx, "vault", "a", UpdateFactRequest{State: "bogus"}); err == nil {
		t.Error("expected error for invalid state")
	}
}

func TestKnowledgeAPI_SetEntryState(t *testing.T) {
	api, vaultDir := apiWithVault(t)
	writeLifecycleFact(t, vaultDir, "a", "title: Fact A\n", "Body A.")
	writeLifecycleFact(t, vaultDir, "b", "title: Fact B\n", "Body B.")
	if err := api.ReindexVault(vaultDir); err != nil {
		t.Fatal(err)
	}

	change, err := api.SetEntryState("a", StateDeprecated, "")
	if err != nil {
		t.Fatal(err)
	}
	if change.Channel != "vault" || change.Previous != StateApproved || change.Fact == nil || change.Fact.State != StateDeprecated {
		t.Fatalf("set state change = %+v", change)
	}

	change, err = api.SetEntryState("a", StateSuperseded, "b")
	if err != nil {
		t.Fatal(err)
	}
	if change.Previous != StateDeprecated || change.Fact.State != StateSuperseded || change.Fact.SupersededBy != "b" {
		t.Fatalf("supersede change = %+v", change.Fact)
	}

	if _, err := api.SetEntryState("missing", StateDraft, ""); !errors.Is(err, ErrEntryNotWritable) {
		t.Errorf("missing entry err = %v, want ErrEntryNotWritable", err)
	}
	if _, err := api.SetEntryState("", StateDraft, ""); !errors.Is(err, ErrEntryNotWritable) {
		t.Errorf("empty id err = %v, want ErrEntryNotWritable", err)
	}
	if _, err := api.SetEntryState("a", StateSuperseded, "missing"); !errors.Is(err, ErrReplacementNotInChannel) {
		t.Errorf("missing replacement err = %v, want ErrReplacementNotInChannel", err)
	}
	if _, err := api.SetEntryState("b", StateSuperseded, ""); err == nil {
		t.Error("expected error for superseded without a replacement")
	}
}
