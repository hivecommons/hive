package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hivecommons/hive/pkg/agentaudit"
	"github.com/hivecommons/hive/pkg/config"
)

// Opt-in relay audit attestation (hivecommons/hive#13458): a relay attaches
// the contributor's local rows, up to the level they consented to share, to
// task_complete / task_failed; the hub writes relay_task_attested and keeps
// shared command rows out of the audit ring.

func redirectContribAuditDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "contrib-audit")
	old := contribAuditDirPath
	contribAuditDirPath = dir
	contribAuditPruneMu.Lock()
	contribAuditLastPrune = time.Time{}
	contribAuditPruneMu.Unlock()
	t.Cleanup(func() { contribAuditDirPath = old })
	return dir
}

func attestRows(rows ...string) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(rows))
	for _, r := range rows {
		out = append(out, json.RawMessage(r))
	}
	return out
}

const (
	attestLifecycleRow = `{"v":2,"level":"lifecycle","event":"assigned","ts":"2026-10-22T00:00:00Z","task_id":"t-att"}`
	attestCommandRow   = `{"v":2,"level":"commands","event":"command","ts":"2026-10-22T00:01:00Z","task_id":"t-att","op":"gh","cmd":"gh pr create","exit":0,"reason":"open the PR","reason_source":"transcript"}`
	attestFetchRow     = `{"v":2,"level":"reasoning","event":"command","ts":"2026-10-22T00:00:30Z","task_id":"t-att","op":"git","cmd":"git fetch origin","exit":0}`
)

func attestConn() *ContributorConnection {
	return &ContributorConnection{
		session: "s1",
		profile: &ContributorProfile{TrustTier: "contributor", GitHubUsername: "Ben", ContributorID: "c-ben"},
	}
}

