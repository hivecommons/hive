package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
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

// Run executes the kick command in the actor and waits for it to exit.
func (b *routerBridge) Run(ctx context.Context, ref *substrateapi.ObjectRef, req substratebridge.RunRequest) (substratebridge.RunResponse, error) {
	var out substratebridge.RunResponse
	body, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	resp, err := b.do(ctx, ref, http.MethodPost, substratebridge.PathRun, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("decoding run response: %w", err)
	}
	return out, nil
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
