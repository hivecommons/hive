package dashboard

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/inbox"
)

// streamTestAudio is the recording the fake storage serves.
var streamTestAudio = []byte("OggS-fake-opus-recording-bytes-0123456789")

// streamTestSecret is the presigned query; it must never leave the hive.
const streamTestSecret = "X-Amz-Signature=topsecret"

// fakeAudioStorage is a cloud store that serves one object behind a
// presigned query, honouring Range unless ignoreRange is set.
type fakeAudioStorage struct {
	mu          sync.Mutex
	status      int
	ignoreRange bool
	ranges      []string
}

func (f *fakeAudioStorage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.ranges = append(f.ranges, r.Header.Get("Range"))
	status, ignore := f.status, f.ignoreRange
	f.mu.Unlock()
	if status != 0 {
		http.Error(w, "<Error><Code>AccessDenied</Code><Message>Request has expired</Message></Error>", status)
		return
	}
	if !strings.Contains(r.URL.RawQuery, streamTestSecret) {
		http.Error(w, "unsigned", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "binary/octet-stream")
	if ignore {
		r.Header.Del("Range")
		w.Header().Del("Accept-Ranges")
	}
	http.ServeContent(w, r, "rec.ogg", time.Time{}, bytes.NewReader(streamTestAudio))
}

func (f *fakeAudioStorage) set(status int, ignoreRange bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.ignoreRange = status, ignoreRange
}

// streamServer wires an Inbox with one audio item whose link points at the
// fake storage, logging into buf.
func streamServer(t *testing.T) (*Server, *inboxAudioSource, *fakeAudioStorage, string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := NewServer(0, logger)
	deps := testDeps(t)
	s.RegisterAPI(deps)
	storage := &fakeAudioStorage{}
	upstream := httptest.NewServer(storage)
	t.Cleanup(upstream.Close)
	link := upstream.URL + "/audiofiles/rec-1.ogg?X-Amz-Expires=86400&" + streamTestSecret
	src := &inboxAudioSource{
		items: []inbox.Item{{Source: "plaud", ID: "rec-1", Title: "cost considerations", DurationMS: 307000}},
		link:  inbox.AudioLink{URL: link, ExpiresAt: time.Now().Add(24 * time.Hour)},
	}
	svc := inbox.NewService(config.InboxConfig{}, inbox.NewStore(filepath.Join(t.TempDir(), "_inbox.json")), []inbox.Source{src}, nil, logger)
	svc.Logger = logger
	svc.AudioClient = upstream.Client()
	deps.Inbox = svc
	return s, src, storage, link, &buf
}

func doStream(s *Server, path, role string, verified bool, rng string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(context.Background())
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	if verified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	s.mux.ServeHTTP(rec, req)
	return rec
}

const streamPath = "/api/inbox/plaud/rec-1/audio/stream"

func TestInboxAudioStreamRoles(t *testing.T) {
	s, src, _, _, _ := streamServer(t)
	for _, tc := range []struct {
		role     string
		verified bool
	}{{"", false}, {config.RoleRead, false}, {"read-only", false}, {config.RoleOwner, false}} {
		if rec := doStream(s, streamPath, tc.role, tc.verified, ""); rec.Code != http.StatusForbidden {
			t.Fatalf("role %q verified=%v = %d, want 403", tc.role, tc.verified, rec.Code)
		}
	}
	if src.links != 0 {
		t.Fatalf("link minted for a refused request (%d)", src.links)
	}
	for _, tc := range []struct {
		role     string
		verified bool
	}{{config.RoleReadWrite, false}, {config.RoleOwner, true}} {
		rec := doStream(s, streamPath, tc.role, tc.verified, "")
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), streamTestAudio) {
			t.Fatalf("%s stream = %d %q", tc.role, rec.Code, rec.Body.String())
		}
	}
	if rec := doStream(s, "/api/inbox/plaud/nope/audio/stream", config.RoleOwner, true, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown item = %d", rec.Code)
	}
}

