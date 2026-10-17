package github

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// The #12733 shape: every coverage finding on a bundled Action cites
// `dist/index.js` and `coverage/lcov.info`. Sharing only those says nothing
// about a shared root cause, so three open siblings on them must NOT make
// the fourth finding fold — it files as its own issue.
func TestIssueRequestWatcher_GeneratedArtefactsDoNotFold(t *testing.T) {
	m := &consolidationMock{}
	srv := consolidationMockServer(t, []openTwin{
		{number: 470, author: testBot, title: "[quality] coverage gap in labeler", body: "src/labeler.ts is untested; see dist/index.js and coverage/lcov.info"},
		{number: 468, author: testBot, title: "[quality] coverage gap in milestone", body: "src/milestone.ts has no unit test; dist/index.js:1200, coverage/lcov.info"},
		{number: 466, author: testBot, title: "[quality] coverage gap in lgtm", body: "src/lgtm.ts uncovered per coverage/lcov.info; compiled into dist/index.js"},
	}, m)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = testBot

	_, res := fileRequest(t, c, withIssueDir(t), IssueRequest{
		Repo: "o/r", Agent: "quality",
		Title: "[quality] coverage gap in approve",
		Body:  "src/approve.ts is untested per coverage/lcov.info; shipped in dist/index.js:3310",
	})
	if m.created != 1 || len(m.commentedOn) != 0 || res.Consolidated {
		t.Fatalf("created=%d comments=%v result=%+v, want a fresh issue — dist/index.js and lcov.info are artefacts, not evidence", m.created, m.commentedOn, res)
	}
}

func TestIsGeneratedArtefactRef(t *testing.T) {
	yes := []string{"dist/index.js", "coverage/lcov.info", "coverage-e2e/lcov.info", "build/app.min.js",
		"vendor/golang.org/x/net/http2/frame.go", "node_modules/left-pad/index.js", "lcov.info",
		"web/static/app.min.js", "pkg/api/v1/api.pb.go", "package-lock.json", "a/b/yarn.lock", "out/main.bundle.js", ".next/server/page.js"}
	for _, p := range yes {
		if !isGeneratedArtefactRef(p) {
			t.Errorf("%q should be a generated artefact", p)
		}
	}
	no := []string{"src/index.js", "pkg/github/client.go", "docs/coverage.md", "cmd/build.go", "distribution/notes.md", "scripts/build-image.sh", "test/fixtures/out.json", "build/esp.sh"}
	for _, p := range no {
		if isGeneratedArtefactRef(p) {
			t.Errorf("%q should NOT be a generated artefact", p)
		}
	}
	set := issueFileRefSet("src/x.ts and dist/index.js and coverage/lcov.info")
	if len(set) != 1 || !set["src/x.ts"] {
		t.Errorf("issueFileRefSet = %v, want only src/x.ts", set)
	}
}

// A same-component stream folds into one tracker; the second and later folds
// must EDIT the existing fold comment, not post a new one — an edit does not
// notify the tracker's subscribers (#12733).
func TestIssueRequestWatcher_FoldAppendsToExistingFoldComment(t *testing.T) {
	m := &consolidationMock{}
	srv := consolidationMockServer(t, []openTwin{
		{number: 63, author: testBot, title: "[sec-check] CDATA bypass", body: "scripts/lib/gate.mjs and test/fixtures/cdata.svg"},
		{number: 61, author: testBot, title: "[sec-check] DOCTYPE bypass", body: "scripts/lib/gate.mjs and test/fixtures/doctype.svg"},
		{number: 58, author: testBot, title: "[sec-check] CSS escape bypass", body: "scripts/lib/gate.mjs and test/fixtures/escape.svg"},
	}, m)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = testBot
	dir := withIssueDir(t)

	for _, req := range []IssueRequest{
		{Repo: "o/r", Agent: "sec-check", Title: "[sec-check] image-set() bypass", Body: "scripts/lib/gate.mjs:88 misses image-set(); test/fixtures/imageset.svg"},
		{Repo: "o/r", Agent: "sec-check", Title: "[sec-check] url() bypass", Body: "scripts/lib/gate.mjs:91 misses url(); test/fixtures/url.svg"},
		{Repo: "o/r", Agent: "sec-check", Title: "[sec-check] @import bypass", Body: "scripts/lib/gate.mjs:95 misses @import; test/fixtures/import.svg"},
	} {
		_, res := fileRequest(t, c, dir, req)
		if !res.OK || !res.Consolidated || res.Number != 58 {
			t.Fatalf("result = %+v, want consolidated into #58", res)
		}
	}
	if m.created != 0 || len(m.commentedOn) != 1 || m.commentedOn[0] != 58 {
		t.Fatalf("created=%d comments=%v, want exactly ONE new comment on #58", m.created, m.commentedOn)
	}
	if m.edited != 2 {
		t.Fatalf("edited=%d, want the two later folds appended by edit", m.edited)
	}
	body := m.commentBodies[0]
	if strings.Count(body, hiveFindingFoldedMarker) != 1 {
		t.Errorf("fold comment should carry the marker once:\n%s", body)
	}
	for _, want := range []string{"image-set() bypass", "url() bypass", "@import bypass"} {
		if !strings.Contains(body, want) {
			t.Errorf("fold comment lacks %q:\n%s", want, body)
		}
	}
}

