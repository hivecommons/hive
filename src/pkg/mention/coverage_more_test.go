package mention

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestParseAdditionalBoundariesAndEmptyLogin(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		login     string
		mentioned bool
		text      string
	}{
		{"empty login", "@hive hi", "", false, ""},
		{"dash boundary rejected", "@hive-bot hi", "hive[bot]", false, ""},
		{"underscore boundary rejected", "@hive_bot hi", "hive[bot]", false, ""},
		{"digit boundary rejected", "@hive2 hi", "hive[bot]", false, ""},
		{"eof mention", "please @hive", "hive[bot]", true, ""},
		{"leading at trimmed", "@hive hi", "@hive[bot]", true, "hi"},
		{"second candidate after bad boundary", "@hive-bad then @hive ok", "hive[bot]", true, "ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.body, tc.login)
			if got.Mentioned != tc.mentioned || got.Text != tc.text {
				t.Fatalf("Parse() = %+v", got)
			}
		})
	}
}

func TestStoreFileRoundTripAndErrors(t *testing.T) {
	dir := filepath.Join("pkg", "mention", "testdata-runtime")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "store-roundtrip.json")
	_ = os.Remove(path)
	t.Cleanup(func() { _ = os.Remove(path) })

	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	wm := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := s.Mark("node"); err != nil {
		t.Fatal(err)
	}
	if err := s.Advance("org/repo", wm); err != nil {
		t.Fatal(err)
	}
	loaded, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Seen("node") || !loaded.Watermark("org/repo").Equal(wm) {
		t.Fatalf("roundtrip seen=%v watermark=%v", loaded.Seen("node"), loaded.Watermark("org/repo"))
	}
	if loaded.Seen("") {
		t.Fatal("empty node id reported seen")
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(path); err == nil {
		t.Fatal("invalid JSON store loaded without error")
	}
}

func TestStoreReadAndSaveErrors(t *testing.T) {
	dir := filepath.Join("pkg", "mention", "testdata-runtime")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(dir); err == nil {
		t.Fatal("directory store path loaded without error")
	}
	badPath := filepath.Join(dir, "missing-parent", "child", "store.json")
	s, err := NewStore(badPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "missing-parent"), []byte("file not dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Join(dir, "missing-parent")) })
	if err := s.Advance("org/repo", time.Now()); err == nil {
		t.Fatal("save through file parent succeeded unexpectedly")
	}
}

func TestHandlerDisabledNoopAndAuthorizationVariants(t *testing.T) {
	var kicked bool
	h := NewHandler(Options{Config: config.GitHubMentionsConfig{Enabled: false}, GitHub: &fakeGH{app: "hive[bot]"}, Kick: func(agent, msg, source string) error { kicked = true; return nil }})
	if err := h.Handle(context.Background(), Event{Body: "@hive hi"}); err != nil || kicked {
		t.Fatalf("disabled handle err=%v kicked=%v", err, kicked)
	}

	cfg := config.GitHubMentionsConfig{Enabled: true, Summoners: []string{"bob"}}
	h = NewHandler(Options{Config: cfg})
	if !h.authorized("BOB", cfg) {
		t.Fatal("explicit summoner did not authorize case-insensitively")
	}
	if h.authorized("alice", cfg) {
		t.Fatal("nil role resolver authorized alice")
	}
}

func TestResolveAgentVariants(t *testing.T) {
	cases := []struct {
		name      string
		cfg       config.GitHubMentionsConfig
		agents    func() []AgentInfo
		named     string
		want      string
		wantError string
	}{
		{"nil resolver", config.GitHubMentionsConfig{}, nil, "", "", "no agent resolver"},
		{"default agent", config.GitHubMentionsConfig{DefaultAgent: "scanner"}, func() []AgentInfo {
			return []AgentInfo{{Name: "scanner", Enabled: true, Converse: true, Mention: true, GovernorKick: true}}
		}, "", "scanner", ""},
		{"named missing", config.GitHubMentionsConfig{}, func() []AgentInfo {
			return []AgentInfo{{Name: "scanner", Enabled: true, Converse: true, Mention: true, GovernorKick: true}}
		}, "other", "", "not configured"},
		{"multiple need selector", config.GitHubMentionsConfig{}, func() []AgentInfo {
			return []AgentInfo{{Name: "a", Enabled: true, Converse: true, Mention: true, GovernorKick: true}, {Name: "b", Enabled: true, Converse: true, Mention: true, GovernorKick: true}}
		}, "", "", "multiple"},
		{"none qualify", config.GitHubMentionsConfig{}, func() []AgentInfo {
			return []AgentInfo{{Name: "a", Enabled: false, Converse: true, Mention: true, GovernorKick: true}}
		}, "", "", "no enabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler(Options{Config: tc.cfg, Agents: tc.agents})
			got, err := h.resolveAgent(tc.named)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("err=%v want %q", err, tc.wantError)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got (%q,%v), want %q", got, err, tc.want)
			}
		})
	}
}

