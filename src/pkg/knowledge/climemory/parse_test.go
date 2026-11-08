package climemory

import (
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return string(data)
}

func TestParseMemoryFormats(t *testing.T) {
	tests := []struct {
		name    string
		cli     string
		fixture string
		want    []Fact
	}{
		{
			name:    "claude MEMORY.md index skips bare topic links and folds wrapped bullets",
			cli:     CLIClaude,
			fixture: "claude-MEMORY.md",
			want: []Fact{
				{Heading: "Memory", Body: "CI is the only verdict; never run go test locally"},
				{Heading: "Memory", Body: "The operator prefers PRs under 400 lines and squash merges."},
				{Heading: "Memory", Body: "User mentioned HIPAA once in passing; do not assume PHI is in scope for this repository."},
				{Heading: "Repo conventions", Body: "Changelog fragments live at the repository root under `changelog.d/`."},
				{Heading: "Repo conventions", Body: "Commit messages are signed with `-s`; DCO trailer is checked in CI; unsigned commits are rejected"},
			},
		},
		{
			name:    "claude topic file keeps paragraphs and code fences",
			cli:     CLIClaude,
			fixture: "claude-topic.md",
			want: []Fact{
				{Heading: "Build pipeline", Body: "CI is the only verdict. Do not run `go test` or `go build` from an agent pane."},
				{Heading: "Build pipeline", Body: "```sh\ngh pr checks 1234\n```"},
				{Heading: "Build pipeline", Body: "Use the failing job log to fix, then push again."},
			},
		},
		{
			name:    "codex memory summary splits per section bullet",
			cli:     CLICodex,
			fixture: "codex-memory_summary.md",
			want: []Fact{
				{Heading: "Preferences", Body: "Prefers table-driven tests with fixtures under testdata/."},
				{Heading: "Preferences", Body: "Avoids time.Sleep in tests."},
				{Heading: "Project facts", Body: "The scheduler primes kicks from the knowledge store before rendering."},
			},
		},
		{
			name:    "gemini drains only the Added Memories section",
			cli:     CLIGemini,
			fixture: "gemini-GEMINI.md",
			want: []Fact{
				{Heading: "Gemini Added Memories", Body: "The hive dashboard listens on port 8080."},
				{Heading: "Gemini Added Memories", Body: "Release lines are cut from the v5 branch."},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseFor(tc.cli, fixture(t, tc.fixture))
			if len(got) != len(tc.want) {
				t.Fatalf("got %d facts, want %d:\n%+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("fact %d:\n got %+v\nwant %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseGeminiWithoutSectionDrainsWholeFile(t *testing.T) {
	got := ParseGeminiMemory("- remembered thing\n- another\n")
	if len(got) != 2 || got[0].Body != "remembered thing" {
		t.Fatalf("got %+v", got)
	}
}

func TestFactTitle(t *testing.T) {
	tests := []struct {
		name string
		fact Fact
		want string
	}{
		{"generic heading dropped, first sentence kept", Fact{Heading: "Memory", Body: "CI is the verdict. Never run tests locally."}, "CI is the verdict"},
		{"specific heading prefixed", Fact{Heading: "Repo conventions", Body: "Sign every commit"}, "Repo conventions: Sign every commit"},
		{"heading not repeated when body starts with it", Fact{Heading: "Build", Body: "Build with just"}, "Build with just"},
		{"code fence uses first line", Fact{Heading: "", Body: "```sh\ngh pr checks\n```"}, "sh"},
		{"long body truncated on a word", Fact{Body: "one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen"}, "one two three four five six seven eight nine ten eleven twelve thirteen fourteen…"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.fact.Title(); got != tc.want {
				t.Errorf("Title() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHashIgnoresWrappingDifferences(t *testing.T) {
	a := Hash("the operator  prefers\n  squash merges")
	b := Hash("the operator prefers squash merges")
	if a != b {
		t.Fatalf("hash differs across whitespace: %s vs %s", a, b)
	}
	if a == Hash("the operator prefers rebase merges") {
		t.Fatal("different facts hashed equal")
	}
}
