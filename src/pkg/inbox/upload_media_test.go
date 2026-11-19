package inbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
	"github.com/hivecommons/hive/pkg/transcribe"
)

var (
	mp3Head  = []byte("ID3\x04\x00\x00\x00\x00\x00\x00audio")
	m4aHead  = []byte("\x00\x00\x00\x20ftypM4A \x00\x00\x00\x00")
	wavHead  = []byte("RIFF\x24\x00\x00\x00WAVEfmt ")
	oggHead  = []byte("OggS\x00\x02\x00\x00")
	webmHead = []byte{0x1a, 0x45, 0xdf, 0xa3, 0x9f, 0x42, 0x86, 0x81}
	adtsHead = []byte{0xff, 0xf1, 0x50, 0x80, 0x02, 0x1f, 0xfc}
	syncHead = []byte{0xff, 0xfb, 0x90, 0x64, 0x00}
)

func TestSniffMedia(t *testing.T) {
	ok := []struct {
		name string
		head []byte
		kind string
	}{
		{"a.mp3", mp3Head, UploadKindAudio}, {"a.mp3", syncHead, UploadKindAudio}, {"a.m4a", m4aHead, UploadKindAudio},
		{"a.aac", adtsHead, UploadKindAudio}, {"a.wav", wavHead, UploadKindAudio}, {"a.ogg", oggHead, UploadKindAudio},
		{"a.opus", oggHead, UploadKindAudio}, {"a.webm", webmHead, UploadKindAudio}, {"a.mp4", m4aHead, UploadKindVideo},
		{"a.MOV", m4aHead, UploadKindVideo},
	}
	for _, tc := range ok {
		f, media, err := SniffMedia(tc.name, tc.head)
		if err != nil || !media || f.kind != tc.kind {
			t.Errorf("SniffMedia(%s) = %+v %v %v", tc.name, f, media, err)
		}
	}
	// A media extension over the wrong content, and media without an
	// extension, are refused; anything else is left to the text pipeline.
	for name, head := range map[string][]byte{"a.mp3": []byte("plain text"), "a.wav": oggHead, "evil.mp4": []byte("MZ\x90\x00"), "clip": mp3Head} {
		if _, media, err := SniffMedia(name, head); err == nil || media || err.Code != UploadErrUnsupported {
			t.Errorf("SniffMedia(%s) = %v %v", name, media, err)
		}
	}
	for name, head := range map[string][]byte{"notes.md": []byte("# hi"), "song.txt": mp3Head} {
		if _, media, err := SniffMedia(name, head); err != nil || media {
			t.Errorf("SniffMedia(%s) = %v %v, want left to the text pipeline", name, media, err)
		}
	}
	if exts := UploadMediaExtensions(); len(exts) != len(uploadMediaExtensions) || exts[0] > exts[1] {
		t.Fatalf("extensions = %v", exts)
	}
}

func addMedia(t *testing.T, e *uploadEnv, name string, head, body []byte) (Item, error) {
	t.Helper()
	return e.svc.AddMedia(context.Background(), MediaInput{Filename: name, Head: head, Body: bytes.NewReader(body), AddedBy: "ana"}, TranscribeOptions{Actor: "ana"})
}

func TestAddMediaStreamsToTheStoreAndPlaysLocally(t *testing.T) {
	e := newUploadEnv(t, config.InboxConfig{})
	body := bytes.Repeat([]byte{0x55}, 4096)
	it, err := addMedia(t, e, "standup.m4a", m4aHead, body)
	if err != nil {
		t.Fatal(err)
	}
	if it.UploadKind != UploadKindAudio || !it.Pending || it.FactState != connector.FactContentPending || it.Title != "standup" || it.Origin != "upload · standup.m4a" {
		t.Fatalf("item = %+v", it)
	}
	m, err := e.src.Store.Get(it.ID)
	if err != nil || m.Size != int64(len(m4aHead)+len(body)) || m.SHA256 == "" || m.MIME != "audio/mp4" {
		t.Fatalf("meta = %+v, %v", m, err)
	}
	info, err := os.Stat(filepath.Join(e.src.Store.Dir, it.ID, uploadOriginalFile))
	if err != nil || info.Mode().Perm() != uploadFileMode {
		t.Fatalf("original mode = %v", err)
	}
	fact, _ := os.ReadFile(it.FactPath)
	if !strings.Contains(string(fact), "attr_upload_content: pending") || !strings.Contains(string(fact), uploadMediaPendingNote) {
		t.Fatalf("fact = %s", fact)
	}
	// Playback and transcription read the file from this hive's disk.
	a, local, err := e.svc.OpenLocalAudio(context.Background(), it)
	if err != nil || !local || a.MIME != "audio/mp4" || a.Size != m.Size {
		t.Fatalf("OpenLocalAudio = %+v %v %v", a, local, err)
	}
	got, _ := io.ReadAll(a.File)
	_ = a.File.Close()
	if !bytes.HasPrefix(got, m4aHead) || len(got) != int(m.Size) {
		t.Fatal("audio bytes differ")
	}
	link, err := e.src.AudioLink(context.Background(), it)
	if err != nil || link.URL != "/api/inbox/upload/"+it.ID+"/audio/stream" {
		t.Fatalf("AudioLink = %+v, %v", link, err)
	}
	c, err := e.svc.Content(context.Background(), it)
	if err != nil || !c.Pending || c.Markdown != "" {
		t.Fatalf("content = %+v, %v", c, err)
	}
	// Documents have no audio.
	doc, _ := e.svc.AddUpload(context.Background(), UploadInput{Filename: "n.md", Data: []byte("# n")})
	if _, err := e.src.AudioLink(context.Background(), doc); !errors.Is(err, ErrNoAudio) {
		t.Fatalf("doc audio = %v", err)
	}
	if _, err := e.src.OpenAudio(context.Background(), doc); !errors.Is(err, ErrNoAudio) {
		t.Fatalf("doc open = %v", err)
	}
}

