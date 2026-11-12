package inbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"text/template"
	"time"
)

// StatusDelegated means the route was handed to a hive agent (no binary in
// the image); the agent files the RFC and the operator links it manually.
const StatusDelegated = "delegated"

// spekTimeout bounds one `spektacular voice --publish` run.
const spekTimeout = 10 * time.Minute

// Kicker sends a prompt to a named hive agent (the dashboard's kick path).
type Kicker func(agent, prompt string) error

// SpekDestination files an item as a spek RFC + child issues by running
// `spektacular voice --from stdin --publish` inside the hive. When the
// binary is missing and Agent is set, it kicks that agent with a prompt
// instead (fallback until the hive image ships spektacular).
type SpekDestination struct {
	Binary      string
	DefaultRepo string
	// Token mints a GitHub token for repo; empty/nil inherits the process env.
	Token func(ctx context.Context, repo string) (string, error)
	// Exec runs the command; nil uses os/exec. Tests inject it.
	Exec func(ctx context.Context, binary string, args []string, env []string, stdin string) (string, error)
	// LookPath reports whether binary is runnable; nil uses exec.LookPath.
	LookPath func(binary string) error
	Agent    string
	Kick     Kicker
	Prefixes []string
}

// Name implements Destination.
func (d *SpekDestination) Name() string { return DestSpekRFC }

// ErrNoRepo is returned when neither the request nor config names a repo.
var ErrNoRepo = errors.New("spek_rfc: no repo given and inbox.spek.repo is unset")

// Deliver implements Destination.
func (d *SpekDestination) Deliver(ctx context.Context, it Item, c Content, req Request) (Result, error) {
	repo := strings.TrimSpace(req.Repo)
	if repo == "" {
		repo = strings.TrimSpace(d.DefaultRepo)
	}
	if repo == "" {
		return Result{}, ErrNoRepo
	}
	title := StripPrefix(firstNonEmpty(c.Title, it.Title), d.prefixes())
	text := c.Text()
	if strings.TrimSpace(text) == "" {
		return Result{}, fmt.Errorf("spek_rfc: item %s has no transcript or summary", it.Key())
	}
	binary := firstNonEmpty(d.Binary, "spektacular")
	if err := d.lookPath(binary); err != nil {
		if d.Agent != "" && d.Kick != nil {
			prompt := RenderAgentPrompt(it, c, repo, title)
			if kerr := d.Kick(d.Agent, prompt); kerr != nil {
				return Result{}, fmt.Errorf("spek_rfc: %s unavailable and kick to %s failed: %w", binary, d.Agent, kerr)
			}
			return Result{Note: "delegated to agent " + d.Agent}, nil
		}
		return Result{}, fmt.Errorf("spek_rfc: %s is not installed in this hive (set inbox.spek.binary or inbox.spek.agent): %w", binary, err)
	}
	stdin := text + "\n\nProvenance: " + c.Provenance + "\n"
	env := os.Environ()
	if d.Token != nil {
		tok, err := d.Token(ctx, repo)
		if err != nil {
			return Result{}, fmt.Errorf("spek_rfc: github token: %w", err)
		}
		if tok != "" {
			env = append(env, "GITHUB_TOKEN="+tok, "GH_TOKEN="+tok)
		}
	}
	rctx, cancel := context.WithTimeout(ctx, spekTimeout)
	defer cancel()
	args := []string{"voice", "--from", "stdin", "--title", title, "--publish", "--repo", repo, "--format", "json"}
	out, err := d.exec(rctx, binary, args, env, stdin)
	if err != nil && isUnknownFlag(out, err) {
		out, err = d.exec(rctx, binary, args[:len(args)-2], env, stdin)
	}
	if err != nil {
		return Result{}, fmt.Errorf("spek_rfc: %w: %s", err, lastLines(out, 5))
	}
	res, perr := ParsePublishOutput(out)
	if perr != nil {
		return Result{}, fmt.Errorf("spek_rfc: %w", perr)
	}
	return res, nil
}

func (d *SpekDestination) prefixes() []string {
	if len(d.Prefixes) > 0 {
		return d.Prefixes
	}
	return DefaultTitlePrefixes
}

