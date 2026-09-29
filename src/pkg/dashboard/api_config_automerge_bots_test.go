package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func TestDiscoverBotAuthors(t *testing.T) {
	prs := []ghpkg.PullRequest{
		{Author: "dependabot[bot]"},
		{Author: "Dependabot[bot]"},
		{Author: "renovate[bot]"},
		{Author: "hive-app[bot]", AppAuthored: true},
		{Author: "hive-app[bot]"},
		{Author: "alice"},
		{Author: ""},
	}
	got := discoverBotAuthors(prs, "hive-app[bot]")
	want := []string{"dependabot[bot]", "renovate[bot]"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestBotAuthorCatalogue(t *testing.T) {
	am := config.AutoMergeConfig{TrustedBotAuthors: []string{"renovate[bot]", "my-org-bot[bot]"}}
	rows := botAuthorCatalogue(am, []string{"dependabot[bot]", "somebot[bot]"})
	byLogin := map[string]map[string]interface{}{}
	for _, r := range rows {
		byLogin[r["login"].(string)] = r
	}
	check := func(login, source string, trusted bool) {
		t.Helper()
		r, ok := byLogin[login]
		if !ok {
			t.Fatalf("missing %s in %v", login, rows)
		}
		if r["source"] != source || r["trusted"] != trusted {
			t.Fatalf("%s: got source=%v trusted=%v want %s/%v", login, r["source"], r["trusted"], source, trusted)
		}
	}
	check("dependabot[bot]", "known", false)
	check("renovate[bot]", "known", true)
	check("somebot[bot]", "discovered", false)
	check("my-org-bot[bot]", "custom", true)

	// nil config falls back to the default trusted set.
	rows = botAuthorCatalogue(config.AutoMergeConfig{}, nil)
	for _, r := range rows {
		if r["login"] == "dependabot[bot]" && r["trusted"] != true {
			t.Fatal("dependabot must be trusted by default")
		}
	}
}

func TestNormalizeBotLoginList(t *testing.T) {
	got := normalizeBotLoginList([]string{" dependabot[bot] ", "", "Dependabot[bot]", "renovate[bot]"})
	if len(got) != 2 || got[0] != "dependabot[bot]" || got[1] != "renovate[bot]" {
		t.Fatalf("got %v", got)
	}
	if got := normalizeBotLoginList(nil); got == nil || len(got) != 0 {
		t.Fatalf("nil input must yield empty non-nil slice, got %#v", got)
	}
}