func TestRelayAttest_LevelsAndStorage(t *testing.T) {
	task := &WSTaskAssign{TaskID: "t-att", Repo: "myorg/repo1", Number: 42}
	rows := attestRows(attestLifecycleRow, attestCommandRow, attestFetchRow)
	tests := []struct {
		name       string
		share      *RelayAuditShare
		wantRow    bool
		wantDetail []string
		notDetail  []string
		wantFile   bool
		wantReason bool
	}{
		{name: "no share is a no-op", share: nil},
		{name: "unrecognised level is ignored", share: &RelayAuditShare{Level: "everything", Rows: rows}},
		{name: "capture off shares nothing", share: &RelayAuditShare{Level: "commands", CaptureLevel: "off", Rows: rows}},
		{
			name:       "lifecycle shares no command rows and stores nothing",
			share:      &RelayAuditShare{Level: "lifecycle", CaptureLevel: "reasoning", LocalRef: "hub|t-att|3", Rows: rows},
			wantRow:    true,
			wantDetail: []string{"task=t-att", "level=lifecycle", "capture_level=reasoning", "local_ref=hub|t-att|3", "rows=1", "commands=0", "dropped=2"},
			notDetail:  []string{"stored=", "clamped="},
		},
		{
			name:       "commands stores command rows without reasons",
			share:      &RelayAuditShare{Level: "commands", CaptureLevel: "reasoning", Rows: rows},
			wantRow:    true,
			wantDetail: []string{"level=commands", "rows=2", "commands=1", "dropped=1", "stored=ben/t-att.jsonl"},
			wantFile:   true,
		},
		{
			name:       "reasoning keeps reasons",
			share:      &RelayAuditShare{Level: "reasoning", CaptureLevel: "reasoning", Rows: rows},
			wantRow:    true,
			wantDetail: []string{"level=reasoning", "rows=3", "commands=2", "stored=ben/t-att.jsonl"},
			notDetail:  []string{"dropped="},
			wantFile:   true,
			wantReason: true,
		},
		{
			name:       "share above capture is clamped",
			share:      &RelayAuditShare{Level: "reasoning", CaptureLevel: "commands", Rows: rows},
			wantRow:    true,
			wantDetail: []string{"level=commands", "capture_level=commands", "clamped=true", "commands=1"},
			wantFile:   true,
		},
		{
			name:       "malformed rows are dropped, not fatal",
			share:      &RelayAuditShare{Level: "commands", Rows: attestRows(`"not an object"`, attestCommandRow)},
			wantRow:    true,
			wantDetail: []string{"rows=1", "commands=1", "dropped=1"},
			wantFile:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := redirectContribAuditDir(t)
			hub, _ := covK2Hub(t)
			hub.attestRelayAudit(attestConn(), task, tt.share)

			got := relayAuditEntries(t, hub, agentaudit.AuditRelayTaskAttested)
			if !tt.wantRow {
				if len(got) != 0 {
					t.Fatalf("relay_task_attested rows = %+v, want none", got)
				}
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Fatalf("contrib-audit dir created without a share: %v", err)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("relay_task_attested rows = %d, want 1", len(got))
			}
			e := got[0]
			if e.User != "Ben" || e.Agent != "c-ben#s1" || e.Repo != "myorg/repo1" || e.Target != 42 {
				t.Errorf("attribution = user %q agent %q repo %q target %d", e.User, e.Agent, e.Repo, e.Target)
			}
			for _, want := range tt.wantDetail {
				if !strings.Contains(e.Detail, want) {
					t.Errorf("detail %q missing %q", e.Detail, want)
				}
			}
			for _, not := range tt.notDetail {
				if strings.Contains(e.Detail, not) {
					t.Errorf("detail %q must not contain %q", e.Detail, not)
				}
			}
			if strings.Contains(e.Detail, "gh pr create") || strings.Contains(e.Detail, "open the PR") {
				t.Errorf("command rows must not be copied into the audit ring: %q", e.Detail)
			}

			file := filepath.Join(dir, "ben", "t-att.jsonl")
			info, err := os.Stat(file)
			if !tt.wantFile {
				if err == nil {
					t.Fatalf("%s written for a share below commands", file)
				}
				return
			}
			if err != nil {
				t.Fatalf("stat %s: %v", file, err)
			}
			if perm := info.Mode().Perm(); perm != 0o600 {
				t.Errorf("file mode = %o, want 600", perm)
			}
			if dinfo, err := os.Stat(filepath.Dir(file)); err != nil || dinfo.Mode().Perm() != 0o700 {
				t.Errorf("dir mode = %v (%v), want 700", dinfo, err)
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			body := string(raw)
			for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
				var row map[string]any
				if err := json.Unmarshal([]byte(line), &row); err != nil {
					t.Fatalf("line %q is not JSON: %v", line, err)
				}
				if row["event"] != "command" {
					t.Errorf("only command rows are stored, got %v", row)
				}
			}
			if hasReason := strings.Contains(body, `"reason"`); hasReason != tt.wantReason {
				t.Errorf("stored reason present = %v, want %v: %s", hasReason, tt.wantReason, body)
			}
		})
	}
}

func TestRelayAttest_TaskFailedFrameCarriesShare(t *testing.T) {
	redirectContribAuditDir(t)
	hub, _ := covK2Hub(t)
	conn := &ContributorConnection{
		profile:        &ContributorProfile{TrustTier: "contributor", GitHubUsername: "ben", ContributorID: "c-ben"},
		currentTask:    &WSTaskAssign{TaskID: "t-att", Kind: "issue", Repo: "myorg/repo1", Number: 21, Title: "t"},
		currentTaskGen: 4,
		taskAssignedAt: time.Now().Add(-90 * time.Second),
	}
	s := &wsSession{h: hub, contributor: conn}

	s.handleTaskFailed(WSMessage{Type: "task_failed", TaskID: "t-att", TaskGen: 4, Reason: "tests failed", FailureKind: "environment",
		AuditShare: &RelayAuditShare{Level: "lifecycle", CaptureLevel: "lifecycle", Rows: attestRows(attestLifecycleRow)}})

	if rows := relayAuditEntries(t, hub, agentaudit.AuditRelayTaskFailed); len(rows) != 1 {
		t.Fatalf("relay_task_failed rows = %d, want 1", len(rows))
	}
	rows := relayAuditEntries(t, hub, agentaudit.AuditRelayTaskAttested)
	if len(rows) != 1 || !strings.Contains(rows[0].Detail, "task=t-att") || !strings.Contains(rows[0].Detail, "level=lifecycle") {
		t.Fatalf("relay_task_attested rows = %+v, want one for t-att at lifecycle", rows)
	}
}

