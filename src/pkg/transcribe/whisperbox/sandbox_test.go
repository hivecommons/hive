package whisperbox

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// tenSecondWAV is the byte size of 10 s of 16 kHz mono s16le WAV.
const tenSecondWAV = wavHeaderBytes + 10*wavBytesPerSecond

// fakeEngines writes fake ffmpeg and whisper-cli scripts into a temp dir:
// ffmpeg writes a 10-second WAV to its last argument; whisper-cli prints
// segment and progress lines, writes its environment to envFile, then
// writes <-of>.json (or sleeps for sleepSecs first).
func fakeEngines(t *testing.T, sleepSecs int) (dir, envFile string) {
	t.Helper()
	dir = t.TempDir()
	envFile = filepath.Join(t.TempDir(), "env.txt")
	ffmpeg := "#!/bin/sh\nfor a; do last=$a; done\nhead -c " + itoa(tenSecondWAV) + " /dev/zero > \"$last\"\n"
	whisper := "#!/bin/sh\n" +
		"env > " + envFile + "\n" +
		"while [ $# -gt 0 ]; do case $1 in -of) of=$2; shift;; esac; shift; done\n" +
		"echo 'whisper_print_progress_callback: progress =  40%' >&2\n" +
		"echo '[00:00:00.000 --> 00:00:04.000]   hello hive'\n" +
		"echo 'some engine diagnostic' >&2\n" +
		"echo '[00:00:04.000 --> 00:00:08.500]   second line'\n" +
		"sleep " + itoa(sleepSecs) + "\n" +
		"printf '{\"result\":{\"language\":\"en\"},\"transcription\":[{\"offsets\":{\"from\":0,\"to\":4000},\"text\":\" hello hive\"}]}' > \"$of.json\"\n"
	for name, body := range map[string]string{"ffmpeg": ffmpeg, "whisper-cli": whisper} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir, envFile
}

func itoa(n int) string { return strconv.Itoa(n) }

