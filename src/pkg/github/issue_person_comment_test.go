package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// The newest person comment on a reporter_triage issue is read for the
// dashboard only: bots, slash commands and hive markers are skipped, the
// body is reduced to its mentions, an unchanged issue is never read twice,
// and a pass spends a bounded number of reads.

var personCommentUpdated = time.Date(2026, 11, 16, 15, 0, 0, 0, time.UTC)

type personWireComment struct {
	ID                int               `json:"id"`
	Body              string            `json:"body"`
	User              map[string]string `json:"user"`
	AuthorAssociation string            `json:"author_association"`
	CreatedAt         time.Time         `json:"created_at"`
}

func personDay(d int) time.Time { return time.Date(2026, 11, d, 12, 0, 0, 0, time.UTC) }

// personPages records the pages the server was asked for.
type personPages struct {
	mu    sync.Mutex
	pages []string
}

func (p *personPages) add(page int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pages = append(p.pages, strconv.Itoa(page))
}

// take returns the pages asked for since the last take.
func (p *personPages) take() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := strings.Join(p.pages, ",")
	p.pages = nil
	return out
}

// personCommentServer serves issue 655's comments 100 to a page with a
// GitHub Link header, and counts the reads and the pages asked for.
func personCommentServer(t *testing.T, comments []personWireComment) (*httptest.Server, *atomic.Int32, *personPages) {
	t.Helper()
	var fetches atomic.Int32
	pages := &personPages{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/server/issues/655/comments", func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		pages.add(page)
		last := (len(comments)-1)/issuePersonCommentPageSize + 1
		if page < last {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=%d&per_page=100>; rel="next", <http://%s%s?page=%d&per_page=100>; rel="last"`,
				r.Host, r.URL.Path, page+1, r.Host, r.URL.Path, last))
		}
		out := []personWireComment{}
		if start := (page - 1) * issuePersonCommentPageSize; start < len(comments) {
			out = comments[start:min(start+issuePersonCommentPageSize, len(comments))]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, &fetches, pages
}

func personCommentIssue(count int, updated time.Time) *gh.Issue {
	return &gh.Issue{
		Number:    gh.Ptr(655),
		Comments:  gh.Ptr(count),
		UpdatedAt: &gh.Timestamp{Time: updated},
		User:      &gh.User{Login: gh.Ptr("reporter")},
	}
}

func personWire(id int, login, association, body string, day int) personWireComment {
	return personWireComment{ID: id, Body: body, User: map[string]string{"login": login}, AuthorAssociation: association, CreatedAt: personDay(day)}
}

func TestLatestPersonCommentSkipsBotsCommandsAndMarkers(t *testing.T) {
	bot := personWire(4, "renovate", "NONE", "bump", 18)
	bot.User["type"] = "Bot"
	server, fetches, _ := personCommentServer(t, []personWireComment{
		personWire(1, "reporter", "NONE", "first", 10),
		personWire(2, "maintainer", "member", "Thanks for filing, @reporter. @jeefy, which fix do you want? mail a@example.com", 16),
		personWire(3, "hive-app[bot]", "NONE", "<!-- hive:reporter-trust-wait repo=acme/server -->", 17),
		bot,
		personWire(5, "hive-app", "NONE", "the hive's own account", 19),
		personWire(6, "maintainer", "MEMBER", "\n  /assign @jeefy", 20),
		personWire(7, "hive-pat-user", "MEMBER", "<!-- hive:claim alice -->\nclaimed", 21),
	})
	c := newTestClient(t, server, "acme", []string{"server"})
	c.appBotLogin = "hive-app"

	got := c.latestPersonComment(context.Background(), "acme", "server", personCommentIssue(7, personCommentUpdated), nil, false, nil)
	if got == nil {
		t.Fatal("no person comment picked")
	}
	if got.Login != "maintainer" || got.AuthorAssociation != "MEMBER" || !got.CreatedAt.Equal(personDay(16)) {
		t.Fatalf("picked %+v, want the maintainer's MEMBER reply of Nov 16 (the slash command and the marker are skipped)", got)
	}
	if strings.Join(got.Mentions, ",") != "reporter,jeefy" {
		t.Fatalf("mentions = %v, want [reporter jeefy] (an e-mail address is not a mention)", got.Mentions)
	}
	if fetches.Load() != 1 {
		t.Fatalf("fetched %d times, want one read", fetches.Load())
	}
}

