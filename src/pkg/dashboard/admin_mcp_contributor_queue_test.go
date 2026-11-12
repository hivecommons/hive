package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
)

// cqQueueServer is a hive with three ready issues: #3 pinned to the front
// and #1 held with a reason, so the default scan order (1, 2, 3) and the
// card's order (3, 2, then held 1) differ.
func cqQueueServer(t *testing.T) (*ContributeWSHub, *Server) {
	t.Helper()
	hub, s := covK2Hub(t)
	setStatusIssues(s,
		intgIssue(1, "first in scan order", "bob", nil),
		intgIssue(2, "second in scan order", "bob", nil),
		intgIssue(3, "third in scan order", "bob", nil),
	)
	s.deps.Config.Hub.ContributeQueueOrder = []string{"myorg/repo1#3"}
	s.deps.Config.Hub.ContributeQueueHold = []string{"myorg/repo1#1"}
	s.deps.Config.Hub.ContributeQueueHoldReasons = map[string]string{"myorg/repo1#1": "waiting on a design call"}
	return hub, s
}

func cqRowKeys(t *testing.T, rows []any) []string {
	t.Helper()
	out := make([]string, 0, len(rows))
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		key, _ := row["key"].(string)
		out = append(out, key)
	}
	return out
}

// The read tool returns the rows the Operations card's GET returns, in the
// same order (pinned first, held last with its reason), and its first
// offerable row is what selectTask offers first.
func TestAdminMCPContributorQueueMatchesTheCardAndSelectTask(t *testing.T) {
	hub, s := cqQueueServer(t)

	rec := doGet(s, "/api/contribute/queue")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET queue = %d %s", rec.Code, rec.Body.String())
	}
	var card struct {
		Queue []any `json:"queue"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
		t.Fatal(err)
	}

	got, err := dashboardAdminMCPProvider{server: s}.Read(context.Background(), adminmcp.ToolContributorQueue, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := got.(map[string]any)["rows"].([]any)
	keys := cqRowKeys(t, rows)
	if strings.Join(keys, ",") != strings.Join(cqRowKeys(t, card.Queue), ",") {
		t.Fatalf("contributor_queue rows %v differ from the card's %v", keys, cqRowKeys(t, card.Queue))
	}
	if strings.Join(keys, ",") != "myorg/repo1#3,myorg/repo1#2,myorg/repo1#1" {
		t.Fatalf("rows = %v, want pinned #3 first and held #1 last", keys)
	}
	held := rows[2].(map[string]any)
	if held["held"] != true || held["held_reason"] != "waiting on a design call" || held["position"] != 3 {
		t.Fatalf("held row = %#v", held)
	}

	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	msg := hub.selectTask(conn)
	if msg == nil || msg.Type != "task_assign" || msg.Number != 3 {
		t.Fatalf("selectTask offered %+v, want #3, the read tool's first row", msg)
	}
}

// ?controls=1 is the queue controls' own state: owner/read-write only, and
// without it the GET is unchanged and public.
func TestContributeQueueControlsViewIsGatedLikeTheControls(t *testing.T) {
	_, s := cqQueueServer(t)
	for role, want := range map[string]int{"": http.StatusForbidden, config.RoleRead: http.StatusForbidden, config.RoleReadWrite: http.StatusOK, config.RoleOwner: http.StatusOK} {
		req := httptest.NewRequest(http.MethodGet, "/api/contribute/queue?controls=1", nil)
		if role != "" {
			req.Header.Set("X-Hive-Role", role)
		}
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("role %q: %d, want %d (%s)", role, rec.Code, want, rec.Body.String())
		}
		if want != http.StatusOK {
			continue
		}
		var body struct {
			Controls struct {
				Order       []string            `json:"order"`
				Hold        []string            `json:"hold"`
				HoldReasons []map[string]string `json:"hold_reasons"`
			} `json:"controls"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if strings.Join(body.Controls.Order, ",") != "myorg/repo1#3" || strings.Join(body.Controls.Hold, ",") != "myorg/repo1#1" ||
			len(body.Controls.HoldReasons) != 1 || body.Controls.HoldReasons[0]["reason"] != "waiting on a design call" {
			t.Fatalf("controls = %+v", body.Controls)
		}
	}
	rec := doGet(s, "/api/contribute/queue")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"controls"`) {
		t.Fatalf("plain GET = %d %s, want public and without controls", rec.Code, rec.Body.String())
	}
}

func cqMCPHandler(s *Server, p dashboardAdminMCPProvider) *adminmcp.Handler {
	return adminmcp.NewHandler(p,
		adminmcp.WithWritesEnabled(true),
		adminmcp.WithWritesUnavailable(s.adminMCPWritesUnavailableReason(p)),
		adminmcp.WithActor(p.user),
		adminmcp.WithPendingStore(adminmcp.NewMemoryPendingStore()),
		adminmcp.WithHiveID("dashboard"),
	)
}

func cqConfirmationID(t *testing.T, out any) string {
	t.Helper()
	m, _ := out.(map[string]any)
	id, _ := m["confirmation_id"].(string)
	if id == "" {
		t.Fatalf("preview = %#v", out)
	}
	return id
}

// The write operations go through the card's own endpoints: pin, hold and
// resume all change exactly what the card would, and a pinned order changed
// between preview and confirm refuses the confirm and writes nothing.
func TestAdminMCPContributorQueueWritesUseTheCardsEndpoints(t *testing.T) {
	_, s := cqQueueServer(t)
	h := cqMCPHandler(s, dashboardAdminMCPProvider{server: s, user: "operator", role: config.RoleReadWrite})
	ctx := context.Background()

	out, err := h.PreviewWrite(ctx, map[string]any{"operation": adminmcp.WriteOpContributorQueueOrder, "args": map[string]any{"pin": "myorg/repo1#2"}})
	if err != nil {
		t.Fatalf("pin preview: %v", err)
	}
	if _, err := h.ConfirmWrite(ctx, map[string]any{"confirmation_id": cqConfirmationID(t, out)}); err != nil {
		t.Fatalf("pin confirm: %v", err)
	}
	if got := strings.Join(s.deps.Config.Hub.ContributeQueueOrder, ","); got != "myorg/repo1#2,myorg/repo1#3" {
		t.Fatalf("stored order = %s", got)
	}

	// Stale: the card saves another order between preview and confirm.
	out, err = h.PreviewWrite(ctx, map[string]any{"operation": adminmcp.WriteOpContributorQueueOrder, "args": map[string]any{"unpin": "myorg/repo1#3"}})
	if err != nil {
		t.Fatalf("unpin preview: %v", err)
	}
	s.deps.Config.Hub.ContributeQueueOrder = []string{"myorg/repo1#3", "myorg/repo1#2"}
	if _, err := h.ConfirmWrite(ctx, map[string]any{"confirmation_id": cqConfirmationID(t, out)}); !errors.Is(err, adminmcp.ErrConfirmationStale) {
		t.Fatalf("stale confirm err = %v", err)
	}
	if got := strings.Join(s.deps.Config.Hub.ContributeQueueOrder, ","); got != "myorg/repo1#3,myorg/repo1#2" {
		t.Fatalf("a stale confirm changed the order: %s", got)
	}

	out, err = h.PreviewWrite(ctx, map[string]any{"operation": adminmcp.WriteOpContributorQueueHold, "args": map[string]any{"key": "myorg/repo1#2", "held": true, "reason": "needs a maintainer first"}})
	if err != nil {
		t.Fatalf("hold preview: %v", err)
	}
	if _, err := h.ConfirmWrite(ctx, map[string]any{"confirmation_id": cqConfirmationID(t, out)}); err != nil {
		t.Fatalf("hold confirm: %v", err)
	}
	hub := s.deps.Config.Hub
	if strings.Join(hub.ContributeQueueHold, ",") != "myorg/repo1#1,myorg/repo1#2" || hub.ContributeQueueHoldReasons["myorg/repo1#2"] != "needs a maintainer first" {
		t.Fatalf("hold = %v reasons = %v", hub.ContributeQueueHold, hub.ContributeQueueHoldReasons)
	}

	out, err = h.PreviewWrite(ctx, map[string]any{"operation": adminmcp.WriteOpContributorQueueHoldClear, "args": map[string]any{}})
	if err != nil {
		t.Fatalf("hold_clear preview: %v", err)
	}
	if _, err := h.ConfirmWrite(ctx, map[string]any{"confirmation_id": cqConfirmationID(t, out)}); err != nil {
		t.Fatalf("hold_clear confirm: %v", err)
	}
	if len(s.deps.Config.Hub.ContributeQueueHold) != 0 || len(s.deps.Config.Hub.ContributeQueueHoldReasons) != 0 {
		t.Fatalf("hold_clear left %v %v", s.deps.Config.Hub.ContributeQueueHold, s.deps.Config.Hub.ContributeQueueHoldReasons)
	}
}

// A read-only caller is refused all three writes at preview time, before
// anything is minted. Even with the MCP's own role check out of the way, the
// card's endpoint refuses to show it the controls' state, so the gate that
// decides is the dashboard handler's.
func TestAdminMCPContributorQueueWritesRefuseReadOnlyCallers(t *testing.T) {
	_, s := cqQueueServer(t)
	ctx := context.Background()
	reader := dashboardAdminMCPProvider{server: s, user: "viewer", role: config.RoleRead}
	ops := map[string]map[string]any{
		adminmcp.WriteOpContributorQueueOrder:     {"pin": "myorg/repo1#2"},
		adminmcp.WriteOpContributorQueueHold:      {"key": "myorg/repo1#2", "held": true},
		adminmcp.WriteOpContributorQueueHoldClear: {},
	}
	h := cqMCPHandler(s, reader)
	for op, args := range ops {
		if _, err := h.PreviewWrite(ctx, map[string]any{"operation": op, "args": args}); !errors.Is(err, adminmcp.ErrWritesDisabled) {
			t.Errorf("%s: read-only preview err = %v, want writes disabled", op, err)
		}
	}
	bypass := adminmcp.NewHandler(reader, adminmcp.WithWritesEnabled(true), adminmcp.WithActor("viewer"), adminmcp.WithPendingStore(adminmcp.NewMemoryPendingStore()))
	for op, args := range ops {
		_, err := bypass.PreviewWrite(ctx, map[string]any{"operation": op, "args": args})
		if err == nil || !strings.Contains(err.Error(), "403") {
			t.Errorf("%s: preview err = %v, want the endpoint's 403", op, err)
		}
	}
	if pending, _ := bypass.PendingStore.List(ctx); len(pending) != 0 {
		t.Fatalf("a refused preview minted %+v", pending)
	}
	if strings.Join(s.deps.Config.Hub.ContributeQueueOrder, ",") != "myorg/repo1#3" || strings.Join(s.deps.Config.Hub.ContributeQueueHold, ",") != "myorg/repo1#1" {
		t.Fatal("a refused preview changed the queue")
	}
}
