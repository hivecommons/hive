package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/hivecommons/hive/crux/sandbox/substrateapi"
	"github.com/hivecommons/hive/crux/sandbox/substratebridge"
)

// routerBridge's substrateBridge implementation: the four verbs the launcher
// drives a kick with, each one HTTP exchange over its own CONNECT tunnel.

// WaitReady polls the bridge's health endpoint until it answers. The actor
// is RUNNING by now, but the bridge process inside still has to come up.
func (b *routerBridge) WaitReady(ctx context.Context, ref *substrateapi.ObjectRef, poll time.Duration) error {
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	var last error
	for {
		resp, err := b.do(ctx, ref, http.MethodGet, substratebridge.PathHealth, nil)
		if err == nil {
			resp.Body.Close()
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last: %v)", ctx.Err(), last)
		case <-ticker.C:
		}
	}
}

// Push replaces the actor's workspace with the local one.
func (b *routerBridge) Push(ctx context.Context, ref *substrateapi.ObjectRef, workspace string) error {
	var buf bytes.Buffer
	if err := substratebridge.Pack(workspace, &buf); err != nil {
		return fmt.Errorf("packing %s: %w", workspace, err)
	}
	resp, err := b.do(ctx, ref, http.MethodPut, substratebridge.PathWorkspace, &buf)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Run executes the kick command in the actor and waits for it to exit. It
// asks the bridge to stream (RunRequest.Stream) and copies stdout and
// stderr to live as they arrive, so the dashboard's terminal pane can follow
// the kick; the returned RunResponse still carries the whole (capped)
// output for the transcript. live may be nil.
func (b *routerBridge) Run(ctx context.Context, ref *substrateapi.ObjectRef, req substratebridge.RunRequest, live io.Writer) (substratebridge.RunResponse, error) {
	var out substratebridge.RunResponse
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	resp, err := b.do(ctx, ref, http.MethodPost, substratebridge.PathRun, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	return substrateDecodeRun(resp.Body, live)
}

// substrateDecodeRun reads a /run answer: Partial frames from a streaming
// bridge, then the final frame with the exit code. A bridge that does not
// stream answers with one complete, non-Partial RunResponse, which is read
// the same way.
func substrateDecodeRun(r io.Reader, live io.Writer) (substratebridge.RunResponse, error) {
	var out substratebridge.RunResponse
	var stdout, stderr bytes.Buffer
	dec := json.NewDecoder(r)
	for {
		var frame substratebridge.RunResponse
		if err := dec.Decode(&frame); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return out, fmt.Errorf("decoding run response: %w", err)
		}
		substrateAppendCapped(&stdout, frame.Stdout)
		substrateAppendCapped(&stderr, frame.Stderr)
		if live != nil {
			_, _ = io.WriteString(live, frame.Stdout)
			_, _ = io.WriteString(live, frame.Stderr)
		}
		if !frame.Partial {
			out.Stdout, out.Stderr = stdout.String(), stderr.String()
			out.ExitCode, out.Error = frame.ExitCode, frame.Error
			return out, nil
		}
	}
}

func substrateAppendCapped(buf *bytes.Buffer, s string) {
	if room := substratebridge.MaxOutput - buf.Len(); len(s) > room {
		s = s[:max(room, 0)]
	}
	buf.WriteString(s)
}

// Pull replaces the local workspace with the actor's, so the commits the
// agent made are what hive's post-kick push broker sees. The new tree is
// unpacked beside the old one and swapped in, so a failed transfer leaves
// the original workspace untouched.
func (b *routerBridge) Pull(ctx context.Context, ref *substrateapi.ObjectRef, workspace string) error {
	resp, err := b.do(ctx, ref, http.MethodGet, substratebridge.PathWorkspace, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return substrateReplaceDir(resp.Body, workspace)
}

func substrateReplaceDir(stream io.Reader, workspace string) error {
	parent := filepath.Dir(filepath.Clean(workspace))
	staging, err := os.MkdirTemp(parent, ".substrate-pull-*")
	if err != nil {
		return err
	}
	if err := substratebridge.Unpack(stream, staging); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("unpacking into %s: %w", staging, err)
	}
	old := staging + ".old"
	if err := os.Rename(workspace, old); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	if err := os.Rename(staging, workspace); err != nil {
		_ = os.Rename(old, workspace)
		_ = os.RemoveAll(staging)
		return err
	}
	return os.RemoveAll(old)
}
