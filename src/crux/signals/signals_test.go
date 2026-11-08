package signals

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
)

var testNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func testPolicy() Policy {
	return Policy{Now: func() time.Time { return testNow }}
}

// fakeToken is credential-shaped for logscrub without being a credential.
const fakeToken = "ghp_0123456789abcdefghijklmnopqrstuvwxyz"

func mustAppend(t *testing.T, b *Book, dir string, in Signal) api.RunSignal {
	t.Helper()
	sig, err := b.Append("run_x", dir, in)
	if err != nil {
		t.Fatalf("Append(%+v): %v", in, err)
	}
	return sig
}

func TestBookAppendsInSeqOrderAndReadPages(t *testing.T) {
	dir := t.TempDir()
	b := NewBook(testPolicy())
	var seen []int64
	var mu sync.Mutex
	b.SetObserver(func(run string, sig api.RunSignal) {
		mu.Lock()
		defer mu.Unlock()
		if run != "run_x" {
			t.Errorf("observer run = %q", run)
		}
		seen = append(seen, sig.Seq)
	})
	mustAppend(t, b, dir, Signal{Kind: api.SignalStarted, Source: api.SignalSourceRunner, Message: "sandbox launched"})
	mustAppend(t, b, dir, Signal{Kind: api.SignalThinking, Source: api.SignalSourceCaller})
	tool := mustAppend(t, b, dir, Signal{Kind: api.SignalTool, Source: api.SignalSourceCaller, Message: "  go test  ", Data: json.RawMessage(`{"tool": "bash", "n": 2}`)})
	if tool.Seq != 3 || tool.Message != "go test" || string(tool.Data) != `{"n":2,"tool":"bash"}` || !tool.At.Equal(testNow) {
		t.Fatalf("tool signal = %+v data=%s", tool, tool.Data)
	}
	if len(seen) != 3 || seen[2] != 3 {
		t.Fatalf("observer saw %v", seen)
	}
	page, err := Read(dir, 0, 2)
	if err != nil || len(page.Signals) != 2 || !page.More || page.NextSeq != 2 || page.Signals[0].Kind != api.SignalStarted {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	page, err = Read(dir, page.NextSeq, 2)
	if err != nil || len(page.Signals) != 1 || page.More || page.NextSeq != 3 || page.Signals[0].Kind != api.SignalTool {
		t.Fatalf("second page = %+v, %v", page, err)
	}
	// Following past the end answers an empty page at the same seq.
	page, err = Read(dir, 3, 0)
	if err != nil || len(page.Signals) != 0 || page.NextSeq != 3 || page.More {
		t.Fatalf("past the end = %+v, %v", page, err)
	}
	if _, err := Read(t.TempDir(), 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no log: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil || info.Mode().Perm() != fileMode {
		t.Fatalf("log mode = %v, %v", info, err)
	}
}

func TestReadClampsTheLimit(t *testing.T) {
	dir := t.TempDir()
	b := NewBook(testPolicy())
	for range MaxPageSize + 1 {
		mustAppend(t, b, dir, Signal{Kind: api.SignalThinking, Source: api.SignalSourceCaller})
	}
	for _, tc := range []struct {
		limit, want int
	}{{0, DefaultPageSize}, {-3, DefaultPageSize}, {MaxPageSize + 50, MaxPageSize}} {
		page, err := Read(dir, 0, tc.limit)
		if err != nil || len(page.Signals) != tc.want || !page.More {
			t.Fatalf("limit %d: %d signals more=%v, %v", tc.limit, len(page.Signals), page.More, err)
		}
	}
}

func TestDoneIsTheLastSignalAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	b := NewBook(testPolicy())
	mustAppend(t, b, dir, Signal{Kind: api.SignalStarted, Source: api.SignalSourceRunner})

	// A new Book (a restarted server) resumes the seq from the file.
	b = NewBook(testPolicy())
	if sig := mustAppend(t, b, dir, Signal{Kind: api.SignalNeedsInput, Source: api.SignalSourceGate, Message: "approve?"}); sig.Seq != 2 {
		t.Fatalf("resumed seq = %d", sig.Seq)
	}
	done := mustAppend(t, b, dir, Signal{Kind: api.SignalDone, Source: api.SignalSourceRunner, Data: json.RawMessage(`{"status":"completed","exit_code":0}`)})
	if done.Seq != 3 {
		t.Fatalf("done seq = %d", done.Seq)
	}
	if _, err := b.Append("run_x", dir, Signal{Kind: api.SignalThinking, Source: api.SignalSourceCaller}); !errors.Is(err, ErrClosed) {
		t.Fatalf("after done: %v", err)
	}
	b = NewBook(testPolicy())
	if _, err := b.Append("run_x", dir, Signal{Kind: api.SignalInput, Source: api.SignalSourceGate}); !errors.Is(err, ErrClosed) {
		t.Fatalf("after done, restarted: %v", err)
	}
	page, err := Read(dir, 0, 0)
	if err != nil || len(page.Signals) != 3 || page.Signals[2].Kind != api.SignalDone {
		t.Fatalf("log = %+v, %v", page, err)
	}
}

func TestSourcesAppendOnlyTheirKinds(t *testing.T) {
	for _, tc := range []struct {
		source, kind string
		ok           bool
	}{
		{api.SignalSourceRunner, api.SignalStarted, true},
		{api.SignalSourceRunner, api.SignalDone, true},
		{api.SignalSourceRunner, api.SignalStep, true},
		{api.SignalSourceRunner, api.SignalThinking, false},
		{api.SignalSourceGate, api.SignalStep, false},
		{api.SignalSourceCaller, api.SignalStep, false},
		{api.SignalSourceGate, api.SignalNeedsInput, true},
		{api.SignalSourceGate, api.SignalInput, true},
		{api.SignalSourceGate, api.SignalBlocked, true},
		{api.SignalSourceGate, api.SignalDone, false},
		{api.SignalSourceCaller, api.SignalThinking, true},
		{api.SignalSourceCaller, api.SignalTool, true},
		{api.SignalSourceCaller, api.SignalBlocked, true},
		{api.SignalSourceCaller, api.SignalNeedsInput, true},
		{api.SignalSourceCaller, api.SignalInput, true},
		{api.SignalSourceCaller, api.SignalStarted, false},
		{api.SignalSourceCaller, api.SignalDone, false},
	} {
		_, err := NewBook(testPolicy()).Append("run_x", t.TempDir(), Signal{Kind: tc.kind, Source: tc.source})
		if tc.ok != (err == nil) || (!tc.ok && !errors.Is(err, ErrBadRequest)) {
			t.Errorf("%s %s: %v", tc.source, tc.kind, err)
		}
	}
	for _, in := range []Signal{
		{Kind: "finished", Source: api.SignalSourceRunner},
		{Kind: api.SignalThinking, Source: "agent"},
	} {
		if _, err := NewBook(testPolicy()).Append("run_x", t.TempDir(), in); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%+v: %v", in, err)
		}
	}
}

