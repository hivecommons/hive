package whisperbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// DefaultEngineDir holds whisper-cli and ffmpeg in the hive image.
	DefaultEngineDir = "/opt/hive-whisper/bin"
	// DefaultMemoryMB caps each engine's address space (RLIMIT_AS). base.en
	// needs about 400 MB; small about 1 GB.
	DefaultMemoryMB = 3072
	// wavHeaderBytes / wavBytesPerSecond turn the 16 kHz mono s16le WAV size
	// into a duration.
	wavHeaderBytes    = 44
	wavBytesPerSecond = 16000 * 2
	// stageTimeoutBase / stageTimeoutRTF bound the transcribe stage once the
	// audio length is known: base + RTF x audio.
	stageTimeoutBase = 60 * time.Second
	stageTimeoutRTF  = 4
	// decodeTimeoutShare is the part of the whole timeout decoding may use.
	decodeTimeoutShare = 3
	// cpuLimitMargin is added to RLIMIT_CPU (seconds) on top of timeout x
	// threads, so the wall-clock timeout fires first in normal operation.
	cpuLimitMargin = 60
	// maxOpenFiles / niceness for the engines.
	maxOpenFiles = 64
	niceness     = "10"
	// maxWAVBytes caps the decoded WAV (RLIMIT_FSIZE): four hours of audio.
	maxWAVBytes = 4 * 3600 * wavBytesPerSecond
	// engineWaitDelay bounds how long a killed engine's pipes may linger.
	engineWaitDelay = 5 * time.Second
	// modelFD is the descriptor the hive passes the model on.
	modelFD = 3
	// dirMode is the work dir mode.
	dirMode = 0o700
)

// Options configure one sandbox run.
type Options struct {
	// ModelPath is the ggml model; "" reads it from descriptor ModelFD.
	ModelPath string
	// ModelFD is the descriptor the model arrives on (0 means 3, where the
	// hive passes it).
	ModelFD int
	// EngineDir holds whisper-cli and ffmpeg.
	EngineDir string
	// Threads for whisper-cli (>= 1).
	Threads int
	// Language is a whisper language code or "auto".
	Language string
	// MaxBytes caps the audio read from stdin (0 disables).
	MaxBytes int64
	// Timeout bounds the whole run.
	Timeout time.Duration
	// MemoryMB caps each engine's address space; 0 means DefaultMemoryMB.
	MemoryMB int
	// WorkRoot is where the private work dir is created ("" = os.TempDir).
	WorkRoot string
	// Limits wraps the engines in prlimit and nice (production: true).
	Limits bool
	// NoNetwork runs the engines in an empty network namespace when the
	// kernel allows it (production: true).
	NoNetwork bool
	// OnWorkDir, when set, is called with the work dir once it exists (tests).
	OnWorkDir func(string)
}