// Mentions inside fenced code, inline code and quoted lines are not asks,
// and neither are bots.
func TestPersonCommentMentionsSkipCodeQuotesAndBots(t *testing.T) {
	c := &Client{appBotLogin: "hive-app"}
	body := strings.Join([]string{
		"> @reporter wrote: it breaks",
		"Try this:",
		"```java",
		"@Override",
		"```",
		"or `@Inject` here; @dependabot rebase, @renovate[bot] and @hive-app can wait.",
		"@Jeefy, can you pick one?",
	}, "\n")
	if got := strings.Join(c.personCommentMentions(body), ","); got != "jeefy" {
		t.Fatalf("mentions = %q, want only jeefy", got)
	}
	if !personCommentIsReply("Thanks\n/assign") || personCommentIsReply("  \n/hive decision proceed") || personCommentIsReply("ok <!-- hive:x -->") {
		t.Fatal("only a first line starting with / or a hive marker makes a comment not a reply")
	}
}

// The cache is keyed on repo#number, updated_at and the comment count: an
// unchanged issue is never read again (a "no person comment" result
// included), and new activity or a new count reads it once more.
func TestLatestPersonCommentCacheKeyedOnUpdatedAtAndCount(t *testing.T) {
	server, fetches, _ := personCommentServer(t, []personWireComment{
		personWire(1, "hive-app[bot]", "NONE", "<!-- hive:reporter-trust-wait -->", 16),
	})
	c := newTestClient(t, server, "acme", []string{"server"})
	read := func(count int, updated time.Time) *IssuePersonComment {
		return c.latestPersonComment(context.Background(), "acme", "server", personCommentIssue(count, updated), nil, false, nil)
	}
	for i := 0; i < 3; i++ {
		if got := read(1, personCommentUpdated); got != nil {
			t.Fatalf("pass %d: a bot-only thread has no person comment, got %+v", i, got)
		}
	}
	if fetches.Load() != 1 {
		t.Fatalf("same updated_at and count must hit the cache, fetched %d times", fetches.Load())
	}
	read(1, personCommentUpdated.Add(time.Minute))
	if fetches.Load() != 2 {
		t.Fatalf("new activity must read again, fetched %d times", fetches.Load())
	}
	read(2, personCommentUpdated.Add(time.Minute))
	if fetches.Load() != 3 {
		t.Fatalf("a new comment count must read again, fetched %d times", fetches.Load())
	}
}

// The newest comments are on the last page. A count behind GitHub follows
// the response's LastPage; a count ahead of it (an empty page past the
// first) restarts from page 1.
func TestLatestPersonCommentFindsTheLastPage(t *testing.T) {
	comments := make([]personWireComment, 0, 250)
	for i := 1; i <= 249; i++ {
		comments = append(comments, personWire(i, "reporter", "NONE", "more", 1))
	}
	comments = append(comments, personWire(250, "maintainer", "OWNER", "@jeefy?", 20))
	server, fetches, pages := personCommentServer(t, comments)
	c := newTestClient(t, server, "acme", []string{"server"})
	read := func(count int, updated time.Time) *IssuePersonComment {
		return c.latestPersonComment(context.Background(), "acme", "server", personCommentIssue(count, updated), nil, false, nil)
	}

	if got := read(250, personCommentUpdated); got == nil || got.Login != "maintainer" || fetches.Load() != 1 || pages.take() != "3" {
		t.Fatalf("250 comments: got %+v after %d reads, want the maintainer from one read of page 3", got, fetches.Load())
	}
	if got := read(120, personCommentUpdated.Add(time.Minute)); got == nil || got.Login != "maintainer" {
		t.Fatalf("count behind: got %+v", got)
	}
	if p := pages.take(); p != "2,3" {
		t.Fatalf("count behind: pages %q, want page 2 then its LastPage 3", p)
	}
	if got := read(450, personCommentUpdated.Add(2*time.Minute)); got == nil || got.Login != "maintainer" {
		t.Fatalf("count ahead: got %+v", got)
	}
	if p := pages.take(); p != "5,1,3" {
		t.Fatalf("count ahead: pages %q, want the empty page 5, then page 1 and its LastPage 3", p)
	}
}

