package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/inbox"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

const inboxTestFact = `---
title: RFC: Voice inbox
type: reference
layer: personal
status: active
source: plaud
connector: plaud
source_id: rec-1
attr_plaud_file_id: rec-1
attr_recorded_at: 2026-11-10T08:58:00Z
synthesized: 2026-11-10T08:58:00Z
---

## Summary (Plaud)

- ship the inbox

## Transcript

Hello hive
`

type inboxTestDest struct {
	name string
	got  []inbox.Request
	err  error
}

func (d *inboxTestDest) Name() string { return d.name }
func (d *inboxTestDest) Deliver(_ context.Context, _ inbox.Item, _ inbox.Content, req inbox.Request) (inbox.Result, error) {
	d.got = append(d.got, req)
	if d.err != nil {
		return inbox.Result{}, d.err
	}
	return inbox.Result{URL: "https://github.com/o/r/issues/1", Children: []string{"https://github.com/o/r/issues/2"}}, nil
}

func newInboxTestService(t *testing.T, cfg config.InboxConfig, dests ...inbox.Destination) *inbox.Service {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "connectors", "personal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plaud-plaud-rec-1.md"), []byte(inboxTestFact), 0o644); err != nil {
		t.Fatal(err)
	}
	src := &inbox.FactSource{ConnectorsDir: filepath.Join(base, "connectors"), SourceType: "plaud"}
	svc := inbox.NewService(cfg, inbox.NewStore(filepath.Join(base, "inbox.json")), []inbox.Source{src}, dests, nil)
	svc.Run = func(fn func()) { fn() }
	return svc
}

func decodeInboxList(t *testing.T, body []byte) InboxListResponse {
	t.Helper()
	var resp InboxListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	return resp
}

