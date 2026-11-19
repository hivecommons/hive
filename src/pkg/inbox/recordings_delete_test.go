package inbox

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// A transcribed recording is deleted for good: audio directory, the fact
// holding the machine transcript, the transcript record and the item state.
// The share history is counted, not followed.
func TestDeleteRecordingRemovesAudioFactAndTranscript(t *testing.T) {
	svc, src, _ := newRecordingService(t, enabledRecording())
	withTranscription(t, svc, config.InboxTranscription{Enabled: true, ConsentAcknowledged: true})
	m := recordAndFinalize(t, svc, "ana", RecordingOptions{})
	svc.Wait()
	if _, err := svc.Items(context.Background()); err != nil {
		t.Fatal(err)
	}
	fact := src.factPath(m.ID)
	data, err := os.ReadFile(fact)
	if err != nil {
		t.Fatalf("no fact after finalize: %v", err)
	}
	if !strings.Contains(string(data), "decisions were made") {
		t.Fatalf("fact has no machine transcript:\n%s", data)
	}
	if rec, ok := svc.transcriptStore().Get(RecordingType, m.ID); !ok || rec.Status != TranscriptionDone {
		t.Fatalf("transcript record = %+v %v", rec, ok)
	}
	key := Key(RecordingType, m.ID)
	if _, err := svc.Store.AppendShare(key, ShareRecord{Destination: DestDibs, Actor: "ana", StartedAt: time.Now(), Status: StatusFiled}); err != nil {
		t.Fatal(err)
	}

	var audits []RecordingAudit
	opts := RecordingOptions{Actor: "ana", Audit: func(a RecordingAudit) { audits = append(audits, a) }}
	if _, err := svc.DeleteRecording(m.ID, "bob", false, opts); !errors.Is(err, ErrRecordingNotYours) {
		t.Fatalf("someone else's delete = %v, want ErrRecordingNotYours", err)
	}
	if _, err := svc.DeleteRecording(m.ID, "", false, opts); !errors.Is(err, ErrRecordingNotYours) {
		t.Fatalf("anonymous delete = %v, want ErrRecordingNotYours", err)
	}
	res, err := svc.DeleteRecording(m.ID, "ana", false, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != m.ID || res.RecordedBy != "ana" || !res.FactRemoved || res.SharesKept != 1 {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(src.Store.dir(m.ID)); !os.IsNotExist(err) {
		t.Fatalf("audio dir still there: %v", err)
	}
	if _, err := os.Stat(fact); !os.IsNotExist(err) {
		t.Fatalf("fact still there: %v", err)
	}
	if _, ok := svc.transcriptStore().Get(RecordingType, m.ID); ok {
		t.Fatal("transcript record survived")
	}
	if h := svc.Store.History(key); len(h) != 0 {
		t.Fatalf("share history survived: %+v", h)
	}
	if _, ok := src.Store.Get(m.ID); ok {
		t.Fatal("metadata survived")
	}
	items, err := src.List(context.Background())
	if err != nil || len(items) != 0 {
		t.Fatalf("listing after delete = %+v, %v", items, err)
	}
	if len(audits) != 1 || audits[0].Action != "inbox_recording_delete" || !strings.Contains(audits[0].Detail(), "recording="+m.ID) || !strings.Contains(audits[0].Detail(), "shares_kept=1") {
		t.Fatalf("audits = %+v", audits)
	}
	if _, err := svc.DeleteRecording(m.ID, "ana", false, opts); !errors.Is(err, ErrRecordingNotFound) {
		t.Fatalf("second delete = %v, want ErrRecordingNotFound", err)
	}
}

func TestDeleteRecordingOwnerInProgressAndTranscribing(t *testing.T) {
	svc, src, _ := newRecordingService(t, enabledRecording())
	m := recordAndFinalize(t, svc, "ana", RecordingOptions{})

	// Refused while its transcription is queued or running; nothing removed.
	for _, st := range []string{TranscriptionQueued, TranscriptionRunning} {
		if err := svc.transcriptStore().Put(TranscriptRecord{Source: RecordingType, ID: m.ID, Status: st}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.DeleteRecording(m.ID, "", true, RecordingOptions{}); !errors.Is(err, ErrRecordingTranscribing) {
			t.Fatalf("%s: delete = %v, want ErrRecordingTranscribing", st, err)
		}
		if _, ok := src.Store.Get(m.ID); !ok {
			t.Fatalf("%s: a refused delete removed the recording", st)
		}
	}
	if err := svc.transcriptStore().Put(TranscriptRecord{Source: RecordingType, ID: m.ID, Status: TranscriptionFailed}); err != nil {
		t.Fatal(err)
	}
	// An owner deletes anyone's recording (no user needed).
	res, err := svc.DeleteRecording(m.ID, "", true, RecordingOptions{})
	if err != nil || res.ID != m.ID || !res.FactRemoved || res.SharesKept != 0 {
		t.Fatalf("owner delete = %+v, %v", res, err)
	}

	// A recording in progress is aborted, not deleted.
	live := startRecording(t, svc, "ana", "audio/webm")
	if _, err := svc.DeleteRecording(live.ID, "ana", false, RecordingOptions{}); !errors.Is(err, ErrRecordingNotFinished) {
		t.Fatalf("in-progress delete = %v, want ErrRecordingNotFinished", err)
	}
	if _, err := svc.DeleteRecording("nope", "", true, RecordingOptions{}); !errors.Is(err, ErrRecordingNotFound) {
		t.Fatalf("unknown = %v, want ErrRecordingNotFound", err)
	}
}

func TestDeleteRecordingWithoutRecordingsOrFact(t *testing.T) {
	svc := NewService(config.InboxConfig{Recording: enabledRecording()}, NewStore(""), nil, nil, nil)
	if _, err := svc.DeleteRecording("r", "ana", false, RecordingOptions{}); !errors.Is(err, ErrRecordingNotFound) {
		t.Fatalf("no recordings wired = %v, want ErrRecordingNotFound", err)
	}

	// A recording whose fact was never written still deletes.
	svc2, src, _ := newRecordingService(t, enabledRecording())
	m := recordAndFinalize(t, svc2, "ana", RecordingOptions{})
	if err := os.Remove(src.factPath(m.ID)); err != nil {
		t.Fatal(err)
	}
	res, err := svc2.DeleteRecording(m.ID, "ana", false, RecordingOptions{})
	if err != nil || res.FactRemoved {
		t.Fatalf("delete without fact = %+v, %v", res, err)
	}
}

func TestRecordingStoreDeleteChecks(t *testing.T) {
	_, src, _ := newRecordingService(t, enabledRecording())
	if _, err := src.Store.Delete("nope", "ana", false); !errors.Is(err, ErrRecordingNotFound) {
		t.Fatalf("unknown = %v", err)
	}
	if _, err := src.Store.Delete("nope", "", false); !errors.Is(err, ErrRecordingNotYours) {
		t.Fatalf("no user, not owner = %v", err)
	}
}

func TestStoreForgetAndTranscriptDeleteMissing(t *testing.T) {
	st := NewStore("")
	if n, err := st.Forget("hive-recording/none"); n != 0 || err != nil {
		t.Fatalf("forget unknown = %d, %v", n, err)
	}
	ts := NewTranscriptStore(t.TempDir())
	if err := ts.Delete(RecordingType, "none"); err != nil {
		t.Fatalf("delete missing transcript = %v", err)
	}
}
