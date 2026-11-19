package inbox

import (
	"time"

	"github.com/hivecommons/hive/pkg/transcribe"
)

// Live progress of machine transcriptions. Runs execute in the order they
// were queued, at most transcribeConcurrency at a time per hive (one by
// default, see transcription_auto.go); each queued or running item has a
// txJob holding its live phase and position. Nothing here is persisted: a
// progress tick is in-memory only, the record on disk changes on status
// transitions.

const (
	// estimateCap is the most an estimated (unmeasured) run shows before it
	// finishes, so a slow upload never reads as done.
	estimateCap = 0.95
	// measuredCap holds a measured run just short of done until the
	// transcript is stored.
	measuredCap = 0.99
	// minMeasuredFraction is the measured progress below which the ETA still
	// comes from the speed estimate (the first ticks are noisy).
	minMeasuredFraction = 0.02
	// etaSmoothing weighs the newest raw ETA against the previous one,
	// decayed by the time since it was computed.
	etaSmoothing = 0.3
)

// txJob is one queued or running transcription.
type txJob struct {
	source, id string
	// ready is closed when the job reaches the head of the queue.
	ready chan struct{}
	// provider / model the run uses; speed is the expected speed.
	provider, model string
	speed           transcribe.Speed

	phase        string
	started      time.Time // run start (zero while queued)
	phaseStarted time.Time // start of the transcribing phase
	audioSeconds float64
	processed    float64 // measured audio seconds done; < 0 unknown
	eta          float64 // smoothed ETA of a measured run
	etaAt        time.Time
}

// txEnqueue appends a job for it; it is ready at once while fewer than
// transcribeConcurrency jobs are ahead of it.
func (s *Service) txEnqueue(it Item, provider, model string, speed transcribe.Speed) *txJob {
	j := &txJob{
		source: it.Source, id: it.ID, ready: make(chan struct{}), provider: provider, model: model,
		speed: speed, processed: -1,
		audioSeconds: float64(it.DurationMS) / float64(time.Second/time.Millisecond),
	}
	s.txQMu.Lock()
	defer s.txQMu.Unlock()
	s.txQueue = append(s.txQueue, j)
	if len(s.txQueue) <= s.transcribeConcurrency() {
		close(j.ready)
	}
	return j
}

// txDequeue removes j and, when j held a run slot, wakes the first job
// still waiting (the one that moves into the last slot).
func (s *Service) txDequeue(j *txJob) {
	s.txQMu.Lock()
	defer s.txQMu.Unlock()
	slots := s.transcribeConcurrency()
	for i, q := range s.txQueue {
		if q == j {
			wasActive := i < slots
			s.txQueue = append(s.txQueue[:i:i], s.txQueue[i+1:]...)
			if wasActive && len(s.txQueue) >= slots {
				close(s.txQueue[slots-1].ready)
			}
			return
		}
	}
}

// txSetSpeed replaces the expected speed of j.
func (s *Service) txSetSpeed(j *txJob, speed transcribe.Speed) {
	s.txQMu.Lock()
	j.speed = speed
	s.txQMu.Unlock()
}

// txProgress applies one provider report to j.
func (s *Service) txProgress(j *txJob, p transcribe.Progress) {
	s.txQMu.Lock()
	defer s.txQMu.Unlock()
	now := s.now()
	if p.Phase != "" && p.Phase != j.phase {
		j.phase = p.Phase
		if p.Phase == transcribe.PhaseTranscribing {
			j.phaseStarted = now
		}
	}
	if j.started.IsZero() {
		j.started = now
	}
	if p.AudioSeconds > 0 {
		j.audioSeconds = p.AudioSeconds
	}
	if p.ProcessedSeconds >= 0 {
		j.processed = p.ProcessedSeconds
		if f := measuredFraction(j.processed, j.audioSeconds); f >= minMeasuredFraction {
			if j.phaseStarted.IsZero() {
				j.phaseStarted = j.started
			}
			raw := now.Sub(j.phaseStarted).Seconds() / f * (1 - f)
			j.eta = smoothETA(j.eta, j.etaAt, raw, now)
			j.etaAt = now
		}
	}
}

