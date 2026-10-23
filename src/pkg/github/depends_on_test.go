package github

import (
	"reflect"
	"testing"
)

func TestParseDependsOn(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []PRLinkedIssue
	}{
		{"none", "Fixes #3\n\nNo ordering here.", nil},
		{"bare", "Depends-on: #12", []PRLinkedIssue{{Number: 12, Repo: "acme/widget", Via: DependsOnVia}}},
		{"case and spacing", "depends on : #7\nDEPENDS-ON:#5", []PRLinkedIssue{
			{Number: 5, Repo: "acme/widget", Via: DependsOnVia},
			{Number: 7, Repo: "acme/widget", Via: DependsOnVia},
		}},
		{"cross repo", "Depends-on: other/lib#40", []PRLinkedIssue{{Number: 40, Repo: "other/lib", Via: DependsOnVia}}},
		{"url", "Depends-on: https://github.com/other/lib/pull/41", []PRLinkedIssue{{Number: 41, Repo: "other/lib", Via: DependsOnVia}}},
		{"list item, several refs, dedup", "- Depends-on: #9, #8 and #9\n> Depends-on: #8", []PRLinkedIssue{
			{Number: 8, Repo: "acme/widget", Via: DependsOnVia},
			{Number: 9, Repo: "acme/widget", Via: DependsOnVia},
		}},
		{"mention outside a line start is ignored", "This depends on: nothing. See #4.\nCloses #2", nil},
		{"closing keyword is not a dependency", "Closes #2\nDepends-on: #3", []PRLinkedIssue{{Number: 3, Repo: "acme/widget", Via: DependsOnVia}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseDependsOn(tc.body, "acme/widget")
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseDependsOn(%q) = %+v, want %+v", tc.body, got, tc.want)
			}
		})
	}
}
