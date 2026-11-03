package github

import "testing"

func TestPRSupersedesNumber(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{"Supersedes #15646\n\nRe-authored.", 15646},
		{"Closes #1\n\nsupersedes #42 (contributor PR)", 42},
		{"Fixes #15646", 0},
		{"", 0},
	}
	for _, tc := range cases {
		n, ok := PRSupersedesNumber(tc.body)
		if n != tc.want || ok != (tc.want > 0) {
			t.Fatalf("PRSupersedesNumber(%q) = %d,%v want %d", tc.body, n, ok, tc.want)
		}
	}
}

func TestIsHiveSupersedingPR(t *testing.T) {
	body := "Supersedes #15646"
	if !HiveSupersedingPR(true, body) || HiveSupersedingPR(false, body) || HiveSupersedingPR(true, "Fixes #1") {
		t.Fatal("HiveSupersedingPR: hive-authored + Supersedes marker only")
	}
	pr := PullRequest{AppAuthored: true, Supersedes: prSupersedesNumberOrZero(body)}
	if !IsHiveSupersedingPR(pr) {
		t.Fatalf("IsHiveSupersedingPR(%+v) = false", pr)
	}
	pr.AppAuthored = false
	if IsHiveSupersedingPR(pr) {
		t.Fatal("a contributor's superseding PR is not the hive's")
	}
	if IsHiveSupersedingPR(PullRequest{AppAuthored: true}) {
		t.Fatal("no Supersedes number → not superseding")
	}
}
