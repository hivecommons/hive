package commands

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

const auditCommandFixture = `{"v":1,"key":"k1","hub":"wss://acme.example/contribute","repository":"github/github.com/org/repo","task_id":"t1","outcome":"success","started_at":"2026-10-01T10:00:00Z","ended_at":"2026-10-01T10:05:00Z"}
{"v":2,"level":"lifecycle","event":"assigned","ts":"2026-10-01T10:00:00Z","hub":"wss://acme.example/contribute","repository":"github/github.com/org/repo","task_id":"t1","backend":"claude","capture":"shim"}
{"v":2,"level":"commands","event":"command","ts":"2026-10-01T10:01:00Z","task_id":"t1","op":"git","cmd":"git push origin HEAD","exit":0,"remote":"origin","refspec":"HEAD","reason":"push the\nfix","reason_source":"transcript"}
{"v":2,"level":"lifecycle","event":"pr_opened","ts":"2026-10-01T10:03:00Z","task_id":"t1","url":"https://github.com/org/repo/pull/9"}
{"v":2,"level":"lifecycle","event":"revoked","ts":"2026-10-02T10:00:00Z","task_id":"t2","reason":"hub cancelled"}
`

func TestHivesAuditUnavailable(t *testing.T) {
	h := newHivesHarness(t)
	if err := h.run(t, "", "hives", "audit"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "Audit: unavailable (retention: last 90 days") {
		t.Fatalf("got %q", h.out.String())
	}
	if err := h.run(t, "", "hives", "audit", "--json"); err != nil {
		t.Fatal(err)
	}
	var view relayAuditView
	if err := json.Unmarshal(h.out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Available || view.Rows == nil || len(view.Rows) != 0 {
		t.Fatalf("json must report unavailable with empty rows: %+v", view)
	}
}

func TestHivesAuditTaskViewAndFilters(t *testing.T) {
	h := newHivesHarness(t)
	if err := os.WriteFile(h.store.RelayActivityPath(), []byte(auditCommandFixture+"garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.run(t, "", "hives", "audit", "--task", "t1"); err != nil {
		t.Fatal(err)
	}
	out := h.out.String()
	for _, want := range []string{
		"2026-10-01T10:00:00Z\tlifecycle\ttask=t1\tassigned\tgithub/github.com/org/repo\tbackend=claude\tcapture=shim",
		"commands\ttask=t1\tgit push origin HEAD\texit=0\tremote=origin\trefspec=HEAD",
		"    reason (transcript): push the fix",
		"pr_opened\thttps://github.com/org/repo/pull/9",
		"off\ttask=t1\toutcome=success",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "task=t2") {
		t.Fatalf("--task leaked another task:\n%s", out)
	}
	if !strings.Contains(h.errs.String(), "Skipped 1 unreadable line") {
		t.Fatalf("malformed line not reported: %q", h.errs.String())
	}
	if err := h.run(t, "", "hives", "audit", "--level", "lifecycle", "--json"); err != nil {
		t.Fatal(err)
	}
	var view relayAuditView
	if err := json.Unmarshal(h.out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Available || len(view.Rows) != 4 {
		t.Fatalf("lifecycle level must drop command rows: %+v", view.Rows)
	}
	if err := h.run(t, "", "hives", "audit", "--since", "2026-10-02T00:00:00Z", "-o", "json"); err != nil {
		t.Fatal(err)
	}
	view = relayAuditView{}
	if err := json.Unmarshal(h.out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Rows) != 1 || view.Rows[0].Event != "revoked" {
		t.Fatalf("--since: %+v", view.Rows)
	}
	if err := h.run(t, "", "hives", "audit", "--task", "none"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "no matching rows") {
		t.Fatalf("got %q", h.out.String())
	}
}

func TestHivesAuditRejectsBadFlags(t *testing.T) {
	h := newHivesHarness(t)
	for _, args := range [][]string{
		{"hives", "audit", "--level", "loud"},
		{"hives", "audit", "--since", "yesterday"},
		{"hives", "audit", "--clear"},
		{"hives", "audit", "--clear", "--level", "commands"},
	} {
		if err := h.run(t, "", args...); err == nil {
			t.Fatalf("%v must fail", args)
		}
	}
}

func TestHivesAuditClearReasoningKeepsRows(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	if err := os.WriteFile(h.store.RelayActivityPath(), []byte(auditCommandFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(h.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.run(t, "", "hives", "audit", "--clear", "--level", "reasoning"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.errs.String(), "Cleared reasons from 1 command row") {
		t.Fatalf("got %q", h.errs.String())
	}
	data, err := os.ReadFile(h.store.RelayActivityPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "push the") || !strings.Contains(string(data), `"op":"git"`) || !strings.Contains(string(data), "hub cancelled") {
		t.Fatalf("clear must drop only the reasoning column:\n%s", data)
	}
	after, err := os.ReadFile(h.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || h.signalCalls != 0 {
		t.Fatal("--clear must not touch profiles or the relay")
	}
}

func TestParseAuditSince(t *testing.T) {
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"24h":                  now.Add(-24 * time.Hour),
		"7d":                   now.Add(-7 * 24 * time.Hour),
		"2026-10-01T00:00:00Z": time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}
	for in, want := range cases {
		got, err := parseAuditSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Fatalf("%s: got %v %v, want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "-1h", "xd", "soon"} {
		if _, err := parseAuditSince(bad, now); err == nil {
			t.Fatalf("%q must fail", bad)
		}
	}
}

func TestHivesSetAuditLevel(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	if err := h.run(t, "", "hives", "set", "other", "--audit-level", "Commands"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "Hive other audit level set to commands") || h.signalCalls != 1 {
		t.Fatalf("got %q signals=%d", h.out.String(), h.signalCalls)
	}
	if got := h.profiles(t).Profiles[1].AuditLevel; got != "commands" {
		t.Fatalf("stored level %q", got)
	}
	if !strings.Contains(h.env(t), "HIVE_RELAY_AUDIT_LEVEL=lifecycle,commands") {
		t.Fatalf("projection missing per-hub levels:\n%s", h.env(t))
	}
	for _, args := range [][]string{
		{"hives", "set", "other"},
		{"hives", "set", "other", "--audit-level", "loud"},
		{"hives", "set", "missing", "--audit-level", "off"},
	} {
		if err := h.run(t, "", args...); err == nil {
			t.Fatalf("%v must fail", args)
		}
	}
	if h.signalCalls != 1 {
		t.Fatalf("failed sets must not signal the relay: %d", h.signalCalls)
	}
}