func testModel(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ggml-test.bin")
	if err := os.WriteFile(p, []byte("model"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSandboxRunsBothEnginesInAPrivateDir(t *testing.T) {
	t.Setenv("HIVE_SECRET_FOR_TEST", "s3cret-token")
	engines, envFile := fakeEngines(t, 0)
	root := t.TempDir()
	var workDir string
	var mode os.FileMode
	o := Options{ModelPath: testModel(t), EngineDir: engines, Threads: 2, Language: "en", Timeout: 30 * time.Second, WorkRoot: root,
		OnWorkDir: func(d string) {
			workDir = d
			if info, err := os.Stat(d); err == nil {
				mode = info.Mode().Perm()
			}
		}}
	var stdout, stderr bytes.Buffer
	if code := Main(context.Background(), o, strings.NewReader("RECORDING"), &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if mode != dirMode || !strings.HasPrefix(workDir, root) {
		t.Fatalf("work dir %s mode %o", workDir, mode)
	}
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Fatalf("work dir not removed: %v", err)
	}
	if !strings.Contains(stdout.String(), `hello hive`) {
		t.Fatalf("stdout = %q", stdout.String())
	}
	var events []Event
	ScanEvents(&stderr, func(ev Event) { events = append(events, ev) }, nil)
	var phases []string
	var audio, best float64
	for _, ev := range events {
		if ev.Phase != "" {
			phases = append(phases, ev.Phase)
		}
		if ev.AudioSeconds > 0 {
			audio = ev.AudioSeconds
		}
		best = max(best, ev.ProcessedSeconds)
	}
	if strings.Join(phases, ",") != "decoding,transcribing,finalizing" || audio != 10 || best != 8.5 {
		t.Fatalf("events = %+v", events)
	}
	if strings.Contains(stdout.String(), "diagnostic") {
		t.Fatal("diagnostics must not reach stdout")
	}
	env, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), "s3cret-token") || strings.Contains(string(env), "HIVE_SECRET") {
		t.Fatalf("engine inherited the caller's environment: %s", env)
	}
}

func TestSandboxDiagnosticsAndTranscriptText(t *testing.T) {
	engines, _ := fakeEngines(t, 0)
	var stdout, stderr bytes.Buffer
	o := Options{ModelPath: testModel(t), EngineDir: engines, Timeout: 30 * time.Second, WorkRoot: t.TempDir()}
	if code := Main(context.Background(), o, strings.NewReader("RECORDING"), &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "some engine diagnostic") {
		t.Fatalf("stderr diagnostics dropped: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "second line") {
		t.Fatal("transcript text from segment lines must not be forwarded to stderr")
	}
}

func TestSandboxTimeoutKillsAndCleansUp(t *testing.T) {
	engines, _ := fakeEngines(t, 30)
	var workDir string
	o := Options{ModelPath: testModel(t), EngineDir: engines, Timeout: 2 * time.Second, WorkRoot: t.TempDir(), OnWorkDir: func(d string) { workDir = d }}
	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := Main(context.Background(), o, strings.NewReader("RECORDING"), &stdout, &stderr)
	if code != ExitTimeout {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if el := time.Since(start); el > 15*time.Second {
		t.Fatalf("timeout took %v: the engine was not killed", el)
	}
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Fatalf("work dir left behind after a timeout: %v", err)
	}
	if !strings.Contains(stderr.String(), "error=transcribe timed out") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestSandboxRefusesOversizeAndEmptyInput(t *testing.T) {
	engines, _ := fakeEngines(t, 0)
	o := Options{ModelPath: testModel(t), EngineDir: engines, Timeout: 10 * time.Second, WorkRoot: t.TempDir(), MaxBytes: 4}
	var out, errb bytes.Buffer
	if code := Main(context.Background(), o, strings.NewReader("12345"), &out, &errb); code != ExitTooLarge {
		t.Fatalf("oversize exit %d", code)
	}
	if code := Main(context.Background(), o, strings.NewReader(""), &out, &errb); code != ExitDecode {
		t.Fatalf("empty exit %d", code)
	}
	o.Timeout = 0
	if code := Main(context.Background(), o, strings.NewReader("1"), &out, &errb); code != ExitUsage {
		t.Fatalf("no timeout exit %d", code)
	}
	o.Timeout, o.ModelPath = time.Second, filepath.Join(t.TempDir(), "missing.bin")
	if code := Main(context.Background(), o, strings.NewReader("1"), &out, &errb); code != ExitSetup {
		t.Fatalf("missing model exit %d", code)
	}
}

func TestIsolationAndLimitArgv(t *testing.T) {
	if got := strings.Join(NetIsolationPrefix(), " "); got != "unshare --user --map-current-user --net --" {
		t.Fatalf("net isolation = %q", got)
	}
	got := strings.Join(LimitPrefix(1024, 2, 100*time.Second), " ")
	for _, want := range []string{"prlimit", "--as=1073741824", "--cpu=260", "--fsize=", "--nofile=64", "--core=0", "-- nice -n 10"} {
		if !strings.Contains(got, want) {
			t.Errorf("limit prefix %q missing %q", got, want)
		}
	}
	if (Options{}).isolationPrefix(context.Background()) != nil {
		t.Fatal("NoNetwork off means no prefix")
	}
}

func TestProtocolRoundTrip(t *testing.T) {
	line := FormatEvent(keyPhase, PhaseTranscribing, keyAudio, "12.50", keyError, "decode failed: bad\nframe")
	ev, ok := ParseEvent(line)
	if !ok || ev.Phase != PhaseTranscribing || ev.AudioSeconds != 12.5 || ev.Error != "decode failed: bad frame" || ev.Progress != -1 {
		t.Fatalf("round trip %q = %+v", line, ev)
	}
	if _, ok := ParseEvent("whisper_init: loading model"); ok {
		t.Fatal("non-protocol line parsed")
	}
	if ev, _ := ParseEvent(LinePrefix + "progress=abc processed_seconds=-3"); ev.Progress != -1 || ev.ProcessedSeconds != -1 {
		t.Fatalf("bad numbers = %+v", ev)
	}
}

func writeEngine(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxDecodeFailures(t *testing.T) {
	engines, _ := fakeEngines(t, 0)
	o := Options{ModelPath: testModel(t), EngineDir: engines, Timeout: 10 * time.Second, WorkRoot: t.TempDir()}
	var out, errb bytes.Buffer
	writeEngine(t, engines, "ffmpeg", "echo 'Invalid data found when processing input' >&2\nexit 1\n")
	if code := Main(context.Background(), o, strings.NewReader("AUDIO"), &out, &errb); code != ExitDecode || !strings.Contains(errb.String(), "Invalid data found") {
		t.Fatalf("decode error exit %d: %s", code, errb.String())
	}
	writeEngine(t, engines, "ffmpeg", "for a; do last=$a; done\nhead -c 10 /dev/zero > \"$last\"\n")
	if code := Main(context.Background(), o, strings.NewReader("AUDIO"), &out, &errb); code != ExitDecode {
		t.Fatalf("empty wav exit %d", code)
	}
	writeEngine(t, engines, "ffmpeg", "for a; do last=$a; done\nhead -c 32044 /dev/zero > \"$last\"\n")
	writeEngine(t, engines, "whisper-cli", "exit 0\n")
	if code := Main(context.Background(), o, strings.NewReader("AUDIO"), &out, &errb); code != ExitTranscribe {
		t.Fatalf("no transcript exit %d", code)
	}
	writeEngine(t, engines, "whisper-cli", "echo 'failed to load model' >&2\nexit 3\n")
	errb.Reset()
	if code := Main(context.Background(), o, strings.NewReader("AUDIO"), &out, &errb); code != ExitTranscribe || !strings.Contains(errb.String(), "transcribe failed") {
		t.Fatalf("whisper error exit %d: %s", code, errb.String())
	}
}

// TestSandboxUnderRealLimits runs the fake engines through prlimit, nice and
// (when the kernel allows it) an empty network namespace, as production does.
func TestSandboxUnderRealLimits(t *testing.T) {
	for _, bin := range []string{"prlimit", "nice"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available: %v", bin, err)
		}
	}
	engines, _ := fakeEngines(t, 0)
	o := Options{ModelPath: testModel(t), EngineDir: engines, Timeout: 30 * time.Second, WorkRoot: t.TempDir(), Limits: true, NoNetwork: true, MemoryMB: 512}
	var out, errb bytes.Buffer
	if code := Main(context.Background(), o, strings.NewReader("AUDIO"), &out, &errb); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "isolation=") || !strings.Contains(out.String(), "hello hive") {
		t.Fatalf("out %q err %q", out.String(), errb.String())
	}
}

func TestSandboxModelFromDescriptor(t *testing.T) {
	engines, _ := fakeEngines(t, 0)
	// whisper-cli must find the model as its own descriptor 3.
	writeEngine(t, engines, "whisper-cli", "while [ $# -gt 0 ]; do case $1 in -of) of=$2; shift;; -m) m=$2; shift;; esac; shift; done\n"+
		"[ \"$m\" = /dev/fd/3 ] && cat /dev/fd/3 | grep -q model || exit 9\nprintf '{}' > \"$of.json\"\n")
	model, err := os.Open(testModel(t))
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()
	// The sandbox owns (and closes) the descriptor it is given: hand it a dup.
	dup := func(f *os.File) int {
		fd, err := syscall.Dup(int(f.Fd()))
		if err != nil {
			t.Fatal(err)
		}
		return fd
	}
	o := Options{ModelFD: dup(model), EngineDir: engines, Timeout: 10 * time.Second, WorkRoot: t.TempDir()}
	var out, errb bytes.Buffer
	if code := Main(context.Background(), o, strings.NewReader("AUDIO"), &out, &errb); code != ExitOK {
		t.Fatalf("model by descriptor: exit %d: %s", code, errb.String())
	}
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	o.ModelFD = dup(dir)
	if code := Main(context.Background(), o, strings.NewReader("AUDIO"), &out, &errb); code != ExitSetup {
		t.Fatalf("a directory descriptor is not a model: exit %d", code)
	}
}
