package policies

import (
	"bufio"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Default policies must name only commands the agent image ships, and only
// comment routes the relay or the proxy allows (hivecommons/hive#20084). An
// agent that follows a template naming a script the image lacks spends turns
// on a command that cannot work, then reports the step as blocked or skips it.
//
// This is a tripwire for the common shapes (a script or binary named by path,
// an "edit the existing" comment instruction, a hold-gated PR said to carry
// `hold`), not a full check of every instruction.

// agentBinDirs are the runtime-stage directories on the agent PATH whose
// COPY destinations count as commands the agent has.
var agentBinDirs = map[string]bool{
	"/usr/local/bin":    true,
	"/usr/local/go/bin": true,
}

// policyCommandAllowlist names command words a template may use although the
// runtime stage does not COPY them into agentBinDirs, each with the reason the
// agent has it anyway. Keep it short: an entry here is a promise someone
// checked the command exists for the agent.
var policyCommandAllowlist = map[string]string{}

// policyCommandWord matches a command word at the start of a code span or
// after a shell separator: an in-repo script path, hivectl, or a hive-* tool.
// Anchoring on command position keeps marker names such as
// `<!-- hive-pr-overlap -->` and labels such as `hive-pause/<id>` out.
var policyCommandWord = regexp.MustCompile(`(?:^|&&|\|\||[|;(]|\$\()\s*((?:src/scripts|bin)/[\w.-]+\.sh|hivectl|hive-[A-Za-z0-9_.-]+)(?:\s|$|[;|)&])`)

var policyCodeSpan = regexp.MustCompile("`([^`\n]+)`")

// runtimeImageCommands parses src/Dockerfile and returns the names the
// runtime stage COPYs into agentBinDirs, plus a map from each COPY source path
// to the installed name.
func runtimeImageCommands(t *testing.T) (names map[string]bool, sources map[string]string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatalf("read src/Dockerfile: %v", err)
	}
	names = map[string]bool{}
	sources = map[string]string{}
	inRuntime := false
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if strings.EqualFold(fields[0], "FROM") {
			last := fields[len(fields)-1]
			inRuntime = len(fields) >= 3 && strings.EqualFold(fields[len(fields)-2], "AS") && last == "runtime"
			continue
		}
		if !inRuntime || !strings.EqualFold(fields[0], "COPY") {
			continue
		}
		var args []string
		for _, f := range fields[1:] {
			if strings.HasPrefix(f, "--") {
				continue
			}
			args = append(args, f)
		}
		if len(args) < 2 {
			continue
		}
		dest := args[len(args)-1]
		srcs := args[:len(args)-1]
		for _, src := range srcs {
			dir, name := path.Dir(dest), path.Base(dest)
			if strings.HasSuffix(dest, "/") {
				dir, name = strings.TrimSuffix(dest, "/"), path.Base(src)
			}
			if !agentBinDirs[dir] {
				continue
			}
			names[name] = true
			sources[src] = name
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan src/Dockerfile: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("found no runtime-stage COPY into the agent PATH; the Dockerfile parser is out of date")
	}
	return names, sources
}

// policyCommandWords returns the command words in a template's inline code
// spans and fenced code blocks.
func policyCommandWords(body string) []string {
	seen := map[string]bool{}
	add := func(code string) {
		for _, m := range policyCommandWord.FindAllStringSubmatch(strings.TrimSpace(code), -1) {
			seen[m[1]] = true
		}
	}
	inFence := false
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			add(line)
			continue
		}
		for _, m := range policyCodeSpan.FindAllStringSubmatch(line, -1) {
			add(m[1])
		}
	}
	out := make([]string, 0, len(seen))
	for w := range seen {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

func defaultPolicyBodies(t *testing.T) map[string]string {
	t.Helper()
	files, err := fs.Glob(DefaultPolicies, "defaults/*.md")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob default policies: %v (%d files)", err, len(files))
	}
	out := map[string]string{}
	for _, f := range files {
		b, err := DefaultPolicies.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		out[path.Base(f)] = string(b)
	}
	return out
}

func TestDefaultPoliciesNameOnlyCommandsTheImageShips(t *testing.T) {
	t.Parallel()
	names, sources := runtimeImageCommands(t)
	for name, body := range defaultPolicyBodies(t) {
		for _, word := range policyCommandWords(body) {
			if names[word] {
				continue
			}
			if _, ok := policyCommandAllowlist[word]; ok {
				continue
			}
			// An in-repo path is fine as the fallback for a checkout when the
			// image installs that file and the template also names the
			// installed command.
			if installed, ok := sources[word]; ok && policyNamesCommand(body, installed) {
				continue
			}
			t.Errorf("%s names `%s`, which the agent image does not ship on PATH (src/Dockerfile runtime stage) and which is not on policyCommandAllowlist", name, word)
		}
	}
}

func policyNamesCommand(body, command string) bool {
	for _, w := range policyCommandWords(body) {
		if w == command {
			return true
		}
	}
	// The installed name need not match the hive-* pattern (issue-coauthor).
	return strings.Contains(body, "`"+command+" ") || strings.Contains(body, "`"+command+"`")
}

func TestPolicyCommandAllowlistEntriesHaveReasons(t *testing.T) {
	t.Parallel()
	for word, reason := range policyCommandAllowlist {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("policyCommandAllowlist entry %q has no reason", word)
		}
	}
}

// commentEditAvailable reports whether the proxy or the relay offers an agent
// any way to edit an existing comment.
func commentEditAvailable(t *testing.T) bool {
	t.Helper()
	rules, err := os.ReadFile(filepath.Join("..", "..", "crux", "proxy", "rules.go"))
	if err != nil {
		t.Fatalf("read proxy rules: %v", err)
	}
	for _, line := range strings.Split(string(rules), "\n") {
		if strings.Contains(line, "issues/comments") && strings.Contains(line, `Method: "PATCH"`) {
			return true
		}
	}
	watcher, err := os.ReadFile(filepath.Join("..", "github", "issue_request_watcher.go"))
	if err != nil {
		t.Fatalf("read issue request watcher: %v", err)
	}
	return strings.Contains(string(watcher), `"edit_comment"`)
}

func TestDefaultPoliciesDoNotAskForCommentEdits(t *testing.T) {
	t.Parallel()
	if commentEditAvailable(t) {
		t.Skip("a comment-edit route exists; edit instructions are satisfiable")
	}
	for name, body := range defaultPolicyBodies(t) {
		if strings.Contains(strings.ToLower(body), "edit the existing") {
			t.Errorf("%s tells the agent to edit an existing comment, but no proxy rule or relay kind allows a comment edit", name)
		}
	}
}

var holdCarried = regexp.MustCompile("carries `hold`")

func TestHoldGatedPoliciesDoNotSayPRsCarryHold(t *testing.T) {
	t.Parallel()
	for name, body := range defaultPolicyBodies(t) {
		if !strings.HasSuffix(name, "-holdgated.md") {
			continue
		}
		if holdCarried.MatchString(body) {
			t.Errorf("%s says a PR carries `hold`; the hive labels hold-gated PRs `ready-for-review` and never `hold`", name)
		}
	}
}
