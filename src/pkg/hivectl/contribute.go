package hivectl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// The contributor-hub half of the named hive profiles (#8097).
//
// profiles.go owns the FILE; this file owns the two things a surface has to do
// to the hub itself — register with one, and ask whether one is answering —
// plus the `gh` lookup that names the contributor.
//
// They live in pkg/hivectl rather than in pkg/hivectl/commands because there
// are now TWO surfaces over the same operations: `hivectl hives` and the TUI's
// Hives pane (#8128). pkg/tui cannot import pkg/hivectl/commands — commands
// imports pkg/tui for the `tui` subcommand — so anything both surfaces need has
// to sit here, which is also the only way "no second implementation" is a
// property of the code rather than a promise in a review comment.

// Registration is a contributor hub's answer to a register call.
type Registration struct {
	RegistrationToken string `json:"registration_token"`
	ContributorID     string `json:"contributor_id"`
	Message           string `json:"message"`
}

// defaultRegisterTimeout bounds a registration POST when the caller has no
// timeout of its own.
const defaultRegisterTimeout = 15 * time.Second

// defaultProbeTimeout bounds a reachability probe when the caller has no
// timeout of its own.
const defaultProbeTimeout = 10 * time.Second

// Register performs the registration half of `just contribute-setup`:
// POST <hubHTTPBase>/api/contribute/register with a GitHub username.
//
// SECURITY (#4408, H7/CWE-522): no Authorization header and no GitHub PAT is
// sent. The hub URL can come from a registry entry, so forwarding a token here
// would let a poisoned registry harvest it. The endpoint identifies the
// contributor by github_username alone and ignores bearer credentials — the
// same contract `just contribute-setup` relies on.
//
// An empty RegistrationToken in a 2xx answer is the "already registered"
// case, which is correct and deliberate on the hub's side: register is
// unauthenticated, so it must never hand an existing contributor's token to
// whoever POSTs their username. Callers turn that into guidance; it is not an
// error here because the hub's own message is part of the answer.
func Register(ctx context.Context, hubHTTPBase, githubUser string, timeout time.Duration) (Registration, error) {
	if timeout <= 0 {
		timeout = defaultRegisterTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body, err := json.Marshal(map[string]string{"github_username": githubUser})
	if err != nil {
		return Registration{}, fmt.Errorf("encode registration request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hubHTTPBase+"/api/contribute/register", bytes.NewReader(body))
	if err != nil {
		return Registration{}, fmt.Errorf("build registration request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Registration{}, fmt.Errorf("register with %s failed: %w\n  is the hub reachable? try: curl -sf %s/api/contribute/status", hubHTTPBase, err, hubHTTPBase)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Registration{}, fmt.Errorf("read registration response from %s: %w", hubHTTPBase, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Registration{}, fmt.Errorf("register with %s returned HTTP %d: %s", hubHTTPBase, resp.StatusCode, truncateForMessage(string(payload)))
	}
	var reg Registration
	if err := json.Unmarshal(payload, &reg); err != nil {
		return Registration{}, fmt.Errorf("hub %s returned a non-JSON registration response: %s", hubHTTPBase, truncateForMessage(string(payload)))
	}
	return reg, nil
}

// ProbeHub reports whether the hub answered its contributor status endpoint.
//
// Any failure — DNS, TLS, timeout, non-2xx — is "not reachable". This is a
// convenience column, not a diagnostic: the operator's next step is the same
// either way, and collapsing every failure into one boolean is what lets a
// caller render the column without deciding which failures deserve their own
// word.
func ProbeHub(ctx context.Context, hub string, timeout time.Duration) bool {
	base, err := HubHTTPBase(hub)
	if err != nil {
		return false
	}
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/contribute/status", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// RunGH executes the `gh` CLI and returns its combined output.
//
// It is a package VAR so tests can replace the subprocess. Production callers
// never reassign it.
var RunGH = func(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Env = withoutGitHubToken(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// withoutGitHubToken strips GITHUB_TOKEN and GH_TOKEN from an environment.
//
// `gh` prefers those variables over its own stored credential, so inheriting
// one from the calling shell would silently act as a DIFFERENT identity than
// the one the contributor signed in as — and, on a CI runner, as the workflow's
// ambient token. Stripping them makes `gh auth` the single source of who we
// are.
func withoutGitHubToken(env []string) []string {
	out := env[:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "GITHUB_TOKEN=") || strings.HasPrefix(kv, "GH_TOKEN=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// GitHubLogin asks the already-installed gh CLI who is signed in.
//
// The contributor flow requires gh anyway (contribute-setup signs in with it),
// so this reuses that identity instead of asking the operator to retype it.
// Both failure paths name the way forward, because "could not determine your
// GitHub login" on its own leaves an operator with nothing to do about it.
func GitHubLogin(ctx context.Context) (string, error) {
	out, err := RunGH(ctx, "api", "user", "--jq", ".login")
	if err != nil {
		return "", fmt.Errorf("could not determine your GitHub login from 'gh' (%w)\n  pass --github-user <login>, or sign in with: gh auth login --web --scopes repo,read:org", err)
	}
	user := strings.TrimSpace(out)
	if user == "" {
		return "", errors.New("'gh api user' returned no login; pass --github-user <login>")
	}
	return user, nil
}

func truncateForMessage(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