func TestHandleAckFailureAfterKickAndNoKick(t *testing.T) {
	var kicks int
	var audit []string
	store := mustStore(t)
	gh := &fakeGH{app: "hive[bot]", ackErr: errors.New("403 issue is locked")}
	h := NewHandler(Options{
		Config: config.GitHubMentionsConfig{Enabled: true},
		GitHub: gh,
		Roles:  func(string) (string, bool) { return config.RoleReadWrite, true },
		Agents: func() []AgentInfo {
			return []AgentInfo{{Name: "scanner", Enabled: true, Converse: true, Mention: true, GovernorKick: true}}
		},
		Store: store,
		Kick:  func(agent, msg, source string) error { kicks++; return nil },
		Audit: func(action, detail, agent string) { audit = append(audit, action+":"+detail) },
	})
	ev := Event{Repo: "org/repo", Number: 1, NodeID: "ack", CommentID: 1, Author: "alice", Body: "@hive hi", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	// The kick was delivered, so a failed reaction must not leave the
	// mention retryable: a retry would kick the agent a second time.
	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("ack failure after a delivered kick returned %v", err)
	}
	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if kicks != 1 || !store.Seen("ack") || !containsAudit(audit, AuditKicked+":") || !containsAudit(audit, "ack=failed") {
		t.Fatalf("kicks=%d seen=%v audit=%v", kicks, store.Seen("ack"), audit)
	}

	audit = nil
	gh = &fakeGH{app: "hive[bot]"}
	h = NewHandler(Options{
		Config: config.GitHubMentionsConfig{Enabled: true},
		GitHub: gh,
		Roles:  func(string) (string, bool) { return config.RoleReadWrite, true },
		Agents: func() []AgentInfo {
			return []AgentInfo{{Name: "scanner", Enabled: true, Converse: true, Mention: true, GovernorKick: true}}
		},
		Store: mustStore(t),
		Audit: func(action, detail, agent string) { audit = append(audit, action+":"+detail) },
	})
	if err := h.Handle(context.Background(), Event{Repo: "org/repo", Number: 1, NodeID: "nokick", CommentID: 2, Author: "alice", Body: "@hive hi", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if !containsAudit(audit, "guard=no-kick") || gh.ack != 0 {
		t.Fatalf("no-kick decline: ack=%d audit=%v", gh.ack, audit)
	}
}

func TestPollerNilPollFailureAndRunCancel(t *testing.T) {
	(*Poller)(nil).Poll(context.Background())
	p := NewPoller(nil, nil, nil, nil, 0, nil)
	p.Poll(context.Background())
	if p.interval != time.Minute {
		t.Fatalf("default interval=%v", p.interval)
	}

	gh := &fakeGH{app: "hive[bot]", listErr: errors.New("list failed")}
	store, _ := NewStore("")
	_ = store.Advance("org/repo", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	p = NewPoller(gh, func() []string { return []string{"org/repo"} }, store, baseHandler(t, gh, &[]string{}, &[]string{}), time.Nanosecond, nil)
	p.Poll(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Run(ctx)
}

func TestRateLimiterDefaultsExpiredWindowAndRelease(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := NewRateLimiter(func() time.Time { return now })
	allow := func() bool {
		_, ok := r.Reserve("k", 1, time.Hour)
		return ok
	}
	if _, ok := r.Reserve("k", 0, time.Hour); ok {
		t.Fatal("zero max allowed")
	}
	if !allow() || allow() {
		t.Fatal("limit did not apply")
	}
	now = now.Add(2 * time.Hour)
	release, ok := r.Reserve("k", 1, time.Hour)
	if !ok {
		t.Fatal("expired hit was not pruned")
	}
	release()
	if !allow() {
		t.Fatal("released slot was not returned to the window")
	}
	if NewRateLimiter(nil).now == nil {
		t.Fatal("nil clock did not default")
	}
}

func TestBuildKickTruncateAndMarkWithoutStore(t *testing.T) {
	msg := buildKickMessage(Event{Author: "alice", Repo: "org/repo", Number: 1, CommentID: 2, HTMLURL: "u"}, strings.Repeat("x", 20000))
	if len([]rune(msg)) != kickLimit {
		t.Fatalf("truncated length=%d", len([]rune(msg)))
	}
	h := NewHandler(Options{})
	if err := h.mark(Event{}); err != nil {
		t.Fatalf("nil store mark err=%v", err)
	}
}
