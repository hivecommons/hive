package testutil

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// commentMarkerBaseline is the number of functions under src/pkg/github
// (including automerge) that post an issue/PR comment through a raw create
// call without any idempotence marker in sight, at the commit that
// introduced this ratchet (hivecommons/hive#17067), measured with the same
// rules TestCommentMarkerRatchet uses.
//
// It is a ratchet, not a target: lower this constant when you convert an
// emitter — to a `<!-- hive:… -->` marker with EditIssueMarkerComment /
// findOwnEntryComment, or to a prstatus section — but it must never go UP.
// Every unmarked emitter is a comment the hive can post again on the next
// sweep; the 2026-11-07 spam audit traced 30–46 such comments a day to them.
//
// v6 carries one more than v5's 45: its own emitters (ApproveIssueRelease,
// inheritDeferredDecisions, noteSignoffAttributionDecision,
// postAdoptionEscalationNote) outnumber the v5-only ones it lacks.
const commentMarkerBaseline = 46

// commentEmitterCalls are the raw comment-create calls the ratchet looks
// for. Marker-aware helpers (EditIssueMarkerComment, UpsertPRStatusSection,
// PostNoteOrFold) are built on these and are what new code should call.
var commentEmitterCalls = []string{
	".Issues.CreateComment(",
	".CreateIssueComment(",
	".CreateIssueCommentBudgeted(",
}

// commentMarkerEvidence: a function containing any of these is treated as
// marker-aware — it carries a `<!-- hive…` marker literal, names a
// marker (`Marker`, `marker`), writes a prstatus section, or upserts.
var commentMarkerEvidence = []string{
	"<!-- hive",
	"arker",
	"prstatus.",
	"Upsert",
}

// commentMarkerAllowList names the transport primitives: the functions
// that ARE the raw create call every marked helper is built on, and so
// cannot carry a marker themselves. Keyed by `path:Recv.Func` relative
// to commentMarkerRoot. Add here only for a new primitive, never for a
// feature emitter.
var commentMarkerAllowList = map[string]bool{
	"client.go:Client.CreateIssueComment":                                 true,
	"issue_comment_budget.go:Client.CreateIssueCommentBudgeted":           true,
	"pr_status_comment.go:clientStatusForge.CreateComment":                true,
	"automerge/automerge_stage_labels.go:engineStatusForge.CreateComment": true,
}

// commentMarkerRoot is the tree the ratchet walks, relative to this package
// (src/internal/testutil): the GitHub client and the auto-merge engine,
// which between them own every bot comment the hive posts.
var commentMarkerRoot = filepath.Join("..", "..", "pkg", "github")

