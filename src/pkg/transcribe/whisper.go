package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/transcribe/whisperbox"
)

const (
	// DefaultWhisperSandbox is the hive-whisper launcher in the hive image.
	DefaultWhisperSandbox = "/usr/local/bin/hive-whisper"
	// DefaultWhisperRunAs is the low-privilege uid the sandbox runs as.
	DefaultWhisperRunAs = "hive-transcribe"
	// WhisperRunAsSelf runs the sandbox as the hive's own uid (no su-exec):
	// weaker isolation, for development hosts without the sandbox user.
	WhisperRunAsSelf = "self"
	// whisperTimeoutBase / whisperTimeoutRTF: the wall-clock limit of a run
	// is base + RTF x the recording length (a hive at a quarter of real
	// time speed still finishes).
	whisperTimeoutBase = 3 * time.Minute
	whisperTimeoutRTF  = 4
	// whisperUnknownDuration is assumed when the source reports no length.
	whisperUnknownDuration = 2 * time.Hour
	// whisperWaitDelay is how long Wait lingers for the sandbox's pipes
	// after cancellation (it enforces its own timeout as another uid).
	whisperWaitDelay = 30 * time.Second
	// whisperMaxOutputBytes bounds the JSON transcript read into memory.
	whisperMaxOutputBytes = 64 << 20
	// whisperDiagLines keeps the last engine diagnostics for errors.
	whisperDiagLines = 5
)

// Whisper transcribes on the hive itself with whisper.cpp in the
// hive-whisper sandbox. No account, and the audio never leaves the spoke:
// the hive streams the downloaded recording into the sandbox's stdin.
type Whisper struct {
	// Models finds or downloads the ggml model.
	Models *WhisperModelStore
	// Model is the model name (DefaultWhisperModel when empty).
	Model string
	// Threads for whisper-cli (>= 1).
	Threads int
	// Language is a language code or auto.
	Language string
	// MaxBytes caps the recording handed to the sandbox.
	MaxBytes int64
	// MemoryMB caps each engine's address space (0: sandbox default).
	MemoryMB int
	// Sandbox is the hive-whisper path ("" = DefaultWhisperSandbox).
	Sandbox string
	// RunAs is the sandbox user ("" = DefaultWhisperRunAs, "self" = no
	// su-exec). SuExec is the su-exec path ("" = looked up on PATH).
	RunAs  string
	SuExec string
	// ExtraArgs are appended to the sandbox flags (tests).
	ExtraArgs []string
}

// Name implements Transcriber.
func (w *Whisper) Name() string { return WhisperName }

func (w *Whisper) model() string {
	if m := strings.TrimSpace(w.Model); m != "" {
		return m
	}
	return DefaultWhisperModel
}

// WhisperTimeout is the wall-clock limit for audio of length d.
func WhisperTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		d = whisperUnknownDuration
	}
	return whisperTimeoutBase + time.Duration(whisperTimeoutRTF*float64(d))
}

// argv builds the sandbox command line (su-exec user hive-whisper flags).
func (w *Whisper) argv(timeout time.Duration, lang string) ([]string, error) {
	sandbox := w.Sandbox
	if sandbox == "" {
		sandbox = DefaultWhisperSandbox
	}
	flags := []string{sandbox,
		"-threads", strconv.Itoa(max(w.Threads, 1)),
		"-lang", whisperLanguage(lang),
		"-max-bytes", strconv.FormatInt(w.MaxBytes, 10),
		"-timeout", timeout.String(),
	}
	if w.MemoryMB > 0 {
		flags = append(flags, "-memory-mb", strconv.Itoa(w.MemoryMB))
	}
	flags = append(flags, w.ExtraArgs...)
	runAs := w.RunAs
	if runAs == "" {
		runAs = DefaultWhisperRunAs
	}
	if runAs == WhisperRunAsSelf {
		return flags, nil
	}
	suexec := w.SuExec
	if suexec == "" {
		p, err := exec.LookPath("su-exec")
		if err != nil {
			return nil, fmt.Errorf("whisper: the sandbox needs su-exec to run as %s: %w", runAs, err)
		}
		suexec = p
	}
	return append([]string{suexec, runAs}, flags...), nil
}

// whisperLanguage maps a BCP-47 hint to whisper's two-letter code.
func whisperLanguage(lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if lang == "" || lang == "auto" {
		return "auto"
	}
	base, _, _ := strings.Cut(lang, "-")
	return base
}

