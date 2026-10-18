package hivectl

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPresetReviewIDAndRevision(t *testing.T) {
	for name, want := range map[string]string{
		"Bluefin ecosystem":     "bluefin-ecosystem",
		"  --Odd__Name!!  ":     "odd__name",
		"!!!":                   "preset",
		strings.Repeat("a", 90): strings.Repeat("a", 64),
	} {
		got := PresetReviewID(name)
		if got != want || ValidateProfileName(got) != nil {
			t.Fatalf("PresetReviewID(%q) = %q, want %q", name, got, want)
		}
	}
	preset := &Preset{Version: 1, Name: "x", Revision: "r1", Hives: []PresetHive{{URL: "https://a.example", Repositories: PresetRepoSelector{AllCurrent: true}}}}
	first, err := PresetRevision(preset)
	if err != nil || !strings.HasPrefix(first, "r1@sha256:") {
		t.Fatalf("revision %q, %v", first, err)
	}
	preset.Hives[0].URL = "https://b.example"
	second, _ := PresetRevision(preset)
	if first == second {
		t.Fatal("edited document under the same label kept its revision")
	}
}

func TestDiffAndApplyPreset(t *testing.T) {
	const (
		a        = "github/github.com/org/a"
		b        = "github/github.com/org/b"
		c        = "github/github.com/org/c"
		personal = "github/github.com/me/personal"
	)
	for _, tc := range []struct {
		name          string
		selection     *RepositorySelection
		otherExclude  []string
		allCurrent    bool
		recommended   []string
		wantDiff      PresetDiff
		wantSelection RepositorySelection
		wantSummary   string
	}{
		{
			name:          "legacy becomes explicit selection",
			recommended:   []string{"GitHub/GitHub.com/Org/A", b},
			wantDiff:      PresetDiff{Additions: []string{a, b}},
			wantSelection: RepositorySelection{Mode: SelectionSelected, Repositories: []string{a, b}, Sources: []string{"p"}},
			wantSummary:   "2 repos",
		},
		{
			name:          "legacy all-current snapshot",
			allCurrent:    true,
			recommended:   []string{a},
			wantDiff:      PresetDiff{Additions: []string{a}},
			wantSelection: RepositorySelection{Mode: SelectionAllCurrent, MembershipSnapshot: []string{a}, Sources: []string{"p"}},
			wantSummary:   "1 repos",
		},
		{
			name:          "empty recommendation stays an explicit empty selection",
			recommended:   nil,
			wantSelection: RepositorySelection{Mode: SelectionSelected, Sources: []string{"p"}},
			wantSummary:   "none",
		},
		{
			name:          "exclusions win and are kept",
			selection:     &RepositorySelection{Mode: SelectionSelected, Repositories: []string{c}, Include: []string{personal}, Exclude: []string{a}},
			otherExclude:  []string{b},
			recommended:   []string{a, b, personal},
			wantDiff:      PresetDiff{Removals: []string{c}, Excluded: []string{a, b}},
			wantSelection: RepositorySelection{Mode: SelectionSelected, Repositories: []string{c}, Include: []string{personal}, Exclude: []string{a}, Sources: []string{"p"}},
			wantSummary:   "2 repos",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set := &ProfileSet{Profiles: []Profile{
				{Name: "acme", Hub: "https://acme.example", RegistrationToken: "secret", Selection: tc.selection},
				{Name: "other", Hub: "https://other.example", Selection: &RepositorySelection{Mode: SelectionSelected, Exclude: tc.otherExclude}},
			}}
			diff, err := set.DiffPreset("acme", tc.recommended)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(diff, tc.wantDiff) {
				t.Fatalf("diff = %+v, want %+v", diff, tc.wantDiff)
			}
			changed, err := set.ApplyPreset("acme", "p", tc.allCurrent, diff.Additions, nil)
			if err != nil || !changed {
				t.Fatalf("apply changed=%v err=%v", changed, err)
			}
			got := *set.Profiles[0].Selection
			if !reflect.DeepEqual(got, tc.wantSelection) {
				t.Fatalf("selection = %+v, want %+v", got, tc.wantSelection)
			}
			if set.SelectionSummary("acme") != tc.wantSummary || set.Profiles[0].RegistrationToken != "secret" {
				t.Fatalf("summary %q, token changed", set.SelectionSummary("acme"))
			}
			again, err := set.ApplyPreset("acme", "P", tc.allCurrent, nil, nil)
			if err != nil || again {
				t.Fatal("re-applying the same review reported a change")
			}
		})
	}
}

func TestApplyPresetRemovalsOnlyWhenPassed(t *testing.T) {
	const a, b = "github/github.com/org/a", "github/github.com/org/b"
	set := &ProfileSet{Profiles: []Profile{{Name: "acme", Hub: "https://acme.example", Selection: &RepositorySelection{Mode: SelectionAllCurrent, MembershipSnapshot: []string{a, b}, Sources: []string{"p"}}}}}
	diff, _ := set.DiffPreset("acme", []string{a})
	if !reflect.DeepEqual(diff.Removals, []string{b}) {
		t.Fatalf("removals = %v", diff.Removals)
	}
	if _, err := set.ApplyPreset("acme", "p", true, nil, nil); err != nil {
		t.Fatal(err)
	}
	if set.SelectionSummary("acme") != "2 repos" {
		t.Fatal("removal applied without confirmation")
	}
	if _, err := set.ApplyPreset("acme", "p", true, nil, diff.Removals); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(set.Profiles[0].Selection.MembershipSnapshot, []string{a}) {
		t.Fatal("confirmed removal not applied")
	}
	if _, err := set.ApplyPreset("missing", "p", true, nil, nil); err == nil {
		t.Fatal("unknown hive accepted")
	}
	if _, err := set.ApplyPreset("acme", "bad id", true, nil, nil); err == nil {
		t.Fatal("invalid preset id accepted")
	}
}

func TestFollowAndRecordPresetReview(t *testing.T) {
	set := &ProfileSet{Profiles: []Profile{{Name: "acme", Hub: "https://acme.example", RegistrationToken: "tok"}}}
	if _, err := set.SetFollowNewRepositories("acme", true); err == nil {
		t.Fatal("follow turned a legacy profile into a wildcard")
	}
	if _, err := set.ApplyPreset("acme", "p", true, nil, nil); err != nil {
		t.Fatal(err)
	}
	if changed, err := set.SetFollowNewRepositories("acme", true); err != nil || !changed {
		t.Fatal("follow not stored")
	}
	if changed, _ := set.SetFollowNewRepositories("acme", true); changed {
		t.Fatal("unchanged follow reported a change")
	}
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if !set.RecordPresetReview("p", "/presets/p.yml", "r1", at) || set.RecordPresetReview("p", "/presets/p.yml", "r1", at.Add(time.Hour)) {
		t.Fatal("re-recording the same revision was not a no-op")
	}
	if !set.RecordPresetReview("p", "/presets/p.yml", "r2", at.Add(time.Hour)) || set.FindPreset("P").ReviewedRevision != "r2" || len(set.Presets) != 1 {
		t.Fatal("new revision not recorded in place")
	}
	set.Version = ProfilesVersion
	if err := set.Validate(); err != nil {
		t.Fatal(err)
	}
}
