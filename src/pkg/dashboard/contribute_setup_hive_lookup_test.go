package dashboard

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The `just contribute-setup` hive picker used to die with a bare
// "recipe `contribute-setup` failed with exit code 5" and no explanation.
//
// /api/saas/my-hives answers with "hives": null for any account that owns no
// SaaS-hosted hive — the normal case for a contributor who only lends their CLI
// to someone else's hive. The old filter, `.hives[]? // .[]`, read that empty
// result as "try something else" and iterated every TOP-LEVEL KEY of the
// response instead (alerts, channel_targets, …), then interpolated .name over
// values that are arrays. jq exits 5 on that, and because the recipe runs under
// `set -euo pipefail`, jq's status aborted the whole recipe — before reaching
// the public-registry fallback on the very next lines, which lists hives fine.
//
// These tests pin the shipped filters themselves (read out of the Justfile, not
// copied here) so the recipe cannot regress to a shape that turns "you have no
// hives of your own" into a fatal, unexplained exit.

// justfileHiveLookupFilters returns the jq filters used by the hive-lookup
// block of contribute-setup, in source order: [my-hives, public registry].
func justfileHiveLookupFilters(t *testing.T) []string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "Justfile")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(raw)

	start := strings.Index(src, "looking up your hives")
	if start < 0 {
		t.Fatal("hive-lookup block not found in the Justfile")
	}
	end := strings.Index(src[start:], "Select a hive")
	if end < 0 {
		t.Fatal("end of the hive-lookup block not found in the Justfile")
	}
	// Scan CODE only: the recipe's comments legitimately quote the broken
	// filter to explain why it is gone, and that must not read as a regression.
	var code []string
	for _, line := range strings.Split(src[start:start+end], "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		code = append(code, line)
	}
	block := strings.Join(code, "\n")

	// The fragile fallback that caused the crash must not come back.
	if strings.Contains(block, "// .[]") {
		t.Error("hive lookup must not fall back to iterating every top-level key (`// .[]`): " +
			"that is what made jq exit 5 on a null .hives")
	}
	// jq's status must never be able to abort the recipe under `set -e`.
	for _, line := range code {
		if strings.Contains(line, "jq -r") && !strings.Contains(line, "|| true") {
			t.Errorf("jq call in the hive lookup is unguarded, so a shape change kills the recipe: %s",
				strings.TrimSpace(line))
		}
	}

	filters := regexp.MustCompile(`jq -r '([^']*)'`).FindAllStringSubmatch(block, -1)
	if len(filters) != 2 {
		t.Fatalf("expected 2 jq filters in the hive-lookup block (my-hives, registry), got %d", len(filters))
	}
	return []string{filters[0][1], filters[1][1]}
}

// runJQ feeds payload to `jq -r <filter>` and returns its stdout, failing the
// test if jq exits non-zero — the exact condition that killed contribute-setup.
func runJQ(t *testing.T, filter, payload string) string {
	t.Helper()
	cmd := exec.Command("jq", "-r", filter)
	cmd.Stdin = strings.NewReader(payload)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("jq exited non-zero (this is the failure mode being guarded): %v\nfilter: %s\nstderr: %s",
			err, filter, errOut.String())
	}
	return strings.TrimSpace(out.String())
}

func TestContributeSetupHiveLookupFiltersAreGuarded(t *testing.T) {
	justfileHiveLookupFilters(t)
}

func TestContributeSetupHiveLookupHandlesNullHives(t *testing.T) {
	filters := justfileHiveLookupFilters(t)
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed; the filter-shape assertions above still ran")
	}
	myHives, registry := filters[0], filters[1]

	// The real /api/saas/my-hives response for an account with no hives of its
	// own: "hives" is null and sibling keys hold arrays and objects. The old
	// filter exited 5 on exactly this.
	nullHives := `{"alerts":{"total":0},"channel_targets":[{"channel":"stable","branch":"v4"}],` +
		`"hives":null,"is_admin":false,"show_my_hives":true,"tracked_branches":["v4"]}`
	if got := runJQ(t, myHives, nullHives); got != "" {
		t.Errorf("null .hives must yield no rows so the lookup falls through to the registry, got %q", got)
	}

	// An account that DOES own hives still lists them, including the
	// project_name spelling and an id-only fallback for an unnamed hive.
	owned := `{"hives":[{"id":"hosted-a","name":"owner/repo"},` +
		`{"id":"hosted-b","project_name":"other/repo"},{"id":"hosted-c"}]}`
	want := "hosted-a|owner/repo\nhosted-b|other/repo\nhosted-c|hosted-c"
	if got := runJQ(t, myHives, owned); got != want {
		t.Errorf("owned hives extracted wrong:\n got: %q\nwant: %q", got, want)
	}

	// The registry fallback lists only online hives, and must survive the same
	// kind of shape drift without taking the recipe down with it.
	reg := `{"hives":[{"id":"a","name":"A","online":true},{"id":"b","name":"B","online":false},{"id":"c","online":true}]}`
	if got := runJQ(t, registry, reg); got != "a|A\nc|c" {
		t.Errorf("registry filter wrong: got %q, want %q", got, "a|A\nc|c")
	}
	if got := runJQ(t, registry, `{"hives":null}`); got != "" {
		t.Errorf("registry with null .hives must yield nothing, got %q", got)
	}
}
