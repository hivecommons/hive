package inbox

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// recordingClock is a settable test clock.
type recordingClock struct{ t time.Time }

func (c *recordingClock) now() time.Time { return c.t }

func enabledRecording() config.InboxRecording {
	on := true
	return config.InboxRecording{Enabled: &on}
}

// webmChunk returns a chunk that starts like a MediaRecorder WebM stream.
func webmChunk(n int) []byte {
	b := append([]byte{}, webmMagic...)
	return append(b, bytes.Repeat([]byte{0x42}, n)...)
}

// mp4Box builds one ISO BMFF box.
func mp4Box(typ string, payload []byte) []byte {
	b := make([]byte, mp4BoxHeader, mp4BoxHeader+len(payload))
	binary.BigEndian.PutUint32(b[:4], uint32(mp4BoxHeader+len(payload)))
	copy(b[4:8], typ)
	return append(b, payload...)
}

func newRecordingService(t *testing.T, cfg config.InboxRecording) (*Service, *RecordingSource, *recordingClock) {
	t.Helper()
	clock := &recordingClock{t: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	store := NewRecordingStore(filepath.Join(dir, "recordings"))
	store.Now = clock.now
	src := &RecordingSource{Store: store, FactDir: filepath.Join(dir, "connectors", RecordingLayer)}
	svc := NewService(config.InboxConfig{Recording: cfg}, NewStore(filepath.Join(dir, "_inbox.json")), nil, nil, nil)
	svc.Now = clock.now
	svc.Run = func(fn func()) { fn() }
	svc.EnableTranscription(NewTranscriptStore(filepath.Join(dir, "transcripts")))
	svc.EnableRecordings(src)
	return svc, src, clock
}

func startRecording(t *testing.T, svc *Service, user, mime string) RecordingMeta {
	t.Helper()
	m, err := svc.StartRecording(RecordingStart{Title: "Standup", MIME: mime, ConsentVersion: RecordingConsentVersion, User: user, Role: "read-write"}, RecordingOptions{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return m
}

func TestRecordingEnabledFlag(t *testing.T) {
	svc, _, _ := newRecordingService(t, config.InboxRecording{})
	if !svc.RecordingSettings().Enabled {
		t.Fatal("recording is not on by default")
	}
	off := false
	svc, _, _ = newRecordingService(t, config.InboxRecording{Enabled: &off})
	if _, err := svc.StartRecording(RecordingStart{MIME: "audio/webm", ConsentVersion: RecordingConsentVersion, User: "a"}, RecordingOptions{}); !errors.Is(err, ErrRecordingDisabled) {
		t.Fatalf("start with enabled: false = %v, want ErrRecordingDisabled", err)
	}
	if svc.RecordingSettings().Enabled {
		t.Fatal("settings report recording enabled with enabled: false")
	}
}

func TestRecordingConsentRequiredAndStored(t *testing.T) {
	svc, src, clock := newRecordingService(t, enabledRecording())
	for _, v := range []string{"", "old-notice"} {
		if _, err := svc.StartRecording(RecordingStart{MIME: "audio/webm", ConsentVersion: v, User: "ana"}, RecordingOptions{}); !errors.Is(err, ErrRecordingConsent) {
			t.Fatalf("consent %q = %v, want ErrRecordingConsent", v, err)
		}
	}
	var audits []RecordingAudit
	m, err := svc.StartRecording(RecordingStart{Title: "Design review", MIME: "audio/webm;codecs=opus", ConsentVersion: RecordingConsentVersion, IncludesTabAudio: true, User: "ana", Role: "owner"},
		RecordingOptions{Actor: "ana", Audit: func(a RecordingAudit) { audits = append(audits, a) }})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(src.Store.Dir, m.ID, recordingMetaFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"consent_version": "` + RecordingConsentVersion + `"`, `"recorded_by": "ana"`, `"includes_tab_audio": true`, `"consent_at": "` + clock.t.Format(time.RFC3339)} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("meta.json lacks %s:\n%s", want, data)
		}
	}
	if len(audits) != 1 || audits[0].Action != "inbox_recording_start" || !strings.Contains(audits[0].Detail(), "consent_version="+RecordingConsentVersion) {
		t.Fatalf("audits = %+v", audits)
	}
	if st, err := os.Stat(filepath.Join(src.Store.Dir, m.ID)); err != nil || st.Mode().Perm() != recordingDirMode {
		t.Fatalf("recording dir mode = %v, %v", st, err)
	}
	if st, err := os.Stat(filepath.Join(src.Store.Dir, m.ID, recordingMetaFile)); err != nil || st.Mode().Perm() != recordingFileMode {
		t.Fatalf("meta mode = %v, %v", st, err)
	}
}

func TestRecordingOneActivePerUser(t *testing.T) {
	svc, _, _ := newRecordingService(t, enabledRecording())
	first := startRecording(t, svc, "ana", "audio/webm")
	_, err := svc.StartRecording(RecordingStart{MIME: "audio/webm", ConsentVersion: RecordingConsentVersion, User: "ana"}, RecordingOptions{})
	var active *RecordingActiveError
	if !errors.As(err, &active) || active.ID != first.ID {
		t.Fatalf("second start = %v, want RecordingActiveError(%s)", err, first.ID)
	}
	startRecording(t, svc, "ben", "audio/webm")
}

func TestRecordingMIMEAndContainerSniff(t *testing.T) {
	svc, _, _ := newRecordingService(t, enabledRecording())
	for _, mt := range []string{"audio/ogg", "video/webm", "text/plain", "", "audio/wav"} {
		if _, err := svc.StartRecording(RecordingStart{MIME: mt, ConsentVersion: RecordingConsentVersion, User: "x"}, RecordingOptions{}); !errors.Is(err, ErrRecordingMIME) {
			t.Fatalf("mime %q = %v, want ErrRecordingMIME", mt, err)
		}
	}
	m := startRecording(t, svc, "ana", "audio/webm")
	for _, junk := range [][]byte{[]byte("<html>not audio</html>"), []byte("RIFF....WAVEfmt "), mp4Box("ftyp", []byte("isom"))} {
		if _, err := svc.AppendRecordingChunk(m.ID, "ana", 0, junk); !errors.Is(err, ErrRecordingNotMedia) {
			t.Fatalf("first chunk %q = %v, want ErrRecordingNotMedia", junk[:4], err)
		}
	}
	mp4 := startRecording(t, svc, "ben", "audio/mp4")
	if _, err := svc.AppendRecordingChunk(mp4.ID, "ben", 0, webmChunk(4)); !errors.Is(err, ErrRecordingNotMedia) {
		t.Fatalf("webm bytes into an mp4 recording = %v, want ErrRecordingNotMedia", err)
	}
	if _, err := svc.AppendRecordingChunk(mp4.ID, "ben", 0, mp4Box("ftyp", []byte("iso5"))); err != nil {
		t.Fatalf("mp4 first chunk: %v", err)
	}
}

func TestRecordingChunkOrderGapAndIdempotentResend(t *testing.T) {
	svc, src, clock := newRecordingService(t, enabledRecording())
	m := startRecording(t, svc, "ana", "audio/webm")
	if _, err := svc.AppendRecordingChunk(m.ID, "ben", 0, webmChunk(8)); !errors.Is(err, ErrRecordingNotYours) {
		t.Fatalf("other user's chunk = %v, want ErrRecordingNotYours", err)
	}
	c0 := webmChunk(8)
	res, err := svc.AppendRecordingChunk(m.ID, "ana", 0, c0)
	if err != nil || res.NextSeq != 1 || res.Duplicate {
		t.Fatalf("chunk 0 = %+v, %v", res, err)
	}
	_, err = svc.AppendRecordingChunk(m.ID, "ana", 2, []byte("later"))
	var gap *RecordingGapError
	if !errors.As(err, &gap) || gap.Expected != 1 || gap.Got != 2 {
		t.Fatalf("gap = %v, want expected 1", err)
	}
	res, err = svc.AppendRecordingChunk(m.ID, "ana", 0, c0)
	if err != nil || !res.Duplicate || res.NextSeq != 1 {
		t.Fatalf("resend = %+v, %v; want duplicate ack", res, err)
	}
	if _, err := svc.AppendRecordingChunk(m.ID, "ana", 0, webmChunk(9)); !errors.Is(err, ErrRecordingMismatch) {
		t.Fatalf("resend with other bytes = %v, want ErrRecordingMismatch", err)
	}
	if _, err := svc.AppendRecordingChunk(m.ID, "ana", 1, []byte("second")); err != nil {
		t.Fatal(err)
	}
	got, _ := src.Store.Get(m.ID)
	if got.Chunks != 2 || got.Bytes != int64(len(c0)+len("second")) || len(got.ChunkSHA256) != 2 {
		t.Fatalf("meta after two chunks = %+v", got)
	}
	part, err := os.Stat(filepath.Join(src.Store.Dir, m.ID, partName(0)))
	if err != nil || part.Mode().Perm() != recordingFileMode {
		t.Fatalf("part mode = %v, %v", part, err)
	}
	// A fresh store (a hive restart) picks the recording up where it was.
	again := NewRecordingStore(src.Store.Dir)
	again.Now = clock.now
	if _, err := again.Append(m.ID, "ana", 2, []byte("third"), enabledRecording()); err != nil {
		t.Fatalf("append after reload: %v", err)
	}
}

func TestRecordingSizeCapsAndQuota(t *testing.T) {
	cfg := enabledRecording()
	cfg.MaxBytes, cfg.QuotaBytes = 40, 60
	svc, _, _ := newRecordingService(t, cfg)
	m := startRecording(t, svc, "ana", "audio/webm")
	if _, err := svc.AppendRecordingChunk(m.ID, "ana", 0, nil); !errors.Is(err, ErrRecordingChunkSize) {
		t.Fatalf("empty chunk = %v", err)
	}
	if _, err := svc.AppendRecordingChunk(m.ID, "ana", 0, make([]byte, RecordingMaxChunkBytes+1)); !errors.Is(err, ErrRecordingChunkSize) {
		t.Fatalf("oversize chunk = %v", err)
	}
	if _, err := svc.AppendRecordingChunk(m.ID, "ana", 0, webmChunk(26)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AppendRecordingChunk(m.ID, "ana", 1, make([]byte, 11)); !errors.Is(err, ErrRecordingTooLarge) {
		t.Fatalf("over max_bytes = %v, want ErrRecordingTooLarge", err)
	}
	other := startRecording(t, svc, "ben", "audio/webm")
	if _, err := svc.AppendRecordingChunk(other.ID, "ben", 0, webmChunk(26)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AppendRecordingChunk(other.ID, "ben", 1, make([]byte, 6)); !errors.Is(err, ErrRecordingQuota) {
		t.Fatalf("over quota_bytes = %v, want ErrRecordingQuota", err)
	}
	if _, err := svc.StartRecording(RecordingStart{MIME: "audio/webm", ConsentVersion: RecordingConsentVersion, User: "cy"}, RecordingOptions{}); !errors.Is(err, ErrRecordingQuota) {
		t.Fatalf("start with the quota used up = %v, want ErrRecordingQuota", err)
	}
	if svc.RecordingSettings().UsedBytes != 60 {
		t.Fatalf("used bytes = %d, want 60", svc.RecordingSettings().UsedBytes)
	}
}

func TestRecordingMaxSeconds(t *testing.T) {
	cfg := enabledRecording()
	cfg.MaxSeconds = 60
	svc, _, clock := newRecordingService(t, cfg)
	m := startRecording(t, svc, "ana", "audio/webm")
	if _, err := svc.AppendRecordingChunk(m.ID, "ana", 0, webmChunk(4)); err != nil {
		t.Fatal(err)
	}
	clock.t = clock.t.Add(time.Minute + recordingClockGrace + time.Second)
	if _, err := svc.AppendRecordingChunk(m.ID, "ana", 1, []byte("late")); !errors.Is(err, ErrRecordingTooLong) {
		t.Fatalf("past max_seconds = %v, want ErrRecordingTooLong", err)
	}
	fin, err := svc.FinalizeRecording(m.ID, "ana", int64(time.Hour/time.Millisecond), RecordingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if fin.DurationMS > int64(time.Minute/time.Millisecond) {
		t.Fatalf("duration %d not clamped to max_seconds", fin.DurationMS)
	}
}

func TestRecordingFinalizeConcatenatesWebM(t *testing.T) {
	svc, src, clock := newRecordingService(t, enabledRecording())
	m := startRecording(t, svc, "ana", "audio/webm")
	chunks := [][]byte{webmChunk(5), []byte("cluster-1"), []byte("cluster-2")}
	for i, c := range chunks {
		clock.t = clock.t.Add(10 * time.Second)
		if _, err := svc.AppendRecordingChunk(m.ID, "ana", i, c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.FinalizeRecording(m.ID, "ben", 0, RecordingOptions{}); !errors.Is(err, ErrRecordingNotYours) {
		t.Fatalf("finalize by someone else = %v", err)
	}
	var audits []RecordingAudit
	fin, err := svc.FinalizeRecording(m.ID, "ana", 25000, RecordingOptions{Audit: func(a RecordingAudit) { audits = append(audits, a) }})
	if err != nil {
		t.Fatal(err)
	}
	if fin.State != RecordingFinalized || fin.AudioFile != "audio.webm" || fin.DurationMS != 25000 || fin.ConcatWarning != "" {
		t.Fatalf("finalized = %+v", fin)
	}
	got, err := os.ReadFile(filepath.Join(src.Store.Dir, m.ID, "audio.webm"))
	if err != nil || !bytes.Equal(got, bytes.Join(chunks, nil)) {
		t.Fatalf("audio.webm = %q, %v; want the chunks in order", got, err)
	}
	if st, _ := os.Stat(filepath.Join(src.Store.Dir, m.ID, "audio.webm")); st.Mode().Perm() != recordingFileMode {
		t.Fatalf("audio mode = %v", st.Mode())
	}
	if _, err := os.Stat(filepath.Join(src.Store.Dir, m.ID, partName(0))); !os.IsNotExist(err) {
		t.Fatalf("parts kept after finalize: %v", err)
	}
	if _, err := svc.AppendRecordingChunk(m.ID, "ana", 3, []byte("more")); !errors.Is(err, ErrRecordingClosed) {
		t.Fatalf("chunk after finalize = %v, want ErrRecordingClosed", err)
	}
	if len(audits) != 1 || audits[0].Action != "inbox_recording_finalize" {
		t.Fatalf("audits = %+v", audits)
	}
	// The fact is written pending, in the personal layer.
	items, err := svc.Items(context.Background())
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %+v, %v", items, err)
	}
	it := items[0]
	if it.Source != RecordingType || it.RecordedBy != "ana" || it.Title != "Standup" || it.DurationMS != 25000 || !it.Pending || it.FactState != connector.FactContentPending || it.Layer != RecordingLayer {
		t.Fatalf("item = %+v", it)
	}
	fact, _ := os.ReadFile(it.FactPath)
	for _, want := range []string{"source: " + RecordingType, "attr_hive_recording_content: pending", "attr_consent_version: " + RecordingConsentVersion, "_" + RecordingPendingText + "_"} {
		if !strings.Contains(string(fact), want) {
			t.Fatalf("fact lacks %q:\n%s", want, fact)
		}
	}
	a, local, err := svc.OpenLocalAudio(context.Background(), it)
	if err != nil || !local || a.MIME != "audio/webm" {
		t.Fatalf("local audio = %+v, %v, %v", a, local, err)
	}
	_ = a.File.Close()
	link, err := svc.AudioLink(context.Background(), it)
	if err != nil || link.URL != "/api/inbox/recordings/"+m.ID+"/audio" {
		t.Fatalf("audio link = %+v, %v", link, err)
	}
}

// Safari records fragmented MP4: the first timeslice chunk carries ftyp and
// moov, every later one moof+mdat. Concatenating them in order must give a
// file whose top-level boxes chain exactly to the end.
func TestRecordingFinalizeFragmentedMP4IsValid(t *testing.T) {
	svc, src, _ := newRecordingService(t, enabledRecording())
	m := startRecording(t, svc, "ana", "audio/mp4")
	chunks := [][]byte{
		append(mp4Box("ftyp", []byte("iso5\x00\x00\x02\x00iso6mp41")), mp4Box("moov", bytes.Repeat([]byte{1}, 40))...),
		append(mp4Box("moof", bytes.Repeat([]byte{2}, 24)), mp4Box("mdat", bytes.Repeat([]byte{3}, 300))...),
		append(mp4Box("moof", bytes.Repeat([]byte{4}, 24)), mp4Box("mdat", bytes.Repeat([]byte{5}, 200))...),
	}
	for i, c := range chunks {
		if _, err := svc.AppendRecordingChunk(m.ID, "ana", i, c); err != nil {
			t.Fatal(err)
		}
	}
	fin, err := svc.FinalizeRecording(m.ID, "ana", 0, RecordingOptions{})
	if err != nil || fin.ConcatWarning != "" || fin.AudioFile != "audio.mp4" {
		t.Fatalf("finalize = %+v, %v", fin, err)
	}
	f, err := os.Open(filepath.Join(src.Store.Dir, m.ID, "audio.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	types, err := MP4TopLevelBoxes(f)
	if err != nil || strings.Join(types, ",") != "ftyp,moov,moof,mdat,moof,mdat" {
		t.Fatalf("boxes = %v, %v", types, err)
	}
	// A truncated chunk breaks the chain and is reported, not hidden.
	if _, err := MP4TopLevelBoxes(bytes.NewReader(append(chunks[0], mp4Box("mdat", make([]byte, 50))[:20]...))); err == nil {
		t.Fatal("truncated mp4 passed the box walk")
	}
	if w := checkRecordingContainer(filepath.Join(src.Store.Dir, m.ID, "audio.mp4"), RecordingContainerMP4); w != "" {
		t.Fatalf("container check = %q", w)
	}
}

func TestRecordingFinalizeEmptyAndAbort(t *testing.T) {
	svc, src, _ := newRecordingService(t, enabledRecording())
	m := startRecording(t, svc, "ana", "audio/webm")
	if _, err := svc.FinalizeRecording(m.ID, "ana", 0, RecordingOptions{}); !errors.Is(err, ErrRecordingEmpty) {
		t.Fatalf("finalize without chunks = %v, want ErrRecordingEmpty", err)
	}
	if _, err := os.Stat(filepath.Join(src.Store.Dir, m.ID)); !os.IsNotExist(err) {
		t.Fatal("empty recording kept on disk")
	}
	m = startRecording(t, svc, "ana", "audio/webm")
	if _, err := svc.AppendRecordingChunk(m.ID, "ana", 0, webmChunk(3)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AbortRecording(m.ID, "ben", false, RecordingOptions{}); !errors.Is(err, ErrRecordingNotYours) {
		t.Fatalf("abort by another read-write user = %v", err)
	}
	if _, err := svc.AbortRecording(m.ID, "olivia", true, RecordingOptions{}); err != nil {
		t.Fatalf("owner abort: %v", err)
	}
	if _, ok := src.Store.Get(m.ID); ok {
		t.Fatal("aborted recording still listed")
	}
	if _, err := svc.AbortRecording(m.ID, "ana", false, RecordingOptions{}); !errors.Is(err, ErrRecordingNotFound) {
		t.Fatalf("abort twice = %v", err)
	}
}

func TestRecordingRetentionSweepKeepsTranscriptAndFact(t *testing.T) {
	svc, src, clock := newRecordingService(t, enabledRecording())
	m := startRecording(t, svc, "ana", "audio/webm")
	if _, err := svc.AppendRecordingChunk(m.ID, "ana", 0, webmChunk(16)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FinalizeRecording(m.ID, "ana", 1000, RecordingOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := svc.transcriptStore().Put(TranscriptRecord{Source: RecordingType, ID: m.ID, Status: TranscriptionDone, Text: "hello team", Provider: "whisper", Model: "base.en", UpdatedAt: clock.t}); err != nil {
		t.Fatal(err)
	}
	items, _ := svc.Items(context.Background())
	if len(items) != 1 || items[0].FactState != connector.FactContentMachine {
		t.Fatalf("machine transcript not written into the fact: %+v", items)
	}
	audio := filepath.Join(src.Store.Dir, m.ID, "audio.webm")

	clock.t = clock.t.Add(4*24*time.Hour + 23*time.Hour)
	if res, err := svc.RecordingSweep(RecordingOptions{}); err != nil || len(res.Expired) != 0 {
		t.Fatalf("sweep before 5 days = %+v, %v", res, err)
	}
	if _, err := os.Stat(audio); err != nil {
		t.Fatalf("audio deleted before retention: %v", err)
	}
	clock.t = clock.t.Add(time.Hour)
	var audits []RecordingAudit
	res, err := svc.RecordingSweep(RecordingOptions{Audit: func(a RecordingAudit) { audits = append(audits, a) }})
	if err != nil || len(res.Expired) != 1 {
		t.Fatalf("sweep at 5 days = %+v, %v", res, err)
	}
	if len(audits) != 1 || audits[0].Action != "inbox_recording_audio_expired" || audits[0].Actor != RecordingSweepActor {
		t.Fatalf("audits = %+v", audits)
	}
	if _, err := os.Stat(audio); !os.IsNotExist(err) {
		t.Fatalf("audio kept past retention: %v", err)
	}
	if rec, ok := svc.transcriptStore().Get(RecordingType, m.ID); !ok || rec.Text != "hello team" {
		t.Fatal("transcript removed by retention")
	}
	items, _ = svc.Items(context.Background())
	if len(items) != 1 {
		t.Fatalf("item gone after retention: %+v", items)
	}
	it := items[0]
	if !it.AudioExpired || it.AudioRetentionDays != config.DefaultInboxRecordingRetentionDays || it.Transcribable || it.FactState != connector.FactContentMachine {
		t.Fatalf("expired item = %+v", it)
	}
	fact, err := os.ReadFile(it.FactPath)
	if err != nil || !strings.Contains(string(fact), "hello team") {
		t.Fatalf("fact after retention = %q, %v", fact, err)
	}
	if _, err := svc.OpenRecordingAudio(m.ID); !errors.Is(err, ErrRecordingExpired) {
		t.Fatalf("open expired audio = %v", err)
	}
	if _, err := svc.AudioLink(context.Background(), it); !errors.Is(err, ErrNoAudio) {
		t.Fatalf("audio link after expiry = %v", err)
	}
	if svc.RecordingSettings().UsedBytes != 0 {
		t.Fatal("expired audio still counts against the quota")
	}
}

func TestRecordingRetentionDaysConfigurable(t *testing.T) {
	cfg := enabledRecording()
	cfg.RetentionDays = 1
	svc, _, clock := newRecordingService(t, cfg)
	m := startRecording(t, svc, "ana", "audio/webm")
	if _, err := svc.AppendRecordingChunk(m.ID, "ana", 0, webmChunk(4)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FinalizeRecording(m.ID, "ana", 0, RecordingOptions{}); err != nil {
		t.Fatal(err)
	}
	clock.t = clock.t.Add(24 * time.Hour)
	if res, _ := svc.RecordingSweep(RecordingOptions{}); len(res.Expired) != 1 {
		t.Fatalf("retention_days 1: %+v", res)
	}
}

func TestRecordingIdleAutoFinalize(t *testing.T) {
	svc, _, clock := newRecordingService(t, enabledRecording())
	m := startRecording(t, svc, "ana", "audio/webm")
	if _, err := svc.AppendRecordingChunk(m.ID, "ana", 0, webmChunk(4)); err != nil {
		t.Fatal(err)
	}
	empty := startRecording(t, svc, "ben", "audio/webm")
	clock.t = clock.t.Add(time.Hour)
	if res, _ := svc.RecordingSweep(RecordingOptions{}); len(res.Finalized)+len(res.Removed) != 0 {
		t.Fatalf("finalized before idle: %+v", res)
	}
	clock.t = clock.t.Add(time.Hour)
	res, err := svc.RecordingSweep(RecordingOptions{})
	if err != nil || len(res.Finalized) != 1 || res.Finalized[0].ID != m.ID || !res.Finalized[0].AutoFinalized || len(res.Removed) != 1 || res.Removed[0].ID != empty.ID {
		t.Fatalf("idle sweep = %+v, %v", res, err)
	}
	items, _ := svc.Items(context.Background())
	if len(items) != 1 || items[0].ID != m.ID {
		t.Fatalf("items after auto-finalize = %+v", items)
	}
	// The user may record again once the old one is finalized.
	startRecording(t, svc, "ana", "audio/webm")
}

func TestRecordingSourceSurvivesSetSources(t *testing.T) {
	svc, _, _ := newRecordingService(t, enabledRecording())
	svc.SetSources(nil)
	if !svc.HasSource(RecordingType) {
		t.Fatal("SetSources dropped the recording source")
	}
	if !svc.Settings().Recording.Enabled || svc.Settings().Recording.ConsentVersion != RecordingConsentVersion || svc.Settings().Recording.MaxChunkBytes != RecordingMaxChunkBytes {
		t.Fatalf("settings = %+v", svc.Settings().Recording)
	}
}

func TestRecordingFetchFromFact(t *testing.T) {
	svc, _, _ := newRecordingService(t, enabledRecording())
	m := startRecording(t, svc, "ana", "audio/webm")
	if _, err := svc.AppendRecordingChunk(m.ID, "ana", 0, webmChunk(4)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FinalizeRecording(m.ID, "ana", 0, RecordingOptions{}); err != nil {
		t.Fatal(err)
	}
	it, ok, _ := svc.Find(context.Background(), RecordingType, m.ID)
	if !ok {
		t.Fatal("finalized recording not found")
	}
	c, err := svc.Content(context.Background(), it)
	if err != nil || !c.Pending || c.Provenance != RecordingType+"://recordings/"+m.ID {
		t.Fatalf("content = %+v, %v", c, err)
	}
	// Without a fact (write failed) the item still reads as pending.
	c, err = (&RecordingSource{Store: NewRecordingStore(t.TempDir())}).Fetch(context.Background(), Item{ID: "x", Title: "t"})
	if err != nil || !c.Pending {
		t.Fatalf("fact-less content = %+v, %v", c, err)
	}
	f, a, err := (&RecordingSource{Store: NewRecordingStore(t.TempDir())}).Store.OpenAudio("nope")
	if f != nil || a.ID != "" || !errors.Is(err, ErrRecordingNotFound) {
		t.Fatalf("open unknown = %v", err)
	}
}

func TestSniffRecordingContainer(t *testing.T) {
	for _, tc := range []struct {
		head []byte
		want string
	}{
		{webmChunk(0), RecordingContainerWebM},
		{mp4Box("ftyp", []byte("M4A ")), RecordingContainerMP4},
		{[]byte("ID3\x04"), ""},
		{[]byte("OggS\x00\x02"), ""},
		{[]byte{0x1A, 0x45}, ""},
		{nil, ""},
	} {
		if got := SniffRecordingContainer(tc.head); got != tc.want {
			t.Errorf("sniff %q = %q, want %q", tc.head, got, tc.want)
		}
	}
	if RecordingContainer("audio/mp4; codecs=\"mp4a.40.2\"") != RecordingContainerMP4 || RecordingContainer("audio/webm;codecs=opus") != RecordingContainerWebM {
		t.Fatal("MediaRecorder MIME parameters not accepted")
	}
}
