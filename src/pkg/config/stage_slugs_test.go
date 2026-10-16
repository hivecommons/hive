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
