package signals

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
)

func fixedClock() func() time.Time {
	t := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	return func() time.Time { t = t.Add(time.Second); return t }
}

func TestLogEmitsReadsAndPages(t *testing.T) {
	dir := t.TempDir()
	log, err := Open(dir, Policy{Now: fixedClock()})
	if err != nil {
		t.Fatal(err)
	}
	first, err := log.Emit(Signal{Kind: api.SignalStarted, Source: api.SignalSourceRunner, Data: json.RawMessage(`{"command":["make"],"token":"ghp_abcdefghijklmnopqrstuvwxyz0123456789"}`)})
	if err != nil || first.Seq != 1 || first.Kind != api.SignalStarted || first.Source != api.SignalSourceRunner {
		t.Fatalf("started = %+v, %v", first, err)
	}
	if strings.Contains(string(first.Data), "ghp_abc") || !strings.Contains(string(first.Data), `"command":["make"]`) {
		t.Fatalf("data not scrubbed or lost: %s", first.Data)
	}
	for _, kind := range []string{api.SignalThinking, api.SignalTool, api.SignalNeedsInput} {
		if _, err := log.Emit(Signal{Kind: kind, Source: api.SignalSourceCaller, By: "project:atlas", Message: "step " + kind}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := log.Emit(Signal{Kind: api.SignalDone, Source: api.SignalSourceRunner, Message: "exit status 0"}); err != nil {
		t.Fatal(err)
	}

	page, err := Read(dir, 0, 2)
	if err != nil || len(page.Signals) != 2 || !page.More || page.NextSeq != 2 || page.Signals[1].By != "project:atlas" {
		t.Fatalf("page 1 = %+v, %v", page, err)
	}
	page, err = Read(dir, page.NextSeq, 0)
	if err != nil || len(page.Signals) != 3 || page.More || page.NextSeq != 5 || page.Signals[2].Kind != api.SignalDone {
		t.Fatalf("page 2 = %+v, %v", page, err)
	}
	if !page.Signals[0].At.Before(page.Signals[2].At) {
		t.Fatalf("timestamps not increasing: %+v", page.Signals)
	}
	// Past the end: empty, next_seq echoes since_seq.
	page, err = Read(dir, 5, 0)
	if err != nil || len(page.Signals) != 0 || page.NextSeq != 5 {
		t.Fatalf("tail = %+v, %v", page, err)
	}
	// Over the cap is clamped, not refused.
	if page, err = Read(dir, 0, MaxPageSize+1); err != nil || len(page.Signals) != 5 {
		t.Fatalf("clamped = %+v, %v", page, err)
	}
	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil || info.Mode().Perm() != fileMode {
		t.Fatalf("file mode = %v, %v", info, err)
	}
}

func TestReopenResumesSeq(t *testing.T) {
	dir := t.TempDir()
	log, _ := Open(dir, Policy{})
	if _, err := log.Emit(Signal{Kind: api.SignalStarted, Source: api.SignalSourceRunner}); err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir, Policy{})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := again.Emit(Signal{Kind: api.SignalDone, Source: api.SignalSourceRunner})
	if err != nil || rec.Seq != 2 {
		t.Fatalf("resumed seq = %+v, %v", rec, err)
	}
}

func TestEmitRefusesWhatTheClosedSetsForbid(t *testing.T) {
	log, _ := Open(t.TempDir(), Policy{})
	cases := []struct {
		name string
		sig  Signal
		want error
	}{
		{"unknown kind", Signal{Kind: "finished", Source: api.SignalSourceRunner}, ErrBadRequest},
		{"unknown source", Signal{Kind: api.SignalDone, Source: "agent"}, ErrBadRequest},
		{"caller claims done", Signal{Kind: api.SignalDone, Source: api.SignalSourceCaller}, ErrKindNotAllowed},
		{"caller claims started", Signal{Kind: api.SignalStarted, Source: api.SignalSourceCaller}, ErrKindNotAllowed},
		{"gate claims tool", Signal{Kind: api.SignalTool, Source: api.SignalSourceGate}, ErrKindNotAllowed},
		{"message too long", Signal{Kind: api.SignalThinking, Source: api.SignalSourceCaller, Message: strings.Repeat("m", MaxMessageBytes+1)}, ErrBadRequest},
		{"data not json", Signal{Kind: api.SignalThinking, Source: api.SignalSourceCaller, Data: json.RawMessage(`{nope`)}, ErrBadRequest},
		{"data two values", Signal{Kind: api.SignalThinking, Source: api.SignalSourceCaller, Data: json.RawMessage(`1 2`)}, ErrBadRequest},
		{"data too large", Signal{Kind: api.SignalThinking, Source: api.SignalSourceCaller, Data: json.RawMessage(`"` + strings.Repeat("d", MaxDataBytes) + `"`)}, ErrDataTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := log.Emit(tc.sig); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v want %v", err, tc.want)
			}
		})
	}
	// Nothing above was written.
	if _, err := Read(filepath.Dir(log.path), 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("refused signals reached the file: %v", err)
	}
	// JSON null data is dropped, not stored.
	rec, err := log.Emit(Signal{Kind: api.SignalInput, Source: api.SignalSourceGate, Data: json.RawMessage(`null`)})
	if err != nil || rec.Data != nil {
		t.Fatalf("null data = %+v, %v", rec, err)
	}
}

func TestReadReportsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{\"seq\":1,\"kind\":\"started\"}\nnot json\n"), fileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir, 0, 0); !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("corrupt = %v", err)
	}
	if _, err := Open(dir, Policy{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("open corrupt = %v", err)
	}
	if _, err := Read(t.TempDir(), 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
}

func TestOutcomeStoreRoundTrips(t *testing.T) {
	st, err := OpenOutcomes(DefaultDir(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.Get("alpha"); err != nil || ok {
		t.Fatalf("empty get = %v %v", ok, err)
	}
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	want := api.KickOutcome{Kind: "ended", At: at.Add(time.Minute), KickAt: at}
	if err := st.Put("alpha", want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.Get("alpha")
	if err != nil || !ok || got.Kind != want.Kind || !got.KickAt.Equal(want.KickAt) {
		t.Fatalf("get = %+v %v %v", got, ok, err)
	}
	// Replaced whole on the next settle.
	want.Kind, want.KickAt = "no-op", at.Add(time.Hour)
	if err := st.Put("alpha", want); err != nil {
		t.Fatal(err)
	}
	if got, _, _ = st.Get("alpha"); got.Kind != "no-op" || !got.KickAt.Equal(want.KickAt) {
		t.Fatalf("replaced = %+v", got)
	}
	for _, bad := range []string{"../etc", "Alpha", "", "a b"} {
		if err := st.Put(bad, want); !errors.Is(err, ErrBadAgentName) {
			t.Fatalf("put %q = %v", bad, err)
		}
		if _, _, err := st.Get(bad); !errors.Is(err, ErrBadAgentName) {
			t.Fatalf("get %q = %v", bad, err)
		}
	}
	if err := os.WriteFile(filepath.Join(st.dir, "beta.json"), []byte("{"), fileMode); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Get("beta"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt = %v", err)
	}
}

func TestKnownKindAndAllowed(t *testing.T) {
	for _, k := range Kinds {
		if !KnownKind(k) {
			t.Errorf("KnownKind(%s) = false", k)
		}
	}
	if KnownKind("") || KnownKind("ok") {
		t.Error("KnownKind admits strangers")
	}
	for _, k := range api.CallerSignalKinds {
		if !Allowed(api.SignalSourceCaller, k) {
			t.Errorf("caller may not %s", k)
		}
	}
	if Allowed(api.SignalSourceCaller, api.SignalDone) || Allowed("", api.SignalDone) {
		t.Error("Allowed lets a caller end a turn")
	}
}