func TestMessageAndDataAreBoundedAndScrubbed(t *testing.T) {
	dir := t.TempDir()
	b := NewBook(testPolicy())
	long := strings.Repeat("é", MaxMessageBytes) // two bytes a rune
	if _, err := b.Append("run_x", dir, Signal{Kind: api.SignalTool, Source: api.SignalSourceCaller, Message: long}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("caller's long message: %v", err)
	}
	// The runner's long reason is cut on a rune boundary, never refused.
	cut := mustAppend(t, b, dir, Signal{Kind: api.SignalStarted, Source: api.SignalSourceRunner, Message: long})
	if len(cut.Message) > MaxMessageBytes || !strings.HasPrefix(long, cut.Message) || len(cut.Message) < MaxMessageBytes-1 {
		t.Fatalf("cut message is %d bytes", len(cut.Message))
	}

	for _, data := range []string{`{"a":`, `{"a":1} {"b":2}`, `"` + strings.Repeat("x", MaxDataBytes) + `"`} {
		if _, err := b.Append("run_x", dir, Signal{Kind: api.SignalTool, Source: api.SignalSourceCaller, Data: json.RawMessage(data)}); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("data %.20q: %v", data, err)
		}
	}
	null := mustAppend(t, b, dir, Signal{Kind: api.SignalTool, Source: api.SignalSourceCaller, Data: json.RawMessage(" null ")})
	if null.Data != nil {
		t.Fatalf("null data = %s", null.Data)
	}

	secret := mustAppend(t, b, dir, Signal{Kind: api.SignalTool, Source: api.SignalSourceCaller,
		Message: "push with " + fakeToken, Data: json.RawMessage(`{"argv":["git","push","` + fakeToken + `"],"big":12345678901234567890}`)})
	if strings.Contains(secret.Message, "ghp_") || strings.Contains(string(secret.Data), "ghp_") || !strings.Contains(string(secret.Data), "12345678901234567890") {
		t.Fatalf("scrubbed = %q %s", secret.Message, secret.Data)
	}
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil || strings.Contains(string(raw), "ghp_") {
		t.Fatalf("log holds the token: %v", err)
	}
}

