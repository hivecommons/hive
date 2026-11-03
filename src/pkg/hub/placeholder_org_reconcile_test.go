package hub

import "testing"

func TestAssignedPlaceholderOrgStuck(t *testing.T) {
	delivered := &SaaSHive{Org: "vllm-d", PrimaryRepo: "kie", ClaimDelivered: true}
	cases := []struct {
		name string
		sh   *SaaSHive
		org  string
		repo string
		want bool
	}{
		{"delivered but live placeholder", delivered, "available-vllmd-03", "kie", true},
		{"meta primary repo only", &SaaSHive{Org: "vllm-d", PrimaryRepo: "kie", ClaimDelivered: true}, "available-x", "", true},
		{"live matches", delivered, "vllm-d", "kie", false},
		{"spoke not reported yet", delivered, "", "", false},
		{"claim not delivered", &SaaSHive{Org: "vllm-d", PrimaryRepo: "kie"}, "available-x", "kie", false},
		{"no repo anywhere", &SaaSHive{Org: "vllm-d", ClaimDelivered: true}, "available-x", "", false},
		{"approved org is itself placeholder", &SaaSHive{Org: "available-y", ClaimDelivered: true}, "available-x", "kie", false},
		{"nil", nil, "available-x", "kie", false},
	}
	for _, tc := range cases {
		if got := assignedPlaceholderOrgStuck(tc.sh, tc.org, tc.repo); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPlaceholderOrgMismatches(t *testing.T) {
	live := []RegistryEntry{
		{ID: "a", Org: "available-a", PrimaryRepo: "kie"},
		{ID: "b", Org: "other"},
		{ID: "c", Org: "VLLM-D"},
		{ID: "d", Org: ""},
		{ID: "e", Org: "x"},
	}
	saas := map[string]*SaaSHive{
		"a": {Org: "vllm-d", ClaimDelivered: true},
		"b": {Org: "vllm-d", ClaimDelivered: true},
		"c": {Org: "vllm-d", ClaimDelivered: true},
		"d": {Org: "vllm-d", ClaimDelivered: true},
	}
	got := placeholderOrgMismatches(live, saas)
	if len(got) != 2 || got[0].HiveID != "a" || !got[0].Placeholder || got[1].HiveID != "b" || got[1].Placeholder {
		t.Fatalf("unexpected mismatches: %+v", got)
	}
}
