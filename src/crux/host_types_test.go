package crux

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResumeHandleHelpers(t *testing.T) {
	if (ResumeHandle{}).String() != "" {
		t.Fatal("empty handle must render as empty")
	}
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	h := ResumeHandle{Backend: "claude", SessionID: "abc", Transcript: transcript}
	if got, want := h.String(), "claude session abc ("+transcript+")"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	if h.TranscriptExists() {
		t.Fatal("transcript not yet written must not exist")
	}
	if err := os.WriteFile(transcript, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !h.TranscriptExists() {
		t.Fatal("written transcript must exist")
	}
	if (ResumeHandle{SessionID: "x"}).TranscriptExists() {
		t.Fatal("handle without a transcript path must report false")
	}
}

func TestLinearCredentialEmpty(t *testing.T) {
	if !(LinearCredential{}).Empty() {
		t.Fatal("zero credential must be empty")
	}
	if (LinearCredential{APIKey: "k"}).Empty() || (LinearCredential{AccessToken: "t"}).Empty() {
		t.Fatal("a set field must make the credential non-empty")
	}
}
