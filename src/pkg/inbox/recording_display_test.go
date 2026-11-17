package inbox

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// recordingPendingFact is the pending fact a finished hive recording gets
// (see RecordingSource.writeFact), reduced to what machineFact reads.
func recordingPendingFact() string {
	return "---\ntitle: Standup\ntype: reference\nlayer: personal\nstatus: active\n" +
		"source: hive-recording\nconnector: hive-recording\nsource_id: rec-9\nattr_hive_recording_content: pending\n" +
		"synthesized: 2026-11-16T09:00:00Z\n---\n\n" +
		"# Standup\n\n- **Recorded:** 2026-11-16T09:00:00Z\n- **Duration:** 4s\n\n_" + RecordingPendingText + "_\n"
}

func displayTestRecord(source, id string) TranscriptRecord {
	return TranscriptRecord{
		Source: source, ID: id, Status: TranscriptionDone, Provider: "whisper", Model: "base.en",
		Text: "We ship the fix today.", UpdatedAt: time.Date(2026, 11, 16, 9, 1, 0, 0, time.UTC),
	}
}

func TestMachineProvenanceWording(t *testing.T) {
	rec := displayTestRecord(RecordingType, "rec-9")
	for _, src := range []string{RecordingType, UploadType} {
		got := machineProvenance(src, src, rec)
		if want := "_Transcript made by this hive with whisper/base.en on 2026-11-16._"; got != want {
			t.Errorf("%s provenance = %q, want %q", src, got, want)
		}
		if !isMachineProvenance(got) {
			t.Errorf("%s provenance not recognised", src)
		}
	}
	got := machineProvenance("plaud", "plaud", rec)
	want := "_Machine transcript made by this hive with whisper/base.en on 2026-11-16, because plaud has no text for this recording yet. It is replaced when plaud produces its own transcript or summary._"
	if got != want {
		t.Errorf("plaud provenance = %q, want %q", got, want)
	}
	if !isMachineProvenance(got) {
		t.Error("third-party provenance not recognised")
	}
	if isMachineProvenance("_Transcript notes from the meeting._") {
		t.Error("ordinary italic text treated as provenance")
	}
}

// The Inbox excerpt of a machine-transcribed fact is the transcript text:
// no provenance line, no envelope markers, no preamble. The stored fact
// keeps all three.
func TestExcerptHidesMachineTranscriptEnvelope(t *testing.T) {
	cases := []struct {
		name, content, source, id string
	}{
		{"hive recording", recordingPendingFact(), RecordingType, "rec-9"},
		{"plaud", audioOnlyFact(), "plaud", "rec-2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			it := Item{Source: tc.source, ID: tc.id}
			out, ok := machineFact(tc.content, it, displayTestRecord(tc.source, tc.id))
			if !ok {
				t.Fatal("machineFact refused the pending fact")
			}
			for _, kept := range []string{"<" + UntrustedTag, "</" + UntrustedTag, untrustedPreamble, "made by this hive with whisper/base.en"} {
				if !strings.Contains(out, kept) {
					t.Errorf("stored fact lost %q:\n%s", kept, out)
				}
			}
			fm, body := splitFrontMatter(out)
			if connector.FactContentState(fm) != connector.FactContentMachine {
				t.Fatalf("fact state = %q", connector.FactContentState(fm))
			}
			ex := itemFromFact(fm, body, "personal", "/tmp/"+tc.id+".md").Excerpt
			if !strings.HasPrefix(ex, "We ship the fix today.") {
				t.Errorf("excerpt = %q, want the transcript text first", ex)
			}
			for _, hidden := range []string{UntrustedTag, "untrusted data", "made by this hive", "nonce="} {
				if strings.Contains(ex, hidden) {
					t.Errorf("excerpt shows %q: %q", hidden, ex)
				}
			}
		})
	}
}

func TestRecordingAudioExpiresAt(t *testing.T) {
	fin := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	m := RecordingMeta{ID: "rec-9", State: RecordingFinalized, AudioFile: "audio.webm", FinalizedAt: fin}
	if got, want := recordingAudioExpiresAt(m, 5), fin.Add(5*24*time.Hour); !got.Equal(want) {
		t.Errorf("expires = %v, want %v", got, want)
	}
	expired := m
	expired.AudioDeletedAt = fin.Add(time.Hour)
	active := m
	active.State = RecordingActive
	for name, mm := range map[string]RecordingMeta{"expired": expired, "active": active} {
		if got := recordingAudioExpiresAt(mm, 5); !got.IsZero() {
			t.Errorf("%s: expires = %v, want zero", name, got)
		}
	}
	if got := recordingAudioExpiresAt(m, 0); !got.IsZero() {
		t.Errorf("no retention: expires = %v, want zero", got)
	}
}
