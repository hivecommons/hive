package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestProjectOrgHealth(t *testing.T) {
	cases := []struct {
		name   string
		p      config.ProjectConfig
		wantOK bool
	}{
		{"placeholder org bare repo", config.ProjectConfig{Org: "available-vllmd-03-ab12", PrimaryRepo: "kie"}, true},
		{"placeholder prefix bare repos entry", config.ProjectConfig{Org: "placeholder", Repos: []string{"kie"}}, true},
		{"unclaimed slot no repo", config.ProjectConfig{Org: "available-vllmd-03-ab12"}, false},
		{"placeholder org full repo", config.ProjectConfig{Org: "available-x", PrimaryRepo: "acme/kie"}, false},
		{"real org bare repo", config.ProjectConfig{Org: "acme", PrimaryRepo: "kie"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, detail, ok := projectOrgHealth(tc.p)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && (st != "fail" || detail == "") {
				t.Fatalf("status=%q detail=%q, want fail with detail", st, detail)
			}
		})
	}
}