func TestRelayAttest_TaskFailedWithoutShareWritesNoAttestation(t *testing.T) {
	redirectContribAuditDir(t)
	hub, _ := covK2Hub(t)
	conn := &ContributorConnection{
		profile:        &ContributorProfile{TrustTier: "contributor", GitHubUsername: "ben", ContributorID: "c-ben"},
		currentTask:    &WSTaskAssign{TaskID: "t-none", Kind: "issue", Repo: "myorg/repo1", Number: 22, Title: "t"},
		currentTaskGen: 1,
	}
	s := &wsSession{h: hub, contributor: conn}

	s.handleTaskFailed(WSMessage{Type: "task_failed", TaskID: "t-none", TaskGen: 1, FailureKind: "task"})

	if rows := relayAuditEntries(t, hub, agentaudit.AuditRelayTaskAttested); len(rows) != 0 {
		t.Fatalf("relay_task_attested written without consent: %+v", rows)
	}
}

func TestContribAuditFileComponent(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Ben", "ben"},
		{"t-123.4", "t-123.4"},
		{"../../etc/passwd", "etc_passwd"},
		{"..", ""},
		{"  ", ""},
		{"a b/c", "a_b_c"},
		{strings.Repeat("x", 200), strings.Repeat("x", 128)},
	}
	for _, tt := range tests {
		if got := contribAuditFileComponent(tt.in); got != tt.want {
			t.Errorf("contribAuditFileComponent(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestPruneContribAudit_RemovesFilesPastMaxAge(t *testing.T) {
	root := redirectContribAuditDir(t)
	now := time.Now()
	oldDir, newDir := filepath.Join(root, "old"), filepath.Join(root, "new")
	for _, d := range []string{oldDir, newDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stale := filepath.Join(oldDir, "t-1.jsonl")
	fresh := filepath.Join(newDir, "t-2.jsonl")
	for _, f := range []string{stale, fresh} {
		if err := os.WriteFile(f, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := now.Add(-(auditMaxAgeDays + 1) * 24 * time.Hour)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatal(err)
	}

	pruneContribAudit(root, now)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale file still present: %v", err)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("empty contributor dir still present: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh file removed: %v", err)
	}
}

func TestContributeAuditShareRequestAdvertised(t *testing.T) {
	s, deps := apiServer(t)
	status := func() any {
		rec := doGet(s, "/api/contribute/status")
		if rec.Code != http.StatusOK {
			t.Fatalf("status code = %d", rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body["audit_share_requested"]
	}
	if got := status(); got != "" {
		t.Fatalf("default audit_share_requested = %#v, want empty", got)
	}
	deps.Config.Hub.ContributeAuditShareRequest = " Commands "
	if got := status(); got != "commands" {
		t.Fatalf("audit_share_requested = %#v, want commands", got)
	}
	deps.Config.Hub.ContributeAuditShareRequest = "everything"
	if got := status(); got != "" {
		t.Fatalf("unrecognised request must advertise nothing, got %#v", got)
	}
}

func TestContributeAuditShareRequestOnAuthOK(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	s.deps = &Dependencies{Config: &config.Config{}}
	s.deps.Config.Hub.ContributeAuditShareRequest = "reasoning"
	token, _ := registerWSUser(t, s, "audit-share-user")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: token, CLIBackend: "claude"})
	authOK := readMsg(t, conn)
	if authOK.Type != "auth_ok" {
		t.Fatalf("expected auth_ok, got %s", authOK.Type)
	}
	if authOK.AuditShareRequested != "reasoning" {
		t.Fatalf("auth_ok audit_share_requested = %q, want reasoning", authOK.AuditShareRequested)
	}
}

func TestContributeAuditShareRequestRenderedBeforeSetup(t *testing.T) {
	raw, err := os.ReadFile("contribute_landing.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{
		`id="onboarding-audit-share"`,
		`ccSetAuditShareRequest(d.audit_share_requested||'')`,
		`el.textContent=level`,
		`--share-audit`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("contribute_landing.go missing %q", want)
		}
	}
	if strings.Index(body, `id="onboarding-audit-share"`) > strings.Index(body, `id="copy-cmds"`) {
		t.Fatal("the requested audit level must be shown before the setup commands")
	}
}
