package inbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hivecommons/hive/pkg/inbox/pdfbox"
	"github.com/hivecommons/hive/pkg/transcribe"
)

// UploadKindPDF is a PDF document.
const UploadKindPDF = "pdf"

// Environment overrides for the PDF sandbox.
const (
	EnvPDFSandbox  = "HIVE_PDF_SANDBOX"
	EnvPDFRunAs    = "HIVE_PDF_RUN_AS"
	EnvPDFMemoryMB = "HIVE_PDF_MEMORY_MB"
)

const (
	// pdfWaitDelay bounds how long a killed sandbox's pipes may linger.
	pdfWaitDelay = 5 * time.Second
	// pdfStderrBytes keeps the tail of the sandbox's diagnostics.
	pdfStderrBytes = 4 << 10
	// pdfStdoutSlack is allowed on top of the character cap for JSON framing.
	pdfStdoutSlack = 1 << 20
	// pdfScannedMessage is what a PDF without a text layer is refused with.
	pdfScannedMessage = "no text layer (scanned PDF); OCR not supported yet"
)

// PDFRuntime runs the hive-pdftext sandbox. The zero value is filled from
// the environment by DefaultPDFRuntime.
type PDFRuntime struct {
	// Sandbox is the hive-pdftext path.
	Sandbox string
	// RunAs is the sandbox uid (default hive-transcribe; "self" runs as the
	// hive's own uid, for development only). SuExec is the su-exec path
	// ("" = looked up on PATH).
	RunAs  string
	SuExec string
	// MemoryMB caps the sandbox address space; Timeout the whole run.
	MemoryMB int
	Timeout  time.Duration
	// MaxPages and MaxChars cap the extraction.
	MaxPages int
	MaxChars int
	// Limits wraps the sandbox in prlimit and nice; NoNetwork in an empty
	// network namespace when the kernel allows it (production: both true).
	Limits    bool
	NoNetwork bool

	isoOnce sync.Once
	iso     bool
}

// DefaultPDFRuntime reads the HIVE_PDF_* overrides.
func DefaultPDFRuntime(getenv func(string) string) *PDFRuntime {
	env := func(k, def string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		return def
	}
	mem, err := strconv.Atoi(env(EnvPDFMemoryMB, ""))
	if err != nil || mem <= 0 {
		mem = pdfbox.DefaultMemoryMB
	}
	return &PDFRuntime{
		Sandbox:   env(EnvPDFSandbox, pdfbox.DefaultSandbox),
		RunAs:     env(EnvPDFRunAs, transcribe.DefaultWhisperRunAs),
		MemoryMB:  mem,
		Timeout:   pdfbox.DefaultTimeout,
		MaxPages:  pdfbox.DefaultMaxPages,
		MaxChars:  pdfbox.DefaultMaxChars,
		Limits:    true,
		NoNetwork: true,
	}
}

// Available reports whether the sandbox can run on this hive, or why not.
func (r *PDFRuntime) Available() (bool, string) {
	if r == nil || !executable(r.Sandbox) {
		return false, "PDF reading is not available on this hive (the hive-pdftext sandbox is not installed)"
	}
	if r.RunAs != transcribe.WhisperRunAsSelf {
		if _, err := user.Lookup(r.RunAs); err != nil {
			return false, "PDF reading is not available on this hive (the sandbox user " + r.RunAs + " does not exist)"
		}
		if r.SuExec == "" {
			if _, err := exec.LookPath("su-exec"); err != nil {
				return false, "PDF reading is not available on this hive (su-exec is missing)"
			}
		}
	}
	return true, ""
}

// userPrefix is the su-exec argv that drops to RunAs (empty for "self").
func (r *PDFRuntime) userPrefix() ([]string, error) {
	if r.RunAs == transcribe.WhisperRunAsSelf {
		return nil, nil
	}
	suexec := r.SuExec
	if suexec == "" {
		p, err := exec.LookPath("su-exec")
		if err != nil {
			return nil, err
		}
		suexec = p
	}
	return []string{suexec, r.RunAs}, nil
}

// isolation reports, once, whether an empty network namespace can be made
// for the sandbox user.
func (r *PDFRuntime) isolation(ctx context.Context, drop []string) bool {
	if !r.NoNetwork {
		return false
	}
	r.isoOnce.Do(func() {
		argv := append(append([]string{}, drop...), append(pdfbox.NetIsolationPrefix(), "true")...)
		probe := exec.CommandContext(ctx, argv[0], argv[1:]...)
		probe.Env = []string{}
		r.iso = probe.Run() == nil
	})
	return r.iso
}

