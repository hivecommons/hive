// Package whisperbox is the sandbox side of the built-in whisper provider:
// the hive-whisper command that decodes one untrusted recording with FFmpeg
// and transcribes it with whisper.cpp, and the line protocol it reports
// progress with.
//
// The hive runs hive-whisper as the dedicated hive-transcribe uid (through
// su-exec) with an empty environment, the audio on stdin, the ggml model as
// an inherited read-only file descriptor, and reads the whisper.cpp JSON
// transcript from stdout. hive-whisper keeps everything in a private 0700
// work dir that it removes on exit, runs both engines under rlimits (address
// space, CPU seconds, file size, open files, no core dumps), a lower CPU
// priority and a wall-clock timeout, and, where the kernel allows it, in an
// empty network namespace.
package whisperbox

import (
	"fmt"
	"strconv"
	"strings"
)

// LinePrefix starts every protocol line hive-whisper writes to stderr. Other
// stderr lines are engine diagnostics.
const LinePrefix = "HIVE-WHISPER "

// Protocol keys.
const (
	keyPhase     = "phase"
	keyAudio     = "audio_seconds"
	keyProgress  = "progress"
	keyProcessed = "processed_seconds"
	keyIsolation = "isolation"
	keyError     = "error"
)

// Phases reported with keyPhase (the same names as transcribe.Phase*).
const (
	PhaseDecoding     = "decoding"
	PhaseTranscribing = "transcribing"
	PhaseFinalizing   = "finalizing"
)

// Exit codes of hive-whisper.
const (
	ExitOK         = 0
	ExitUsage      = 2
	ExitTooLarge   = 3
	ExitTimeout    = 4
	ExitDecode     = 5
	ExitTranscribe = 6
	ExitSetup      = 7
)

// Event is one parsed protocol line. Numbers are < 0 when absent.
type Event struct {
	Phase            string
	AudioSeconds     float64
	Progress         float64
	ProcessedSeconds float64
	Isolation        string
	Error            string
}

// FormatEvent renders one key=value protocol line (without newline). Error
// text is last and runs to the end of the line.
func FormatEvent(kv ...string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(LinePrefix))
	for i := 0; i+1 < len(kv); i += 2 {
		v := strings.ReplaceAll(kv[i+1], "\n", " ")
		if kv[i] != keyError {
			v = strings.ReplaceAll(v, " ", "_")
		}
		b.WriteString(" " + kv[i] + "=" + v)
	}
	return b.String()
}

// ParseEvent parses a protocol line; ok is false for any other line.
func ParseEvent(line string) (Event, bool) {
	rest, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), LinePrefix)
	if !ok {
		return Event{}, false
	}
	ev := Event{AudioSeconds: -1, Progress: -1, ProcessedSeconds: -1}
	if i := strings.Index(rest, keyError+"="); i >= 0 {
		ev.Error = strings.TrimSpace(rest[i+len(keyError)+1:])
		rest = rest[:i]
	}
	for _, f := range strings.Fields(rest) {
		k, v, found := strings.Cut(f, "=")
		if !found {
			continue
		}
		switch k {
		case keyPhase:
			ev.Phase = v
		case keyIsolation:
			ev.Isolation = v
		case keyAudio:
			ev.AudioSeconds = parseNonNegative(v)
		case keyProgress:
			ev.Progress = parseNonNegative(v)
		case keyProcessed:
			ev.ProcessedSeconds = parseNonNegative(v)
		}
	}
	return ev, true
}

func parseNonNegative(v string) float64 {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return -1
	}
	return f
}

func formatSeconds(s float64) string { return strconv.FormatFloat(s, 'f', 2, 64) }

// errorLine renders a fatal error line.
func errorLine(format string, args ...any) string {
	return FormatEvent(keyError, fmt.Sprintf(format, args...))
}
