package dashboard

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/taskmcp"
	"github.com/hivecommons/hive/pkg/config"
)

// The relay sends a task's lease bearer to whatever task_assign's mcp.url
// names, so that URL may only ever be an address of the assigning hive.
// hub.url is the hub itself and applyDefaults fills it on every hive; it used
// to be the last fallback, and a hub.dashboard_url left behind by a disabled
// hub was honoured too, so a hive with no public address of its own told its
// contributors to present its bearer to the hub.

// remoteMCPHubURL is what applyDefaults gives hub.url on every hive.
const remoteMCPHubURL = "https://hive.hivecommons.dev"

// remoteMCPHive is a hive with task_mcp.remote_enabled, a dashboard token to
// sign lease bearers with, and one task leased to alice.
func remoteMCPHive(t *testing.T, mutate func(*config.Config)) (*ContributeWSHub, *WSTaskAssign) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)
	redirectContributeWSDisk(t, dir)
	cfg := &config.Config{}
	cfg.TaskMCP.RemoteEnabled = true
	cfg.Dashboard.Port = 3001
	cfg.Hub.URL = remoteMCPHubURL
	mutate(cfg)
	s := newTestServer()
	s.authToken = "secret"
	s.deps = &Dependencies{Config: cfg}
	h := NewContributeWSHub(s.logger, s)
	t.Cleanup(h.Close)
	s.contributeHub = h
	assign := &WSTaskAssign{TaskID: "task-url", Kind: "issue", Repo: "owner/repo", Number: 42}
	if err := h.recordLeaseForKey("alice-id", assign.TaskID, assign.Repo, assign.Number, assign.identityKey(), "trusted", 2, time.Now()); err != nil {
		t.Fatalf("recordLeaseForKey: %v", err)
	}
	return h, assign
}

// leaseHoldsMCPToken reports whether a lease bearer was minted for the task.
func leaseHoldsMCPToken(t *testing.T, h *ContributeWSHub, identity, taskID string) bool {
	t.Helper()
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	l := h.leaseForLocked(identity, taskID)
	if l == nil {
		t.Fatalf("no lease recorded for %s on %s", identity, taskID)
	}
	return l.mcpTokenID != "" || l.mcpTokenHash != ""
}

func TestRemoteMCPBlockWithheldWithoutAPublicAddressOfThisHive(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"hub-less hive without dashboard.public_url", func(cfg *config.Config) {}},
		{"hub-less hive with a stale hub.dashboard_url", func(cfg *config.Config) {
			cfg.Hub.DashboardURL = "https://old-name.hive.hivecommons.dev"
		}},
		{"hub spoke the hub has not given a dashboard_url yet", func(cfg *config.Config) {
			cfg.HiveID = "hive-example"
			cfg.Hub.Enabled = true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, assign := remoteMCPHive(t, tc.mutate)
			if got := h.server.taskMCPURL(); got != "" {
				t.Errorf("taskMCPURL = %q, want none: it is not an address of this hive", got)
			}
			mcp := h.mintTaskMCPForAssignment("alice-id", assign, time.Now().Add(leaseTTL), "alice")
			if mcp != nil {
				t.Errorf("task_assign.mcp.url = %q (bearer attached: %v), want no mcp block", mcp.URL, mcp.Token != "")
			}
			if leaseHoldsMCPToken(t, h, "alice-id", assign.TaskID) {
				t.Error("a lease bearer was minted although there is no URL to present it to")
			}
		})
	}
}

