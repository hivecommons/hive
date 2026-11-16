package inbox

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
	"github.com/hivecommons/hive/pkg/transcribe"
)

// noSelfHTTP fails a test that makes any HTTP call: a recording's audio is
// read from disk, never fetched from the hive's own endpoint.
type noSelfHTTP struct{ t *testing.T }

func (n noSelfHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	n.t.Errorf("unexpected HTTP request for a hive recording: %s", r.URL)
	return nil, errors.New("no HTTP")
}

func withTranscription(t *testing.T, svc *Service, tx config.InboxTranscription) *transcribe.Fake {
	t.Helper()
	cfg := svc.Config()
	cfg.Transcription = tx
	svc.UpdateConfig(cfg)
	fake := &transcribe.Fake{Result: transcribe.Result{Segments: []transcribe.Segment{{Text: "decisions were made"}}, Duration: 2 * time.Second}}
	svc.NewTranscriber = func(config.InboxTranscription) (transcribe.Transcriber, error) { return fake, nil }
	svc.Getenv = func(string) string { return "" }
	svc.AudioClient = &http.Client{Transport: noSelfHTTP{t}}
	svc.TranscribeBackoff = func(int) time.Duration { return 0 }
	return fake
}

func recordAndFinalize(t *testing.T, svc *Service, user string, opts RecordingOptions) RecordingMeta {
	t.Helper()
	m := startRecording(t, svc, user, "audio/webm")
	if _, err := svc.AppendRecordingChunk(m.ID, user, 0, webmChunk(30)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AppendRecordingChunk(m.ID, user, 1, []byte("cluster")); err != nil {
		t.Fatal(err)
	}
	fin, err := svc.FinalizeRecording(m.ID, user, 2000, opts)
	if err != nil {
		t.Fatal(err)
	}
	return fin
}

func TestRecordingAutoTranscribedOnFinalize(t *testing.T) {
	svc, src, _ := newRecordingService(t, enabledRecording())
	// Manual mode: a recording is transcribed anyway, once transcription
	// is enabled with consent.
	fake := withTranscription(t, svc, config.InboxTranscription{Enabled: true, ConsentAcknowledged: true})
	var audits []TranscribeAudit
	m := recordAndFinalize(t, svc, "ana", RecordingOptions{Actor: "ana", TranscribeAudit: func(a TranscribeAudit) { audits = append(audits, a) }})
	svc.Wait()
	if fake.Calls() != 1 {
		t.Fatalf("transcriber calls = %d, want 1 (auto-transcribe on finalize)", fake.Calls())
	}
	if fake.Bytes() != m.Bytes {
		t.Fatalf("transcriber read %d bytes, want the %d-byte local file", fake.Bytes(), m.Bytes)
	}
	rec, ok := svc.transcriptStore().Get(RecordingType, m.ID)
	if !ok || rec.Status != TranscriptionDone || !strings.Contains(rec.Text, "decisions were made") || rec.AudioSize != m.Bytes {
		t.Fatalf("transcript = %+v, %v", rec, ok)
	}
	if len(audits) != 1 || audits[0].Actor != "ana" || audits[0].Outcome != TranscriptionDone {
		t.Fatalf("transcribe audits = %+v", audits)
	}
	items, _ := svc.Items(context.Background())
	if len(items) != 1 || items[0].Pending || items[0].FactState != connector.FactContentMachine {
		t.Fatalf("item after transcription = %+v", items)
	}
	fact, _ := os.ReadFile(items[0].FactPath)
	if !strings.Contains(string(fact), "decisions were made") {
		t.Fatalf("fact lacks the transcript:\n%s", fact)
	}
	c, err := svc.Content(context.Background(), items[0])
	if err != nil || !strings.Contains(c.Transcript, "decisions were made") {
		t.Fatalf("content = %+v, %v", c, err)
	}
	_ = src
}

func TestRecordingNotTranscribedUntilTranscriptionEnabled(t *testing.T) {
	svc, _, _ := newRecordingService(t, enabledRecording())
	fake := withTranscription(t, svc, config.InboxTranscription{})
	if svc.RecordingSettings().Transcribes {
		t.Fatal("settings say recordings are transcribed while transcription is off")
	}
	m := recordAndFinalize(t, svc, "ana", RecordingOptions{})
	svc.Wait()
	if fake.Calls() != 0 {
		t.Fatal("a recording was transcribed while transcription is disabled")
	}
	if _, ok := svc.transcriptStore().Get(RecordingType, m.ID); ok {
		t.Fatal("a transcript record exists while transcription is disabled")
	}
	items, _ := svc.Items(context.Background())
	if len(items) != 1 || items[0].TranscribeUnavailable != TranscribeBlockedDisabled {
		t.Fatalf("item = %+v, want transcribe_unavailable=disabled", items)
	}
	// Enabled without consent is not enough (and is rejected by config).
	if n, _ := svc.TranscribeSweep(context.Background(), TranscribeOptions{}); n != 0 {
		t.Fatalf("sweep without transcription queued %d", n)
	}
	// Enabled with consent, still manual mode: the sweep picks it up.
	withTranscription(t, svc, config.InboxTranscription{Enabled: true, ConsentAcknowledged: true})
	n, err := svc.TranscribeSweep(context.Background(), TranscribeOptions{Actor: AutoTranscribeActor})
	svc.Wait()
	if err != nil || n != 1 {
		t.Fatalf("sweep queued %d, %v; want the recording", n, err)
	}
	if rec, ok := svc.transcriptStore().Get(RecordingType, m.ID); !ok || rec.Status != TranscriptionDone {
		t.Fatalf("transcript after enabling = %+v", rec)
	}
	if !svc.RecordingSettings().Transcribes {
		t.Fatal("settings do not report automatic transcription")
	}
}

func TestRecordingExpiredAudioIsNotTranscribed(t *testing.T) {
	svc, _, clock := newRecordingService(t, enabledRecording())
	fake := withTranscription(t, svc, config.InboxTranscription{})
	m := recordAndFinalize(t, svc, "ana", RecordingOptions{})
	clock.t = clock.t.Add(config.DefaultInboxRecordingRetentionDays * 24 * time.Hour)
	if res, _ := svc.RecordingSweep(RecordingOptions{}); len(res.Expired) != 1 {
		t.Fatalf("not expired: %+v", res)
	}
	withTranscription(t, svc, config.InboxTranscription{Enabled: true, ConsentAcknowledged: true})
	if n, _ := svc.TranscribeSweep(context.Background(), TranscribeOptions{}); n != 0 {
		t.Fatalf("sweep queued %d expired recordings", n)
	}
	svc.Wait()
	if fake.Calls() != 0 {
		t.Fatal("expired audio was sent to the transcriber")
	}
	it, ok, _ := svc.Find(context.Background(), RecordingType, m.ID)
	if !ok {
		t.Fatal("expired recording not listed")
	}
	if _, err := svc.Transcribe(context.Background(), it, TranscribeOptions{}); err != nil {
		t.Fatalf("manual transcribe of expired audio = %v (want a skipped state)", err)
	}
	rec, _ := svc.transcriptStore().Get(RecordingType, m.ID)
	if rec.Status != TranscriptionSkipped || !strings.HasPrefix(rec.Reason, SkipUnsupported) {
		t.Fatalf("expired manual transcribe = %+v, want skipped unsupported", rec)
	}
}

func TestRecordingTranscriptionTooLarge(t *testing.T) {
	svc, _, _ := newRecordingService(t, enabledRecording())
	fake := withTranscription(t, svc, config.InboxTranscription{Enabled: true, ConsentAcknowledged: true, MaxBytes: 10})
	m := recordAndFinalize(t, svc, "ana", RecordingOptions{})
	svc.Wait()
	rec, _ := svc.transcriptStore().Get(RecordingType, m.ID)
	if rec.Status != TranscriptionSkipped || !strings.HasPrefix(rec.Reason, SkipTooLarge) {
		t.Fatalf("over max_bytes = %+v", rec)
	}
	_ = fake
}
