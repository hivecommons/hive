package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
)

const holdSourcesBotLogin11564 = "hive-app[bot]"

// holdSourcesE2EServer serves one open repository: the given issues and pull
// requests, plus the comments and hold-label events of the held pull requests.
// A write to GitHub fails the test: hold-source detection is read-only.
func holdSourcesE2EServer(t *testing.T, issues, prs []map[string]any, comments, events map[string][]map[string]any) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			t.Errorf("unexpected GitHub write: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets/")
		switch {
		case path == "issues":
			_ = json.NewEncoder(w).Encode(issues)
		case path == "pulls":
			_ = json.NewEncoder(w).Encode(prs)
		case strings.HasPrefix(path, "issues/") && strings.HasSuffix(path, "/comments"):
			_ = json.NewEncoder(w).Encode(comments[strings.Split(path, "/")[1]])
		case strings.HasPrefix(path, "issues/") && strings.HasSuffix(path, "/events"):
			_ = json.NewEncoder(w).Encode(events[strings.Split(path, "/")[1]])
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func holdSourcesWirePR(number int, labels ...string) map[string]any {
	wire := make([]map[string]string, 0, len(labels))
	for _, label := range labels {
		wire = append(wire, map[string]string{"name": label})
	}
	at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	return map[string]any{
		"number": number, "title": "feat: a change", "state": "open",
		"user":       map[string]string{"login": holdSourcesBotLogin11564},
		"labels":     wire,
		"created_at": at, "updated_at": at,
		"head": map[string]any{"sha": "abc123", "ref": "feature"},
		"base": map[string]any{"ref": "main"},
	}
}

// The #11507 contract for part B: hold sources detected during enumeration
// flow through status building into the ownership decisions, with no fallback
// in between. A PR held only by the level gate is the reviewer's turn with a
// Release hold action; a held issue Hive's record does not attribute is a
// person's; a sentinel-only held issue is decided by the sentinel rule.
func TestOwnershipHoldSourcesEndToEnd11564(t *testing.T) {
	old := github.HoldSourcesPath
	github.HoldSourcesPath = filepath.Join(t.TempDir(), github.HoldSourcesFile)
	t.Cleanup(func() { github.HoldSourcesPath = old })

	now := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	wireIssue := func(number int, labels ...string) map[string]any {
		wire := make([]map[string]string, 0, len(labels))
		for _, label := range labels {
			wire = append(wire, map[string]string{"name": label})
		}
		return map[string]any{
			"number": number, "title": "an issue", "state": "open",
			"user":       map[string]string{"login": "alice"},
			"labels":     wire,
			"created_at": now, "updated_at": now,
			"html_url": "https://github.com/acme/widgets/issues/1",
		}
	}
	levelHeld := holdSourcesWirePR(7, "hold")
	personHeld := holdSourcesWirePR(8, "hold")
	notice := "<!-- hive:level-hold {\"agent\":\"quality\"} -->\n> Held for human review by the hive's ACMM level gate."
	srv := holdSourcesE2EServer(t,
		[]map[string]any{
			wireIssue(21, "hold"),
			wireIssue(22, config.DefaultSentinelLabel, "hold"),
			wireIssue(23, config.DefaultSentinelLabel),
		},
		[]map[string]any{levelHeld, personHeld},
		map[string][]map[string]any{
			"7": {{"body": notice, "user": map[string]string{"login": holdSourcesBotLogin11564}}},
		},
		map[string][]map[string]any{
			"7": {{"event": "labeled", "label": map[string]string{"name": "hold"}, "actor": map[string]string{"login": holdSourcesBotLogin11564}, "created_at": now}},
			"8": {{"event": "labeled", "label": map[string]string{"name": "hold"}, "actor": map[string]string{"login": "alice"}, "created_at": now}},
		},
	)

	level := 4
	cfg := &config.Config{
		ACMMLevel: &level,
		Project:   config.ProjectConfig{Org: "acme", Repos: []string{"widgets"}},
	}
	client := github.NewClientForTest(srv.URL, "acme", []string{"widgets"}, slog.New(slog.DiscardHandler))
	client.SetAppBotLogin(holdSourcesBotLogin11564)
	client.SetSentinelAlertLabel(config.DefaultSentinelLabel)
	client.SetHoldLabels([]string{config.DefaultSentinelLabel})

	actionable, err := client.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	repos := buildRepos(cfg, actionable, governor.State{})
	if len(repos) != 1 {
		t.Fatalf("repos = %d, want 1", len(repos))
	}
	applyOwnershipDecisions(cfg, repos, nil, "idle", time.Now().UTC(), nil)

	decisions := repos[0].Ownership
	pr := decisions[ownershipKey(ownershipKindPR, 7)]
	checkOwnership11565(t, pr, want11565{OwnershipVerbReview, OwnershipReasonLevelGateHold})
	if pr.Action != OwnershipActionReleaseHold {
		t.Fatalf("PR held only by the level gate: action = %q, want %q", pr.Action, OwnershipActionReleaseHold)
	}
	checkOwnership11565(t, decisions[ownershipKey(ownershipKindPR, 8)], notYours11565)
	checkOwnership11565(t, decisions[ownershipKey(ownershipKindIssue, 21)], notYours11565)
	checkOwnership11565(t, decisions[ownershipKey(ownershipKindIssue, 22)], notYours11565)
	checkOwnership11565(t, decisions[ownershipKey(ownershipKindIssue, 23)], want11565{OwnershipVerbInspect, OwnershipReasonSentinelFlagged})
}

// A level-held PR that a non-App bot (a prow bot answering /hold) re-labels is
// not provably held only by the level gate: it lists level_gate and unknown,
// the decision is hold_source_unknown, and Release hold is not offered.
func TestOwnershipHoldSourcesNonAppBotReapplyEndToEnd11564(t *testing.T) {
	old := github.HoldSourcesPath
	github.HoldSourcesPath = filepath.Join(t.TempDir(), github.HoldSourcesFile)
	t.Cleanup(func() { github.HoldSourcesPath = old })

	now := time.Now().UTC().Add(-time.Hour)
	earlier := now.Add(-time.Minute).Format(time.RFC3339)
	later := now.Format(time.RFC3339)
	notice := "<!-- hive:level-hold {\"agent\":\"quality\"} -->\n> Held for human review by the hive's ACMM level gate."
	srv := holdSourcesE2EServer(t, nil,
		[]map[string]any{holdSourcesWirePR(7, "hold")},
		map[string][]map[string]any{
			"7": {{"body": notice, "user": map[string]string{"login": holdSourcesBotLogin11564}}},
		},
		map[string][]map[string]any{
			"7": {
				{"event": "labeled", "label": map[string]string{"name": "hold"}, "actor": map[string]string{"login": holdSourcesBotLogin11564}, "created_at": earlier},
				{"event": "labeled", "label": map[string]string{"name": "hold"}, "actor": map[string]string{"login": "kubestellar-prow[bot]"}, "created_at": later},
			},
		},
	)

	level := 4
	cfg := &config.Config{
		ACMMLevel: &level,
		Project:   config.ProjectConfig{Org: "acme", Repos: []string{"widgets"}},
	}
	client := github.NewClientForTest(srv.URL, "acme", []string{"widgets"}, slog.New(slog.DiscardHandler))
	client.SetAppBotLogin(holdSourcesBotLogin11564)

	actionable, err := client.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	repos := buildRepos(cfg, actionable, governor.State{})
	if len(repos) != 1 {
		t.Fatalf("repos = %d, want 1", len(repos))
	}
	applyOwnershipDecisions(cfg, repos, nil, "idle", time.Now().UTC(), nil)

	pr := repos[0].Ownership[ownershipKey(ownershipKindPR, 7)]
	checkOwnership11565(t, pr, want11565{OwnershipVerbReview, OwnershipReasonHoldSourceUnknown})
	if pr.Action == OwnershipActionReleaseHold {
		t.Fatalf("PR re-held by a non-App bot: Release hold must not be offered, got action %q", pr.Action)
	}
}
