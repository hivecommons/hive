package github

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
)

const openHeldTestBot = "hive-app[bot]"

func capRepos(caps map[string]int) func(string) int {
	return func(repo string) int {
		repo = strings.ToLower(repo)
		if n, ok := caps[repo]; ok {
			return n
		}
		if i := strings.LastIndex(repo, "/"); i >= 0 {
			return caps[repo[i+1:]]
		}
		return 0
	}
}

func heldHoldItem(repo string, number int, author string) HoldItem {
	return HoldItem{Repo: repo, Number: number, Type: "pr", Author: author, Labels: []string{"hold"}}
}

// Only PRs the hive App opened that are still held count; human PRs, other
// bots' PRs and held issues never do, and repos are counted separately.
func TestCountOpenHeldHivePRs(t *testing.T) {
	c := &Client{org: "o", appBotLogin: openHeldTestBot}
	items := []HoldItem{
		heldHoldItem("r", 1, openHeldTestBot),
		heldHoldItem("o/r", 2, "Hive-App[bot]"),
		heldHoldItem("r", 3, "alice"),
		heldHoldItem("r", 4, "dependabot[bot]"),
		{Repo: "r", Number: 5, Type: "issue", Author: openHeldTestBot},
		heldHoldItem("other", 6, openHeldTestBot),
	}
	got := c.countOpenHeldHivePRs(items)
	if got["o/r"] != 2 || got["o/other"] != 1 || len(got) != 2 {
		t.Fatalf("counts = %v, want o/r=2 and o/other=1", got)
	}
	if got := (&Client{org: "o"}).countOpenHeldHivePRs(items); len(got) != 0 {
		t.Errorf("without a known App login nothing may count as hive-opened: %v", got)
	}
}

// With no cap configured nothing is stamped and nothing is refused, however
// many held hive PRs a repo has.
func TestOpenHeldPRCap_DefaultOff(t *testing.T) {
	c := &Client{org: "o", appBotLogin: openHeldTestBot}
	a := &ActionableResult{}
	for i := 1; i <= 40; i++ {
		a.Hold.Items = append(a.Hold.Items, heldHoldItem("r", i, openHeldTestBot))
	}
	c.StampOpenHeldPRs(a)
	if a.OpenHeldPRs != nil {
		t.Errorf("no cap configured but stamped %v", a.OpenHeldPRs)
	}
	if r := c.openHeldPRCapRefusal("r"); r != "" {
		t.Errorf("no cap configured but refused: %s", r)
	}
	c.SetMaxOpenHeldPRsFunc(capRepos(map[string]int{"other": 3}))
	c.StampOpenHeldPRs(a)
	if a.OpenHeldPRs != nil {
		t.Errorf("an uncapped repo was stamped: %v", a.OpenHeldPRs)
	}
	if r := c.openHeldPRCapRefusal("o/r"); r != "" {
		t.Errorf("an uncapped repo was refused: %s", r)
	}
}