// txLive overlays the live progress of the item's job onto st. It reports
// false when the item has no queued or running job.
func (s *Service) txLive(source, id string, st *TranscriptionState) bool {
	s.txQMu.Lock()
	defer s.txQMu.Unlock()
	for i, j := range s.txQueue {
		if j.source != source || j.id != id {
			continue
		}
		st.AudioSeconds = j.audioSeconds
		if j.started.IsZero() {
			// Jobs inside a run slot wait for nothing; the rest wait for
			// this many runs to finish.
			st.QueuePosition = max(0, i-s.transcribeConcurrency()+1)
			return true
		}
		started := j.started
		st.StartedAt, st.Phase = &started, j.phase
		now := s.now()
		est := estimateProgress(etaInput{
			Elapsed: now.Sub(j.started).Seconds(), AudioSeconds: j.audioSeconds, Processed: j.processed,
			Speed: j.speed, SmoothedETA: j.eta, ETAAge: now.Sub(j.etaAt).Seconds(),
		})
		st.Progress, st.ETASeconds, st.Estimated = est.Progress, est.ETA, est.Estimated
		if j.processed >= 0 {
			st.ProcessedSeconds = j.processed
		}
		return true
	}
	return false
}

// ActiveTranscription is one queued or running item with its live state.
type ActiveTranscription struct {
	Source string             `json:"source"`
	ID     string             `json:"id"`
	State  TranscriptionState `json:"state"`
}

// ActiveTranscriptions lists the queued and running items in queue order
// with their live progress: the cheap poll the Inbox uses while a job runs.
func (s *Service) ActiveTranscriptions() []ActiveTranscription {
	s.txQMu.Lock()
	keys := make([][2]string, 0, len(s.txQueue))
	for _, j := range s.txQueue {
		keys = append(keys, [2]string{j.source, j.id})
	}
	s.txQMu.Unlock()
	store := s.transcriptStore()
	out := make([]ActiveTranscription, 0, len(keys))
	for _, k := range keys {
		var st TranscriptionState
		if store != nil {
			if rec, ok := store.Get(k[0], k[1]); ok {
				st = rec.State()
			}
		}
		if !s.txLive(k[0], k[1], &st) {
			continue
		}
		out = append(out, ActiveTranscription{Source: k[0], ID: k[1], State: st})
	}
	return out
}

// etaInput is everything the progress estimate needs, in seconds.
type etaInput struct {
	// Elapsed is the time since the run started.
	Elapsed float64
	// AudioSeconds is the recording length (0 when unknown).
	AudioSeconds float64
	// Processed is the measured audio transcribed; < 0 when unknown.
	Processed float64
	// Speed is the expected speed (measured on this hive, else typical).
	Speed transcribe.Speed
	// SmoothedETA is the last smoothed measured ETA and ETAAge its age.
	SmoothedETA, ETAAge float64
}

// etaEstimate is the progress shown for a running job.
type etaEstimate struct {
	Progress  float64
	ETA       float64
	Estimated bool
}

// estimateProgress computes progress (0..1) and the time left. A measured
// run (Processed known and past minMeasuredFraction) uses the processed
// share and the smoothed rate; otherwise progress is elapsed over the
// expected duration from Speed, capped at estimateCap until done.
func estimateProgress(in etaInput) etaEstimate {
	if f := measuredFraction(in.Processed, in.AudioSeconds); f >= minMeasuredFraction {
		return etaEstimate{Progress: min(f, measuredCap), ETA: max(in.SmoothedETA-max(in.ETAAge, 0), 0)}
	}
	expected := in.Speed.Seconds(in.AudioSeconds)
	if in.AudioSeconds <= 0 || expected <= 0 {
		return etaEstimate{Estimated: true}
	}
	elapsed := max(in.Elapsed, 0)
	return etaEstimate{Progress: min(elapsed/expected, estimateCap), ETA: max(expected-elapsed, 0), Estimated: true}
}

// measuredFraction is processed/audio clamped to 0..1, or 0 when unknown.
func measuredFraction(processed, audio float64) float64 {
	if processed < 0 || audio <= 0 {
		return 0
	}
	return min(processed/audio, 1)
}

// smoothETA blends a raw ETA with the previous one (counted down by the
// time since it was computed), so one fast or slow tick does not make the
// "about N min left" text jump.
func smoothETA(prev float64, prevAt time.Time, raw float64, now time.Time) float64 {
	if prevAt.IsZero() || prev <= 0 {
		return raw
	}
	decayed := max(prev-now.Sub(prevAt).Seconds(), 0)
	return etaSmoothing*raw + (1-etaSmoothing)*decayed
}

// expectedSpeed is this hive's measured speed for provider/model, else the
// provider's typical speed. Measured speeds fold the provider's overhead
// into the real-time factor, so they carry none of their own.
func expectedSpeed(store *TranscriptStore, provider, model string) transcribe.Speed {
	if store != nil {
		if m, ok := store.Speed(provider, model); ok {
			return transcribe.Speed{RTF: m.RTF}
		}
	}
	return transcribe.TypicalSpeed(provider)
}

// txIsLive reports whether a transcription job for the item is in the queue.
func (s *Service) txIsLive(source, id string) bool {
	var st TranscriptionState
	return s.txLive(source, id, &st)
}
