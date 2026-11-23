package github

import (
	"net/http"
	"os"
	"testing"

	"github.com/hivecommons/hive/pkg/prgate"
)

func servePRGateFixture(t *testing.T, name string, gotQuery *string) *Client {
	t.Helper()
	raw, err := os.ReadFile("testdata/pr_gate/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return prGateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if gotQuery != nil {
			*gotQuery = r.Method + " " + r.URL.Path + "?" + r.URL.RawQuery
		}
		_, _ = w.Write(raw)
	})
}

func TestListPRGateOpenPRsReadsOnePageOfOpenPRs(t *testing.T) {
	var got string
	c := servePRGateFixture(t, "open_pulls.json", &got)
	prs, err := c.ListPRGateOpenPRs(t.Context(), "example-org/example-repo", prgate.MaxBackstopPRsPerRepo)
	if err != nil {
		t.Fatal(err)
	}
	if want := "GET /repos/example-org/example-repo/pulls?direction=desc&per_page=100&sort=updated&state=open"; got != want {
		t.Errorf("request = %q, want %q", got, want)
	}
	want := []prgate.OpenPR{
		{Number: 12, HeadSHA: prGateTestHead, Title: "✨ add thing"},
		{Number: 11, HeadSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Title: "🐛 draft fix", Draft: true},
	}
	if len(prs) != len(want) {
		t.Fatalf("prs = %+v", prs)
	}
	for i := range want {
		if prs[i] != want[i] {
			t.Errorf("pr %d = %+v, want %+v", i, prs[i], want[i])
		}
	}
}

// Only allowlisted gate contexts are returned: DCO, dco and every other
// context on the head are dropped, so the backstop can never read (and so
// never adopt or compare) a context it may not post.
func TestReadPRGateHeadStatusesKeepsOnlyGateContexts(t *testing.T) {
	var got string
	c := servePRGateFixture(t, "combined_status.json", &got)
	have, err := c.ReadPRGateHeadStatuses(t.Context(), "example-org/example-repo", prGateTestHead)
	if err != nil {
		t.Fatal(err)
	}
	if want := "GET /repos/example-org/example-repo/commits/" + prGateTestHead + "/status?per_page=100"; got != want {
		t.Errorf("request = %q, want %q", got, want)
	}
	if len(have) != 1 {
		t.Fatalf("statuses = %+v, want only the gate context", have)
	}
	st := have[prgate.ContextVerifyShadow]
	if st.State != "success" || st.Description != "PR Type: success; PR Issue: success" {
		t.Errorf("gate status = %+v", st)
	}
}

func TestPRGateSweepWithoutAClientIsANoop(t *testing.T) {
	var p *PRGate
	if res := p.Sweep(t.Context(), nil, []string{"example-org/example-repo"}); res != (prgate.SweepResult{}) {
		t.Errorf("result = %+v", res)
	}
}