// At the cap the relay refuses with the exact capacity line; once a held PR
// is released (its hold removed, e.g. the #13606 test/docs auto-release) or
// closed, it leaves the hold list, stops counting and the repo opens again.
func TestOpenHeldPRCap_RefusesAtCapAndReleases(t *testing.T) {
	c := &Client{org: "o", appBotLogin: openHeldTestBot}
	c.SetMaxOpenHeldPRsFunc(capRepos(map[string]int{"r": 2}))
	a := &ActionableResult{Hold: HoldResult{Items: []HoldItem{
		heldHoldItem("r", 1, openHeldTestBot),
		heldHoldItem("r", 2, openHeldTestBot),
		heldHoldItem("r", 3, "alice"),
	}}}
	c.StampOpenHeldPRs(a)
	o, ok := OpenHeldPRsFor(a.OpenHeldPRs, "r")
	if !ok || o.Count != 2 || o.Max != 2 || !o.AtCapacity() {
		t.Fatalf("stamp = %+v (ok=%v), want 2 of 2 at capacity", o, ok)
	}
	for _, repo := range []string{"r", "o/r", "O/R"} {
		r := c.openHeldPRCapRefusal(repo)
		if !strings.HasPrefix(r, RepoAtCapacityLine) || !strings.Contains(r, "o/r") {
			t.Errorf("refusal for %q = %q, want it to start with %q and name the repo", repo, r, RepoAtCapacityLine)
		}
	}
	if r := c.openHeldPRCapRefusal("other"); r != "" {
		t.Errorf("another repo was refused: %s", r)
	}

	// PR #2's hold is removed: it is no longer in the hold list.
	a.Hold.Items = []HoldItem{heldHoldItem("r", 1, openHeldTestBot), heldHoldItem("r", 3, "alice")}
	c.StampOpenHeldPRs(a)
	if r := c.openHeldPRCapRefusal("r"); r != "" {
		t.Errorf("a released PR still counts: %s", r)
	}
	if o, _ := OpenHeldPRsFor(a.OpenHeldPRs, "o/r"); o.Count != 1 || o.AtCapacity() {
		t.Errorf("stamp after release = %+v, want 1 of 2", o)
	}

	// A held PR the relay opens counts at once, before the next stamp.
	c.noteOpenHeldPROpened("o/r")
	if r := c.openHeldPRCapRefusal("r"); !strings.HasPrefix(r, RepoAtCapacityLine) {
		t.Errorf("a just-opened held PR did not count: %q", r)
	}
}

// End to end through the relay: a request for a repo at its cap opens
// nothing and is quarantined with the capacity line; a repo below its cap
// still opens.
func TestPRRequestWatcher_RefusesRepoAtCapacity(t *testing.T) {
	created := 0
	srv := newPRMockServer(t, "", &created)
	defer srv.Close()
	c := testClient(t, srv.URL)
	c.appBotLogin = openHeldTestBot
	c.SetMaxOpenHeldPRsFunc(capRepos(map[string]int{"r": 1}))
	c.StampOpenHeldPRs(&ActionableResult{Hold: HoldResult{Items: []HoldItem{heldHoldItem("r", 9, openHeldTestBot)}}})

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	reqPath, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "scanner/fix-1", Title: "[scanner] fix: thing", Body: "Fixes #1", Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessPRRequestsOnce(context.Background())

	if created != 0 {
		t.Fatalf("%d PRs opened on a repo at capacity, want 0", created)
	}
	if _, err := os.Stat(reqPath + ".rejected"); err != nil {
		t.Errorf("request was not quarantined: %v", err)
	}
	resBytes, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("result file missing: %v", err)
	}
	var res PRResponse
	if err := json.Unmarshal(resBytes, &res); err != nil {
		t.Fatal(err)
	}
	if res.OK || !strings.Contains(res.Error, RepoAtCapacityLine) {
		t.Errorf("result = %+v, want a refusal carrying %q", res, RepoAtCapacityLine)
	}

	// The PR is released: below the cap the next request opens.
	c.StampOpenHeldPRs(&ActionableResult{})
	if _, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "scanner/fix-2", Title: "[scanner] fix: thing", Body: "Fixes #1", Agent: "scanner"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessPRRequestsOnce(context.Background())
	if created != 1 {
		t.Errorf("below the cap: %d PRs created, want 1", created)
	}
}

func TestWarnCapWithoutBotLogin(t *testing.T) {
	run := func(login string, caps map[string]int) (int, string) {
		var buf bytes.Buffer
		c := &Client{org: "o", repos: []string{"r", "s"}, appBotLogin: login,
			logger: slog.New(slog.NewTextHandler(&buf, nil))}
		c.SetMaxOpenHeldPRsFunc(capRepos(caps))
		for i := 0; i < 3; i++ {
			c.StampOpenHeldPRs(&ActionableResult{})
		}
		return strings.Count(buf.String(), "max_open_held_prs is set but cannot be enforced"), buf.String()
	}
	if n, out := run("", map[string]int{"r": 2}); n != 1 || !strings.Contains(out, "r") {
		t.Errorf("want exactly one warning naming the capped repo, got %d: %s", n, out)
	}
	if n, _ := run(openHeldTestBot, map[string]int{"r": 2}); n != 0 {
		t.Errorf("warned despite a known App login")
	}
	if n, _ := run("", nil); n != 0 {
		t.Errorf("warned with no cap configured")
	}
}
