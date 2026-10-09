package compliance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var attestNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func TestNormalizeAttestation(t *testing.T) {
	long := strings.Repeat("x", MaxAttestationNoteLen+1)
	cases := []struct {
		name    string
		in      Attestation
		wantErr string
		want    Attestation
	}{
		{
			name: "valid trimmed and lowercased",
			in:   Attestation{Framework: " SOC2-Type2 ", ReviewedOn: " 2026-10-08 ", By: " alice ", Note: " ok "},
			want: Attestation{Framework: "soc2-type2", ReviewedOn: "2026-10-08", By: "alice", Note: "ok", At: attestNow},
		},
		{name: "missing framework", in: Attestation{ReviewedOn: "2026-10-08", By: "a"}, wantErr: "framework is required"},
		{name: "unknown framework", in: Attestation{Framework: "pci", ReviewedOn: "2026-10-08", By: "a"}, wantErr: "unknown framework"},
		{name: "bad date", in: Attestation{Framework: "soc2-type2", ReviewedOn: "10/08/2026", By: "a"}, wantErr: "YYYY-MM-DD"},
		{name: "future date", in: Attestation{Framework: "soc2-type2", ReviewedOn: "2026-10-12", By: "a"}, wantErr: "future"},
		{name: "no user", in: Attestation{Framework: "soc2-type2", ReviewedOn: "2026-10-08"}, wantErr: "user"},
		{name: "long note", in: Attestation{Framework: "soc2-type2", ReviewedOn: "2026-10-08", By: "a", Note: long}, wantErr: "note"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeAttestation(tc.in, attestNow)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestAttestationStorePersistAndList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "attest.jsonl")
	s, err := NewAttestationStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	items := []Attestation{
		{Framework: "soc2-type2", ReviewedOn: "2026-10-01", By: "alice", At: attestNow.Add(-48 * time.Hour)},
		{Framework: "fedramp-moderate", ReviewedOn: "2026-10-02", By: "bob", At: attestNow.Add(-24 * time.Hour)},
		{Framework: "soc2-type2", ReviewedOn: "2026-10-08", By: "alice", At: attestNow, Note: "q4"},
	}
	for _, a := range items {
		if err := s.Add(a); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	// A torn or foreign line is skipped on reload.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString("{not json\n\n{\"framework\":\"\"}\n")
	_ = f.Close()

	re, err := NewAttestationStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	cases := []struct {
		name         string
		framework    string
		since, until time.Time
		wantBy       []string
	}{
		{name: "all newest first", wantBy: []string{"alice", "bob", "alice"}},
		{name: "framework filter", framework: " SOC2-TYPE2", wantBy: []string{"alice", "alice"}},
		{name: "since", since: attestNow.Add(-24 * time.Hour), wantBy: []string{"alice", "bob"}},
		{name: "until exclusive", until: attestNow, wantBy: []string{"bob", "alice"}},
		{name: "none", framework: "iso27001-annex-a", wantBy: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := re.List(tc.framework, tc.since, tc.until)
			if got == nil {
				t.Fatal("List returned nil, want empty slice")
			}
			if len(got) != len(tc.wantBy) {
				t.Fatalf("got %d, want %d (%+v)", len(got), len(tc.wantBy), got)
			}
			for i, a := range got {
				if a.By != tc.wantBy[i] {
					t.Fatalf("[%d].By = %q, want %q", i, a.By, tc.wantBy[i])
				}
			}
		})
	}
}

func TestAttestationStoreMemoryAndCap(t *testing.T) {
	s, err := NewAttestationStore("")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < MaxAttestations+3; i++ {
		if err := s.Add(Attestation{Framework: "soc2-type2", By: "a", At: attestNow.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	got := s.List("", time.Time{}, time.Time{})
	if len(got) != MaxAttestations {
		t.Fatalf("len = %d, want %d", len(got), MaxAttestations)
	}
	if want := attestNow.Add(3 * time.Second); !got[len(got)-1].At.Equal(want) {
		t.Fatalf("oldest kept = %s, want %s", got[len(got)-1].At, want)
	}
}

func TestAttestationStoreErrors(t *testing.T) {
	dir := t.TempDir()
	// Reading a directory fails with something other than not-exist.
	if _, err := NewAttestationStore(dir); err == nil {
		t.Fatal("expected read error for a directory path")
	}
	missing, err := NewAttestationStore(filepath.Join(dir, "missing.jsonl"))
	if err != nil || len(missing.List("", time.Time{}, time.Time{})) != 0 {
		t.Fatalf("missing file: err=%v", err)
	}
	// A parent that is a file makes MkdirAll fail; the add still lands in memory.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, _ := NewAttestationStore(filepath.Join(blocker, "a.jsonl"))
	if err := s.Add(Attestation{Framework: "soc2-type2", At: attestNow}); err == nil {
		t.Fatal("expected mkdir error")
	}
	if len(s.List("", time.Time{}, time.Time{})) != 1 {
		t.Fatal("attestation not kept in memory after persist failure")
	}
	// A directory at the file path makes OpenFile fail.
	asDir := filepath.Join(dir, "asdir")
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s2 := &AttestationStore{path: asDir}
	if err := s2.Add(Attestation{Framework: "soc2-type2", At: attestNow}); err == nil {
		t.Fatal("expected open error")
	}
}
