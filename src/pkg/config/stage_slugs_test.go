package config

import "testing"

func TestKnownStageSlugRegistry(t *testing.T) {
	saved := stageSlugs
	t.Cleanup(func() { stageSlugs = saved })

	stageSlugs = nil
	if !KnownStageSlug("anything") {
		t.Fatal("with no table registered every slug must be accepted")
	}
	RegisterStageSlugs([]string{"fixing", "merge-queue"})
	if !KnownStageSlug("fixing") || KnownStageSlug("nope") {
		t.Fatal("registered slugs must be recognised and others rejected")
	}
	if got := KnownStageSlugs(); len(got) != 2 {
		t.Fatalf("KnownStageSlugs = %v", got)
	}
}

// Validation must know the vocabulary even in a program importing only
// config, without a higher-level package linking stagelabels for its init.
func TestDefaultStageSlugs(t *testing.T) {
	if !KnownStageSlug("pr-in-progress") || KnownStageSlug("misspelled-stage") {
		t.Fatal("default registry must accept pipeline stages and reject unknown slugs")
	}
	slugs := KnownStageSlugs()
	if len(slugs) == 0 {
		t.Fatal("default registry must contain the stage vocabulary")
	}
	slugs[0] = "mutated-by-caller"
	if KnownStageSlug(slugs[0]) {
		t.Fatal("KnownStageSlugs must return a defensive copy")
	}
}
