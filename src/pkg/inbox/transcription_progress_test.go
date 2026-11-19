package inbox

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/transcribe"
)

const floatTol = 1e-6

func near(a, b float64) bool { return math.Abs(a-b) < floatTol }

func TestEstimateProgress(t *testing.T) {
	deepgram := transcribe.TypicalSpeed(transcribe.DeepgramName) // 10s + 0.05x
	cases := []struct {
		name          string
		in            etaInput
		progress, eta float64
		estimated     bool
	}{
		{"estimate halfway", etaInput{Elapsed: 12.5, AudioSeconds: 300, Processed: -1, Speed: deepgram}, 0.5, 12.5, true},
		{"estimate capped while overdue", etaInput{Elapsed: 100, AudioSeconds: 300, Processed: -1, Speed: deepgram}, estimateCap, 0, true},
		{"unknown length", etaInput{Elapsed: 5, Processed: -1, Speed: deepgram}, 0, 0, true},
		{"measured", etaInput{Elapsed: 60, AudioSeconds: 300, Processed: 150, Speed: deepgram, SmoothedETA: 40, ETAAge: 4}, 0.5, 36, false},
		{"measured capped short of done", etaInput{AudioSeconds: 300, Processed: 300, SmoothedETA: 1}, measuredCap, 1, false},
		{"first noisy tick falls back to the estimate", etaInput{Elapsed: 12.5, AudioSeconds: 300, Processed: 1, Speed: deepgram}, 0.5, 12.5, true},
		{"stale smoothed ETA never goes negative", etaInput{AudioSeconds: 100, Processed: 50, SmoothedETA: 3, ETAAge: 10}, 0.5, 0, false},
	}
	for _, c := range cases {
		got := estimateProgress(c.in)
		if !near(got.Progress, c.progress) || !near(got.ETA, c.eta) || got.Estimated != c.estimated {
			t.Errorf("%s: got %+v, want progress %v eta %v estimated %v", c.name, got, c.progress, c.eta, c.estimated)
		}
	}
}

func TestSmoothETAAndSpeedEMA(t *testing.T) {
	t0 := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	if got := smoothETA(0, time.Time{}, 120, t0); got != 120 {
		t.Fatalf("first ETA is raw: %v", got)
	}
	// Previous 100s ETA computed 10s ago decays to 90; blend with raw 60.
	if got, want := smoothETA(100, t0, 60, t0.Add(10*time.Second)), etaSmoothing*60+(1-etaSmoothing)*90; !near(got, want) {
		t.Fatalf("smoothed = %v, want %v", got, want)
	}

	s := foldSpeed(SpeedSample{}, 0.5)
	if s.RTF != 0.5 || s.Runs != 1 {
		t.Fatalf("first sample = %+v", s)
	}
	s = foldSpeed(s, 0.3)
	if want := speedEMAAlpha*0.3 + (1-speedEMAAlpha)*0.5; !near(s.RTF, want) || s.Runs != 2 {
		t.Fatalf("ema = %+v, want %v", s, want)
	}
	if got := foldSpeed(SpeedSample{}, 1e6); got.RTF != maxSpeedRTF {
		t.Fatalf("clamped high = %+v", got)
	}
	if got := foldSpeed(SpeedSample{}, 0); got.RTF != minSpeedRTF {
		t.Fatalf("clamped low = %+v", got)
	}

	store := NewTranscriptStore(t.TempDir())
	if _, ok := store.Speed("whisper", "base.en"); ok {
		t.Fatal("no speed before any run")
	}
	if got := expectedSpeed(store, "whisper", "base.en"); got != transcribe.TypicalSpeed("whisper") {
		t.Fatalf("typical speed first = %+v", got)
	}
	if err := store.RecordSpeed("whisper", "base.en", 150, 300); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSpeed("whisper", "base.en", 0, 300); err != nil {
		t.Fatal("a zero-length run is ignored, not an error")
	}
	if err := store.AddUsage(time.Now(), 300, 1); err != nil {
		t.Fatal(err)
	}
	got, ok := store.Speed("whisper", "base.en")
	if !ok || got.RTF != 0.5 || got.Runs != 1 {
		t.Fatalf("measured speed survives usage writes: %+v %v", got, ok)
	}
	if sp := expectedSpeed(store, "whisper", "base.en"); sp.RTF != 0.5 || sp.Overhead != 0 {
		t.Fatalf("measured speed is used: %+v", sp)
	}
}

