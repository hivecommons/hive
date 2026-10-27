package holdguard

import "testing"

// #14534: only the merge of a base commit into the requested head is hygiene.
func TestIsHygieneMerge(t *testing.T) {
	onBase := func(sha string) bool { return sha == "b" || sha == "b2" }
	tests := []struct {
		name      string
		head      Commit
		requested string
		onBase    func(string) bool
		want      bool
	}{
		{"merge of base into requested head", Commit{SHA: "m", Parents: []string{"h", "b"}}, "h", onBase, true},
		{"merge of a newer base tip into requested head", Commit{SHA: "m", Parents: []string{"h", "b2"}}, "h", onBase, true},
		{"single-parent push on the requested head", Commit{SHA: "m", Parents: []string{"h"}}, "h", onBase, false},
		{"merge of a non-base commit", Commit{SHA: "m", Parents: []string{"h", "x"}}, "h", onBase, false},
		{"merge into another head", Commit{SHA: "m", Parents: []string{"x", "b"}}, "h", onBase, false},
		{"parents swapped", Commit{SHA: "m", Parents: []string{"b", "h"}}, "h", onBase, false},
		{"octopus merge", Commit{SHA: "m", Parents: []string{"h", "b", "x"}}, "h", onBase, false},
		{"no parent information", Commit{SHA: "m"}, "h", onBase, false},
		{"empty second parent", Commit{SHA: "m", Parents: []string{"h", ""}}, "h", func(string) bool { return true }, false},
		{"unknown requested head", Commit{SHA: "m", Parents: []string{"", "b"}}, "", onBase, false},
		{"empty head SHA", Commit{Parents: []string{"h", "b"}}, "h", onBase, false},
		{"no base membership check", Commit{SHA: "m", Parents: []string{"h", "b"}}, "h", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsHygieneMerge(tt.head, tt.requested, tt.onBase); got != tt.want {
				t.Fatalf("IsHygieneMerge(%+v, %q) = %v, want %v", tt.head, tt.requested, got, tt.want)
			}
		})
	}
}