func TestInboxEndpoints(t *testing.T) {
	s, deps := apiServer(t)
	// No service: still 200 with an empty list so the panel renders its
	// empty state instead of "Loading inbox…" forever (#17918).
	rec0 := doGet(s, "/api/inbox")
	if rec0.Code != http.StatusOK {
		t.Fatalf("inbox without service = %d, want 200: %s", rec0.Code, rec0.Body.String())
	}
	if empty := decodeInboxList(t, rec0.Body.Bytes()); empty.Items == nil || len(empty.Items) != 0 || empty.Sources == nil || empty.Settings.Rules == nil {
		t.Fatalf("empty inbox = %s", rec0.Body.String())
	}
	if rec := doGet(s, "/api/inbox/settings"); rec.Code != http.StatusNotFound {
		t.Fatalf("settings without service = %d, want 404", rec.Code)
	}
	spek := &inboxTestDest{name: inbox.DestSpekRFC}
	dibs := &inboxTestDest{name: inbox.DestDibs, err: errors.New("dibs down")}
	deps.Config.Inbox = config.InboxConfig{Spek: config.InboxSpek{Repo: "myorg/rfcs"}}
	deps.Inbox = newInboxTestService(t, deps.Config.Inbox, spek, dibs)
	deps.KnowledgeConnectors = newTestConnectorSyncer(t, &authConn{state: connector.AuthState{Connected: true, Account: "andy@example.com"}})

	rec := doGet(s, "/api/inbox")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeInboxList(t, rec.Body.Bytes())
	if len(resp.Items) != 1 || resp.Items[0].ID != "rec-1" || resp.Items[0].FactSlug != "plaud-plaud-rec-1" || resp.Unprocessed != 1 {
		t.Fatalf("list = %+v", resp)
	}
	if len(resp.Repos) != 2 || resp.Repos[0] != "myorg/rfcs" || resp.Repos[1] != "repo1" {
		t.Fatalf("repos = %v", resp.Repos)
	}
	if len(resp.Sources) != 2 || resp.Sources[0].Type != "plaud" || resp.Sources[0].Configured || resp.Settings.SpekRepo != "myorg/rfcs" {
		t.Fatalf("sources/settings = %+v %+v", resp.Sources, resp.Settings)
	}

	if rec := doPostNoOwner(s, "/api/inbox/route", map[string]string{"source": "plaud", "item_id": "rec-1", "destination": "spek_rfc"}); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner route = %d, want 403", rec.Code)
	}
	if rec := doPost(s, "/api/inbox/route", map[string]string{"source": "plaud", "item_id": "nope", "destination": "spek_rfc"}); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown item = %d", rec.Code)
	}
	if rec := doPost(s, "/api/inbox/route", map[string]string{"source": "plaud", "item_id": "rec-1", "destination": "spek_rfc", "repo": "bad repo"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad repo = %d", rec.Code)
	}
	if rec := doPost(s, "/api/inbox/route", map[string]string{"source": "plaud", "item_id": "rec-1", "destination": "nowhere"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown destination = %d: %s", rec.Code, rec.Body.String())
	}
	rec = doPost(s, "/api/inbox/route", map[string]string{"source": "plaud", "item_id": "rec-1", "destination": "spek_rfc"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("route = %d: %s", rec.Code, rec.Body.String())
	}
	if len(spek.got) != 1 || spek.got[0].Repo != "myorg/rfcs" {
		t.Fatalf("spek requests = %+v (default repo must apply)", spek.got)
	}
	resp = decodeInboxList(t, doGet(s, "/api/inbox").Body.Bytes())
	r := resp.Items[0].Routes[inbox.DestSpekRFC]
	if r == nil || r.Status != inbox.StatusFiled || r.URL != "https://github.com/o/r/issues/1" || len(r.Children) != 1 || resp.Unprocessed != 0 {
		t.Fatalf("spek route = %+v unprocessed=%d", r, resp.Unprocessed)
	}
	rec = doPost(s, "/api/inbox/route", map[string]string{"source": "plaud", "item_id": "rec-1", "destination": "dibs"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("dibs route = %d: %s", rec.Code, rec.Body.String())
	}
	resp = decodeInboxList(t, doGet(s, "/api/inbox").Body.Bytes())
	if d := resp.Items[0].Routes[inbox.DestDibs]; d == nil || d.Status != inbox.StatusFailed || d.Error != "dibs down" {
		t.Fatalf("dibs route = %+v", d)
	}

	rec = doPost(s, "/api/inbox/dismiss", map[string]any{"items": []map[string]string{{"source": "plaud", "item_id": "rec-1"}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss = %d: %s", rec.Code, rec.Body.String())
	}
	resp = decodeInboxList(t, doGet(s, "/api/inbox").Body.Bytes())
	if !resp.Items[0].Dismissed {
		t.Fatalf("item not dismissed: %+v", resp.Items[0])
	}
}

func TestInboxSettingsPersistRules(t *testing.T) {
	s, deps := apiServer(t)
	deps.Inbox = newInboxTestService(t, config.InboxConfig{})
	if rec := doPost(s, "/api/inbox/settings", map[string]any{"rules": []map[string]string{{"destination": "spek_rfc"}}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("rule without filter = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if rec := doPost(s, "/api/inbox/settings", map[string]any{"spek_repo": "not-a-repo"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad repo = %d, want 400", rec.Code)
	}
	rec := doPost(s, "/api/inbox/settings", map[string]any{
		"rules":     []map[string]string{{"source": "plaud", "title_prefix": "RFC:", "destination": "spek_rfc", "repo": "o/r"}},
		"spek_repo": "o/r",
		"dibs_url":  "https://dibs.example/",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("settings = %d: %s", rec.Code, rec.Body.String())
	}
	var st inbox.Settings
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Rules) != 1 || st.Rules[0].TitlePrefix != "RFC:" || st.SpekRepo != "o/r" || st.DibsURL != "https://dibs.example" {
		t.Fatalf("settings = %+v", st)
	}
	if deps.Config.Inbox.Spek.Repo != "o/r" || len(deps.Config.Inbox.Rules) != 1 || deps.Inbox.Config().Spek.Repo != "o/r" {
		t.Fatalf("config not updated: %+v", deps.Config.Inbox)
	}
	if rec := doGet(s, "/api/inbox/settings"); rec.Code != http.StatusOK {
		t.Fatalf("settings get = %d", rec.Code)
	}
}

// completerConn is an Authenticator that also accepts pasted completions.
type completerConn struct {
	authConn
	got connector.AuthCompletion
}

func (c *completerConn) CompleteAuth(_ context.Context, comp connector.AuthCompletion) (connector.AuthState, error) {
	c.got = comp
	if comp.Tokens == "" && comp.RedirectURL == "" {
		return c.state, errors.New("nothing pasted")
	}
	c.state = connector.AuthState{Connected: true, Account: "andy@example.com", CheckedAt: time.Now()}
	return c.state, nil
}

func TestKnowledgeConnectorAuthComplete(t *testing.T) {
	s, deps := apiServer(t)
	deps.KnowledgeConnectors = newTestConnectorSyncer(t, &authConn{})
	if rec := doPost(s, "/api/knowledge/connectors/wiki/auth/complete", map[string]string{"tokens": "{}"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-completer = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	conn := &completerConn{}
	deps.KnowledgeConnectors = newTestConnectorSyncer(t, conn)
	if rec := doPostNoOwner(s, "/api/knowledge/connectors/wiki/auth/complete", map[string]string{"tokens": "{}"}); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner = %d, want 403", rec.Code)
	}
	if rec := doPost(s, "/api/knowledge/connectors/wiki/auth/complete", map[string]string{}); rec.Code != http.StatusBadGateway {
		t.Fatalf("empty completion = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	rec := doPost(s, "/api/knowledge/connectors/wiki/auth/complete", map[string]string{"redirect_url": "http://localhost:8199/auth/callback?code=x&state=y"})
	if rec.Code != http.StatusOK {
		t.Fatalf("complete = %d: %s", rec.Code, rec.Body.String())
	}
	if st := decodeAuthState(t, rec.Body.Bytes()); !st.Connected || conn.got.RedirectURL == "" {
		t.Fatalf("state = %+v got = %+v", st, conn.got)
	}
}