// Error is a sandbox failure with its exit code.
type Error struct {
	Code int
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func fail(code int, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Run executes one sandboxed transcription: stdin is the recording, stdout
// receives the whisper.cpp JSON transcript, stderr the protocol lines and
// engine diagnostics. The work dir is removed whatever happens.
func Run(ctx context.Context, o Options, stdin io.Reader, stdout, stderr io.Writer) error {
	if o.Threads < 1 {
		o.Threads = 1
	}
	if o.EngineDir == "" {
		o.EngineDir = DefaultEngineDir
	}
	if o.MemoryMB <= 0 {
		o.MemoryMB = DefaultMemoryMB
	}
	if o.Timeout <= 0 {
		return fail(ExitUsage, "a timeout is required")
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	syscall.Umask(0o077)
	work, err := os.MkdirTemp(o.WorkRoot, "hive-whisper-")
	if err != nil {
		return fail(ExitSetup, "work dir: %v", err)
	}
	defer os.RemoveAll(work)
	if err := os.Chmod(work, dirMode); err != nil {
		return fail(ExitSetup, "work dir mode: %v", err)
	}
	if o.OnWorkDir != nil {
		o.OnWorkDir(work)
	}
	emit := func(kv ...string) { fmt.Fprintln(stderr, FormatEvent(kv...)) }

	model, extra, err := o.model()
	if err != nil {
		return err
	}
	if extra != nil {
		defer extra.Close()
	}
	prefix := o.isolationPrefix(ctx)
	if len(prefix) > 0 {
		emit(keyIsolation, "netns")
	} else {
		emit(keyIsolation, "none")
	}

	input := filepath.Join(work, "input")
	if err := copyCapped(input, stdin, o.MaxBytes); err != nil {
		return err
	}

	emit(keyPhase, PhaseDecoding)
	wav := filepath.Join(work, "audio.wav")
	decodeCtx, decodeCancel := context.WithTimeout(ctx, o.Timeout/decodeTimeoutShare)
	err = o.engine(decodeCtx, prefix, nil, nil, stderr, "ffmpeg",
		"-nostdin", "-hide_banner", "-loglevel", "error", "-i", input,
		"-vn", "-sn", "-dn", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-f", "wav", wav)
	decodeTimedOut := decodeCtx.Err() != nil
	decodeCancel()
	if err != nil {
		return stageError(ctx.Err() != nil || decodeTimedOut, ExitDecode, "decode", err)
	}
	_ = os.Remove(input)
	info, err := os.Stat(wav)
	if err != nil || info.Size() <= wavHeaderBytes {
		return fail(ExitDecode, "decode produced no audio")
	}
	audio := float64(info.Size()-wavHeaderBytes) / wavBytesPerSecond
	emit(keyPhase, PhaseTranscribing, keyAudio, formatSeconds(audio))

	stageCtx, stageCancel := context.WithTimeout(ctx, stageTimeoutBase+time.Duration(audio*stageTimeoutRTF*float64(time.Second)))
	defer stageCancel()
	out := filepath.Join(work, "out")
	lang := strings.TrimSpace(o.Language)
	if lang == "" {
		lang = "auto"
	}
	// Segment lines on stdout carry transcript text: they feed progress and
	// are never forwarded. Unrecognised stderr lines are diagnostics.
	prog := &progressState{emit: emit, audio: audio}
	outW, errW := &lineWriter{state: prog}, &lineWriter{state: prog, diag: stderr}
	err = o.engine(stageCtx, prefix, extra, outW, errW, "whisper-cli",
		"-m", model, "-f", wav, "-t", strconv.Itoa(o.Threads), "-l", lang,
		"-oj", "-of", out, "-pp", "-sns")
	outW.flush()
	errW.flush()
	if err != nil {
		return stageError(ctx.Err() != nil || stageCtx.Err() != nil, ExitTranscribe, "transcribe", err)
	}
	emit(keyPhase, PhaseFinalizing)
	f, err := os.Open(out + ".json")
	if err != nil {
		return fail(ExitTranscribe, "transcribe produced no transcript")
	}
	defer f.Close()
	if _, err := io.Copy(stdout, f); err != nil {
		return fail(ExitTranscribe, "write transcript: %v", err)
	}
	return nil
}

// stageError maps an engine failure: a deadline is a timeout, else code.
// timedOut must be read before the stage's context is cancelled.
func stageError(timedOut bool, code int, what string, err error) error {
	if timedOut {
		return fail(ExitTimeout, "%s timed out", what)
	}
	return fail(code, "%s failed: %v", what, err)
}

// model returns the model path to hand whisper-cli and, for the descriptor
// form, the file to pass on as the child's descriptor 3.
func (o Options) model() (string, *os.File, error) {
	if o.ModelPath != "" {
		if _, err := os.Stat(o.ModelPath); err != nil {
			return "", nil, fail(ExitSetup, "model: %v", err)
		}
		return o.ModelPath, nil, nil
	}
	fd := o.ModelFD
	if fd <= 0 {
		fd = modelFD
	}
	f := os.NewFile(uintptr(fd), "model")
	if f == nil {
		return "", nil, fail(ExitSetup, "no model on descriptor %d", fd)
	}
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return "", nil, fail(ExitSetup, "descriptor %d is not a model file", fd)
	}
	// whisper-cli receives the descriptor as its own fd 3 (ExtraFiles).
	return "/dev/fd/" + strconv.Itoa(modelFD), f, nil
}

// isolationPrefix returns the argv prefix that runs a command with no
// network (a fresh user + network namespace), or nil when unavailable.
func (o Options) isolationPrefix(ctx context.Context) []string {
	if !o.NoNetwork {
		return nil
	}
	prefix := NetIsolationPrefix()
	probe := exec.CommandContext(ctx, prefix[0], append(prefix[1:], "true")...)
	probe.Env = []string{}
	if probe.Run() != nil {
		return nil
	}
	return prefix
}

// NetIsolationPrefix is util-linux unshare into a new user namespace (the
// same uid mapped) with an empty network namespace: only loopback, down.
func NetIsolationPrefix() []string {
	return []string{"unshare", "--user", "--map-current-user", "--net", "--"}
}

// LimitPrefix is the prlimit + nice argv for one engine run.
func LimitPrefix(memoryMB, threads int, timeout time.Duration) []string {
	cpu := int(timeout.Seconds())*max(threads, 1) + cpuLimitMargin
	return []string{
		"prlimit",
		"--as=" + strconv.FormatInt(int64(memoryMB)<<20, 10),
		"--cpu=" + strconv.Itoa(cpu),
		"--fsize=" + strconv.Itoa(maxWAVBytes),
		"--nofile=" + strconv.Itoa(maxOpenFiles),
		"--core=0",
		"--", "nice", "-n", niceness,
	}
}

// engine runs one engine binary from EngineDir with the isolation and limit
// prefixes, an empty environment and the model descriptor when given.
func (o Options) engine(ctx context.Context, prefix []string, model *os.File, stdout, stderr io.Writer, name string, args ...string) error {
	argv := append([]string{}, prefix...)
	if o.Limits {
		deadline, ok := ctx.Deadline()
		left := o.Timeout
		if ok {
			left = time.Until(deadline)
		}
		argv = append(argv, LimitPrefix(o.MemoryMB, o.Threads, left)...)
	}
	argv = append(argv, filepath.Join(o.EngineDir, name))
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	if model != nil {
		cmd.ExtraFiles = []*os.File{model}
	}
	if stdout == nil {
		stdout = io.Discard
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = engineWaitDelay
	cmd.Cancel = func() error {
		// Kill the whole group: prlimit, nice and the engine.
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	return cmd.Run()
}

// copyCapped writes stdin to path, failing past limit bytes.
func copyCapped(path string, r io.Reader, limit int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(ExitSetup, "input: %v", err)
	}
	defer f.Close()
	src := r
	if limit > 0 {
		src = io.LimitReader(r, limit+1)
	}
	n, err := io.Copy(f, src)
	if err != nil {
		return fail(ExitSetup, "read audio: %v", err)
	}
	if limit > 0 && n > limit {
		return fail(ExitTooLarge, "audio exceeds %d bytes", limit)
	}
	if n == 0 {
		return fail(ExitDecode, "no audio on stdin")
	}
	return nil
}

var (
	// progressRe matches whisper-cli -pp lines: "...: progress =  42%".
	progressRe = regexp.MustCompile(`progress\s*=\s*(\d+)%`)
	// segmentRe matches whisper-cli segment lines: "[00:01:02.000 --> 00:01:05.500]".
	segmentRe = regexp.MustCompile(`^\[(\d+):(\d+):(\d+)[.,](\d+)\s+-->\s+(\d+):(\d+):(\d+)[.,](\d+)\]`)
)

// progressState turns whisper-cli stdout segment lines and stderr progress
// lines into protocol events, never going backwards.
type progressState struct {
	mu    sync.Mutex
	emit  func(...string)
	audio float64
	best  float64
}

// lineWriter splits one engine stream into lines for progressState; other
// lines go to diag (nil drops them).
type lineWriter struct {
	state *progressState
	diag  io.Writer
	buf   []byte
}

func (w *lineWriter) Write(b []byte) (int, error) {
	w.buf = append(w.buf, b...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		w.line(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	return len(b), nil
}

func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.line(string(w.buf))
		w.buf = nil
	}
}

func (w *lineWriter) line(l string) {
	l = strings.TrimSpace(l)
	p := w.state
	p.mu.Lock()
	defer p.mu.Unlock()
	if m := segmentRe.FindStringSubmatch(l); m != nil {
		end := clock(m[5], m[6], m[7], m[8])
		if end > p.best && p.audio > 0 {
			p.best = min(end, p.audio)
			p.emit(keyProcessed, formatSeconds(p.best))
		}
		return
	}
	if m := progressRe.FindStringSubmatch(l); m != nil {
		pct, _ := strconv.Atoi(m[1])
		if processed := float64(min(pct, 100)) / 100 * p.audio; processed > p.best {
			p.best = processed
			p.emit(keyProgress, strconv.FormatFloat(float64(pct)/100, 'f', 2, 64), keyProcessed, formatSeconds(processed))
		}
		return
	}
	if l != "" && w.diag != nil {
		fmt.Fprintln(w.diag, l)
	}
}

func clock(h, m, s, frac string) float64 {
	hv, _ := strconv.Atoi(h)
	mv, _ := strconv.Atoi(m)
	sv, _ := strconv.Atoi(s)
	fv, _ := strconv.ParseFloat("0."+frac, 64)
	return float64(hv*3600+mv*60+sv) + fv
}

// Main is the hive-whisper entry point; it returns the exit code.
func Main(ctx context.Context, o Options, stdin io.Reader, stdout, stderr io.Writer) int {
	err := Run(ctx, o, stdin, stdout, stderr)
	if err == nil {
		return ExitOK
	}
	fmt.Fprintln(stderr, errorLine("%v", err))
	var se *Error
	if errors.As(err, &se) {
		return se.Code
	}
	return ExitTranscribe
}

// ScanEvents reads r line by line, calling onEvent for protocol lines and
// onOther for everything else, until EOF.
func ScanEvents(r io.Reader, onEvent func(Event), onOther func(string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		if ev, ok := ParseEvent(sc.Text()); ok {
			onEvent(ev)
		} else if onOther != nil {
			onOther(sc.Text())
		}
	}
}
