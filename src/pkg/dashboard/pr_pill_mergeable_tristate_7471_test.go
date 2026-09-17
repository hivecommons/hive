package dashboard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// #2365 turned PullRequest.Mergeable from a bool into the tri-state string
// "yes" / "no" / "" so that never-fetched could not read as "no". Two readers
// were never moved off the bool: the frontend's PR pills tested truthiness
// ("no" is truthy in JavaScript, so every PR whose mergeability had been
// fetched wore the green ✓ — ~90% of the pills on a hosted hive while GitHub
// reported them DIRTY or BLOCKED), and status_builder's mergeableCount asserted
// a bool that the string could never satisfy. These tests pin the string
// comparison at every reader so the mismatch cannot come back (#7471).

// TestCollectAgentStats_MergeableCountTriState covers both shapes OpenPrs can
// carry: the github.PullRequest values buildRepos stores in-process (the
// production path — a bool assertion on a map never even reached the field
// there) and the JSON map a replayed snapshot decodes to. Only "yes" counts;
// "no" and unknown do not.
func TestCollectAgentStats_MergeableCountTriState(t *testing.T) {
	statsConfig := []any{
		map[string]any{"key": "mergeable", "source": "status", "field": "mergeableCount"},
	}
	cases := []struct {
		name string
		prs  []any
		want int
	}{
		{
			name: "structs as buildRepos stores them",
			prs: []any{
				github.PullRequest{Number: 1, Mergeable: github.MergeableYes, MergeableState: "clean"},
				github.PullRequest{Number: 2, Mergeable: github.MergeableYes, MergeableState: "unstable"},
				github.PullRequest{Number: 3, Mergeable: github.MergeableNo, MergeableState: "dirty"},
				github.PullRequest{Number: 4, Mergeable: github.MergeableNo, MergeableState: "blocked"},
				github.PullRequest{Number: 5}, // never fetched: MergeableUnknown
				&github.PullRequest{Number: 6, Mergeable: github.MergeableYes},
			},
			want: 3,
		},
		{
			name: "maps as a JSON round-trip decodes them",
			prs: []any{
				map[string]any{"number": 1, "mergeable": "yes"},
				map[string]any{"number": 2, "mergeable": "yes", "mergeable_state": "unstable"},
				map[string]any{"number": 3, "mergeable": "no"},
				map[string]any{"number": 4, "mergeable": ""},
				map[string]any{"number": 5},
				// The pre-#2365 bool is not a shape the wire carries any more;
				// counting it would let a stale producer inflate the number.
				// (One legacy true against two "yes" also keeps this case from
				// passing on the bool comparison by coincidence.)
				map[string]any{"number": 6, "mergeable": true},
			},
			want: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := &StatusPayload{
				Agents: []FrontendAgent{{Name: "scanner", StatsConfig: statsConfig}},
				Repos:  []FrontendRepo{{Name: "r", OpenPrs: tc.prs}},
			}
			got := CollectAgentStats(payload)["scanner"]["mergeable"]
			if got != tc.want {
				t.Fatalf("mergeableCount = %v, want %d", got, tc.want)
			}
		})
	}
}