// A failed append keeps the request queued, like a failed post.
func TestIssueRequestWatcher_FoldAppendFailureRetries(t *testing.T) {
	m := &consolidationMock{}
	srv := consolidationMockServer(t, []openTwin{
		{number: 63, author: testBot, title: "[sec-check] a", body: "scripts/lib/gate.mjs and test/a.svg"},
		{number: 61, author: testBot, title: "[sec-check] b", body: "scripts/lib/gate.mjs and test/b.svg"},
		{number: 58, author: testBot, title: "[sec-check] c", body: "scripts/lib/gate.mjs and test/c.svg"},
	}, m)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = testBot
	dir := withIssueDir(t)

	fileRequest(t, c, dir, IssueRequest{Repo: "o/r", Agent: "sec-check", Title: "[sec-check] d", Body: "scripts/lib/gate.mjs:1 test/d.svg"})
	m.editStatus = http.StatusBadGateway
	reqPath, res := fileRequest(t, c, dir, IssueRequest{Repo: "o/r", Agent: "sec-check", Title: "[sec-check] e", Body: "scripts/lib/gate.mjs:2 test/e.svg"})
	if res.OK || m.created != 0 || len(m.commentedOn) != 1 {
		t.Fatalf("result=%+v created=%d comments=%v, want a retryable failure with no new post", res, m.created, m.commentedOn)
	}
	if !fileExists(reqPath) {
		t.Error("request must stay queued for retry")
	}
}

// Relay `comment` kind: a verification marker already on the issue means the
// agent re-verified on a later kick; acknowledge without posting (#12733). A
// different head SHA in the marker is a new verification and posts.
func TestIssueRequestWatcher_VerificationCommentPostedOnce(t *testing.T) {
	m := &consolidationMock{}
	srv := consolidationMockServer(t, nil, m)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = testBot
	dir := withIssueDir(t)

	first := "<!-- hive-verification: head=abc123 -->\n<!-- hive-466-verified -->\nStill reproduces at abc123."
	for i := 0; i < 2; i++ {
		_, res := fileRequest(t, c, dir, IssueRequest{Repo: "o/r", Agent: "quality", Kind: "comment", Number: 466, Body: first})
		if !res.OK || res.Number != 466 {
			t.Fatalf("attempt %d: result = %+v", i+1, res)
		}
		if i == 1 && !res.AlreadyExisted {
			t.Errorf("second verification should be acknowledged as already existing: %+v", res)
		}
	}
	if len(m.commentedOn) != 1 {
		t.Fatalf("comments=%v, want the verification posted once", m.commentedOn)
	}
	_, res := fileRequest(t, c, dir, IssueRequest{Repo: "o/r", Agent: "quality", Kind: "comment", Number: 466,
		Body: "<!-- hive-verification: head=def456 -->\nStill reproduces at def456."})
	if !res.OK || res.AlreadyExisted || len(m.commentedOn) != 2 {
		t.Fatalf("new head SHA should post: result=%+v comments=%v", res, m.commentedOn)
	}
	// An unmarked comment is never deduped.
	_, res = fileRequest(t, c, dir, IssueRequest{Repo: "o/r", Agent: "quality", Kind: "comment", Number: 466, Body: "plain note"})
	if !res.OK || len(m.commentedOn) != 3 {
		t.Fatalf("plain comment should post: result=%+v comments=%v", res, m.commentedOn)
	}
}

