package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// --------------------------------------------------------------------------
// #5522 — findDigestComment must paginate.
//
// The bug: findDigestCommentDetail read ONE page (PerPage: 50) and stopped.
// On any target carrying more than ~50 comments the bot could not see its own
// digest, so the post path fell through to CREATE instead of EDIT, and every
// comment it created pushed the digest one position further out of reach.
//
// The fixtures below deliberately place the digest BEYOND the first page. A
// fixture with the marker at position 3 would pass against the unpaginated
// code and prove nothing.
// --------------------------------------------------------------------------

// commentPager serves a synthetic comment list with GitHub's Link header
// pagination, honouring page/per_page and direction=desc.
type commentPager struct {
	// bodies is the comment list in CHRONOLOGICAL order (oldest first), the
	// order GitHub returns for direction=asc.
	bodies []string
	// authors parallels bodies; empty means a plain human login.
	authors []string
	// pageRequests records the ?page= value of each request, so a test can
	// assert how many pages were actually walked.
	pageRequests []int
	// lastDirection records the direction parameter of the final request.
	lastDirection string
}

func (p *commentPager) handler(t *testing.T, basePath string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		perPage, _ := strconv.Atoi(q.Get("per_page"))
		if perPage <= 0 {
			perPage = 30
		}
		page, _ := strconv.Atoi(q.Get("page"))
		if page <= 0 {
			page = 1
		}
		p.pageRequests = append(p.pageRequests, page)
		p.lastDirection = q.Get("direction")

		// Build the ordered view the caller asked for.
		type item struct {
			id     int
			body   string
			author string
		}
		ordered := make([]item, 0, len(p.bodies))
		for i, b := range p.bodies {
			a := ""
			if i < len(p.authors) {
				a = p.authors[i]
			}
			ordered = append(ordered, item{id: 1000 + i, body: b, author: a})
		}
		if q.Get("direction") == "desc" {
			for i, j := 0, len(ordered)-1; i < j; i, j = i+1, j-1 {
				ordered[i], ordered[j] = ordered[j], ordered[i]
			}
		}

		start := (page - 1) * perPage
		if start > len(ordered) {
			start = len(ordered)
		}
		end := start + perPage
		if end > len(ordered) {
			end = len(ordered)
		}
		slice := ordered[start:end]

		out := make([]map[string]any, 0, len(slice))
		for _, it := range slice {
			user := map[string]any{"login": "humanreviewer", "type": "User"}
			if it.author != "" {
				user = map[string]any{"login": it.author, "type": "Bot"}
			}
			out = append(out, map[string]any{
				"id":   it.id,
				"body": it.body,
				"user": user,
			})
		}
		if end < len(ordered) {
			next := fmt.Sprintf("<%s%s?per_page=%d&page=%d>; rel=\"next\"",
				"http://"+r.Host, basePath, perPage, page+1)
			w.Header().Set("Link", next)
		}
		_ = json.NewEncoder(w).Encode(out)
	}
}

// digestAt builds n comments with the digest marker at index markerIdx
// (0-based, chronological). markerIdx < 0 means no digest at all.
func digestAt(n, markerIdx int) ([]string, []string) {
	bodies := make([]string, n)
	authors := make([]string, n)
	for i := range bodies {
		bodies[i] = fmt.Sprintf("ordinary human discussion comment #%d", i)
	}
	if markerIdx >= 0 && markerIdx < n {
		bodies[markerIdx] = advisoryDigestPrefix + " — findings for this cycle"
		authors[markerIdx] = "hive-app[bot]"
	}
	return bodies, authors
}

func newPagerServer(t *testing.T, org, repo string, issue int, p *commentPager) *httptest.Server {
	t.Helper()
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", org, repo, issue)
	mux := http.NewServeMux()
	mux.HandleFunc(path, p.handler(t, path))
	return httptest.NewServer(mux)
}

