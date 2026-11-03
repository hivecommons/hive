package pushbroker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const workspaceObservationOutputLimit = 64 << 10

var errWorkspaceObservation = errors.New("pushbroker: workspace file observation unavailable")

// WorkspaceFileObservation is read-only evidence of a changed regular file
// against an observed head and origin. It grants no broker mutation authority.
type WorkspaceFileObservation struct {
	Path   string
	Head   string
	Origin string
}

// ObserveWorkspaceFile never runs diff, filters, hooks, remote helpers or
// mutation commands. Unchanged/ignored files return nil; unsupported config,
// normalization, unstable metadata and failed reads remain unverifiable.
func ObserveWorkspaceFile(ctx context.Context, path string) (*WorkspaceFileObservation, error) {
	observation, err := observeWorkspaceFile(ctx, path, workspaceObservationRunner{})
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return observation, err
}

func observeWorkspaceFile(ctx context.Context, path string, runner CommandRunner) (*WorkspaceFileObservation, error) {
	info, err := os.Lstat(path)
	if err != nil || !filepath.IsAbs(path) || !info.Mode().IsRegular() {
		return nil, errWorkspaceObservation
	}
	canonicalPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, errWorkspaceObservation
	}
	dir := filepath.Dir(canonicalPath)
	for {
		if ctx.Err() != nil {
			return nil, errWorkspaceObservation
		}
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			break
		} else if !os.IsNotExist(err) || filepath.Dir(dir) == dir {
			return nil, errWorkspaceObservation
		}
		dir = filepath.Dir(dir)
	}
	expectedRoot := dir
	env := workspaceObservationEnv(os.Environ(), expectedRoot)
	git := func(args ...string) ([]byte, error) {
		return runner.Run(ctx, dir, env, "git", append([]string{"--literal-pathspecs"}, args...)...)
	}
	config, err := git("config", "--list", "-z", "--show-scope")
	if err != nil || len(workspaceConfigOffenders(config, DefaultRemote, nil)) != 0 {
		return nil, errWorkspaceObservation
	}
	root, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return nil, errWorkspaceObservation
	}
	canonicalRoot, err := filepath.EvalSymlinks(strings.TrimSpace(string(root)))
	if err != nil || canonicalRoot != expectedRoot {
		return nil, errWorkspaceObservation
	}
	relative, err := filepath.Rel(canonicalRoot, canonicalPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, errWorkspaceObservation
	}
	relative = filepath.ToSlash(relative)
	dir = canonicalRoot
	head, err := git("rev-parse", "--verify", "HEAD")
	if err != nil {
		return nil, errWorkspaceObservation
	}
	origin, err := git("remote", "get-url", DefaultRemote)
	if err != nil || len(bytes.TrimSpace(head)) == 0 || len(bytes.TrimSpace(origin)) == 0 {
		return nil, errWorkspaceObservation
	}
	tree, err := git("ls-tree", "--full-tree", "-z", strings.TrimSpace(string(head)), "--", relative)
	if err != nil {
		return nil, errWorkspaceObservation
	}
	if len(tree) == 0 {
		files, err := git("ls-files", "--cached", "--others", "--exclude-standard", "-z", "--", relative)
		if err != nil {
			return nil, errWorkspaceObservation
		}
		if !slices.Contains(splitNUL(files), relative) {
			return nil, nil
		}
	} else {
		records := splitNUL(tree)
		if len(records) != 1 {
			return nil, errWorkspaceObservation
		}
		metadata, name, ok := strings.Cut(records[0], "\t")
		fields := strings.Fields(metadata)
		if !ok || name != relative || len(fields) != 3 || fields[1] != "blob" ||
			(fields[0] != "100644" && fields[0] != "100755") {
			return nil, errWorkspaceObservation
		}
		// --no-filters is essential: even a config change after the allowlist
		// read must not execute a clean/process filter as the observer's UID.
		hash, err := git("hash-object", "--no-filters", "--", canonicalPath)
		if err != nil {
			return nil, errWorkspaceObservation
		}
		if strings.TrimSpace(string(hash)) == fields[2] {
			return nil, nil
		}
		if !workspaceObservationNormalizationSafe(config) {
			return nil, errWorkspaceObservation
		}
		attributes, err := git("check-attr", "-z", "filter", "text", "eol", "ident", "working-tree-encoding", "crlf", "--", relative)
		if err != nil || !workspaceObservationAttributesSafe(attributes, relative) {
			return nil, errWorkspaceObservation
		}
	}
	// Bind the result to stable actual metadata, not an earlier worker's head,
	// a replacement object, an inherited GIT_DIR, or a changing source path.
	for _, check := range []struct {
		args []string
		want []byte
	}{
		{[]string{"rev-parse", "--show-toplevel"}, root},
		{[]string{"rev-parse", "--verify", "HEAD"}, head},
		{[]string{"remote", "get-url", DefaultRemote}, origin},
		{[]string{"config", "--list", "-z", "--show-scope"}, config},
	} {
		got, err := git(check.args...)
		if err != nil || !bytes.Equal(got, check.want) {
			return nil, errWorkspaceObservation
		}
	}
	after, err := os.Lstat(path)
	resolved, resolveErr := filepath.EvalSymlinks(path)
	if ctx.Err() != nil || err != nil || resolveErr != nil || resolved != canonicalPath || !after.Mode().IsRegular() ||
		!os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return nil, errWorkspaceObservation
	}
	return &WorkspaceFileObservation{Path: relative, Head: strings.TrimSpace(string(head)), Origin: strings.TrimSpace(string(origin))}, nil
}