// argv is su-exec user, prlimit + nice, unshare, then hive-pdftext.
func (r *PDFRuntime) argv(ctx context.Context, maxBytes int64) ([]string, error) {
	drop, err := r.userPrefix()
	if err != nil {
		return nil, err
	}
	argv := append([]string{}, drop...)
	if r.Limits {
		argv = append(argv, pdfbox.LimitPrefix(r.MemoryMB, r.Timeout, maxBytes)...)
	}
	if r.isolation(ctx, drop) {
		argv = append(argv, pdfbox.NetIsolationPrefix()...)
	}
	return append(argv, r.Sandbox,
		"-max-bytes", strconv.FormatInt(maxBytes, 10),
		"-max-pages", strconv.Itoa(r.MaxPages),
		"-max-chars", strconv.Itoa(r.MaxChars)), nil
}

// tailBuffer keeps the last n bytes written.
type tailBuffer struct {
	n int
	b []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.n {
		t.b = t.b[len(t.b)-t.n:]
	}
	return len(p), nil
}

// capBuffer refuses output past n bytes.
type capBuffer struct {
	n int
	bytes.Buffer
}

var errPDFOutputTooLarge = errors.New("the sandbox wrote more than expected")

func (c *capBuffer) Write(p []byte) (int, error) {
	if c.Len()+len(p) > c.n {
		return 0, errPDFOutputTooLarge
	}
	return c.Buffer.Write(p)
}

// Run extracts one PDF in the sandbox. The process group is killed at the
// timeout.
func (r *PDFRuntime) Run(ctx context.Context, data []byte) (pdfbox.Result, *UploadError) {
	if ok, why := r.Available(); !ok {
		return pdfbox.Result{}, uploadErr(UploadErrUnsupported, "%s", why)
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = pdfbox.DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv, err := r.argv(ctx, int64(len(data)))
	if err != nil {
		return pdfbox.Result{}, uploadErr(UploadErrUnsupported, "PDF reading is not available on this hive (%s)", err)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LC_ALL=C"}
	cmd.Stdin = bytes.NewReader(data)
	out := &capBuffer{n: r.maxChars() + pdfStdoutSlack}
	errTail := &tailBuffer{n: pdfStderrBytes}
	cmd.Stdout, cmd.Stderr = out, errTail
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// The whole group: nice, unshare and the sandbox itself.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = pdfWaitDelay
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return pdfbox.Result{}, uploadErr(UploadErrParse, "reading the PDF took longer than %s", timeout)
	}
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) && ee.ExitCode() == pdfbox.ExitTooLarge {
			return pdfbox.Result{}, uploadErr(UploadErrTooLarge, "the PDF is too large")
		}
		reason := lastLine(string(errTail.b))
		if reason == "" {
			reason = runErr.Error()
		}
		return pdfbox.Result{}, uploadErr(UploadErrParse, "the PDF could not be read (%s)", strings.TrimPrefix(reason, "hive-pdftext: "))
	}
	var res pdfbox.Result
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		return pdfbox.Result{}, uploadErr(UploadErrParse, "the PDF sandbox answered something unreadable")
	}
	return res, nil
}

func (r *PDFRuntime) maxChars() int {
	if r.MaxChars > 0 {
		return r.MaxChars
	}
	return pdfbox.DefaultMaxChars
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// pdfMarkdown renders a sandbox result: one "## Page N" section per page
// with text. A PDF without any text is refused as scanned.
func pdfMarkdown(res pdfbox.Result) (Extracted, *UploadError) {
	if !res.HasText() {
		return Extracted{}, uploadErr(UploadErrParse, "%s", pdfScannedMessage)
	}
	var b strings.Builder
	for i, t := range res.Text {
		if strings.TrimSpace(t) == "" {
			continue
		}
		fmt.Fprintf(&b, "## Page %d\n\n%s\n\n", i+1, strings.TrimSpace(t))
	}
	md := strings.TrimSpace(b.String())
	if res.Truncated {
		md += fmt.Sprintf("\n\n_Read %d of %d pages (the page or character limit was reached). The whole file is stored on this hive._", res.Read, res.Pages)
	}
	summary := fmt.Sprintf("PDF, %d %s", res.Pages, plural(res.Pages, "page", "pages"))
	return Extracted{Markdown: md, Summary: summary, Title: truncateRunes(res.Title, uploadTitleRunes)}, nil
}
