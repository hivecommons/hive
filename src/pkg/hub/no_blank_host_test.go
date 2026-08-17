package hub

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestNoCodePathStoresABlankGitHubHost is a source-level guard.
//
// github_host is the SINGLE stored input for a hive's identity (#2386): app_id,
// app_slug, api_url and base_url are all derived from it. A blank one therefore
// means "work it out yourself", which is exactly the ambiguity that hid the
// 2026-07-31 incident — a GHE app_id beside an empty api_url read as *unset*
// rather than *mismatched*, and 25 of 50 hub records carried no github_host at
// all.
//
// Explicit public github.com used to be stored as a blank host PLUS the
// "public" sentinel in GitHubBaseURL, because a blank was the only way to stop
// backfillGitHubHostFromCluster re-GHE-ing the hive on the next line. Storing
// the real host achieves the same protection — that backfill only ever fills an
// EMPTY value — without an absent field anyone has to interpret.
//
// A source scan rather than a behavioural test: the assignment sites are spread
// across three handlers, and what matters is that NO path anywhere reintroduces
// one. A behavioural test would have to enumerate the handlers and would pass
// while a fourth path quietly did it.
func TestNoCodePathStoresABlankGitHubHost(t *testing.T) {
	// Matches h.GitHubHost = "" / entry.GitHubHost = "" and friends.
	blank := regexp.MustCompile(`\.GitHubHost\s*=\s*""`)

	var offenders []string
	scanned := 0
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil // fixtures may legitimately construct a blank
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		scanned++
		for i, line := range strings.Split(string(src), "\n") {
			if blank.MatchString(line) {
				offenders = append(offenders, filepath.ToSlash(path)+":"+strconv.Itoa(i+1)+" "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A source scan that reads nothing passes for the wrong reason. Mutating
	// the walk root to a bad path made this test green WITH a real offender in
	// the tree, so the count is asserted: it is the difference between "no
	// offenders" and "no files looked at".
	if scanned < 20 {
		t.Fatalf("only %d source files scanned — the walk is not covering pkg/hub, so a passing result proves nothing", scanned)
	}
	if len(offenders) > 0 {
		t.Errorf("a code path stores a BLANK github_host. It is the single stored input for hive identity, so a blank means \"infer it\" — the ambiguity this package exists to remove. Store the real host (publicForgeHost for public github.com); backfillGitHubHostFromCluster only fills an EMPTY value, so a stated host blocks it just as a blank did:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
