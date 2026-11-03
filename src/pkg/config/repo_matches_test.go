package config

import "testing"

func TestRepoMatchesAny(t *testing.T) {
	cases := []struct {
		name     string
		full     string
		bare     string
		patterns []string
		want     bool
	}{
		{"exact full name", "projectbluefin/dakota", "dakota", []string{"projectbluefin/dakota"}, true},
		{"exact bare name", "projectbluefin/dakota", "dakota", []string{"dakota"}, true},
		{"full entry is a prefix of another repo", "projectbluefin/dakota-iso", "dakota-iso", []string{"projectbluefin/dakota"}, false},
		{"bare entry is a prefix of another repo", "projectbluefin/dakota-iso", "dakota-iso", []string{"dakota"}, false},
		{"bare entry is a substring of the owner", "projectbluefin/common", "common", []string{"bluefin"}, false},
		{"glob matches the prefixed repo", "projectbluefin/dakota-iso", "dakota-iso", []string{"projectbluefin/dakota*"}, true},
		{"glob matches the repo itself", "projectbluefin/dakota", "dakota", []string{"projectbluefin/dakota*"}, true},
		{"anchored regex matches exactly", "projectbluefin/dakota", "dakota", []string{`/^projectbluefin\/dakota$/`}, true},
		{"anchored regex skips the prefixed repo", "projectbluefin/dakota-iso", "dakota-iso", []string{`/^projectbluefin\/dakota$/`}, false},
		{"regex against the bare name", "projectbluefin/dakota-iso", "dakota-iso", []string{`/^dakota-/`}, true},
		{"invalid regex never matches", "projectbluefin/dakota", "dakota", []string{`/dakota(/`}, false},
		{"case-insensitive full name", "projectbluefin/dakota", "dakota", []string{"ProjectBluefin/Dakota"}, true},
		{"case-insensitive bare name", "projectbluefin/dakota", "dakota", []string{"DAKOTA"}, true},
		{"surrounding whitespace is ignored", "projectbluefin/dakota", "dakota", []string{"  projectbluefin/dakota "}, true},
		{"empty entry never matches", "projectbluefin/dakota", "dakota", []string{"", "  "}, false},
		{"lone slash is not an empty regex", "projectbluefin/dakota", "dakota", []string{"/"}, false},
		{"later entry matches", "projectbluefin/dakota", "dakota", []string{"other/repo", "dakota"}, true},
		{"no patterns", "projectbluefin/dakota", "dakota", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RepoMatchesAny(tc.full, tc.bare, tc.patterns); got != tc.want {
				t.Errorf("RepoMatchesAny(%q, %q, %q) = %v, want %v", tc.full, tc.bare, tc.patterns, got, tc.want)
			}
		})
	}
}

// The title, label and author filters keep substring semantics for plain
// entries: only repository matching became exact.
func TestMatchesAnyKeepsSubstringSemantics(t *testing.T) {
	if !MatchesAny("projectbluefin/dakota-iso", []string{"projectbluefin/dakota"}) {
		t.Error("MatchesAny must keep matching a plain entry as a substring")
	}
}