// A comment list the wait notice scan already read costs no call, and the
// per-pass budget stops reads: a row past it keeps its cached comment.
func TestLatestPersonCommentReusesTheListAndKeepsTheBudget(t *testing.T) {
	server, fetches, _ := personCommentServer(t, []personWireComment{
		personWire(1, "maintainer", "MEMBER", "@jeefy?", 16),
	})
	c := newTestClient(t, server, "acme", []string{"server"})
	listed := []*gh.IssueComment{{
		User: &gh.User{Login: gh.Ptr("maintainer")}, AuthorAssociation: gh.Ptr("MEMBER"),
		Body: gh.Ptr("@hanthor?"), CreatedAt: &gh.Timestamp{Time: personDay(16)},
	}}
	pass := &personCommentPass{reads: 1}
	got := c.latestPersonComment(context.Background(), "acme", "server", personCommentIssue(1, personCommentUpdated), listed, true, pass)
	if got == nil || strings.Join(got.Mentions, ",") != "hanthor" || fetches.Load() != 0 || pass.reads != 1 {
		t.Fatalf("listed: got %+v, fetches %d, reads left %d; want the listed comment for no read", got, fetches.Load(), pass.reads)
	}
	moved := personCommentUpdated.Add(time.Minute)
	if got := c.latestPersonComment(context.Background(), "acme", "server", personCommentIssue(1, moved), nil, false, pass); got == nil || fetches.Load() != 1 || pass.reads != 0 {
		t.Fatalf("one read in budget: got %+v, fetches %d", got, fetches.Load())
	}
	later := personCommentUpdated.Add(2 * time.Minute)
	got = c.latestPersonComment(context.Background(), "acme", "server", personCommentIssue(1, later), nil, false, pass)
	if got == nil || strings.Join(got.Mentions, ",") != "jeefy" || fetches.Load() != 1 {
		t.Fatalf("over budget: got %+v, fetches %d; want the cached comment and no read", got, fetches.Load())
	}
}

// No comments on the listing: nothing to read. A read error keeps the cached
// comment, is not cached itself, and is logged once per pass.
func TestLatestPersonCommentNoCommentsAndErrors(t *testing.T) {
	var fetches atomic.Int32
	var fail atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/server/issues/655/comments", func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		if fail.Load() {
			http.Error(w, `{"message":"boom"}`, http.StatusUnprocessableEntity)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]personWireComment{personWire(1, "maintainer", "MEMBER", "@jeefy?", 16)})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	c := newTestClient(t, server, "acme", []string{"server"})
	var logs syncBuffer
	c.logger = slog.New(slog.NewTextHandler(&logs, nil))
	read := func(updated time.Time, count int, pass *personCommentPass) *IssuePersonComment {
		return c.latestPersonComment(context.Background(), "acme", "server", personCommentIssue(count, updated), nil, false, pass)
	}

	if got := read(personCommentUpdated, 0, nil); got != nil || fetches.Load() != 0 {
		t.Fatalf("no comments: got %+v after %d reads, want nil and no read", got, fetches.Load())
	}
	if got := read(personCommentUpdated, 1, nil); got == nil || got.Login != "maintainer" {
		t.Fatalf("first read: got %+v", got)
	}
	fail.Store(true)
	pass := newPersonCommentPass()
	for i := 1; i <= 3; i++ {
		if got := read(personCommentUpdated.Add(time.Duration(i)*time.Minute), 1, pass); got == nil || got.Login != "maintainer" {
			t.Fatalf("read error %d: got %+v, want the cached comment", i, got)
		}
	}
	if n := strings.Count(logs.String(), "comment read failed"); n != 1 {
		t.Fatalf("logged %d read failures in one pass, want 1", n)
	}
	before := fetches.Load()
	read(personCommentUpdated.Add(3*time.Minute), 1, nil)
	if fetches.Load() == before {
		t.Fatal("a failed read must not be cached")
	}
}

// syncBuffer is a bytes.Buffer safe for a logger and a reader.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
