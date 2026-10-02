package upstreamwatch

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		item Item
		want Class
	}{
		{"security label wins over feat prefix", Item{Title: "feat: add login", Labels: []string{"security"}}, ClassSecurity},
		{"security label alias", Item{Title: "chore: deps", Labels: []string{"Vulnerability"}}, ClassSecurity},
		{"security title keyword", Item{Title: "Fix security hole in parser"}, ClassSecurity},
		{"cve in title", Item{Title: "patch CVE-2026-1234"}, ClassSecurity},
		{"security scope prefix", Item{Title: "security: rotate key"}, ClassSecurity},
		{"bug label", Item{Title: "something", Labels: []string{"Bug"}}, ClassBugfix},
		{"feature label", Item{Title: "something", Labels: []string{"enhancement"}}, ClassFeature},
		{"fix prefix", Item{Title: "fix: off-by-one"}, ClassBugfix},
		{"fix prefix with scope", Item{Title: "fix(parser): off-by-one"}, ClassBugfix},
		{"feat prefix breaking", Item{Title: "feat!: new api"}, ClassFeature},
		{"chore prefix", Item{Title: "chore: bump deps"}, ClassChore},
		{"docs prefix is chore", Item{Title: "docs: update readme"}, ClassChore},
		{"label beats prefix", Item{Title: "chore: tidy", Labels: []string{"bug"}}, ClassBugfix},
		{"no prefix no label", Item{Title: "Update the thing"}, ClassChore},
		{"empty", Item{}, ClassChore},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.item); got != tc.want {
				t.Fatalf("Classify(%q, %v) = %s, want %s", tc.item.Title, tc.item.Labels, got, tc.want)
			}
		})
	}
}

func TestConventionalPrefix(t *testing.T) {
	for title, want := range map[string]string{
		"feat: x":            "feat",
		"fix(scope): y":      "fix",
		"CHORE: z":           "chore",
		"feat!: breaking":    "feat",
		"no colon here":      "",
		"too many words: hi": "",
		":leading":           "",
		"  fix : spaced":     "fix",
	} {
		if got := conventionalPrefix(title); got != want {
			t.Errorf("conventionalPrefix(%q) = %q, want %q", title, got, want)
		}
	}
}

func TestEstimateDifficulty(t *testing.T) {
	cases := []struct {
		name string
		item Item
		want Difficulty
	}{
		{"tiny pr is easy", Item{Kind: KindPR, Files: []string{"a.go"}, Additions: 3, Deletions: 1}, DifficultyEasy},
		{"two files small is easy", Item{Kind: KindPR, Files: []string{"a.go", "b.go"}, Additions: 40, Deletions: 5}, DifficultyEasy},
		{"many lines is hard", Item{Kind: KindPR, Files: []string{"a.go"}, Additions: 600}, DifficultyHard},
		{"many files is hard", Item{Kind: KindPR, Files: make([]string, 12), Additions: 10}, DifficultyHard},
		{"mid is moderate", Item{Kind: KindPR, Files: []string{"a.go", "b.go", "c.go"}, Additions: 80, Deletions: 20}, DifficultyModerate},
		{"release is moderate", Item{Kind: KindRelease}, DifficultyModerate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EstimateDifficulty(tc.item); got != tc.want {
				t.Fatalf("EstimateDifficulty = %s, want %s", got, tc.want)
			}
		})
	}
}
