package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const (
	rdApp    = "hive-app[bot]"
	rdPrev   = "1111111111111111111111111111111111111111"
	rdMerge  = "2222222222222222222222222222222222222222"
	rdBase   = "3333333333333333333333333333333333333333"
	rdMerge0 = "4444444444444444444444444444444444444444"
	rdBase0  = "5555555555555555555555555555555555555555"
)

type rdCommit struct {
	SHA       string
	Parents   []string
	Author    string
	Committer string
}

func rdHiveMerge(sha, firstParent, baseParent string) rdCommit {
	return rdCommit{SHA: sha, Parents: []string{firstParent, baseParent}, Author: rdApp, Committer: "web-flow"}
}

func rdCompareJSON(mergeBase string, commits []rdCommit, files ...string) map[string]any {
	cs := []map[string]any{}
	for _, c := range commits {
		ps := []map[string]any{}
		for _, p := range c.Parents {
			ps = append(ps, map[string]any{"sha": p})
		}
		cs = append(cs, map[string]any{"sha": c.SHA, "parents": ps, "author": map[string]any{"login": c.Author}, "committer": map[string]any{"login": c.Committer}})
	}
	fs := []map[string]any{}
	for _, f := range files {
		fs = append(fs, map[string]any{"filename": f, "additions": 100, "deletions": 1})
	}
	return map[string]any{"merge_base_commit": map[string]any{"sha": mergeBase}, "total_commits": len(cs), "commits": cs, "files": fs}
}

// rdServer answers GET .../compare/<base>...<head> from compares and records
// each compare asked for.
func rdServer(t *testing.T, compares map[string]map[string]any) (*Client, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/repos/acme/app/compare/"
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, prefix)
		mu.Lock()
		calls = append(calls, key)
		mu.Unlock()
		body, ok := compares[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, "acme", []string{"acme/app"})
	c.appBotLogin = rdApp
	return c, &calls
}

// rdSingleMerge is the projectbluefin/fsdk-containers#482 shape: one
// update-branch merge of main (rdBase) onto the approved head (rdPrev), the
// base changing two files the PR does not touch.
func rdSingleMerge() map[string]map[string]any {
	return map[string]map[string]any{
		rdPrev + "..." + rdMerge: rdCompareJSON(rdPrev, []rdCommit{rdHiveMerge(rdMerge, rdPrev, rdBase)}, "elements/valkey/valkey.bst", "docs/skills/index.md"),
		"main..." + rdMerge:      rdCompareJSON(rdBase, nil, "elements/cups/libcupsfilters.bst"),
		rdPrev + "..." + rdBase:  rdCompareJSON("0000000000000000000000000000000000000000", nil, "elements/valkey/valkey.bst", "docs/skills/index.md"),
	}
}

func TestReviewDeltaDiffRecognisesHiveBaseSync(t *testing.T) {
	c, calls := rdServer(t, rdSingleMerge())
	d := c.ReviewDeltaDiff(context.Background(), "acme/app", "main", rdPrev, rdMerge)
	if !d.Known || !d.BaseSync || len(d.Files) != 2 {
		t.Fatalf("diff = %+v, want a known base-sync with 2 files", d)
	}
	if strings.Join(d.PRFiles, ",") != "elements/cups/libcupsfilters.bst" || strings.Join(d.BaseFiles, ",") != "elements/valkey/valkey.bst,docs/skills/index.md" {
		t.Fatalf("PRFiles=%v BaseFiles=%v", d.PRFiles, d.BaseFiles)
	}
	if !d.BaseSyncOnly() {
		t.Fatal("planner predicate rejects the fixture")
	}
	if len(*calls) != 3 {
		t.Fatalf("compares = %v, want 3 (delta, PR side, base side)", *calls)
	}
}

// A verdict recorded under an abbreviated head (review.SameHeadSHA) still
// anchors the chain.
func TestReviewDeltaDiffBaseSyncAbbreviatedPreviousHead(t *testing.T) {
	compares := rdSingleMerge()
	short := rdPrev[:7]
	compares[short+"..."+rdMerge] = compares[rdPrev+"..."+rdMerge]
	compares[short+"..."+rdBase] = compares[rdPrev+"..."+rdBase]
	c, _ := rdServer(t, compares)
	if d := c.ReviewDeltaDiff(context.Background(), "acme/app", "main", short, rdMerge); !d.BaseSync {
		t.Fatalf("abbreviated reviewed head broke the base-sync proof: %+v", d)
	}
}

func TestReviewDeltaDiffBaseSyncTwoMerges(t *testing.T) {
	compares := map[string]map[string]any{
		rdPrev + "..." + rdMerge: rdCompareJSON(rdPrev, []rdCommit{rdHiveMerge(rdMerge0, rdPrev, rdBase0), rdHiveMerge(rdMerge, rdMerge0, rdBase)}, "a.txt"),
		"main..." + rdMerge:      rdCompareJSON(rdBase, nil, "pr.go"),
		rdPrev + "..." + rdBase:  rdCompareJSON("0000000000000000000000000000000000000000", []rdCommit{{SHA: rdBase0}, {SHA: rdBase}}, "a.txt"),
	}
	c, _ := rdServer(t, compares)
	if d := c.ReviewDeltaDiff(context.Background(), "acme/app", "main", rdPrev, rdMerge); !d.BaseSync {
		t.Fatalf("two hive merges of base commits not recognised: %+v", d)
	}
	// The earlier merged parent is not on the base below the last one.
	compares[rdPrev+"..."+rdBase] = rdCompareJSON("0000000000000000000000000000000000000000", []rdCommit{{SHA: rdBase}}, "a.txt")
	if d := c.ReviewDeltaDiff(context.Background(), "acme/app", "main", rdPrev, rdMerge); d.BaseSync {
		t.Fatalf("earlier merge of a non-base commit counted as a base-sync: %+v", d)
	}
}