func workspaceObservationEnv(env []string, root string) []string {
	env = slices.DeleteFunc(PushEnv(env), func(entry string) bool {
		return strings.HasPrefix(entry, "GIT_") || strings.HasPrefix(entry, pushTokenEnvVar+"=")
	})
	// The manager inspects an agent-owned checkout. Trust only this discovered
	// canonical worktree, never a global safe.directory wildcard.
	overrides := append(workspaceConfigOverrides(DefaultRemote, ""),
		[2]string{"core.attributesFile", os.DevNull}, [2]string{"safe.directory", root})
	env = append(env, configEnv(overrides)...)
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM="+os.DevNull, "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_NO_REPLACE_OBJECTS=1",
		"GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL=", "GIT_ATTR_NOSYSTEM=1")
}

func workspaceObservationNormalizationSafe(config []byte) bool {
	fields := strings.Split(string(config), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		key, value, _ := strings.Cut(fields[i+1], "\n")
		switch strings.ToLower(key) {
		case "core.autocrlf":
			if value != "false" && value != "0" {
				return false
			}
		case "core.eol":
			return false
		}
	}
	return true
}

func workspaceObservationAttributesSafe(out []byte, path string) bool {
	fields := splitNUL(out)
	const attributeCount = 6
	const fieldsPerAttribute = 3
	if len(fields) != attributeCount*fieldsPerAttribute {
		return false
	}
	for i := 0; i < len(fields); i += fieldsPerAttribute {
		if fields[i] != path || (fields[i+2] != "unspecified" && fields[i+2] != "unset") {
			return false
		}
	}
	return true
}

type workspaceObservationRunner struct{}

func (workspaceObservationRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir, command.Env = dir, env
	var output workspaceObservationOutput
	command.Stdout, command.Stderr = &output, io.Discard
	err := command.Run()
	return output.buffer.Bytes(), err
}

type workspaceObservationOutput struct {
	buffer bytes.Buffer
}

func (out *workspaceObservationOutput) Write(p []byte) (int, error) {
	if len(p) > workspaceObservationOutputLimit-out.buffer.Len() {
		return 0, errWorkspaceObservation
	}
	return out.buffer.Write(p)
}
