package dashboard

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/inbox"
	"github.com/hivecommons/hive/pkg/transcribe"
)

func doPostRole(s *Server, path, role string, verified bool, body any) *httptest.ResponseRecorder {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, &b)
	req.Header.Set("Content-Type", "application/json")
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
		req.Header.Set("X-Hive-User", role+"@example.com")
	}
	if verified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	s.mux.ServeHTTP(rec, req)
	return rec
}

// newTranscribeTestServer wires an Inbox with two pending recordings whose
// audio is served by a local server, and a fake provider.
func newTranscribeTestServer(t *testing.T, tx config.InboxTranscription) (*Server, *inbox.Service, *transcribe.Fake, string) {
	t.Helper()
	s, deps := apiServer(t)
	audio := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "AUDIOBYTES")
	}))
	t.Cleanup(audio.Close)
	src := &inboxAudioSource{
		items: []inbox.Item{
			{Source: "plaud", ID: "rec-1", Title: "One", Pending: true, DurationMS: 60000},
			{Source: "plaud", ID: "rec-2", Title: "Two", Pending: true, DurationMS: 120000},
		},
		link: inbox.AudioLink{URL: audio.URL + "/a.mp3?X-Amz-Signature=topsecret", ExpiresAt: time.Now().Add(time.Hour)},
	}
	deps.Config.Inbox = config.InboxConfig{Transcription: tx}
	svc := inbox.NewService(deps.Config.Inbox, inbox.NewStore(filepath.Join(t.TempDir(), "_inbox.json")), []inbox.Source{src}, nil, nil)
	svc.Run = func(fn func()) { fn() }
	svc.EnableTranscription(inbox.NewTranscriptStore(filepath.Join(t.TempDir(), "transcripts")))
	fake := &transcribe.Fake{Result: transcribe.Result{Segments: []transcribe.Segment{{Speaker: "Speaker 1", Text: "hi"}}, Duration: time.Minute}}
	svc.NewTranscriber = func(config.InboxTranscription) (transcribe.Transcriber, error) { return fake, nil }
	svc.Getenv = func(k string) string {
		if k == "DEEPGRAM_API_KEY" {
			return "dg-test-key"
		}
		return ""
	}
	deps.Inbox = svc
	return s, svc, fake, audio.URL
}

func TestInboxTranscribeRoleMatrixAndAudit(t *testing.T) {
	s, _, fake, audioURL := newTranscribeTestServer(t, config.InboxTranscription{Enabled: true, ConsentAcknowledged: true})
	path := func(id string) string { return "/api/inbox/plaud/" + id + "/transcribe" }

	for _, tc := range []struct {
		role     string
		verified bool
	}{{"", false}, {"read-only", false}, {"owner", false}} {
		if rec := doPostRole(s, path("rec-1"), tc.role, tc.verified, nil); rec.Code != http.StatusForbidden {
			t.Fatalf("role %q verified=%v = %d, want 403", tc.role, tc.verified, rec.Code)
		}
	}
	if fake.Calls() != 0 {
		t.Fatal("a refused request must not upload")
	}
	if rec := doPostRole(s, path("nope"), "owner", true, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown item = %d", rec.Code)
	}
	for _, tc := range []struct {
		role, id string
	}{{"read-write", "rec-1"}, {"owner", "rec-2"}} {
		rec := doPostRole(s, path(tc.id), tc.role, tc.role == "owner", nil)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s transcribe = %d: %s", tc.role, rec.Code, rec.Body.String())
		}
		var got InboxTranscribeResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.State.Status != inbox.TranscriptionQueued || got.ItemID != tc.id {
			t.Fatalf("%s response = %+v %v", tc.role, got, err)
		}
	}
	if fake.Calls() != 2 {
		t.Fatalf("uploads = %d", fake.Calls())
	}
	if rec := doPostRole(s, path("rec-1"), "owner", true, nil); rec.Code != http.StatusConflict {
		t.Fatalf("already transcribed = %d", rec.Code)
	}

	entries := s.audit.Recent(50)
	var runs []AuditEntry
	for _, e := range entries {
		if e.Action == "inbox_transcribe" {
			runs = append(runs, e)
		}
	}
	if len(runs) != 2 {
		t.Fatalf("inbox_transcribe entries = %+v", runs)
	}
	for _, e := range runs {
		for _, want := range []string{"source=plaud", "provider=deepgram", "model=nova-3", "audio_seconds=60", "estimated_usd=", "outcome=done"} {
			if !strings.Contains(e.Detail, want) {
				t.Errorf("audit detail %q missing %q", e.Detail, want)
			}
		}
		for _, leak := range []string{"topsecret", audioURL, "dg-test-key"} {
			if strings.Contains(e.Detail, leak) {
				t.Errorf("audit detail leaks %q: %q", leak, e.Detail)
			}
		}
	}
	if runs[0].User == "" {
		t.Fatalf("audit entry has no actor: %+v", runs[0])
	}

	// The listing and content reflect the machine transcript.
	list := decodeInboxList(t, doGet(s, "/api/inbox").Body.Bytes())
	for _, it := range list.Items {
		if it.Pending || it.Transcribable || it.Transcription == nil || it.Transcription.Status != inbox.TranscriptionDone {
			t.Fatalf("listed item = %+v", it)
		}
	}
	if !list.Settings.Transcription.KeyPresent || list.Settings.Transcription.TodayItems != 2 || list.Settings.Transcription.MonthMinutesUsed != 2 {
		t.Fatalf("transcription settings = %+v", list.Settings.Transcription)
	}
	// This source already returns text, so the upstream transcript wins
	// over the machine one in the content view.
	body := doGet(s, "/api/inbox/plaud/rec-1/content").Body.String()
	if strings.Contains(body, "transcript_label") || !strings.Contains(body, "[00:01] A: words") {
		t.Fatalf("content = %s", body)
	}
}

