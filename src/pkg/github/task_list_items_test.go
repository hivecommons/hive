package github

import (
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func TestListTaskListItems(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []taskListItem
	}{
		{
			name: "empty body",
			body: "",
			want: nil,
		},
		{
			name: "no task items",
			body: "just prose\n- a plain bullet\n",
			want: nil,
		},
		{
			name: "checked and unchecked in document order",
			body: "- [ ] first\n- [x] second\n- [ ] third",
			want: []taskListItem{
				{Checked: false, Text: "first"},
				{Checked: true, Text: "second"},
				{Checked: false, Text: "third"},
			},
		},
		{
			name: "uppercase X counts as checked",
			body: "- [X] shouted done",
			want: []taskListItem{{Checked: true, Text: "shouted done"}},
		},
		{
			name: "star and plus bullets with leading indent",
			body: "  * [ ] star item\n\t+ [x] plus item",
			want: []taskListItem{
				{Checked: false, Text: "star item"},
				{Checked: true, Text: "plus item"},
			},
		},
		{
			name: "item text is trimmed",
			body: "- [ ]    padded text   ",
			want: []taskListItem{{Checked: false, Text: "padded text"}},
		},
		{
			name: "empty item text",
			body: "- [ ]",
			want: []taskListItem{{Checked: false, Text: ""}},
		},
		{
			name: "items inside fenced code block are excluded",
			body: "- [ ] real\n```\n- [ ] fenced fake\n- [x] another fake\n```\n- [x] also real",
			want: []taskListItem{
				{Checked: false, Text: "real"},
				{Checked: true, Text: "also real"},
			},
		},
		{
			name: "tilde fence also excludes items",
			body: "~~~\n- [ ] hidden\n~~~\n- [ ] visible",
			want: []taskListItem{{Checked: false, Text: "visible"}},
		},
		{
			name: "shorter closing fence does not close a longer opener",
			body: "````\n```\n- [ ] still fenced\n````\n- [x] out",
			want: []taskListItem{{Checked: true, Text: "out"}},
		},
		{
			name: "mismatched fence char does not close the fence",
			body: "```\n~~~\n- [ ] still fenced",
			want: nil,
		},
		{
			name: "unclosed fence swallows the rest of the body",
			body: "- [ ] before\n```\n- [ ] after opener",
			want: []taskListItem{{Checked: false, Text: "before"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := listTaskListItems(tt.body)
			if len(got) != len(tt.want) {
				t.Fatalf("listTaskListItems(%q) = %v, want %v", tt.body, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("item %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func mkComment(login, body string) *gh.IssueComment {
	return &gh.IssueComment{
		User: &gh.User{Login: gh.Ptr(login)},
		Body: gh.Ptr(body),
	}
}

func TestHasSelfAuthorizationNotice(t *testing.T) {
	const marker = "hivecommons/hive#5117"

	tests := []struct {
		name        string
		comments    []*gh.IssueComment
		appBotLogin string
		want        bool
	}{
		{
			name:        "no comments",
			comments:    nil,
			appBotLogin: "hive[bot]",
			want:        false,
		},
		{
			name:        "nil comment entries are skipped",
			comments:    []*gh.IssueComment{nil, nil},
			appBotLogin: "hive[bot]",
			want:        false,
		},
		{
			name: "marker from app bot is found",
			comments: []*gh.IssueComment{
				mkComment("hive[bot]", "self-authorization notice, see "+marker),
			},
			appBotLogin: "hive[bot]",
			want:        true,
		},
		{
			name: "app bot login match is case-insensitive",
			comments: []*gh.IssueComment{
				mkComment("Hive[Bot]", "see "+marker),
			},
			appBotLogin: "hive[bot]",
			want:        true,
		},
		{
			name: "forged marker from another user is ignored",
			comments: []*gh.IssueComment{
				mkComment("mallory", "see "+marker),
			},
			appBotLogin: "hive[bot]",
			want:        false,
		},
		{
			name: "empty appBotLogin accepts marker from any author",
			comments: []*gh.IssueComment{
				mkComment("anyone", "see "+marker),
			},
			appBotLogin: "",
			want:        true,
		},
		{
			name: "whitespace-only appBotLogin accepts marker from any author",
			comments: []*gh.IssueComment{
				mkComment("anyone", "see "+marker),
			},
			appBotLogin: "   ",
			want:        true,
		},
		{
			name: "app bot comment without marker is not a notice",
			comments: []*gh.IssueComment{
				mkComment("hive[bot]", "just a status update"),
			},
			appBotLogin: "hive[bot]",
			want:        false,
		},
		{
			name: "marker found after skipping non-matching comments",
			comments: []*gh.IssueComment{
				nil,
				mkComment("mallory", "see "+marker),
				mkComment("hive[bot]", "no marker here"),
				mkComment("hive[bot]", "notice: "+marker),
			},
			appBotLogin: "hive[bot]",
			want:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasSelfAuthorizationNotice(tt.comments, tt.appBotLogin); got != tt.want {
				t.Errorf("hasSelfAuthorizationNotice() = %v, want %v", got, tt.want)
			}
		})
	}
}