var (
	funcDeclRE     = regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?([A-Za-z0-9_]+)`)
	funcRecvDeclRE = regexp.MustCompile(`^func\s+\(\s*\w*\s*\*?([A-Za-z0-9_]+)\s*\)\s*([A-Za-z0-9_]+)`)
)

// commentEmitters scans one Go source file and returns the identities
// (`relpath:Recv.Func`) of functions that call a raw comment create,
// split into those with marker evidence and those without. Top-level
// `func ` lines delimit functions; `//` comment lines are not calls.
func commentEmitters(relPath string, src string) (marked, unmarked []string) {
	lines := strings.Split(src, "\n")
	var starts []int
	for i, l := range lines {
		if strings.HasPrefix(l, "func ") {
			starts = append(starts, i)
		}
	}
	for k, s := range starts {
		e := len(lines)
		if k+1 < len(starts) {
			e = starts[k+1]
		}
		body := lines[s:e]
		hit := false
		for _, l := range body[1:] {
			if strings.HasPrefix(strings.TrimSpace(l), "//") {
				continue
			}
			for _, call := range commentEmitterCalls {
				if strings.Contains(l, call) {
					hit = true
					break
				}
			}
			if hit {
				break
			}
		}
		if !hit {
			continue
		}
		name := ""
		if m := funcRecvDeclRE.FindStringSubmatch(lines[s]); m != nil {
			name = m[1] + "." + m[2]
		} else if m := funcDeclRE.FindStringSubmatch(lines[s]); m != nil {
			name = m[1]
		}
		ident := relPath + ":" + name
		text := strings.Join(body, "\n")
		evidence := false
		for _, mk := range commentMarkerEvidence {
			if strings.Contains(text, mk) {
				evidence = true
				break
			}
		}
		if evidence {
			marked = append(marked, ident)
		} else {
			unmarked = append(unmarked, ident)
		}
	}
	return marked, unmarked
}

// TestCommentMarkerRatchet fails when the number of unmarked raw comment
// emitters under pkg/github exceeds commentMarkerBaseline, and when an
// allow-listed primitive no longer exists (so the list cannot rot). Runs
// in well under a second: a file walk and substring scans.
func TestCommentMarkerRatchet(t *testing.T) {
	var marked, unmarked []string
	files := 0
	err := filepath.WalkDir(commentMarkerRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		files++
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(commentMarkerRoot, path)
		if err != nil {
			return err
		}
		m, u := commentEmitters(filepath.ToSlash(rel), string(data))
		marked = append(marked, m...)
		unmarked = append(unmarked, u...)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", commentMarkerRoot, err)
	}
	// Fail closed: no files means the wrong working directory.
	if files == 0 {
		t.Fatalf("no .go files found under %s — is the test running from src/internal/testutil?", commentMarkerRoot)
	}

	seen := map[string]bool{}
	var counted []string
	for _, id := range unmarked {
		seen[id] = true
		if !commentMarkerAllowList[id] {
			counted = append(counted, id)
		}
	}
	for id := range commentMarkerAllowList {
		if !seen[id] {
			t.Errorf("allow-listed %q is no longer an unmarked emitter; remove it from commentMarkerAllowList", id)
		}
	}
	sort.Strings(counted)
	total := len(counted)
	switch {
	case total > commentMarkerBaseline:
		t.Fatalf("%d functions under pkg/github post comments without a marker (%d marker-aware), above the baseline of %d.\n"+
			"Every bot comment must be idempotent across sweeps: carry a `<!-- hive:… -->` marker and edit in place "+
			"(EditIssueMarkerComment / findOwnEntryComment) or write a prstatus section (UpsertPRStatusSection). "+
			"If this is a deliberate one-shot, convert another emitter so the total does not grow.\n%s",
			total, len(marked), commentMarkerBaseline, listEmitters(counted))
	case total < commentMarkerBaseline:
		t.Logf("%d unmarked emitters, below the baseline of %d — lower commentMarkerBaseline in "+
			"internal/testutil/comment_marker_ratchet_test.go to %d so the ratchet keeps biting",
			total, commentMarkerBaseline, total)
	}
}

func listEmitters(ids []string) string {
	var b strings.Builder
	b.WriteString("unmarked emitters:\n")
	for _, id := range ids {
		fmt.Fprintf(&b, "  %s\n", id)
	}
	return b.String()
}

// The scanner's rules on synthetic sources: a raw create with no marker
// counts, marker evidence in the same function clears it, a commented-out
// call is not a call, and a receiver method is named Recv.Func.
func TestCommentEmittersRules(t *testing.T) {
	tests := []struct {
		name         string
		src          string
		wantMarked   []string
		wantUnmarked []string
	}{
		{
			name:         "raw create without marker",
			src:          "package x\n\nfunc (c *Client) note() {\n\tc.CreateIssueComment(ctx, repo, n, body)\n}\n",
			wantUnmarked: []string{"f.go:Client.note"},
		},
		{
			name:       "marker literal in function",
			src:        "package x\n\nfunc (c *Client) note() {\n\tbody := \"<!-- hive:x -->\" + s\n\tc.CreateIssueCommentBudgeted(ctx, repo, n, body)\n}\n",
			wantMarked: []string{"f.go:Client.note"},
		},
		{
			name:       "prstatus writer",
			src:        "package x\n\nfunc post() {\n\tprstatus.Write(ctx, f, k, key, text)\n\tf.c.client.Issues.CreateComment(ctx, o, r, n, cm)\n}\n",
			wantMarked: []string{"f.go:post"},
		},
		{
			name: "commented-out call is not an emitter",
			src:  "package x\n\nfunc (c *Client) note() {\n\t// c.CreateIssueComment(ctx, repo, n, body)\n}\n",
		},
		{
			name:         "two functions, one marked",
			src:          "package x\n\nfunc a() {\n\tc.CreateIssueComment(ctx, repo, n, body)\n}\n\nfunc b() {\n\tif marker != \"\" {\n\t\tc.CreateIssueComment(ctx, repo, n, body)\n\t}\n}\n",
			wantMarked:   []string{"f.go:b"},
			wantUnmarked: []string{"f.go:a"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			marked, unmarked := commentEmitters("f.go", tc.src)
			if strings.Join(marked, ",") != strings.Join(tc.wantMarked, ",") {
				t.Errorf("marked = %v, want %v", marked, tc.wantMarked)
			}
			if strings.Join(unmarked, ",") != strings.Join(tc.wantUnmarked, ",") {
				t.Errorf("unmarked = %v, want %v", unmarked, tc.wantUnmarked)
			}
		})
	}
}