// TestTranscriptionQueueAndLiveProgress runs two items through the queue
// and inspects the live state mid-run: the running job's phase, measured
// progress and ETA, the waiting job's place in line, and the lightweight
// ActiveTranscriptions view.
func TestTranscriptionQueueAndLiveProgress(t *testing.T) {
	srv := audioServer(t, http.StatusOK, txAudio)
	e := newTxEnv(t, activeTx(), srv.URL)
	e.svc.Getenv = func(k string) string {
		if k == "DEEPGRAM_API_KEY" {
			return "dg-secret-value"
		}
		return ""
	}
	e.src.items = append(e.src.items, Item{Source: "plaud", ID: "rec-2", Title: "Retro", Pending: true, DurationMS: 60000})
	var pending []func()
	e.svc.Run = func(fn func()) { pending = append(pending, fn) }
	ctx := context.Background()
	for _, id := range []string{"rec-1", "rec-2"} {
		it, ok, err := e.svc.Find(ctx, "plaud", id)
		if !ok || err != nil {
			t.Fatalf("find %s = %v %v", id, ok, err)
		}
		if _, err := e.svc.Transcribe(ctx, it, e.opts()); err != nil {
			t.Fatal(err)
		}
	}
	active := e.svc.ActiveTranscriptions()
	if len(active) != 2 || active[0].ID != "rec-1" || active[1].ID != "rec-2" || active[1].State.QueuePosition != 1 || active[1].State.Status != TranscriptionQueued {
		t.Fatalf("queued = %+v", active)
	}

	var mid []ActiveTranscription
	e.fake.Ticks = []transcribe.Progress{
		{Phase: transcribe.PhaseTranscribing, ProcessedSeconds: 0},
		{Phase: transcribe.PhaseTranscribing, ProcessedSeconds: 45},
	}
	e.fake.OnTick = func(p transcribe.Progress) {
		if p.ProcessedSeconds == 0 {
			e.now = e.now.Add(30 * time.Second)
			return
		}
		mid = e.svc.ActiveTranscriptions()
	}
	pending[0]()
	if len(mid) != 2 {
		t.Fatalf("mid-run = %+v", mid)
	}
	run := mid[0].State
	if run.Status != TranscriptionRunning || run.Phase != transcribe.PhaseTranscribing || !near(run.Progress, 0.5) || run.Estimated ||
		run.ProcessedSeconds != 45 || run.AudioSeconds != 90 || !near(run.ETASeconds, 30) || run.StartedAt == nil || run.QueuePosition != 0 {
		t.Fatalf("running state = %+v", run)
	}
	if mid[1].State.QueuePosition != 1 || mid[1].State.Phase != "" {
		t.Fatalf("waiting state = %+v", mid[1].State)
	}
	raw, _ := json.Marshal(mid)
	if strings.Contains(string(raw), "dg-secret") || strings.Contains(string(raw), "secret-signature") {
		t.Fatalf("live state leaks the key or link: %s", raw)
	}

	// rec-1 is done; its 30s for 90s of audio taught the hive its speed.
	if rec, _ := e.store.Get("plaud", "rec-1"); rec.Status != TranscriptionDone {
		t.Fatalf("rec-1 = %+v", rec)
	}
	if sp, ok := e.store.Speed("deepgram", "nova-3"); !ok || !near(sp.RTF, 30.0/90) {
		t.Fatalf("measured speed = %+v %v", sp, ok)
	}
	active = e.svc.ActiveTranscriptions()
	if len(active) != 1 || active[0].ID != "rec-2" || active[0].State.QueuePosition != 0 {
		t.Fatalf("after rec-1 = %+v", active)
	}

	// rec-2 has no measured ticks: the estimate comes from the measured speed.
	e.fake.Ticks = []transcribe.Progress{{Phase: transcribe.PhaseTranscribing, ProcessedSeconds: -1}}
	e.fake.OnTick = func(transcribe.Progress) {
		e.now = e.now.Add(10 * time.Second)
		mid = e.svc.ActiveTranscriptions()
	}
	pending[1]()
	est := mid[0].State
	// 60s of audio at a measured 1/3 real-time factor: 20s expected, 10s in.
	if !est.Estimated || !near(est.Progress, 0.5) || !near(est.ETASeconds, 10) || est.Phase != transcribe.PhaseTranscribing {
		t.Fatalf("estimated state = %+v", est)
	}
	if got := e.svc.ActiveTranscriptions(); len(got) != 0 {
		t.Fatalf("queue drained = %+v", got)
	}
	if it := e.item(t); it.Transcription == nil || it.Transcription.Status != TranscriptionDone || it.Transcription.Phase != "" || it.Transcription.StartedAt != nil {
		t.Fatalf("finished item carries no live progress: %+v", it.Transcription)
	}
}

func TestTranscriptionProviderResolution(t *testing.T) {
	e := newTxEnv(t, activeTx(), "http://audio.invalid")
	env := map[string]string{}
	e.svc.Getenv = func(k string) string { return env[k] }
	cases := []struct {
		provider string
		key      bool
		want     string
		ready    bool
	}{
		{"", false, "deepgram", false},
		{"auto", true, "deepgram", true},
		{"deepgram", true, "deepgram", true},
		{"whisper", true, "whisper", false},
		{"gemini", true, "gemini", false},
	}
	for _, c := range cases {
		delete(env, "DEEPGRAM_API_KEY")
		if c.key {
			env["DEEPGRAM_API_KEY"] = "dg-secret-value"
		}
		cfg := config.InboxTranscription{Enabled: true, ConsentAcknowledged: true, Provider: c.provider}
		got := e.svc.resolveProvider(cfg)
		if got.Name != c.want || got.Available != c.ready {
			t.Errorf("provider %q key %v: resolved %+v, want %s ready %v", c.provider, c.key, got, c.want, c.ready)
		}
	}
	env["DEEPGRAM_API_KEY"] = "dg-secret-value"
	st := e.svc.TranscriptionSettings()
	if st.Provider != "auto" || st.ResolvedProvider != "deepgram" || st.ResolvedModel != "nova-3" || len(st.Providers) != 3 || st.Providers[0].Name != "whisper" {
		t.Fatalf("settings = %+v", st)
	}
	raw, _ := json.Marshal(st)
	if strings.Contains(string(raw), "dg-secret") {
		t.Fatalf("settings leak the key: %s", raw)
	}
	for _, p := range st.Providers {
		if p.AudioGoesTo == "" || p.Detail == "" || p.Status == "" {
			t.Errorf("provider status incomplete: %+v", p)
		}
	}
}
