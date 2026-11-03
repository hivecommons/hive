package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/firsttimer"
	"github.com/hivecommons/hive/pkg/github"
)

func TestFirstTimerCandidatesProjection(t *testing.T) {
	now := time.Now()
	res := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{
		{Repo: "widgets", Number: 1, Author: "newbie", AuthorAssociation: "FIRST_TIME_CONTRIBUTOR", Labels: []string{"size/s"}, CreatedAt: now, URL: "u1"},
		{Repo: "acme/other", Number: 2, Author: "hive-app[bot]", AppAuthored: true, AuthorAssociation: "CONTRIBUTOR"},
		{Repo: "widgets", Number: 3, Author: "x", State: "closed"},
		{Repo: "widgets", Number: 0, Author: "x"},
		{Repo: "widgets", Number: 4, Author: "relay-person", HiveAttributed: true, AuthorNonHuman: false, Draft: true},
	}}}
	cands := firstTimerCandidates("acme", res)
	if len(cands) != 3 {
		t.Fatalf("cands %+v", cands)
	}
	if cands[0].Repo != "acme/widgets" || cands[0].Author != "newbie" || cands[0].AuthorAssociation != "FIRST_TIME_CONTRIBUTOR" || cands[0].URL != "u1" || len(cands[0].Labels) != 1 {
		t.Fatalf("cand[0] %+v", cands[0])
	}
	if cands[1].Repo != "acme/other" || !cands[1].AuthorNonHuman {
		t.Fatalf("app-authored must project as non-human: %+v", cands[1])
	}
	if !cands[2].HiveAttributed || !cands[2].Draft {
		t.Fatalf("cand[2] %+v", cands[2])
	}
	if firstTimerCandidates("acme", nil) != nil {
		t.Fatal("nil result")
	}
}

// firstTimerFakeGitHub serves the comment list/create and PR search the
// welcome touches.
type firstTimerFakeGitHub struct {
	mu       sync.Mutex
	comments map[int][]map[string]any
	searches int
}

func (f *firstTimerFakeGitHub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && p == "/search/issues":
			f.searches++
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "items": []map[string]any{{"number": 7}}})
		case r.Method == http.MethodGet && strings.Contains(p, "/issues/") && strings.HasSuffix(p, "/comments"):
			n := issueNumberFromPath(p)
			_ = json.NewEncoder(w).Encode(f.comments[n])
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/comments"):
			n := issueNumberFromPath(p)
			b, _ := io.ReadAll(r.Body)
			var in struct{ Body string }
			_ = json.Unmarshal(b, &in)
			f.comments[n] = append(f.comments[n], map[string]any{"id": 100 + len(f.comments[n]), "body": in.Body})
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 100, "body": in.Body})
		default:
			http.Error(w, r.Method+" "+p, http.StatusNotFound)
		}
	})
}

func issueNumberFromPath(p string) int {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		if part == "issues" && i+1 < len(parts) {
			n := 0
			for _, ch := range parts[i+1] {
				if ch < '0' || ch > '9' {
					break
				}
				n = n*10 + int(ch-'0')
			}
			return n
		}
	}
	return 0
}

func (f *firstTimerFakeGitHub) bodies(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.comments[n] {
		out = append(out, c["body"].(string))
	}
	return out
}