// TestFindDigestComment_MarkerBeyondFirstPage is the direct regression test
// for #5522. 260 comments with the digest at chronological index 130 — the
// exact middle, so it is beyond the first page in BOTH orderings: page 1
// oldest-first (the old PerPage:50 ascending read) covers 0..49, and page 1
// newest-first covers 259..160. Only a scan that actually turns the page can
// reach it. A fixture with the marker near either end would be found by the
// unpaginated code and would prove nothing.
func TestFindDigestComment_MarkerBeyondFirstPage(t *testing.T) {
	org, repo, issue := "testorg", "testrepo", 686
	const markerIdx = 130
	bodies, authors := digestAt(260, markerIdx)
	p := &commentPager{bodies: bodies, authors: authors}
	server := newPagerServer(t, org, repo, issue, p)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.appAuth = &AppAuth{}
	c.appBotLogin = "hive-app[bot]"

	id, err := c.findDigestComment(context.Background(), org, repo, issue)
	if err != nil {
		t.Fatalf("findDigestComment: %v", err)
	}
	want := 1000 + markerIdx
	if id != want {
		t.Fatalf("comment id = %d, want %d — the digest sits at chronological index 130 of 260, "+
			"so a single-page scan cannot see it and the caller would CREATE a duplicate", id, want)
	}
	if len(p.pageRequests) < 2 {
		t.Errorf("pages requested = %v, want more than one — a marker this deep is unreachable in one page", p.pageRequests)
	}
}

// TestFindDigestComment_NewestFirstFindsRecentInOnePage pins the ordering
// choice: the bot's own digest is overwhelmingly likely to be the most recent
// comment, so the common case must still cost exactly one API call even on a
// target with hundreds of comments.
func TestFindDigestComment_NewestFirstFindsRecentInOnePage(t *testing.T) {
	org, repo, issue := "testorg", "testrepo", 686
	bodies, authors := digestAt(400, 399) // newest comment is the digest
	p := &commentPager{bodies: bodies, authors: authors}
	server := newPagerServer(t, org, repo, issue, p)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.appAuth = &AppAuth{}
	c.appBotLogin = "hive-app[bot]"

	id, err := c.findDigestComment(context.Background(), org, repo, issue)
	if err != nil {
		t.Fatalf("findDigestComment: %v", err)
	}
	if id != 1000+399 {
		t.Fatalf("comment id = %d, want %d", id, 1000+399)
	}
	if len(p.pageRequests) != 1 {
		t.Errorf("pages requested = %v, want exactly 1 — a recent digest must not cost a full walk", p.pageRequests)
	}
	if p.lastDirection != "desc" {
		t.Errorf("direction = %q, want %q — the scan must run newest-first", p.lastDirection, "desc")
	}
}

// TestFindDigestComment_ScanIsBounded pins the cap stated in
// advisoryDigestScanMaxPages. A target with far more comments than the cap
// allows must stop at the cap rather than walking unbounded history every
// ~60s cycle, and must report "not found" (id 0) so the caller CREATEs a new
// digest that every later cycle then finds on page 1.
func TestFindDigestComment_ScanIsBounded(t *testing.T) {
	org, repo, issue := "testorg", "testrepo", 686
	total := advisoryDigestScanMaxPages*advisoryDigestCommentsPerPage + 500
	bodies, authors := digestAt(total, 0) // digest is the OLDEST comment
	p := &commentPager{bodies: bodies, authors: authors}
	server := newPagerServer(t, org, repo, issue, p)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.appAuth = &AppAuth{}
	c.appBotLogin = "hive-app[bot]"

	id, err := c.findDigestComment(context.Background(), org, repo, issue)
	if err != nil {
		t.Fatalf("findDigestComment: %v", err)
	}
	if id != 0 {
		t.Errorf("comment id = %d, want 0 — a digest beyond the cap is reported absent", id)
	}
	if len(p.pageRequests) != advisoryDigestScanMaxPages {
		t.Errorf("pages requested = %d, want exactly %d (the cap)", len(p.pageRequests), advisoryDigestScanMaxPages)
	}
}

