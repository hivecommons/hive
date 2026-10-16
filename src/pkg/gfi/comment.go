package gfi

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// Vars is the data a comment template renders.
type Vars struct {
	Candidate
	Links
	// Reasons are the scorer's positive signals (1–2 are quoted).
	Reasons []string
	// AgentsMDURL is the repository's AGENTS.md when it has one.
	AgentsMDURL string
}

// Marker returns the comment marker so a template can place it.
func (v Vars) Marker() string { return Marker }

// ClaimURL is the deep link into this hive's relay for this issue.
func (v Vars) ClaimURL() string { return ClaimURL(v.Origin, v.Repo, v.Number) }

// RelayURL is the hive's relay landing page.
func (v Vars) RelayURL() string { return strings.TrimRight(v.Origin, "/") + "/contribute" }

// Docs returns DocsBase or the default.
func (v Vars) Docs() string {
	if strings.TrimSpace(v.DocsBase) != "" {
		return strings.TrimRight(v.DocsBase, "/")
	}
	return DefaultDocsBase
}

// TopReasons returns at most two reasons.
func (v Vars) TopReasons() []string {
	if len(v.Reasons) > 2 {
		return v.Reasons[:2]
	}
	return v.Reasons
}

// Hive is the hive's display name or "this hive".
func (v Vars) Hive() string {
	if strings.TrimSpace(v.HiveID) != "" {
		return v.HiveID
	}
	return "this hive"
}

// AgentPrompt is the one-line prompt a contributor can hand to their coding
// agent. It carries everything the agent needs to land the fix through the
// contributor lane without the human relaying rules.
func (v Vars) AgentPrompt() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Fix %s in github.com/%s (%s). First open %s and claim the issue through the hive relay (sign in with GitHub). ",
		"#"+strconv.Itoa(v.Number), v.Repo, v.IssueURL(), v.ClaimURL())
	b.WriteString("Work on a fork/branch; keep the change scoped to the issue; sign every commit with DCO (git commit -s); ")
	fmt.Fprintf(&b, "open a PR whose description contains the line 'Fixes #%d' so the hive's contributor lane can auto-merge it once CI is green.", v.Number)
	if v.AgentsMDURL != "" {
		fmt.Fprintf(&b, " Read and follow %s before changing code.", v.AgentsMDURL)
	}
	return b.String()
}

// DefaultTemplate is the welcome comment. It is a text/template over Vars;
// operators override it with good_first_issue.comment_template. The first
// line must stay the marker so the sweep can find and later supersede it.
const DefaultTemplate = `{{ .Marker }}
### 🌱 Good first issue — bring your own coding agent

Hi! This repository is maintained with [hive]({{ .Docs }}/README.md) and welcomes contributions made **with your own AI coding agent** (Claude Code, Codex, Copilot CLI, Bob, …). {{ .Hive }} picked this issue as a good place to start because:
{{ range .TopReasons }}
- {{ . }}{{ end }}

**How to land it (no waiting on a human):**

1. **Claim it through the relay:** open {{ .ClaimURL }} and sign in with GitHub. Claiming stamps the issue so hive leaves it to you.
2. **Fork or branch**, make the change with your agent, and sign each commit with DCO (` + "`git commit -s`" + `).
3. **Open a PR whose description says ` + "`Fixes #{{ .Number }}`" + `** — that closing keyword is how hive's contributor lane links the PR to your claim.
4. Hive runs CI and **merges green PRs from claimed contributors automatically**.

<details>
<summary>Paste this to your agent</summary>

` + "```text" + `
{{ .AgentPrompt }}
` + "```" + `
</details>

If nobody claims it, hive may pick it up itself — claiming first prevents that. Docs: [contributor relay]({{ .Docs }}/contributor-relay.md) · [contributor lane auto-merge]({{ .Docs }}/hive-merge.md#contributor-lane-auto-merge) · [trust tiers]({{ .Docs }}/contributor-trust-and-roles.md){{ if .AgentsMDURL }} · [AGENTS.md]({{ .AgentsMDURL }}){{ end }}.
`

// ParseTemplate parses an operator override, or DefaultTemplate when text is
// blank. A template that omits the marker is rejected: without it the sweep
// could not find its own comment and would post again every pass.
func ParseTemplate(text string) (*template.Template, error) {
	if strings.TrimSpace(text) == "" {
		text = DefaultTemplate
	}
	if !strings.Contains(text, ".Marker") && !strings.Contains(text, Marker) {
		return nil, fmt.Errorf("gfi: comment template must contain {{ .Marker }} or %q", Marker)
	}
	return template.New("gfi").Option("missingkey=error").Parse(text)
}

// RenderComment executes tmpl over v.
func RenderComment(tmpl *template.Template, v Vars) (string, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, v); err != nil {
		return "", err
	}
	out := strings.TrimSpace(buf.String()) + "\n"
	if !strings.Contains(out, Marker) {
		return "", fmt.Errorf("gfi: rendered comment lost the marker")
	}
	return out, nil
}

// SupersededComment is the body the welcome comment is edited to when the
// hive takes the issue itself.
func SupersededComment(c Candidate, reason string, l Links, at time.Time) string {
	hive := l.HiveID
	if strings.TrimSpace(hive) == "" {
		hive = "this hive"
	}
	relay := strings.TrimRight(strings.TrimSpace(l.Origin), "/") + "/contribute"
	return fmt.Sprintf("%s\n~~Good first issue~~ — **superseded**: %s started working on this issue itself (%s, %s), so the `%s` label has been removed. "+
		"Thanks for looking! Other issues open to new contributors are listed on the relay page: %s\n",
		SupersededMarker, hive, reason, at.UTC().Format(time.RFC3339), Label, relay)
}