func TestVerificationMarker(t *testing.T) {
	cases := map[string]string{
		"<!-- hive-466-verified -->\nok":                    "<!-- hive-466-verified -->",
		"x <!-- hive-verification: head=abc -->":            "<!-- hive-verification: head=abc -->",
		"<!-- hive-verified -->":                            "<!-- hive-verified -->",
		"<!-- hive-finding-folded -->":                      "",
		"<!-- hive:claim who=quality kind=agent -->":        "",
		"<!-- hive-finding: HASH -->":                       "",
		"verified it manually, no marker":                   "",
		"<!-- hive-quality-verification sha=1 --> trailing": "<!-- hive-quality-verification sha=1 -->",
	}
	for in, want := range cases {
		if got := verificationMarker(in); got != want {
			t.Errorf("verificationMarker(%q) = %q, want %q", in, got, want)
		}
	}
}

// Relay `claim` kind: a claim whose only predecessor is a fresh hive
// verification is the re-verify loop; it is refused with a reason. A human
// comment after the verification is a state change and lets it through.
func TestIssueRequestWatcher_ClaimRefusedRightAfterVerification(t *testing.T) {
	m := &consolidationMock{}
	srv := consolidationMockServer(t, nil, m)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = testBot
	dir := withIssueDir(t)

	fileRequest(t, c, dir, IssueRequest{Repo: "o/r", Agent: "quality", Kind: "comment", Number: 466, Body: "<!-- hive-466-verified -->\nverified"})
	reqPath, res := fileRequest(t, c, dir, IssueRequest{Repo: "o/r", Agent: "quality", Kind: "claim", Number: 466})
	if res.OK || !strings.Contains(res.Error, "verified this issue") {
		t.Fatalf("claim after verification should be refused: %+v", res)
	}
	if !fileExists(reqPath + ".denied") {
		t.Error("refused claim should be parked as .denied, not retried")
	}

	m.commentedOn = append(m.commentedOn, 466)
	m.commentBodies = append(m.commentBodies, "maintainer: please fix this one")
	verified, err := c.issueLatestCommentIsRecentVerification(context.Background(), "o/r", 466, time.Now())
	if err != nil || verified {
		t.Fatalf("a later comment is a state change: verified=%v err=%v", verified, err)
	}
}

// The per-issue daily budget caps unattended comments and refunds a failed
// post; the relay reports the suppression as a denial, not a retry.
func TestIssueCommentBudget(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	b := &issueCommentBudget{perDay: 2, now: func() time.Time { return now }}
	for i := 0; i < 2; i++ {
		if ok, _ := b.allow("o/r#1"); !ok {
			t.Fatalf("comment %d should be within budget", i+1)
		}
	}
	if ok, used := b.allow("o/r#1"); ok || used != 2 {
		t.Fatalf("third comment should be suppressed (ok=%v used=%d)", ok, used)
	}
	if ok, _ := b.allow("o/r#2"); !ok {
		t.Fatal("budget is per issue")
	}
	b.refund("o/r#1")
	if ok, _ := b.allow("o/r#1"); !ok {
		t.Fatal("a refunded charge frees a slot")
	}
	now = now.Add(issueCommentBudgetWindow + time.Minute)
	if ok, used := b.allow("o/r#1"); !ok || used != 0 {
		t.Fatalf("window rolled over: ok=%v used=%d", ok, used)
	}
	unlimited := &issueCommentBudget{perDay: -1}
	for i := 0; i < 50; i++ {
		if ok, _ := unlimited.allow("k"); !ok {
			t.Fatal("non-positive cap disables the budget")
		}
	}

	m := &consolidationMock{}
	srv := consolidationMockServer(t, nil, m)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = testBot
	c.commentBudget.perDay = 2
	dir := withIssueDir(t)
	for i := 0; i < 2; i++ {
		if _, res := fileRequest(t, c, dir, IssueRequest{Repo: "o/r", Agent: "quality", Kind: "comment", Number: 7, Body: "note " + string(rune('a'+i))}); !res.OK {
			t.Fatalf("comment %d: %+v", i+1, res)
		}
	}
	reqPath, res := fileRequest(t, c, dir, IssueRequest{Repo: "o/r", Agent: "quality", Kind: "comment", Number: 7, Body: "note c"})
	if res.OK || !strings.Contains(res.Error, "budget") || len(m.commentedOn) != 2 {
		t.Fatalf("third relay comment should be refused by the budget: result=%+v comments=%v", res, m.commentedOn)
	}
	if !fileExists(reqPath + ".denied") {
		t.Error("budget refusal should park the request, not retry it")
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
