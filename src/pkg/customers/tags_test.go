package customers

import (
	"errors"
	"strings"
	"testing"
)

func TestCleanTag(t *testing.T) {
	good := map[string]string{
		"Maintainer":      "maintainer",
		"  end-user  ":    "end-user",
		"Early   Adopter": "early adopter",
		"tier_1":          "tier_1",
	}
	for in, want := range good {
		got, err := CleanTag(in)
		if err != nil || got != want {
			t.Errorf("CleanTag(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "   ", "vip!", "<b>", strings.Repeat("x", MaxTagLen+1), "café"} {
		if _, err := CleanTag(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("CleanTag(%q) = %v, want ErrInvalid", bad, err)
		}
	}
}

func tagOf(t *testing.T, p Person, name string) Tag {
	t.Helper()
	i := tagIndex(p.Tags, name)
	if i < 0 {
		t.Fatalf("no tag %q on %+v", name, p.Tags)
	}
	return p.Tags[i]
}

func TestAutoTagsFollowRulesUntilAPersonDecides(t *testing.T) {
	s := openStore(t)
	p := mustAdd(t, s, "Ada")

	changed, err := s.ApplyAutoTags(p.ID, map[string]string{TagMaintainer: "role:owner", TagContributor: "history:merged_pr"})
	if err != nil || !changed {
		t.Fatalf("ApplyAutoTags = %v, %v", changed, err)
	}
	got, _ := s.Get(p.ID)
	m := tagOf(t, got, TagMaintainer)
	if m.State != TagOn || m.Source != SourceAuto || m.Rule != "role:owner" {
		t.Fatalf("maintainer = %+v", m)
	}

	// The same rules again change nothing and do not rewrite the file.
	updated := got.UpdatedAt
	if changed, err := s.ApplyAutoTags(p.ID, map[string]string{TagMaintainer: "role:owner", TagContributor: "history:merged_pr"}); err != nil || changed {
		t.Fatalf("idempotent ApplyAutoTags = %v, %v", changed, err)
	}
	if again, _ := s.Get(p.ID); !again.UpdatedAt.Equal(updated) {
		t.Fatal("an unchanged sweep rewrote the person")
	}

	// A rule that no longer holds turns its automatic tag off.
	if changed, _ := s.ApplyAutoTags(p.ID, map[string]string{TagMaintainer: "role:owner"}); !changed {
		t.Fatal("dropping a rule must change the tags")
	}
	got, _ = s.Get(p.ID)
	if got.HasTag(TagContributor) || !got.HasTag(TagMaintainer) {
		t.Fatalf("active tags = %v", got.ActiveTags())
	}
	if c := tagOf(t, got, TagContributor); len(c.History) != 1 || c.History[0].State != TagOn {
		t.Fatalf("contributor history = %+v", c.History)
	}

	// A person turns maintainer off: the sweep must not turn it back on.
	if _, err := s.SetTag(p.ID, "Maintainer", false, "owner"); err != nil {
		t.Fatal(err)
	}
	if changed, _ := s.ApplyAutoTags(p.ID, map[string]string{TagMaintainer: "role:owner"}); changed {
		t.Fatal("the sweep changed a manual tag")
	}
	got, _ = s.Get(p.ID)
	m = tagOf(t, got, TagMaintainer)
	if m.State != TagOff || m.Source != SourceManual || m.By != "owner" {
		t.Fatalf("manual off was overwritten: %+v", m)
	}

	// And a tag a person turned on stays on when no rule earns it.
	if _, err := s.SetTag(p.ID, TagStakeholder, true, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyAutoTags(p.ID, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(p.ID)
	if !got.HasTag(TagStakeholder) {
		t.Fatal("the sweep turned a manual tag off")
	}

	// "Reset to automatic" hands maintainer back to the rules.
	if _, err := s.ResetTag(p.ID, TagMaintainer, "owner"); err != nil {
		t.Fatal(err)
	}
	if changed, _ := s.ApplyAutoTags(p.ID, map[string]string{TagMaintainer: "role:owner"}); !changed {
		t.Fatal("a reset tag must follow the rules again")
	}
	got, _ = s.Get(p.ID)
	m = tagOf(t, got, TagMaintainer)
	if m.State != TagOn || m.Source != SourceAuto {
		t.Fatalf("after reset = %+v", m)
	}
	if len(m.History) < 3 {
		t.Fatalf("history lost entries: %+v", m.History)
	}
}

func TestTagErrors(t *testing.T) {
	s := openStore(t)
	p := mustAdd(t, s, "Ada")
	if _, err := s.SetTag(p.ID, "bad!", true, "owner"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad tag = %v", err)
	}
	if _, err := s.SetTag("p_missing", "ok", true, "owner"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing person = %v", err)
	}
	if _, err := s.ResetTag(p.ID, "never-set", "owner"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reset of an unknown tag = %v", err)
	}
	if _, err := s.ResetTag(p.ID, "bad!", "owner"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reset bad tag = %v", err)
	}
	if _, err := s.ApplyAutoTags("p_missing", map[string]string{TagMaintainer: "r"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("auto tags on missing person = %v", err)
	}
	// A malformed automatic tag name is skipped, not stored.
	if changed, err := s.ApplyAutoTags(p.ID, map[string]string{"bad!": "r"}); err != nil || changed {
		t.Fatalf("malformed auto tag = %v, %v", changed, err)
	}
	for i := 0; i < MaxTags; i++ {
		if _, err := s.SetTag(p.ID, "t"+string(rune('a'+i%26))+string(rune('a'+i/26)), true, "owner"); err != nil {
			t.Fatalf("tag %d: %v", i, err)
		}
	}
	if _, err := s.SetTag(p.ID, "one-too-many", true, "owner"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("over the tag cap = %v", err)
	}
	// Free-form tags are manual and survive a reset as off.
	got, err := s.ResetTag(p.ID, "taa", "owner")
	if err != nil || got.HasTag("taa") {
		t.Fatalf("reset free-form = %v, %v", got.ActiveTags(), err)
	}
}