func (d *SpekDestination) lookPath(binary string) error {
	if d.LookPath != nil {
		return d.LookPath(binary)
	}
	_, err := exec.LookPath(binary)
	return err
}

func (d *SpekDestination) exec(ctx context.Context, binary string, args, env []string, stdin string) (string, error) {
	if d.Exec != nil {
		return d.Exec(ctx, binary, args, env, stdin)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func isUnknownFlag(out string, err error) bool {
	s := strings.ToLower(out + " " + err.Error())
	return strings.Contains(s, "unknown flag") || strings.Contains(s, "flag provided but not defined") || strings.Contains(s, "unknown shorthand flag")
}

// publishJSON is the `spektacular publish --format json` envelope (see
// spektacular internal/publish.Result).
type publishJSON struct {
	RFCURL   string `json:"rfc_url"`
	Children []struct {
		URL string `json:"url"`
	} `json:"children"`
}

var issueURLRe = regexp.MustCompile(`https://[^\s"'<>)]+/[^\s"'<>)/]+/[^\s"'<>)/]+/(?:issues|pull)/\d+`)

// ParsePublishOutput accepts either the JSON envelope (`rfc_url`,
// `children[].url`) or plain text where the first issue URL is the RFC and
// the rest are children (what `spektacular voice` prints).
func ParsePublishOutput(out string) (Result, error) {
	trimmed := strings.TrimSpace(out)
	if start := strings.Index(trimmed, "{"); start >= 0 {
		var pj publishJSON
		if dec := json.NewDecoder(strings.NewReader(trimmed[start:])); dec.Decode(&pj) == nil && pj.RFCURL != "" {
			res := Result{URL: pj.RFCURL}
			for _, ch := range pj.Children {
				if ch.URL != "" {
					res.Children = append(res.Children, ch.URL)
				}
			}
			return res, nil
		}
	}
	urls := issueURLRe.FindAllString(out, -1)
	seen := map[string]bool{}
	var uniq []string
	for _, u := range urls {
		if !seen[u] {
			seen[u] = true
			uniq = append(uniq, u)
		}
	}
	if len(uniq) == 0 {
		return Result{}, fmt.Errorf("no RFC URL in spektacular output: %s", lastLines(out, 3))
	}
	return Result{URL: uniq[0], Children: uniq[1:]}, nil
}

var agentPromptTmpl = template.Must(template.New("spek").Parse(`Turn the voice recording below into a spek RFC with child issues on {{.Repo}}.

Steps:
1. Ensure the spektacular CLI is available ("go install github.com/hivecommons/spektacular@latest" or a release binary).
2. In the {{.Repo}} clone, run "spektacular init" if .spektacular/ is missing.
3. Create a spec named "{{.Name}}" seeded from the transcript and summary below. Fill it non-interactively using your judgment; list anything unclear under "Open questions".
4. Run: spektacular publish {{.Name}} --repo {{.Repo}} --allow-draft --format json
5. Reply with the rfc_url and the child issue URLs from the JSON output.

Provenance: {{.Provenance}}
Title: {{.Title}}

{{if .Summary}}## Summary
{{.Summary}}

{{end}}## Transcript
{{.Transcript}}
`))

// RenderAgentPrompt is the fallback kick prompt used when the hive image
// lacks the spektacular binary.
func RenderAgentPrompt(it Item, c Content, repo, title string) string {
	transcript := c.Transcript
	if transcript == "" {
		transcript = c.Markdown
	}
	var buf bytes.Buffer
	_ = agentPromptTmpl.Execute(&buf, map[string]string{
		"Repo":       repo,
		"Name":       specName(title),
		"Title":      title,
		"Summary":    c.Summary,
		"Transcript": transcript,
		"Provenance": c.Provenance,
	})
	return buf.String()
}

// specName derives a kebab-case spec name from a title.
func specName(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	name := strings.TrimRight(b.String(), "-")
	if name == "" {
		name = "voice-rfc"
	}
	if len(name) > 60 {
		name = strings.TrimRight(name[:60], "-")
	}
	return name
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