func TestRemoteMCPURLNamesThisHive(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.Config)
		want   string
	}{
		{"dashboard.public_url on a hub-less hive", func(cfg *config.Config) {
			cfg.Dashboard.PublicURL = "https://hive.example"
		}, "https://hive.example"},
		{"dashboard.public_url beside a stale hub.dashboard_url", func(cfg *config.Config) {
			cfg.Dashboard.PublicURL = "https://hive.example"
			cfg.Hub.DashboardURL = "https://old-name.hive.hivecommons.dev"
		}, "https://hive.example"},
		{"hub spoke with the dashboard_url its hub gave it", func(cfg *config.Config) {
			cfg.HiveID = "hive-example"
			cfg.Hub.Enabled = true
			cfg.Hub.DashboardURL = "https://example.hive.hivecommons.dev/"
		}, "https://example.hive.hivecommons.dev"},
		{"dashboard.public_url still comes first on a hub spoke", func(cfg *config.Config) {
			cfg.Hub.Enabled = true
			cfg.Hub.DashboardURL = "https://example.hive.hivecommons.dev"
			cfg.Dashboard.PublicURL = "https://hive.example"
		}, "https://hive.example"},
		{"a dashboard.public_url the operator set is used as written", func(cfg *config.Config) {
			cfg.Dashboard.PublicURL = "http://localhost:3001"
		}, "http://localhost:3001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, assign := remoteMCPHive(t, tc.mutate)
			want := tc.want + taskmcp.EndpointPath
			mcp := h.mintTaskMCPForAssignment("alice-id", assign, time.Now().Add(leaseTTL), "alice")
			if mcp == nil {
				t.Fatalf("no mcp block, want a bearer for %s", want)
			}
			if mcp.URL != want || mcp.Token == "" {
				t.Fatalf("task_assign.mcp.url = %q (bearer attached: %v), want a bearer for %s", mcp.URL, mcp.Token != "", want)
			}
			if !leaseHoldsMCPToken(t, h, "alice-id", assign.TaskID) {
				t.Error("the minted bearer was not recorded on the lease")
			}
		})
	}
}

// End-to-end through selectTask: the task_assign a contributor receives.
func TestSelectTaskAssignCarriesRemoteMCPBlockOnlyForThisHive(t *testing.T) {
	assignFor := func(t *testing.T, mutate func(*config.Config)) (*ContributeWSHub, *ContributorConnection, *WSMessage) {
		t.Helper()
		hub, s := covK2Hub(t)
		oneActionableIssue(s)
		s.authToken = "secret"
		s.deps.Config.TaskMCP.RemoteEnabled = true
		s.deps.Config.Hub.URL = remoteMCPHubURL
		mutate(s.deps.Config)
		conn := &ContributorConnection{
			profile:    &ContributorProfile{GitHubUsername: "alice", ContributorID: "alice-id", TrustTier: "trusted"},
			cliBackend: "claude",
		}
		msg := hub.selectTask(conn)
		if msg == nil || msg.Type != "task_assign" {
			t.Fatalf("expected task_assign, got %+v", msg)
		}
		return hub, conn, msg
	}

	t.Run("no public address", func(t *testing.T) {
		hub, conn, msg := assignFor(t, func(cfg *config.Config) {})
		if msg.MCP != nil {
			t.Errorf("task_assign.mcp.url = %q (bearer attached: %v), want no mcp block", msg.MCP.URL, msg.MCP.Token != "")
		}
		wire, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(wire, &fields); err != nil {
			t.Fatal(err)
		}
		if _, ok := fields["mcp"]; ok {
			t.Error("task_assign on the wire still has an mcp key")
		}
		if leaseHoldsMCPToken(t, hub, identityOf(conn), msg.TaskID) {
			t.Error("a lease bearer was minted although task_assign carries no mcp block")
		}
	})

	t.Run("dashboard.public_url", func(t *testing.T) {
		_, _, msg := assignFor(t, func(cfg *config.Config) { cfg.Dashboard.PublicURL = "https://hive.example" })
		want := "https://hive.example" + taskmcp.EndpointPath
		if msg.MCP == nil {
			t.Fatalf("no mcp block, want a bearer for %s", want)
		}
		if msg.MCP.URL != want || msg.MCP.Token == "" {
			t.Fatalf("task_assign.mcp.url = %q (bearer attached: %v), want a bearer for %s", msg.MCP.URL, msg.MCP.Token != "", want)
		}
	})
}