func TestReviewDeltaDiffNotBaseSync(t *testing.T) {
	cases := map[string]func(map[string]map[string]any){
		"human-authored merge": func(m map[string]map[string]any) {
			c := rdHiveMerge(rdMerge, rdPrev, rdBase)
			c.Author = "octocat"
			m[rdPrev+"..."+rdMerge] = rdCompareJSON(rdPrev, []rdCommit{c}, "elements/valkey/valkey.bst", "docs/skills/index.md")
		},
		"other bot's merge": func(m map[string]map[string]any) {
			c := rdHiveMerge(rdMerge, rdPrev, rdBase)
			c.Author = "renovate[bot]"
			m[rdPrev+"..."+rdMerge] = rdCompareJSON(rdPrev, []rdCommit{c}, "elements/valkey/valkey.bst", "docs/skills/index.md")
		},
		"App-identity merge pushed with git": func(m map[string]map[string]any) {
			c := rdHiveMerge(rdMerge, rdPrev, rdBase)
			c.Committer = rdApp
			m[rdPrev+"..."+rdMerge] = rdCompareJSON(rdPrev, []rdCommit{c}, "elements/valkey/valkey.bst", "docs/skills/index.md")
		},
		"non-merge commit": func(m map[string]map[string]any) {
			c := rdHiveMerge(rdMerge, rdPrev, rdBase)
			c.Parents = []string{rdPrev}
			m[rdPrev+"..."+rdMerge] = rdCompareJSON(rdPrev, []rdCommit{c}, "elements/valkey/valkey.bst", "docs/skills/index.md")
		},
		"non-merge commit before the merge": func(m map[string]map[string]any) {
			m[rdPrev+"..."+rdMerge] = rdCompareJSON(rdPrev, []rdCommit{
				{SHA: rdMerge0, Parents: []string{rdPrev}, Author: rdApp, Committer: "web-flow"},
				rdHiveMerge(rdMerge, rdMerge0, rdBase),
			}, "elements/valkey/valkey.bst", "docs/skills/index.md")
		},
		"merge onto another commit than the reviewed head": func(m map[string]map[string]any) {
			m[rdPrev+"..."+rdMerge] = rdCompareJSON(rdPrev, []rdCommit{rdHiveMerge(rdMerge, rdMerge0, rdBase)}, "elements/valkey/valkey.bst", "docs/skills/index.md")
		},
		"commits stop short of the head": func(m map[string]map[string]any) {
			m[rdPrev+"..."+rdMerge] = rdCompareJSON(rdPrev, []rdCommit{rdHiveMerge(rdMerge0, rdPrev, rdBase)}, "elements/valkey/valkey.bst", "docs/skills/index.md")
		},
		"merged parent not on the base": func(m map[string]map[string]any) {
			m["main..."+rdMerge] = rdCompareJSON(rdBase0, nil, "elements/cups/libcupsfilters.bst")
		},
		"PR side unreadable": func(m map[string]map[string]any) {
			delete(m, "main..."+rdMerge)
		},
		"base side unreadable": func(m map[string]map[string]any) {
			delete(m, rdPrev+"..."+rdBase)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			compares := rdSingleMerge()
			mutate(compares)
			c, _ := rdServer(t, compares)
			d := c.ReviewDeltaDiff(context.Background(), "acme/app", "main", rdPrev, rdMerge)
			if !d.Known || d.BaseSync || d.PRFiles != nil || d.BaseFiles != nil {
				t.Fatalf("diff = %+v, want known, not a base-sync", d)
			}
		})
	}
	t.Run("no App login or no base ref: no extra compare", func(t *testing.T) {
		c, calls := rdServer(t, rdSingleMerge())
		if d := c.ReviewDeltaDiff(context.Background(), "acme/app", "", rdPrev, rdMerge); d.BaseSync || len(*calls) != 1 {
			t.Fatalf("no base ref: BaseSync=%v compares=%v", d.BaseSync, *calls)
		}
		// A merge whose author GitHub could not link to any account has an
		// empty login; an unset App login must not match it.
		unlinked := rdSingleMerge()
		m := rdHiveMerge(rdMerge, rdPrev, rdBase)
		m.Author = ""
		unlinked[rdPrev+"..."+rdMerge] = rdCompareJSON(rdPrev, []rdCommit{m}, "elements/valkey/valkey.bst", "docs/skills/index.md")
		c, calls = rdServer(t, unlinked)
		c.appBotLogin = ""
		if d := c.ReviewDeltaDiff(context.Background(), "acme/app", "main", rdPrev, rdMerge); d.BaseSync || len(*calls) != 1 {
			t.Fatalf("no App login: BaseSync=%v compares=%v", d.BaseSync, *calls)
		}
	})
}
