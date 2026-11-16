package inbox

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/transcribe"
)

func TestRecordingErrorsAndDisabledPaths(t *testing.T) {
	if !strings.Contains((&RecordingActiveError{ID: "x"}).Error(), "(x)") {
		t.Fatal("active error lacks the id")
	}
	if !strings.Contains((&RecordingGapError{Got: 3, Expected: 1}).Error(), "seq=1") {
		t.Fatal("gap error lacks the expected seq")
	}
	if d := (RecordingAudit{Fields: []string{"a", "1", "b", "2", "odd"}}).Detail(); d != "a=1, b=2" {
		t.Fatalf("detail = %q", d)
	}

	// No recording source at all.
	bare := NewService(config.InboxConfig{}, NewStore(filepath.Join(t.TempDir(), "s.json")), nil, nil, nil)
	if _, err := bare.StartRecording(RecordingStart{}, RecordingOptions{}); !errors.Is(err, ErrRecordingDisabled) {
		t.Fatalf("start without source = %v", err)
	}
	if _, err := bare.AppendRecordingChunk("x", "u", 0, []byte{1}); !errors.Is(err, ErrRecordingDisabled) {
		t.Fatalf("append without source = %v", err)
	}
	if _, err := bare.FinalizeRecording("x", "u", 0, RecordingOptions{}); !errors.Is(err, ErrRecordingDisabled) {
		t.Fatalf("finalize without source = %v", err)
	}
	if _, err := bare.AbortRecording("x", "u", false, RecordingOptions{}); !errors.Is(err, ErrRecordingDisabled) {
		t.Fatalf("abort without source = %v", err)
	}
	if _, err := bare.OpenRecordingAudio("x"); !errors.Is(err, ErrRecordingNotFound) {
		t.Fatalf("open without source = %v", err)
	}
	if res, err := bare.RecordingSweep(RecordingOptions{}); err != nil || len(res.Expired)+len(res.Finalized)+len(res.Removed) != 0 {
		t.Fatalf("sweep without source = %+v, %v", res, err)
	}
	if bare.RecordingSettings().Enabled {
		t.Fatal("enabled without a source")
	}
	if _, local, err := bare.OpenLocalAudio(context.Background(), Item{Source: "plaud"}); local || err != nil {
		t.Fatalf("non-local source = %v, %v", local, err)
	}

	svc, src, _ := newRecordingService(t, enabledRecording())
	for _, call := range []func() error{
		func() error { _, err := svc.AppendRecordingChunk("missing", "ana", 0, []byte{1}); return err },
		func() error { _, err := svc.FinalizeRecording("missing", "ana", 0, RecordingOptions{}); return err },
		func() error { _, err := svc.OpenRecordingAudio("missing"); return err },
	} {
		if err := call(); !errors.Is(err, ErrRecordingNotFound) {
			t.Fatalf("unknown recording = %v", err)
		}
	}
	if _, err := svc.StartRecording(RecordingStart{MIME: "audio/webm", ConsentVersion: RecordingConsentVersion}, RecordingOptions{}); !errors.Is(err, ErrRecordingBadRequest) {
		t.Fatalf("start without user = %v", err)
	}
	m := startRecording(t, svc, "ana", "audio/webm")
	if _, err := svc.OpenRecordingAudio(m.ID); !errors.Is(err, ErrNoAudio) {
		t.Fatalf("audio of an active recording = %v", err)
	}
	if _, err := src.AudioLink(context.Background(), Item{ID: m.ID}); !errors.Is(err, ErrNoAudio) {
		t.Fatalf("link of an active recording = %v", err)
	}
	if _, err := svc.AppendRecordingChunk(m.ID, "ana", 0, webmChunk(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FinalizeRecording(m.ID, "ana", 0, RecordingOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FinalizeRecording(m.ID, "ana", 0, RecordingOptions{}); !errors.Is(err, ErrRecordingClosed) {
		t.Fatalf("finalize twice = %v", err)
	}
	if _, err := svc.AbortRecording(m.ID, "ana", false, RecordingOptions{}); !errors.Is(err, ErrRecordingClosed) {
		t.Fatalf("abort finalized = %v", err)
	}
	if got := len(src.Store.List()); got != 1 {
		t.Fatalf("list = %d", got)
	}
	// A recording without a fact directory still finalizes.
	if err := (&RecordingSource{}).writeFact(RecordingMeta{}); err != nil {
		t.Fatalf("writeFact without dir = %v", err)
	}
}

func TestRecordingStoreLoadSkipsJunk(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"not-an-id", "20261116t090000-0000000a", "20261116t090000-0000000b"} {
		if err := os.MkdirAll(filepath.Join(dir, name), recordingDirMode); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "20261116t090000-0000000a", recordingMetaFile), []byte("{not json"), recordingFileMode)
	_ = os.WriteFile(filepath.Join(dir, "20261116t090000-0000000b", recordingMetaFile), []byte(`{"id":"other"}`), recordingFileMode)
	_ = os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), recordingFileMode)
	if got := NewRecordingStore(dir).List(); len(got) != 0 {
		t.Fatalf("junk loaded: %+v", got)
	}
	if got := NewRecordingStore(filepath.Join(dir, "absent")).UsedBytes(); got != 0 {
		t.Fatalf("absent dir used = %d", got)
	}
}

