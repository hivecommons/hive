package github

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stageengine/stageenginetest"
)

// projectbluefin/server#637: a Renovate digest bump (authored by the
// custom-named mergeraptor[bot] App) edits only Justfile. An architect's
// refactor of the same Justfile plus two test files contains that set, and
// the overlapping-files check refused the refactor in favour of the bump.
// Only PRs this hive's agents opened may absorb a request.
func TestCreatePR_DuplicateGuardOnlyCountsHiveAgentPRs(t *testing.T) {
	const appBot = "hivecommons-hive[bot]"
	headFiles := []string{"Justfile", "tests/unit/test_nvidia_container_toolkit_sysext.py", "tests/unit/test_sysext_export_contract.py"}
	for _, tc := range []struct {
		name       string
		author     string
		appLogin   string
		extraBots  []string
		wantNumber int
	}{
		{name: "renovate-style custom App bump does not absorb the refactor", author: "mergeraptor[bot]", appLogin: appBot, wantNumber: 99},
		{name: "renovate bump does not absorb the refactor", author: "renovate[bot]", appLogin: appBot, wantNumber: 99},
		{name: "dependabot bump does not absorb the refactor", author: "dependabot[bot]", appLogin: appBot, wantNumber: 99},
		{name: "the hive's own agent PR still absorbs it", author: appBot, appLogin: appBot, wantNumber: 637},
		{name: "login match ignores case", author: "HiveCommons-Hive[bot]", appLogin: appBot, wantNumber: 637},
		{name: "a configured additional hive bot still absorbs it", author: "other-hive[bot]", appLogin: appBot, extraBots: []string{"other-hive[bot]"}, wantNumber: 637},
		{name: "no hive bot login known: no PR is treated as an agent's", author: "some-app[bot]", wantNumber: 99},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &guardServer{
				open:      []guardPR{{Number: 637, Ref: "renovate/registry.fedoraproject.org-fedora-44", User: tc.author, Files: []string{"Justfile"}}},
				headFiles: headFiles,
			}
			srv := g.start(t)
			c := NewClientForTest(srv.URL, "o", nil, prTestLogger())
			c.SetAppBotLogin(tc.appLogin)
			c.SetAdditionalBotLogins(tc.extraBots)
			res, err := c.CreatePR(context.Background(), "o/r", "arch/refactor-sysext-export", "main", "title", "Refs #638")
			if err != nil {
				t.Fatalf("CreatePR: %v", err)
			}
			if res.Number != tc.wantNumber {
				t.Fatalf("got #%d reason %q, want #%d", res.Number, res.DuplicateReason, tc.wantNumber)
			}
			if tc.wantNumber == 99 {
				if res.IsDuplicate() || g.created != 1 || g.comments != 0 {
					t.Errorf("want a fresh PR and no redirect: duplicate=%v created=%d comments=%d", res.IsDuplicate(), g.created, g.comments)
				}
				return
			}
			if res.DuplicateReason != DuplicateReasonOverlappingFiles || g.created != 0 {
				t.Errorf("want an overlapping-files refusal: reason %q created=%d", res.DuplicateReason, g.created)
			}
		})
	}
}