func TestACorruptLogIsReported(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{\"seq\":1,\"kind\":\"started\"}\n\nnot json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir, 0, 0); !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("Read: %v", err)
	}
	if _, err := NewBook(testPolicy()).Append("run_x", dir, Signal{Kind: api.SignalTool, Source: api.SignalSourceCaller}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Append: %v", err)
	}
}

func TestAppendFailsWhenTheRunDirectoryIsGone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	if _, err := NewBook(testPolicy()).Append("run_x", dir, Signal{Kind: api.SignalTool, Source: api.SignalSourceCaller}); err == nil {
		t.Fatal("append into a missing directory succeeded")
	}
}

func TestAgentOutcomesSaveAndLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), DirName, AgentsDirName)
	store, err := OpenAgentOutcomes(dir, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != dirMode {
		t.Fatalf("dir = %v, %v", info, err)
	}
	if _, err := store.Load("alpha"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nothing saved: %v", err)
	}
	kick := testNow.Add(-time.Minute)
	first := api.KickOutcome{Kind: "question", Reason: "use " + fakeToken + "?", At: testNow, KickAt: kick}
	if err := store.Save("alpha", first); err != nil {
		t.Fatal(err)
	}
	second := api.KickOutcome{Kind: "ended", At: testNow.Add(time.Minute), KickAt: testNow}
	if err := store.Save("alpha", second); err != nil {
		t.Fatal(err)
	}
	// A reopened store (a restarted server) reads the latest record.
	store, err = OpenAgentOutcomes(dir, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Load("alpha")
	if err != nil || got.Agent != "alpha" || got.Outcome.Kind != "ended" || !got.Outcome.KickAt.Equal(testNow) || !got.SavedAt.Equal(testNow) {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	if err := store.Save("beta", first); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Load("beta"); err != nil || strings.Contains(got.Outcome.Reason, "ghp_") || got.Outcome.Reason == "" {
		t.Fatalf("scrubbed reason = %+v, %v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("dir holds %v, %v (no temp files may remain)", entries, err)
	}
}

func TestAgentOutcomesRefuseNamesThatAreNotIdentifiers(t *testing.T) {
	store, err := OpenAgentOutcomes(t.TempDir(), testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "../escape", "Alpha", "a/b", strings.Repeat("a", maxAgentNameLen+1)} {
		if err := store.Save(name, api.KickOutcome{Kind: "ended"}); !errors.Is(err, ErrBadRequest) {
			t.Errorf("Save(%q): %v", name, err)
		}
		if _, err := store.Load(name); !errors.Is(err, ErrBadRequest) {
			t.Errorf("Load(%q): %v", name, err)
		}
	}
}

func TestAgentOutcomesReportCorruptionAndUnusableDirectories(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenAgentOutcomes(dir, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "alpha.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("alpha"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt: %v", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAgentOutcomes(filepath.Join(file, "agents"), testPolicy()); err == nil {
		t.Fatal("opened a directory under a file")
	}
	// A store whose directory vanished cannot save.
	gone := filepath.Join(t.TempDir(), "agents")
	store, err = OpenAgentOutcomes(gone, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("alpha", api.KickOutcome{Kind: "ended"}); err == nil {
		t.Fatal("saved into a missing directory")
	}
}