func TestMP4BoxWalkEdges(t *testing.T) {
	large := make([]byte, mp4LargeBoxHeader+4)
	binary.BigEndian.PutUint32(large[:4], 1)
	copy(large[4:8], "mdat")
	binary.BigEndian.PutUint64(large[8:16], uint64(len(large)))
	stream := append(mp4Box("ftyp", []byte("iso5")), large...)
	if types, err := MP4TopLevelBoxes(bytes.NewReader(stream)); err != nil || strings.Join(types, ",") != "ftyp,mdat" {
		t.Fatalf("64-bit box = %v, %v", types, err)
	}
	toEnd := append(mp4Box("ftyp", nil), 0, 0, 0, 0, 'm', 'd', 'a', 't', 9, 9)
	if types, err := MP4TopLevelBoxes(bytes.NewReader(toEnd)); err != nil || len(types) != 2 {
		t.Fatalf("size-0 box = %v, %v", types, err)
	}
	for name, bad := range map[string][]byte{
		"tiny size":      {0, 0, 0, 4, 'f', 't', 'y', 'p'},
		"trailing bytes": append(mp4Box("ftyp", nil), 1, 2, 3),
		"bad large size": {0, 0, 0, 1, 'm', 'd', 'a', 't', 0, 0, 0, 0, 0, 0, 0, 2},
		"short large":    {0, 0, 0, 1, 'm', 'd', 'a', 't', 0, 0},
	} {
		if _, err := MP4TopLevelBoxes(bytes.NewReader(bad)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	dir := t.TempDir()
	webm := filepath.Join(dir, "a.webm")
	_ = os.WriteFile(webm, []byte("nope"), recordingFileMode)
	if w := checkRecordingContainer(webm, RecordingContainerWebM); w == "" {
		t.Fatal("bad webm header passed")
	}
	mp4 := filepath.Join(dir, "a.mp4")
	_ = os.WriteFile(mp4, mp4Box("moov", nil), recordingFileMode)
	if w := checkRecordingContainer(mp4, RecordingContainerMP4); w == "" {
		t.Fatal("mp4 without ftyp passed")
	}
	if w := checkRecordingContainer(filepath.Join(dir, "absent"), RecordingContainerMP4); w == "" {
		t.Fatal("missing file passed")
	}
}

// reopenOpener counts opens so the retry path is covered.
type reopenOpener struct {
	path  string
	opens int
}

func (r *reopenOpener) OpenAudio(context.Context, Item) (LocalAudio, error) {
	r.opens++
	f, err := os.Open(r.path)
	if err != nil {
		return LocalAudio{}, err
	}
	st, _ := f.Stat()
	return LocalAudio{File: f, MIME: "audio/webm", Size: st.Size()}, nil
}

func TestLocalTranscribeAudioReopensForRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.webm")
	if err := os.WriteFile(path, webmChunk(12), recordingFileMode); err != nil {
		t.Fatal(err)
	}
	svc, _, _ := newRecordingService(t, enabledRecording())
	op := &reopenOpener{path: path}
	rec := &TranscriptRecord{}
	job := svc.txEnqueue(Item{Source: "x", ID: "y"}, "fake", "m", transcribe.Speed{})
	defer svc.txDequeue(job)
	a, cleanup, err := svc.localTranscribeAudio(context.Background(), Item{ID: "y"}, op, config.InboxTranscription{}, rec, job)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if a.MIME != "audio/webm" || a.Size != 16 || rec.AudioSize != 16 {
		t.Fatalf("audio = %+v, rec size %d", a, rec.AudioSize)
	}
	for i := 0; i < 2; i++ {
		rc, err := a.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		n, _ := io.Copy(io.Discard, rc)
		_ = rc.Close()
		if n != 16 {
			t.Fatalf("read %d bytes", n)
		}
	}
	if op.opens != 2 {
		t.Fatalf("opens = %d, want the first handle reused then one reopen", op.opens)
	}
	// An unused first handle is closed by cleanup; an open error is returned.
	_, cleanup2, err := svc.localTranscribeAudio(context.Background(), Item{ID: "y"}, op, config.InboxTranscription{}, rec, job)
	if err != nil {
		t.Fatal(err)
	}
	cleanup2()
	cleanup2()
	if _, _, err := svc.localTranscribeAudio(context.Background(), Item{}, &reopenOpener{path: filepath.Join(t.TempDir(), "absent")}, config.InboxTranscription{}, rec, job); err == nil {
		t.Fatal("missing file opened")
	}
	if _, _, err := svc.localTranscribeAudio(context.Background(), Item{}, op, config.InboxTranscription{MaxBytes: 4}, rec, job); !errors.Is(err, transcribe.ErrTooLarge) {
		t.Fatalf("over max_bytes = %v", err)
	}
}

func TestRecordingAutoTranscribeSweepActorAndBusy(t *testing.T) {
	svc, src, clock := newRecordingService(t, enabledRecording())
	fake := withTranscription(t, svc, config.InboxTranscription{Enabled: true, ConsentAcknowledged: true})
	var actors []string
	m := startRecording(t, svc, "ana", "audio/webm")
	if _, err := svc.AppendRecordingChunk(m.ID, "ana", 0, webmChunk(8)); err != nil {
		t.Fatal(err)
	}
	clock.t = clock.t.Add(3 * time.Hour)
	if _, err := svc.RecordingSweep(RecordingOptions{TranscribeAudit: func(a TranscribeAudit) { actors = append(actors, a.Actor) }}); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	if fake.Calls() != 1 || len(actors) != 1 || actors[0] != RecordingSweepActor {
		t.Fatalf("idle-finalized recording: calls %d, actors %v", fake.Calls(), actors)
	}
	// Already transcribed: finalize-time auto-transcribe is a no-op.
	got, _ := src.Store.Get(m.ID)
	svc.autoTranscribeRecording(src, got, RecordingOptions{})
	svc.Wait()
	if fake.Calls() != 1 {
		t.Fatal("a transcribed recording was transcribed again")
	}
	// No actor at all falls back to the auto-transcribe actor.
	got.ID = "unknown"
	svc.autoTranscribeRecording(src, got, RecordingOptions{})
}
