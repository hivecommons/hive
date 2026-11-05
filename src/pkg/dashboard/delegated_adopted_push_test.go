package dashboard

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/utah#919: a fork PR the hive adopted and then recorded
// fork-rehome-refused on was decided "hive's fixer: … then it merges" at
// 19:52:57Z, and four seconds later the hive said the opposite. With the
// refusal recorded on the head, the delegate keeps the park for the person
// who asked, posts no `/hive decision`, and names why.
func TestDelegatedAdoptedForkRehomeRefusedStaysWithPerson(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fork    bool
		refused bool
		noEdits bool
		rule    string
	}{
		{name: "same-repo adopted branch: the hive's fixer", rule: delegatedecision.RuleAdoptedFixer},
		{name: "fork with maintainer edits: the hive's fixer", fork: true, rule: delegatedecision.RuleAdoptedFixer},
		{name: "fork-rehome-refused on the head: the person", fork: true, refused: true, rule: delegatedecision.RuleAdoptedPushRefused},
		{name: "fork without maintainer edits: the person", fork: true, noEdits: true, rule: delegatedecision.RuleAdoptedPushRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := &repoItemDecisionMock{prForeign: true, prFork: tc.fork, prNoMaintainerEdits: tc.noEdits, prHeadSHA: "f0b5e2f", prLabels: []string{"stage/085-needs-decision"}}
			srv := newDelegatedServer(t, mock)
			srv.stageStore = stagelabels.NewStore("", time.Now)
			key := stagelabels.Key("testorg/testrepo", 8)
			srv.stageStore.Observe(key, stagelabels.AdoptedByHive, "contributor-lane:adopted-by-hive")
			if tc.refused {
				srv.stageStore.MarkForkRehomeRefused(key, "f0b5e2f")
			}
			w := postDelegated(t, srv, 8, "writer", `{"type":"pr","reason":"contributor-lane:adopted-by-hive reviewer-verdict:requires_human"}`, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
			}
			job := decodeDelegatedJob(t, w)
			if job.Decision == nil || job.Decision.Rule != tc.rule {
				t.Fatalf("decision = %+v err=%s", job.Decision, job.Error)
			}
			posted := strings.Join(mock.comments, "\n")
			if tc.rule == delegatedecision.RuleAdoptedPushRefused {
				if strings.Contains(posted, "/hive decision") || strings.Contains(posted, "hive's fixer") || !job.Decision.KeepsPark {
					t.Fatalf("a refused push must post no direction for the fixer: keepsPark=%v comments=%v", job.Decision.KeepsPark, mock.comments)
				}
				if why := map[bool]string{true: "fork-rehome-refused", false: "maintainers"}[tc.refused]; !strings.Contains(job.Decision.Rationale, why) {
					t.Fatalf("rationale %q does not say why", job.Decision.Rationale)
				}
			}
		})
	}
}