// TestPullRequestMergeableWireShape pins what the frontend actually receives:
// the verdict is a string and the raw GitHub state rides beside it under
// mergeable_state, absent when it was never fetched. The pill tooltip and the
// disabled Queue auto-merge button read that field.
func TestPullRequestMergeableWireShape(t *testing.T) {
	raw, err := json.Marshal(github.PullRequest{Number: 7, Mergeable: github.MergeableNo, MergeableState: "dirty"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["mergeable"] != "no" {
		t.Fatalf("mergeable on the wire = %#v, want the string \"no\"", m["mergeable"])
	}
	if m["mergeable_state"] != "dirty" {
		t.Fatalf("mergeable_state on the wire = %#v, want \"dirty\"", m["mergeable_state"])
	}

	raw, err = json.Marshal(github.PullRequest{Number: 8})
	if err != nil {
		t.Fatal(err)
	}
	m = nil
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["mergeable"] != "" {
		t.Fatalf("unfetched mergeable on the wire = %#v, want \"\"", m["mergeable"])
	}
	if _, present := m["mergeable_state"]; present {
		t.Fatalf("unfetched mergeable_state should be omitted, got %#v", m["mergeable_state"])
	}
}

// TestPRPillMergeableReadersUseTriStateHelper is the structure half: every
// place index.html reads a PR's mergeable field goes through prMergeable(),
// and no truthiness test on the raw field remains. The behavioural half
// (TestPRPillMergeableHelperBehaviour) executes the helper; this one catches
// a fourth reader added later that bypasses it.
func TestPRPillMergeableReadersUseTriStateHelper(t *testing.T) {
	html := indexHTML(t)

	for _, bad := range []string{
		"p.mergeable ?",
		"filter(p => p.mergeable)",
		"(p.mergeable ? ' (merge eligible)'",
	} {
		if strings.Contains(html, bad) {
			t.Errorf("index.html still tests the tri-state mergeable string for truthiness: %q", bad)
		}
	}

	// The pill's icon and tint, and both mergeable counters, use the helper.
	if n := strings.Count(html, "prMergeable(p)"); n < 1 {
		t.Errorf("the PR pill does not derive its ✓ from prMergeable(p)")
	}
	if n := strings.Count(html, ".filter(prMergeable)"); n != 2 {
		t.Errorf("expected both mergeable counters to filter with prMergeable, found %d", n)
	}
	if !strings.Contains(html, "prMergeNote(p)") {
		t.Errorf("the PR pill tooltip does not use prMergeNote(p)")
	}

	// The Queue auto-merge action is withheld (rendered disabled, with the
	// GitHub state as the reason) when the verdict is "no" — and only then:
	// an unknown verdict means not fetched yet, never refused.
	if !strings.Contains(html, "const notMergeable = p.mergeable === 'no';") {
		t.Errorf("the Queue auto-merge action is not gated on mergeable === 'no'")
	}
	if !strings.Contains(html, `<button class="repo-pr-pill" disabled title="${esc(notMergeableTip)}">Queue auto-merge</button>`) {
		t.Errorf("a not-mergeable PR should render Queue auto-merge disabled with a reason")
	}
}

// TestPRPillMergeableHelperBehaviour executes prMergeable() and prMergeNote()
// under node against every value the wire can carry. The "no" row is the
// reproduction from #7471: it is truthy, and before the fix it drew the ✓.
func TestPRPillMergeableHelperBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the mergeable pill rule was NOT executed by this run; TestPRPillMergeableReadersUseTriStateHelper still ran")
	}

	html := indexHTML(t)
	script := jsFunc(t, html, "prMergeable") + "\n" +
		jsFunc(t, html, "prMergeNote") + "\n" + prPillMergeableAssertions

	path := filepath.Join(t.TempDir(), "pill.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path).CombinedOutput(); err != nil {
		t.Fatalf("PR pill mergeable check failed:\n%s", strings.TrimSpace(string(out)))
	}
}

const prPillMergeableAssertions = `
let fails = 0;
function check(name, cond) {
  if (!cond) { fails++; console.log('FAIL ' + name); }
}

// The verdict is a string. Only "yes" is eligible.
check('"yes" is mergeable', prMergeable({ mergeable: 'yes' }) === true);
check('"no" is NOT mergeable (it is truthy — the #7471 bug)', prMergeable({ mergeable: 'no' }) === false);
check('"" (unknown) is NOT mergeable', prMergeable({ mergeable: '' }) === false);
check('absent field is NOT mergeable', prMergeable({}) === false);
check('null PR is NOT mergeable', prMergeable(null) === false);
// The pre-#2365 bool is not on the wire; a stale producer must not light the pill.
check('legacy true is NOT mergeable', prMergeable({ mergeable: true }) === false);

// The counters filter with the helper directly, so it must work as a callback.
check('works as a filter callback',
  [{ mergeable: 'yes' }, { mergeable: 'no' }, { mergeable: '' }, {}].filter(prMergeable).length === 1);

// The tooltip names the GitHub state behind the verdict.
const clean = prMergeNote({ mergeable: 'yes', mergeable_state: 'clean' });
check('eligible + clean says eligible', clean.includes('merge eligible'));
check('eligible + clean names the state', clean.includes('clean'));

const unstable = prMergeNote({ mergeable: 'yes', mergeable_state: 'unstable' });
check('unstable is still eligible', unstable.includes('merge eligible'));
check('unstable warns that non-required checks may be red', unstable.includes('non-required checks may be red'));

const dirty = prMergeNote({ mergeable: 'no', mergeable_state: 'dirty' });
check('"no" does not say eligible', !dirty.includes('merge eligible'));
check('"no" says not mergeable', dirty.includes('not mergeable'));
check('"no" names the state', dirty.includes('dirty'));

const blocked = prMergeNote({ mergeable: 'no', mergeable_state: 'blocked' });
check('blocked names the state', blocked.includes('blocked'));

const unknown = prMergeNote({ mergeable: '' });
check('unknown does not say eligible', !unknown.includes('merge eligible'));
check('unknown does not say not mergeable', !unknown.includes('not mergeable'));
check('unknown says it is not yet known', unknown.includes('not yet known'));

// A payload predating mergeable_state still renders without "undefined".
const noState = prMergeNote({ mergeable: 'yes' });
check('missing state renders no undefined', !noState.includes('undefined'));
check('missing state still says eligible', noState.includes('merge eligible'));
check('missing state on "no" renders no undefined', !prMergeNote({ mergeable: 'no' }).includes('undefined'));

if (fails) { console.log(fails + ' check(s) failed'); process.exit(1); }
`
