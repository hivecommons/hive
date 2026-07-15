package resolve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

func init() {
	RegisterResolver("script", newScriptResolver)
}

// scriptResolver resolves a variable to the trimmed stdout of a command.
//
// Security model:
//   - Gated: the factory refuses to build unless Policy.AllowExec is true. Since
//     the policy is honored only from the trusted config seed (the config
//     package discards the dashboard overlay's security block), a user-editable
//     overlay can never enable script execution.
//   - No shell: the command is an argv slice run directly via exec.CommandContext,
//     never `sh -c` on a string, so there is no shell-injection surface.
//   - Bounded: each run has a context timeout (Policy.ExecTimeoutS). A timeout or
//     non-zero exit yields ErrUnresolved (the ${VAR} is left literal), never a
//     crash and never a partial/garbage value.
//   - Scrubbed environment: the child inherits only a minimal PATH-style env, not
//     the hive process environment, so secrets in hive's env are not exposed to
//     arbitrary configured scripts.
type scriptResolver struct {
	command []string
	timeout time.Duration
	dflt    string
	hasDflt bool
	tag     string // non-secret provenance: "script:<sha8-of-argv>"
}

func newScriptResolver(spec VarSpec, pol Policy) (Resolver, error) {
	if !pol.AllowExec {
		return nil, errors.New("script resolvers are disabled (set variables.security.allow_exec in the seed config to enable)")
	}
	if len(spec.Command) == 0 {
		return nil, fmt.Errorf("script variable %q has no command", spec.Name)
	}
	sum := sha256.Sum256([]byte(strings.Join(spec.Command, "\x00")))
	return &scriptResolver{
		command: spec.Command,
		timeout: time.Duration(pol.ExecTimeoutS) * time.Second,
		dflt:    spec.Default,
		hasDflt: spec.HasDefault,
		tag:     "script:" + hex.EncodeToString(sum[:])[:8],
	}, nil
}

// scrubbedEnv is the minimal environment a script resolver runs with. It
// deliberately excludes hive's process environment (which holds tokens/keys).
var scrubbedEnv = []string{
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
}

func (r *scriptResolver) Resolve(req Request) (Result, error) {
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}

	// #nosec G204 — command is an operator-declared argv slice from the trusted
	// seed config (gated by AllowExec), executed without a shell. Not derived
	// from untrusted input.
	cmd := exec.CommandContext(ctx, r.command[0], r.command[1:]...)
	cmd.Env = scrubbedEnv
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	// stderr is intentionally discarded from the value (never injected into the
	// prompt); it is left unset so it goes nowhere.

	if err := cmd.Run(); err != nil {
		// Timeout or non-zero exit: fall back to a default if provided, else
		// leave the variable unresolved (literal). Never surface partial output.
		if r.hasDflt {
			return Result{Value: r.dflt, Source: "default"}, nil
		}
		return Result{}, ErrUnresolved
	}
	return Result{Value: strings.TrimRight(stdout.String(), "\n"), Source: r.tag}, nil
}