// When the guard refuses a request on overlapping files, the issues the
// request was for are not staged under the blocking PR (it shares files, not
// issues) and each gets a note naming the branch and the blocking PR. A
// closes-issue refusal still stages the issue under the PR that closes it.
func TestPRRequestWatcher_OverlapRefusalNotesTheBlockedIssue(t *testing.T) {
	const appBot = "hivecommons-hive[bot]"
	run := func(t *testing.T, g *guardServer, body string) (*guardServer, *stageenginetest.Recorder, PRResponse) {
		t.Helper()
		srv := g.start(t)
		c := testClient(t, srv.URL)
		enableInProgress(c, "hive-a", time.Hour)
		c.SetAppBotLogin(appBot)
		sink := &stageenginetest.Recorder{}
		c.SetStageFactSink(sink)
		dir := t.TempDir()
		old := prRequestDirForTest
		prRequestDirForTest = dir
		t.Cleanup(func() { prRequestDirForTest = old })
		reqPath, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "arch/refactor-sysext-export", Title: "refactor(just): share one export body", Body: body, Agent: "architect", IssueN: []int{638}})
		if err != nil {
			t.Fatal(err)
		}
		c.ProcessPRRequestsOnce(context.Background())
		raw, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
		if err != nil {
			t.Fatalf("result file: %v", err)
		}
		var resp PRResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatal(err)
		}
		return g, sink, resp
	}

	t.Run("overlapping files", func(t *testing.T) {
		g, sink, resp := run(t, &guardServer{
			open:      []guardPR{{Number: 640, Ref: "scanner/fix-639", User: appBot, Files: []string{"Justfile"}}},
			headFiles: []string{"Justfile", "tests/unit/test_sysext_export_contract.py"},
		}, "Refs #638")
		if !resp.OK || resp.Number != 640 || resp.DuplicateReason != DuplicateReasonOverlappingFiles {
			t.Fatalf("result = %+v, want an overlapping-files refusal onto #640", resp)
		}
		if facts := sink.All(); len(facts) != 0 {
			t.Errorf("issue staged under a PR that does not reference it: %+v", facts)
		}
		var onIssue, onPR int
		for _, p := range g.commentPaths {
			switch {
			case strings.HasSuffix(p, "/issues/638/comments"):
				onIssue++
			case strings.HasSuffix(p, "/issues/640/comments"):
				onPR++
			}
		}
		if onIssue != 1 || onPR != 1 {
			t.Errorf("comment paths = %v, want one note on #638 and one redirect on #640", g.commentPaths)
		}
	})

	t.Run("note is not stacked on a retried request", func(t *testing.T) {
		g, _, _ := run(t, &guardServer{
			open:             []guardPR{{Number: 640, Ref: "scanner/fix-639", User: appBot, Files: []string{"Justfile"}}},
			headFiles:        []string{"Justfile", "tests/unit/test_sysext_export_contract.py"},
			existingComments: []string{duplicateGuardIssueNote("architect", "arch/refactor-sysext-export", 640)},
		}, "Refs #638")
		if len(g.commentPaths) != 0 {
			t.Errorf("comment paths = %v, want none (both notes already present)", g.commentPaths)
		}
	})

	t.Run("closes-issue refusal still stages the issue", func(t *testing.T) {
		g, sink, resp := run(t, &guardServer{
			open: []guardPR{{Number: 641, Ref: "scanner/fix-638", Body: "Fixes #638", User: appBot}},
		}, "Closes #638")
		if resp.DuplicateReason != DuplicateReasonClosesIssue {
			t.Fatalf("result = %+v, want a closes-issue refusal", resp)
		}
		facts := sink.All()
		if len(facts) != 1 || facts[0].Ref.Number != 638 || facts[0].PR == nil || facts[0].PR.Number != 641 {
			t.Errorf("facts = %+v, want #638 staged under #641", facts)
		}
		for _, p := range g.commentPaths {
			if strings.HasSuffix(p, "/issues/638/comments") {
				t.Errorf("closes-issue refusal left a guard note on the issue: %v", g.commentPaths)
			}
		}
	})
}

func TestDuplicateGuardIssueNote(t *testing.T) {
	got := duplicateGuardIssueNote("architect", "arch/x", 640)
	for _, want := range []string{duplicateGuardMarker, "Hive agent `architect`", "`arch/x`", "open PR #640", "still on the remote"} {
		if !strings.Contains(got, want) {
			t.Errorf("note missing %q:\n%s", want, got)
		}
	}
	if anon := duplicateGuardIssueNote("", "arch/x", 640); !strings.Contains(anon, "A hive agent pushed") {
		t.Errorf("anonymous note = %q", anon)
	}
}
