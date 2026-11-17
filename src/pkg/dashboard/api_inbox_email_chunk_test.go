package dashboard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/inbox"
	"github.com/hivecommons/hive/pkg/mailin/wire"
)

var chunkDeliverySeq int

func newChunkDeliveryID() string {
	chunkDeliverySeq++
	return strings.Repeat("0", 32-len(strconv.Itoa(chunkDeliverySeq))) + strconv.Itoa(chunkDeliverySeq)
}

// chunkPost posts one piece as the hub would.
func chunkPost(s *Server, delivery, bodyType string, piece []byte, key string) *httptest.ResponseRecorder {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, inboxEmailChunkPath, bytes.NewReader(piece))
	req.Header.Set(webhookRelayTimestampHeader, ts)
	req.Header.Set(webhookRelaySignatureHeader, signWebhookRelayForTest(key, ts, delivery, piece))
	req.Header.Set(webhookRelayDeliveryHeader, delivery)
	req.Header.Set(wire.HeaderBodyType, bodyType)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// sendChunked splits body into n pieces and posts them in order, returning
// the last answer.
func sendChunked(s *Server, id, bodyType string, body []byte, n int) *httptest.ResponseRecorder {
	size := (len(body) + n - 1) / n
	var rec *httptest.ResponseRecorder
	for i := 0; i < n; i++ {
		lo, hi := min(i*size, len(body)), min((i+1)*size, len(body))
		rec = chunkPost(s, wire.ChunkDelivery(id, i, n), bodyType, body[lo:hi], testMailRelayKey)
		if i < n-1 && rec.Code != http.StatusOK {
			return rec
		}
	}
	return rec
}

func multipartDelivery(t *testing.T, d wire.Delivery) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := wire.WriteMultipart(mw, d); err != nil {
		t.Fatal(err)
	}
	return mw.FormDataContentType(), buf.Bytes()
}

func videoAttachment(data []byte) wire.Attachment {
	sum := sha256.Sum256(data)
	return wire.Attachment{Name: "beach.mp4", Kind: wire.AttachVideo, ContentType: "video/mp4", Size: len(data), SHA256: hex.EncodeToString(sum[:]), Data: data}
}

