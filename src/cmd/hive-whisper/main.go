// Command hive-whisper is the sandbox the built-in whisper provider runs one
// Inbox transcription in. The hive starts it as the hive-transcribe uid
// through su-exec, with an empty environment, the recording on stdin and
// the ggml model on file descriptor 3; it writes the whisper.cpp JSON
// transcript to stdout and progress lines to stderr. See
// pkg/transcribe/whisperbox and src/docs/inbox-transcription.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hivecommons/hive/pkg/transcribe/whisperbox"
)

// version is the sandbox protocol version printed by -version.
const version = "hive-whisper 1"

// defaultTimeout applies when the hive passes no -timeout.
const defaultTimeout = time.Hour

func main() { os.Exit(runMain(os.Args[1:])) }

// runMain runs with the process's stdio, stopping on SIGTERM/SIGINT/SIGHUP.
func runMain(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	return run(ctx, args, os.Stdin, os.Stdout, os.Stderr)
}

// run parses args and executes one sandboxed transcription.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var o whisperbox.Options
	fs := flag.NewFlagSet("hive-whisper", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.StringVar(&o.ModelPath, "model", "", "ggml model path (default: read from file descriptor 3)")
	fs.StringVar(&o.EngineDir, "engine-dir", whisperbox.DefaultEngineDir, "directory holding whisper-cli and ffmpeg")
	fs.IntVar(&o.Threads, "threads", 1, "whisper-cli threads")
	fs.StringVar(&o.Language, "lang", "auto", "spoken language code or auto")
	fs.Int64Var(&o.MaxBytes, "max-bytes", 0, "largest recording accepted on stdin (0: no cap)")
	fs.DurationVar(&o.Timeout, "timeout", defaultTimeout, "wall-clock limit for the whole run")
	fs.IntVar(&o.MemoryMB, "memory-mb", whisperbox.DefaultMemoryMB, "address-space limit per engine, MiB")
	fs.StringVar(&o.WorkRoot, "work-root", "", "where the private work dir is created (default: $TMPDIR or /tmp)")
	fs.BoolVar(&o.Limits, "limits", true, "run engines under prlimit and nice")
	fs.BoolVar(&o.NoNetwork, "no-network", true, "run engines in an empty network namespace when the kernel allows it")
	if err := fs.Parse(args); err != nil {
		return whisperbox.ExitUsage
	}
	if *showVersion {
		fmt.Fprintln(stdout, version)
		return whisperbox.ExitOK
	}
	return whisperbox.Main(ctx, o, stdin, stdout, stderr)
}