// TestFindDigestComment_ExhaustsWithoutMarker is the negative control: a
// paginated scan that finds nothing must return 0 and must not report an
// error, and must stop when the list is exhausted rather than at the cap.
func TestFindDigestComment_ExhaustsWithoutMarker(t *testing.T) {
	org, repo, issue := "testorg", "testrepo", 686
	bodies, authors := digestAt(250, -1)
	p := &commentPager{bodies: bodies, authors: authors}
	server := newPagerServer(t, org, repo, issue, p)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.appAuth = &AppAuth{}
	c.appBotLogin = "hive-app[bot]"

	id, err := c.findDigestComment(context.Background(), org, repo, issue)
	if err != nil {
		t.Fatalf("findDigestComment: %v", err)
	}
	if id != 0 {
		t.Errorf("comment id = %d, want 0", id)
	}
	// 250 comments at 100/page = 3 pages, well under the cap.
	if len(p.pageRequests) != 3 {
		t.Errorf("pages requested = %d, want 3 (list exhausted, not capped)", len(p.pageRequests))
	}
}

// TestFindDigestComment_BotFallbackSurvivesPagination pins that the
// not-provably-ours bot fallback (a slug mismatch between config and the real
// App) still works across pages: a fallback seen on page 1 must not be
// discarded when later pages yield nothing, or the bot would create a
// duplicate every cycle on a misconfigured slug.
func TestFindDigestComment_BotFallbackSurvivesPagination(t *testing.T) {
	org, repo, issue := "testorg", "testrepo", 686
	bodies, authors := digestAt(250, 249) // newest, but authored by ANOTHER bot
	authors[249] = "some-other-app[bot]"
	p := &commentPager{bodies: bodies, authors: authors}
	server := newPagerServer(t, org, repo, issue, p)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.appAuth = &AppAuth{}
	c.appBotLogin = "hive-app[bot]" // does NOT match the comment author

	id, err := c.findDigestComment(context.Background(), org, repo, issue)
	if err != nil {
		t.Fatalf("findDigestComment: %v", err)
	}
	if id != 1000+249 {
		t.Errorf("comment id = %d, want %d — the bot-authored fallback found on page 1 "+
			"must survive the rest of the scan", id, 1000+249)
	}
}

// TestFindDigestComment_HumanAuthoredIsSkippedAcrossPages pins that the
// App-can-never-edit rule (#1927 / kalantar-msb/soft-reflective#1) still
// applies once the scan is paginated: a human-authored digest comment beyond
// the first page must be skipped, not adopted.
func TestFindDigestComment_HumanAuthoredIsSkippedAcrossPages(t *testing.T) {
	org, repo, issue := "testorg", "testrepo", 686
	bodies, authors := digestAt(250, 5) // deep in history
	authors[5] = ""                     // human author
	p := &commentPager{bodies: bodies, authors: authors}
	server := newPagerServer(t, org, repo, issue, p)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.appAuth = &AppAuth{}
	c.appBotLogin = "hive-app[bot]"

	id, err := c.findDigestComment(context.Background(), org, repo, issue)
	if err != nil {
		t.Fatalf("findDigestComment: %v", err)
	}
	if id != 0 {
		t.Errorf("comment id = %d, want 0 — an App can never edit a human-authored comment", id)
	}
}

// TestFindDigestComment_PerPageIsMaximised guards against a silent regression
// to a small page size, which would multiply the round trips needed to cover
// the same history.
func TestFindDigestComment_PerPageIsMaximised(t *testing.T) {
	if advisoryDigestCommentsPerPage != 100 {
		t.Errorf("advisoryDigestCommentsPerPage = %d, want 100 (GitHub's maximum)", advisoryDigestCommentsPerPage)
	}
	org, repo, issue := "testorg", "testrepo", 686
	var gotPerPage string
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/%d/comments", org, repo, issue),
		func(w http.ResponseWriter, r *http.Request) {
			gotPerPage = r.URL.Query().Get("per_page")
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	if _, err := c.findDigestComment(context.Background(), org, repo, issue); err != nil {
		t.Fatalf("findDigestComment: %v", err)
	}
	if gotPerPage != strconv.Itoa(advisoryDigestCommentsPerPage) {
		t.Errorf("per_page = %q, want %q", gotPerPage, strconv.Itoa(advisoryDigestCommentsPerPage))
	}
	_ = strings.TrimSpace("")
}