func TestInboxAudioStreamRangeAndHeaders(t *testing.T) {
	s, src, storage, link, logs := streamServer(t)
	rec := doStream(s, streamPath, config.RoleOwner, true, "bytes=4-9")
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("range = %d: %s", rec.Code, rec.Body.String())
	}
	want := "bytes 4-9/" + strconv.Itoa(len(streamTestAudio))
	if got := rec.Header().Get("Content-Range"); got != want {
		t.Fatalf("Content-Range = %q, want %q", got, want)
	}
	if !bytes.Equal(rec.Body.Bytes(), streamTestAudio[4:10]) {
		t.Fatalf("range body = %q", rec.Body.String())
	}
	if storage.ranges[len(storage.ranges)-1] != "bytes=4-9" {
		t.Fatalf("Range not passed through: %v", storage.ranges)
	}
	h := rec.Header()
	if h.Get("Cache-Control") != "no-store" || h.Get("Accept-Ranges") != "bytes" || h.Get("Content-Length") != "6" {
		t.Fatalf("headers = %v", h)
	}
	// binary/octet-stream from storage is replaced by the link's type.
	if ct := h.Get("Content-Type"); ct != "audio/ogg" {
		t.Fatalf("Content-Type = %q, want audio/ogg", ct)
	}

	// A second request (seek) reuses the minted link from memory.
	if rec := doStream(s, streamPath, config.RoleOwner, true, "bytes=0-"); rec.Code != http.StatusPartialContent {
		t.Fatalf("seek = %d", rec.Code)
	}
	if src.links != 1 {
		t.Fatalf("links minted = %d, want 1 for one playback", src.links)
	}
	var streams int
	for _, e := range s.audit.Recent(50) {
		if e.Action == "inbox_audio_stream" {
			streams++
			if strings.Contains(e.Detail, "topsecret") || strings.Contains(e.Detail, link) {
				t.Fatalf("audit leaks the link: %q", e.Detail)
			}
		}
	}
	if streams != 1 {
		t.Fatalf("inbox_audio_stream entries = %d, want 1 (one per minted link)", streams)
	}

	// Upstream that ignores Range: the whole file comes back as 200.
	storage.set(0, true)
	rec = doStream(s, streamPath, config.RoleOwner, true, "bytes=4-9")
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), streamTestAudio) || rec.Header().Get("Content-Range") != "" {
		t.Fatalf("ignored range = %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("full reply must be no-store")
	}
	if strings.Contains(logs.String(), "topsecret") {
		t.Fatalf("logs leak the presigned link: %s", logs.String())
	}
}

func TestInboxAudioStreamUpstreamRefused(t *testing.T) {
	s, src, storage, link, logs := streamServer(t)
	if rec := doStream(s, streamPath, config.RoleReadWrite, false, ""); rec.Code != http.StatusOK {
		t.Fatalf("first play = %d", rec.Code)
	}
	// The link expires (S3 answers 403 AccessDenied "Request has expired").
	storage.set(http.StatusForbidden, false)
	rec := doStream(s, streamPath, config.RoleReadWrite, false, "bytes=0-")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expired link = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "refused the audio link") || !strings.Contains(body, "HTTP 403") {
		t.Fatalf("error must say the link was refused: %s", body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("error reply must be no-store")
	}
	// The cached link is refused, so one fresh link is minted before giving up.
	if src.links != 2 {
		t.Fatalf("links minted = %d, want a retry with a fresh link", src.links)
	}
	for _, out := range []string{body, logs.String()} {
		if strings.Contains(out, "topsecret") || strings.Contains(out, link) {
			t.Fatalf("presigned link leaked: %s", out)
		}
	}
	// Storage recovers: the next play works with a fresh link.
	storage.set(0, false)
	if rec := doStream(s, streamPath, config.RoleReadWrite, false, ""); rec.Code != http.StatusOK {
		t.Fatalf("recovered = %d", rec.Code)
	}
	if src.links != 3 {
		t.Fatalf("links minted = %d, want the refused link dropped from memory", src.links)
	}
}