func TestRunFirstTimerWelcomeWiring(t *testing.T) {
	fake := &firstTimerFakeGitHub{comments: map[int][]map[string]any{}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	logger := slog.New(slog.DiscardHandler)
	gh := github.NewClientForTest(srv.URL, "acme", []string{"widgets"}, logger)
	cfg := gfiTestConfig()
	dir := t.TempDir()
	tmplPath := filepath.Join(dir, "welcome.tmpl")
	if err := os.WriteFile(tmplPath, []byte("{{ .Marker }}\nHowdy @{{ .Author }} from {{ .Hive }}: {{ .LeaderboardURL }}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Governor.FirstTimerWelcome.TemplatePath = tmplPath
	state := newFirstTimerState()
	known := func(login string) bool { return login == "already-relaying" }
	now := time.Now()
	last := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{
		{Repo: "widgets", Number: 7, Author: "newbie", AuthorAssociation: "FIRST_TIME_CONTRIBUTOR", CreatedAt: now.Add(-time.Hour)},
		{Repo: "widgets", Number: 8, Author: "oldhand", AuthorAssociation: "CONTRIBUTOR", CreatedAt: now},
		{Repo: "widgets", Number: 9, Author: "already-relaying", AuthorAssociation: "FIRST_TIME_CONTRIBUTOR", CreatedAt: now},
		{Repo: "widgets", Number: 10, Author: "someone", AuthorAssociation: "FIRST_TIME_CONTRIBUTOR", Labels: []string{"author/clanker"}, CreatedAt: now},
		{Repo: "widgets", Number: 11, Author: "bot-thing[bot]", AuthorNonHuman: true, AuthorAssociation: "FIRST_TIME_CONTRIBUTOR", CreatedAt: now},
	}}}
	ctx := context.Background()

	// No public URL: silent, nothing posted.
	noURL := gfiTestConfig()
	noURL.Dashboard.PublicURL = ""
	runFirstTimerWelcome(ctx, gh, noURL, nil, newFirstTimerState(), known, last, logger)
	if len(fake.bodies(7)) != 0 {
		t.Fatal("must not post without a public URL")
	}

	runFirstTimerWelcome(ctx, gh, cfg, nil, state, known, last, logger)
	got := fake.bodies(7)
	if len(got) != 1 || !strings.HasPrefix(got[0], firsttimer.Marker) || !strings.Contains(got[0], "Howdy @newbie from acme-hive: https://acme.hive.example/contribute/leaderboard") {
		t.Fatalf("welcome on #7 = %q", got)
	}
	for _, n := range []int{8, 9, 10, 11} {
		if len(fake.bodies(n)) != 0 {
			t.Fatalf("PR #%d must not be welcomed", n)
		}
	}
	if !state.sweeper.Welcomed("acme/widgets", "newbie") {
		t.Fatal("state must remember the author")
	}

	// A second pass (and a second PR by the same author) posts nothing and
	// costs no GitHub call: memory short-circuits before the forge.
	fake.mu.Lock()
	searches := fake.searches
	fake.mu.Unlock()
	last.PRs.Items = append(last.PRs.Items, github.PullRequest{Repo: "widgets", Number: 12, Author: "newbie", AuthorAssociation: "FIRST_TIME_CONTRIBUTOR", CreatedAt: now})
	runFirstTimerWelcome(ctx, gh, cfg, nil, state, known, last, logger)
	fake.mu.Lock()
	after := fake.searches
	fake.mu.Unlock()
	if len(fake.bodies(7)) != 1 || len(fake.bodies(12)) != 0 || after != searches {
		t.Fatalf("second pass: #7=%d #12=%d searches %d→%d", len(fake.bodies(7)), len(fake.bodies(12)), searches, after)
	}

	// Disabled: nothing, even for a fresh state.
	off := false
	cfg.Governor.FirstTimerWelcome.Enabled = &off
	fresh := newFirstTimerState()
	runFirstTimerWelcome(ctx, gh, cfg, nil, fresh, known, last, logger)
	if len(fake.bodies(12)) != 0 || fresh.sweeper.Stats().Welcomed != 0 {
		t.Fatal("disabled welcome must do nothing")
	}

	// Nil guards.
	runFirstTimerWelcome(ctx, nil, cfg, nil, fresh, known, last, logger)
	runFirstTimerWelcome(ctx, gh, nil, nil, fresh, known, last, logger)
	runFirstTimerWelcome(ctx, gh, cfg, nil, nil, known, last, logger)
	runFirstTimerWelcome(ctx, gh, cfg, nil, fresh, known, nil, logger)
}

func TestFirstTimerTemplateTextLoadsOnce(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	state := newFirstTimerState()
	if got := state.templateText(config.FirstTimerWelcomeConfig{}, logger); got != "" {
		t.Fatalf("unset path = %q", got)
	}
	// Unreadable path falls back to the default (empty override) and is
	// not retried.
	state = newFirstTimerState()
	bad := config.FirstTimerWelcomeConfig{TemplatePath: filepath.Join(t.TempDir(), "missing.tmpl")}
	if got := state.templateText(bad, logger); got != "" || !state.loaded {
		t.Fatalf("missing file: %q loaded=%v", got, state.loaded)
	}
	// Once loaded, a later config change is not re-read.
	dir := t.TempDir()
	p := filepath.Join(dir, "w.tmpl")
	if err := os.WriteFile(p, []byte("{{ .Marker }} x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := state.templateText(config.FirstTimerWelcomeConfig{TemplatePath: p}, logger); got != "" {
		t.Fatalf("must not re-read after load: %q", got)
	}
	if got := newFirstTimerState().templateText(config.FirstTimerWelcomeConfig{TemplatePath: p}, nil); got != "{{ .Marker }} x" {
		t.Fatalf("fresh state reads the file: %q", got)
	}
}

func TestFirstTimerKnownContributor(t *testing.T) {
	b := &boot{}
	if b.firstTimerKnownContributor("nobody-registered-here") {
		t.Fatal("unknown login must not be known")
	}
	if b.firstTimerState() == nil || b.firstTimerState() != b.firstTimer {
		t.Fatal("firstTimerState must be lazily built and cached")
	}
}
