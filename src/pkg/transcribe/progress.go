package transcribe

// Phases a transcription run reports while it works. The Inbox shows them
// on the item's progress bar.
const (
	// PhaseDownloading: the hive is fetching the audio from the source.
	PhaseDownloading = "downloading"
	// PhaseDecoding: the audio is being converted for the engine (whisper).
	PhaseDecoding = "decoding"
	// PhaseTranscribing: the provider is turning speech into text.
	PhaseTranscribing = "transcribing"
	// PhaseFinalizing: the transcript is being parsed and stored.
	PhaseFinalizing = "finalizing"
)

// Progress is one progress report from a provider. Processed is the audio
// already transcribed, when the provider can tell (whisper.cpp prints it as
// it goes); a negative Processed means the provider cannot measure it and
// the Inbox estimates progress from the provider's typical speed instead.
type Progress struct {
	Phase string
	// ProcessedSeconds is the audio transcribed so far; < 0 when unknown.
	ProcessedSeconds float64
	// AudioSeconds is the total audio length, when the provider learnt it
	// (for example from the decoded file); 0 when unknown.
	AudioSeconds float64
}

// Report sends p to a.Progress when one is set.
func (a Audio) Report(p Progress) {
	if a.Progress != nil {
		a.Progress(p)
	}
}

// ReportPhase reports a phase change without a measured position.
func (a Audio) ReportPhase(phase string) {
	a.Report(Progress{Phase: phase, ProcessedSeconds: -1})
}

// Speed is how long a provider typically takes: Overhead seconds plus RTF
// seconds of processing per second of audio (the real-time factor; 0.5 means
// a 10-minute recording takes 5 minutes). The Inbox uses it for the time
// estimate before, or instead of, measured progress.
type Speed struct {
	RTF      float64
	Overhead float64
}

// Seconds is the expected wall-clock time for audio seconds of recording.
func (s Speed) Seconds(audio float64) float64 { return s.Overhead + s.RTF*audio }

// Typical speeds by provider, used until the hive has measured its own.
// They are deliberately conservative so the first estimate runs long rather
// than short.
const (
	// deepgramTypicalRTF: Deepgram pre-recorded runs far faster than real
	// time; most of the wait is the upload.
	deepgramTypicalRTF      = 0.05
	deepgramTypicalOverhead = 10
	// whisperTypicalRTF: whisper.cpp base.en on 2 to 4 vCPU. The first run on
	// a hive uses this, later runs use the measured value.
	whisperTypicalRTF      = 0.6
	whisperTypicalOverhead = 5
	// geminiTypicalRTF: one generateContent call with inline audio.
	geminiTypicalRTF      = 0.15
	geminiTypicalOverhead = 15
	// defaultTypicalRTF covers providers without a known speed.
	defaultTypicalRTF      = 0.5
	defaultTypicalOverhead = 10
)

// Provider names used across adapters. The whisper and gemini adapters
// report these; DeepgramName lives with its adapter.
const (
	WhisperName = "whisper"
	GeminiName  = "gemini"
)

// TypicalSpeed returns the conservative default speed of a provider.
func TypicalSpeed(provider string) Speed {
	switch provider {
	case DeepgramName:
		return Speed{RTF: deepgramTypicalRTF, Overhead: deepgramTypicalOverhead}
	case WhisperName:
		return Speed{RTF: whisperTypicalRTF, Overhead: whisperTypicalOverhead}
	case GeminiName:
		return Speed{RTF: geminiTypicalRTF, Overhead: geminiTypicalOverhead}
	case FakeName:
		return Speed{}
	}
	return Speed{RTF: defaultTypicalRTF, Overhead: defaultTypicalOverhead}
}