func TestAddMediaCapsQuotaAndRetention(t *testing.T) {
	e := newUploadEnv(t, config.InboxConfig{Recording: config.InboxRecording{MaxBytes: 1024}, Uploads: config.InboxUploads{QuotaBytes: 1 << 20}})
	code := func(err error) string {
		var ue *UploadError
		if errors.As(err, &ue) {
			return ue.Code
		}
		return "?"
	}
	if _, err := addMedia(t, e, "big.mp3", mp3Head, bytes.Repeat([]byte{1}, 2048)); code(err) != UploadErrTooLarge {
		t.Fatalf("over the recording cap = %v", err)
	}
	if list, _ := e.src.Store.List(); len(list) != 0 {
		t.Fatal("a refused stream left an upload behind")
	}
	if _, err := addMedia(t, e, "fake.mp3", []byte("not audio"), nil); code(err) != UploadErrUnsupported {
		t.Fatalf("mismatch = %v", err)
	}
	it, err := addMedia(t, e, "memo.mp3", mp3Head, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	st := e.svc.Settings().Uploads
	if st.MaxMediaBytes != 1024 || st.MediaRetentionDays != config.DefaultInboxRecordingRetentionDays || len(st.MediaExtensions) == 0 {
		t.Fatalf("settings = %+v", st)
	}
	// The original goes after the recordings retention; the item stays.
	e.now = e.now.Add(time.Duration(config.DefaultInboxRecordingRetentionDays+1) * hoursPerDay * time.Hour)
	if n, err := e.src.SweepRetention(); err != nil || n != 1 {
		t.Fatalf("retention = %d, %v", n, err)
	}
	if _, err := e.src.AudioLink(context.Background(), it); !errors.Is(err, ErrNoAudio) {
		t.Fatalf("expired audio link = %v", err)
	}
	if _, err := e.src.OpenAudio(context.Background(), it); !errors.Is(err, ErrNoAudio) {
		t.Fatalf("expired open = %v", err)
	}
	items, _ := e.svc.Items(context.Background())
	if len(items) != 1 {
		t.Fatalf("items after retention = %d", len(items))
	}
	// A full quota refuses before streaming.
	full := newUploadEnv(t, config.InboxConfig{Uploads: config.InboxUploads{QuotaBytes: 1}})
	if _, err := full.svc.AddUpload(context.Background(), UploadInput{Filename: "a.txt", Data: []byte("a")}); code(err) != UploadErrQuota {
		t.Fatalf("doc over quota = %v", err)
	}
	_ = os.MkdirAll(full.src.Store.Dir, 0o700)
	_ = os.WriteFile(filepath.Join(full.src.Store.Dir, "pad"), []byte("pp"), 0o600)
	if _, err := addMedia(t, full, "a.mp3", mp3Head, nil); code(err) != UploadErrQuota {
		t.Fatalf("media over quota = %v", err)
	}
}

func TestAddMediaQueuesTranscription(t *testing.T) {
	e := newUploadEnv(t, config.InboxConfig{Transcription: activeTx()})
	e.svc.Run = func(fn func()) { fn() }
	e.svc.EnableTranscription(NewTranscriptStore(filepath.Join(t.TempDir(), "transcripts")))
	e.svc.NewTranscriber = func(config.InboxTranscription) (transcribe.Transcriber, error) {
		return &transcribe.Fake{Result: transcribe.Result{Segments: []transcribe.Segment{{Text: "hello"}}, Duration: time.Second}}, nil
	}
	e.svc.Getenv = func(k string) string {
		if k == "DEEPGRAM_API_KEY" {
			return "k"
		}
		return ""
	}
	it, err := addMedia(t, e, "memo.ogg", oggHead, []byte("body"))
	if err != nil {
		t.Fatal(err)
	}
	items, _ := e.svc.Items(context.Background())
	for _, got := range items {
		if got.ID == it.ID && (got.Transcription == nil || got.Transcription.Status == "") {
			t.Fatalf("no transcription was queued: %+v", got)
		}
	}
}