func TestInboxTranscribeGatesAndSkips(t *testing.T) {
	s, _, _, _ := newTranscribeTestServer(t, config.InboxTranscription{})
	if rec := doPostRole(s, "/api/inbox/plaud/rec-1/transcribe", "owner", true, nil); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("disabled = %d: %s", rec.Code, rec.Body.String())
	}
	s, _, fake, _ := newTranscribeTestServer(t, config.InboxTranscription{Enabled: true, ConsentAcknowledged: true, MaxDurationMinutes: 1})
	rec := doPostRole(s, "/api/inbox/plaud/rec-2/transcribe", "owner", true, nil)
	var got InboxTranscribeResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.State.Status != inbox.TranscriptionSkipped || !strings.HasPrefix(got.State.Reason, inbox.SkipTooLong) {
		t.Fatalf("too long = %d %s", rec.Code, rec.Body.String())
	}
	if fake.Calls() != 0 {
		t.Fatal("a skipped item must not upload")
	}
	if e := s.audit.Recent(1); len(e) != 1 || e[0].Action != "inbox_transcribe" || !strings.Contains(e[0].Detail, "outcome=skipped") {
		t.Fatalf("skip audit = %+v", e)
	}
	// Without a service the endpoint is a 404 like the other item routes.
	s2, _ := apiServer(t)
	if rec := doPostRole(s2, "/api/inbox/plaud/rec-1/transcribe", "owner", true, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("no inbox = %d", rec.Code)
	}
}

func TestInboxTranscribeBusyIsConflict(t *testing.T) {
	s, svc, _, _ := newTranscribeTestServer(t, config.InboxTranscription{Enabled: true, ConsentAcknowledged: true})
	// Hold the run so the item stays queued.
	release := make(chan struct{})
	svc.Run = func(fn func()) { go func() { <-release; fn() }() }
	if rec := doPostRole(s, "/api/inbox/plaud/rec-1/transcribe", "owner", true, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("first = %d", rec.Code)
	}
	if rec := doPostRole(s, "/api/inbox/plaud/rec-1/transcribe", "read-write", false, nil); rec.Code != http.StatusConflict {
		t.Fatalf("busy = %d", rec.Code)
	}
	close(release)
	svc.Wait()
}

func TestInboxSettingsTranscriptionConsentGate(t *testing.T) {
	s, deps := apiServer(t)
	deps.Inbox = newInboxTestService(t, config.InboxConfig{})
	rec := doPost(s, "/api/inbox/settings", map[string]any{"transcription": map[string]any{"enabled": true}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "consent_acknowledged") {
		t.Fatalf("enable without consent = %d: %s", rec.Code, rec.Body.String())
	}
	rec = doPost(s, "/api/inbox/settings", map[string]any{"transcription": map[string]any{
		"enabled": true, "consent_acknowledged": true, "mode": "manual", "api_key_env": "MY_DG_KEY", "monthly_budget_minutes": 300,
	}})
	if rec.Code != http.StatusOK {
		t.Fatalf("enable = %d: %s", rec.Code, rec.Body.String())
	}
	var st inbox.Settings
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Transcription.Enabled || st.Transcription.APIKeyEnv != "MY_DG_KEY" || st.Transcription.MonthlyBudgetMinutes != 300 || st.Transcription.KeyPresent {
		t.Fatalf("settings = %+v", st.Transcription)
	}
	if !deps.Config.Inbox.Transcription.Active() || !deps.Inbox.Config().Transcription.Active() {
		t.Fatalf("config not updated: %+v", deps.Config.Inbox.Transcription)
	}
	if strings.Contains(rec.Body.String(), "dg-") {
		t.Fatal("settings must never carry a key value")
	}
}

// TestInboxTranscriptionsLiveProgress: GET /api/inbox/transcriptions lists
// queued and running jobs with their place in line, never caches, and is
// empty once the queue drains.
func TestInboxTranscriptionsLiveProgress(t *testing.T) {
	s, svc, _, _ := newTranscribeTestServer(t, config.InboxTranscription{Enabled: true, ConsentAcknowledged: true})
	var pending []func()
	svc.Run = func(fn func()) { pending = append(pending, fn) }
	for _, id := range []string{"rec-1", "rec-2"} {
		if rec := doPostRole(s, "/api/inbox/plaud/"+id+"/transcribe", "owner", true, nil); rec.Code != http.StatusAccepted {
			t.Fatalf("queue %s = %d: %s", id, rec.Code, rec.Body.String())
		}
	}
	rec := doGet(s, "/api/inbox/transcriptions")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("live = %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	var got InboxTranscriptionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Active) != 2 || got.Active[1].ID != "rec-2" || got.Active[1].State.QueuePosition != 1 {
		t.Fatalf("live body = %s %v", rec.Body.String(), err)
	}
	if strings.Contains(rec.Body.String(), "dg-test-key") || strings.Contains(rec.Body.String(), "topsecret") {
		t.Fatalf("live body leaks: %s", rec.Body.String())
	}
	for _, fn := range pending {
		fn()
	}
	rec = doGet(s, "/api/inbox/transcriptions")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Active == nil || len(got.Active) != 0 {
		t.Fatalf("drained = %s %v", rec.Body.String(), err)
	}

	empty, _ := apiServer(t)
	if rec := doGet(empty, "/api/inbox/transcriptions"); rec.Code != http.StatusNotFound {
		t.Fatalf("no inbox = %d", rec.Code)
	}
}
