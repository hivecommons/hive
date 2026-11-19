package pushbroker

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// The broker runs git INSIDE a workspace the sandboxed agent wrote. The
// sandbox exists so that the agent never holds a credential; the broker is the
// one process that does, and it runs every git command below as the hive's
// own UID, outside the sandbox. Git, however, reads the repository's own
// configuration — .git/config, .git/hooks/, .gitattributes — before it reads
// anything the broker says on the command line, and several of those keys make
// git EXECUTE a program or REDIRECT the push. An agent that writes any of them
// into the workspace turns the broker into (a) arbitrary code execution as the
// hive's UID on the host and (b) exfiltration of the push token to a host of
// the agent's choosing. Two layers close that:
//
//  1. workspaceConfigOverrides are applied to EVERY broker git invocation at
//     command scope (GIT_CONFIG_*), which outranks the repository's own
//     files: hooks are routed to /dev/null, the fsmonitor and credential
//     helper lists are reset, commit signing is off, and the remote the
//     broker pushes to is pinned to the repository it was told to push to,
//     not to whatever .git/config says "origin" is now.
//  2. rejectHostileWorkspaceConfig refuses the push outright when the
//     repository-scoped configuration contains any key outside the small set
//     a fresh clone and an ordinary agent session produce. Keys like
//     url.*.insteadOf, http.proxy / http.sslVerify, filter.*.clean and
//     core.askPass have no override that is safe against every spelling, so
//     the only sound answer to seeing them is to not run git at all.
func workspaceConfigOverrides(remote, repo string) [][2]string {
	pairs := [][2]string{
		{"core.hooksPath", os.DevNull},
		{"core.fsmonitor", "false"},
		{"credential.helper", ""},
		{"commit.gpgsign", "false"},
	}
	if url := expectedRemoteURL(repo); url != "" {
		pairs = append(pairs,
			[2]string{"remote." + remote + ".url", url},
			[2]string{"remote." + remote + ".pushurl", url},
		)
	}
	return pairs
}

// expectedRemoteURL is the URL SandboxExecutor cloned repo from, and the only
// URL the broker will push to. Empty when repo is not an "owner/name" pair.
func expectedRemoteURL(repo string) string {
	owner, name, ok := strings.Cut(strings.TrimSpace(repo), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return ""
	}
	return "https://" + pushRemoteHost + "/" + owner + "/" + name + ".git"
}

// configEnv renders pairs as the GIT_CONFIG_COUNT/KEY/VALUE triplets git reads
// as command-scope configuration. Only the variable names and the values
// above reach the environment; no secret does.
func configEnv(pairs [][2]string) []string {
	env := make([]string, 0, 1+2*len(pairs))
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(pairs)))
	for i, p := range pairs {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, p[0]),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, p[1]),
		)
	}
	return env
}

// allowedWorkspaceConfigKeys are the exact repository-scoped keys a fresh
// `git clone` writes plus the identity keys an agent CLI commonly sets. Keys
// are compared lowercased, as `git config --list` prints them.
var allowedWorkspaceConfigKeys = map[string]struct{}{
	"core.repositoryformatversion": {},
	"core.filemode":                {},
	"core.bare":                    {},
	"core.logallrefupdates":        {},
	"core.ignorecase":              {},
	"core.precomposeunicode":       {},
	"core.symlinks":                {},
	"core.autocrlf":                {},
	"core.eol":                     {},
	"core.safecrlf":                {},
	"core.hidedotfiles":            {},
	"core.protectntfs":             {},
	"extensions.worktreeconfig":    {},
	"extensions.objectformat":      {},
	"user.name":                    {},
	"user.email":                   {},
	"safe.directory":               {},
	"pull.rebase":                  {},
	"push.default":                 {},
	"push.autosetupremote":         {},
	"init.defaultbranch":           {},
	"fetch.prune":                  {},
}

// allowedWorkspaceConfigPrefixes cover the per-branch tracking keys and the
// purely cosmetic sections, none of which can make git execute or redirect.
var allowedWorkspaceConfigPrefixes = []string{"color.", "advice."}

// rejectHostileWorkspaceConfig reads the repository-scoped (local and
// worktree) configuration of the workspace and refuses the push when it holds
// any key outside the allowlist, or a remote URL for the push remote that is
// not the repository the broker was told to push to.
func (b *Broker) rejectHostileWorkspaceConfig(ctx context.Context) error {
	out, err := b.git(ctx, "config", "--list", "-z", "--show-scope")
	if err != nil {
		return fmt.Errorf("reading workspace git config: %w", err)
	}
	remote := strings.ToLower(b.remote())
	want := expectedRemoteURL(b.Repo)
	offenders := workspaceConfigOffenders(out, remote, &want)
	if len(offenders) == 0 {
		return nil
	}
	return fmt.Errorf("pushbroker: refusing to run git in a workspace whose repository config sets %s; a sandbox workspace may only carry the keys a fresh clone writes", strings.Join(offenders, ", "))
}

func workspaceConfigOffenders(out []byte, remote string, expectedURL *string) []string {
	var offenders []string
	// The record layout is "<scope>\x00<key>\n<value>\x00", so splitting on
	// NUL yields alternating scope and key/value elements.
	fields := strings.Split(string(out), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		scope := fields[i]
		key, value, _ := strings.Cut(fields[i+1], "\n")
		if scope != "local" && scope != "worktree" {
			continue
		}
		key = strings.ToLower(key)
		if workspaceConfigKeyAllowed(key, remote) {
			if expectedURL != nil && (key == "remote."+remote+".url" || key == "remote."+remote+".pushurl") && !sameRemoteURL(value, *expectedURL) {
				offenders = append(offenders, key+"="+value)
			}
			continue
		}
		offenders = append(offenders, key)
	}
	return offenders
}

func workspaceConfigKeyAllowed(key, remote string) bool {
	if _, ok := allowedWorkspaceConfigKeys[key]; ok {
		return true
	}
	for _, prefix := range allowedWorkspaceConfigPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	switch key {
	case "remote." + remote + ".url", "remote." + remote + ".pushurl", "remote." + remote + ".fetch":
		return true
	}
	if rest, ok := strings.CutPrefix(key, "branch."); ok {
		switch {
		case strings.HasSuffix(rest, ".remote"), strings.HasSuffix(rest, ".merge"), strings.HasSuffix(rest, ".rebase"):
			return true
		}
	}
	return false
}

// sameRemoteURL compares two https remote URLs ignoring case, a trailing
// slash and the optional ".git" suffix.
func sameRemoteURL(got, want string) bool {
	norm := func(u string) string {
		u = strings.ToLower(strings.TrimSpace(u))
		u = strings.TrimSuffix(u, "/")
		u = strings.TrimSuffix(u, ".git")
		return u
	}
	return want != "" && norm(got) == norm(want)
}