func TestInboxEmailChunkedMultipart(t *testing.T) {
	s, src := mailFilesServer(t)
	video := bytes.Repeat([]byte("v"), 3000)
	id := newChunkDeliveryID()
	d := filesDelivery("bob@example.com", true, videoAttachment(video))
	d.ID, d.Files.Note = id, "from the beach"
	ct, body := multipartDelivery(t, d)

	rec := sendChunked(s, id, ct, body, 3)
	var out struct {
		Items    []string `json:"items"`
		Awaiting bool     `json:"awaiting_approval"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || len(out.Items) != 1 || out.Awaiting {
		t.Fatalf("chunked delivery: %d %s", rec.Code, rec.Body)
	}
	rc, m, err := src.Open(out.Items[0])
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if m.Kind != wire.AttachVideo || m.Size != int64(len(video)) || m.Note != "from the beach" {
		t.Fatalf("stored %+v", m)
	}
	if _, err := os.Stat(src.StageDir(id)); !os.IsNotExist(err) {
		t.Fatal("staging left behind")
	}
	// The file route serves a video as a download.
	rec = serve(s, userReq(http.MethodGet, "/api/inbox/email/"+out.Items[0]+"/file", "alice", "owner", ""))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "video/mp4" || !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("video file: %d %v", rec.Code, rec.Header())
	}

	// JSON in pieces works too (an older hive-mx, relayed by a new hub).
	id = newChunkDeliveryID()
	d = filesDelivery("stranger@example.org", true, wire.Attachment{Name: "n.md", Kind: wire.AttachMarkdown, ContentType: "text/markdown", Data: []byte("# chunked json")})
	d.ID = id
	jsonBody, _ := json.Marshal(d)
	rec = sendChunked(s, id, "application/json", jsonBody, 2)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || !out.Awaiting {
		t.Fatalf("chunked json: %d %s", rec.Code, rec.Body)
	}
}

func TestInboxEmailChunkRejects(t *testing.T) {
	s, src := mailFilesServer(t)
	id := newChunkDeliveryID()
	good := wire.ChunkDelivery(id, 0, 2)
	if rec := chunkPost(s, good, "application/json", []byte("x"), "wrong-key"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", rec.Code)
	}
	if rec := chunkPost(s, id, "application/json", []byte("x"), testMailRelayKey); rec.Code != http.StatusBadRequest {
		t.Fatalf("not a piece id: %d", rec.Code)
	}
	// Replaying a piece is refused.
	if rec := chunkPost(s, good, "application/json", []byte("x"), testMailRelayKey); rec.Code != http.StatusOK {
		t.Fatalf("first piece: %d %s", rec.Code, rec.Body)
	}
	if rec := chunkPost(s, good, "application/json", []byte("x"), testMailRelayKey); rec.Code != http.StatusUnauthorized {
		t.Fatalf("replayed piece: %d", rec.Code)
	}
	src.DropStage(id)

	// Bodies that do not decode, or are not files for this delivery.
	other := filesDelivery("bob@example.com", true, videoAttachment([]byte("v")))
	other.ID = newChunkDeliveryID() // not the piece's delivery id
	ct, mismatched := multipartDelivery(t, other)
	invite := wire.Delivery{ID: "x", Kind: wire.KindInvite}
	for name, c := range map[string]struct {
		bodyType string
		body     []byte
	}{
		"junk multipart": {"multipart/form-data; boundary=zz", []byte("junk")},
		"no boundary":    {"multipart/form-data", []byte("junk")},
		"bad type":       {"text/plain", []byte("x")},
		"no type":        {"", []byte("x")},
		"bad json":       {"application/json", []byte("{")},
		"other delivery": {ct, mismatched},
		"invite": {"application/json", func() []byte {
			b, _ := json.Marshal(invite)
			return b
		}()},
	} {
		id := newChunkDeliveryID()
		if rec := chunkPost(s, wire.ChunkDelivery(id, 0, 1), c.bodyType, c.body, testMailRelayKey); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}

	// Too large as a whole, and a full spool.
	t.Setenv(envInboxEmailMaxBytes, "4")
	if rec := chunkPost(s, wire.ChunkDelivery(newChunkDeliveryID(), 0, 2), "application/json", []byte("12345"), testMailRelayKey); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the cap: %d", rec.Code)
	}
	t.Setenv(envInboxEmailMaxBytes, "")
	if inboxEmailMaxBytes() != inboxEmailDefaultMaxBytes {
		t.Fatal("default cap")
	}
	full := &inbox.EmailSource{Dir: t.TempDir(), QuotaBytes: 1}
	s.deps.Inbox.EnableEmail(full)
	if rec := chunkPost(s, wire.ChunkDelivery(newChunkDeliveryID(), 0, 2), "application/json", []byte("12345"), testMailRelayKey); rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("spool full: %d", rec.Code)
	}
	// A missing piece when the last arrives.
	s.deps.Inbox.EnableEmail(src)
	if rec := chunkPost(s, wire.ChunkDelivery(newChunkDeliveryID(), 1, 2), "application/json", []byte("x"), testMailRelayKey); rec.Code != http.StatusBadRequest {
		t.Fatalf("gap: %d", rec.Code)
	}
	// Files off, no source, no relay key, a piece over the cap.
	t.Setenv("HIVE_MEETINGS_EMAIL_FILES_ENABLED", "off")
	if rec := chunkPost(s, wire.ChunkDelivery(newChunkDeliveryID(), 0, 2), "application/json", []byte("x"), testMailRelayKey); rec.Code != http.StatusConflict {
		t.Fatalf("files off: %d", rec.Code)
	}
	t.Setenv("HIVE_MEETINGS_EMAIL_FILES_ENABLED", "")
	s.deps.Inbox.EnableEmail(nil)
	if rec := chunkPost(s, wire.ChunkDelivery(newChunkDeliveryID(), 0, 2), "application/json", []byte("x"), testMailRelayKey); rec.Code != http.StatusConflict {
		t.Fatalf("no source: %d", rec.Code)
	}
	big := httptest.NewRequest(http.MethodPost, inboxEmailChunkPath, bytes.NewReader(make([]byte, wire.MaxRelayChunkBytes+1)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("piece over the cap: %d", rec.Code)
	}
	withWebhookRelayKey(t, "")
	if rec := chunkPost(s, wire.ChunkDelivery(newChunkDeliveryID(), 0, 2), "application/json", []byte("x"), testMailRelayKey); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no relay key: %d", rec.Code)
	}
}