// Transcribe implements Transcriber.
func (w *Whisper) Transcribe(ctx context.Context, a Audio) (Result, error) {
	if w.Models == nil {
		return Result{}, errors.New("whisper: no model store")
	}
	if a.Open == nil {
		return Result{}, errors.New("whisper: no audio")
	}
	a.ReportPhase(PhaseDownloading)
	modelPath, err := w.Models.Ensure(ctx, w.model())
	if err != nil {
		return Result{}, err
	}
	model, err := os.Open(modelPath)
	if err != nil {
		return Result{}, fmt.Errorf("whisper: open model: %w", err)
	}
	defer model.Close()
	timeout := WhisperTimeout(a.Duration)
	argv, err := w.argv(timeout, a.Lang)
	if err != nil {
		return Result{}, err
	}
	rc, err := a.Open(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("whisper: open audio: %w", err)
	}
	defer rc.Close()
	body := &bodyReader{r: rc}

	// The sandbox runs as another uid and enforces its own timeout; the
	// context only stops waiting for it.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// Nothing from the hive's environment (tokens, keys) reaches the sandbox.
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LC_ALL=C"}
	cmd.ExtraFiles = []*os.File{model}
	cmd.Stdin = body
	var stdout limitedBuffer
	stdout.limit = whisperMaxOutputBytes
	cmd.Stdout = &stdout
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, fmt.Errorf("whisper: %w", err)
	}
	cmd.WaitDelay = whisperWaitDelay
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("whisper: start sandbox: %w", err)
	}
	var (
		mu      sync.Mutex
		audio   float64
		failure string
		diag    []string
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		whisperbox.ScanEvents(stderr, func(ev whisperbox.Event) {
			mu.Lock()
			if ev.AudioSeconds > 0 {
				audio = ev.AudioSeconds
			}
			if ev.Error != "" {
				failure = ev.Error
			}
			total := audio
			mu.Unlock()
			switch {
			case ev.Phase != "":
				a.Report(Progress{Phase: ev.Phase, ProcessedSeconds: -1, AudioSeconds: max(ev.AudioSeconds, 0)})
			case ev.ProcessedSeconds >= 0:
				a.Report(Progress{Phase: PhaseTranscribing, ProcessedSeconds: ev.ProcessedSeconds, AudioSeconds: total})
			}
		}, func(line string) {
			mu.Lock()
			diag = append(diag, line)
			if len(diag) > whisperDiagLines {
				diag = diag[len(diag)-whisperDiagLines:]
			}
			mu.Unlock()
		})
	}()
	werr := cmd.Wait()
	<-done
	if errors.Is(body.err, ErrTooLarge) {
		return Result{}, ErrTooLarge
	}
	if werr != nil {
		var exitErr *exec.ExitError
		code := -1
		if errors.As(werr, &exitErr) {
			code = exitErr.ExitCode()
		}
		mu.Lock()
		msg := failure
		if msg == "" && len(diag) > 0 {
			msg = strings.Join(diag, "; ")
		}
		mu.Unlock()
		switch {
		case code == whisperbox.ExitTooLarge:
			return Result{}, ErrTooLarge
		case ctx.Err() != nil:
			return Result{}, fmt.Errorf("whisper: stopped: %w", ctx.Err())
		case code == whisperbox.ExitTimeout:
			return Result{}, fmt.Errorf("whisper: timed out after %s (the hive's CPU is too slow for this recording, or it is busy)", timeout.Round(time.Second))
		case msg != "":
			return Result{}, fmt.Errorf("whisper: %s", msg)
		}
		return Result{}, fmt.Errorf("whisper: sandbox failed: %v", werr)
	}
	if stdout.over {
		return Result{}, errors.New("whisper: transcript too large")
	}
	res, err := ParseWhisperJSON(stdout.Bytes(), w.model())
	if err != nil {
		return Result{}, err
	}
	mu.Lock()
	if audio > 0 {
		res.Duration = seconds(audio)
	}
	mu.Unlock()
	return res, nil
}

// limitedBuffer collects up to limit bytes and records an overflow.
type limitedBuffer struct {
	bytes.Buffer
	limit int
	over  bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		b.over = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// whisperJSON is the part of whisper-cli -oj output the hive reads.
type whisperJSON struct {
	Result struct {
		Language string `json:"language"`
	} `json:"result"`
	Transcription []struct {
		Offsets struct {
			From int64 `json:"from"`
			To   int64 `json:"to"`
		} `json:"offsets"`
		Text string `json:"text"`
	} `json:"transcription"`
}

// whisperNoise are whisper's non-speech annotations, dropped from segments.
var whisperNoise = map[string]bool{"[BLANK_AUDIO]": true, "[MUSIC]": true, "[NOISE]": true, "[SILENCE]": true, "(SILENCE)": true}

// ParseWhisperJSON converts whisper-cli JSON into segments ([mm:ss] text,
// no speakers: whisper does not diarize).
func ParseWhisperJSON(data []byte, model string) (Result, error) {
	var parsed whisperJSON
	if err := json.Unmarshal(data, &parsed); err != nil {
		return Result{}, fmt.Errorf("whisper: decode transcript: %w", err)
	}
	out := Result{Provider: WhisperName, Model: model, Language: parsed.Result.Language}
	var last int64
	for _, s := range parsed.Transcription {
		text := strings.TrimSpace(s.Text)
		if text == "" || whisperNoise[strings.ToUpper(text)] {
			continue
		}
		out.Segments = append(out.Segments, Segment{Start: time.Duration(s.Offsets.From) * time.Millisecond, Text: text})
		last = max(last, s.Offsets.To)
	}
	out.Duration = time.Duration(last) * time.Millisecond
	return out, nil
}
