package firsttimer

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// DefaultDocsBase is where the hive's own contributor docs live.
const DefaultDocsBase = "https://github.com/hivecommons/hive/blob/v5/src/docs"

// Vars is the data a comment template renders.
type Vars struct {
	Candidate
	Links
}

// Marker returns the comment marker so a template can place it.
func (v Vars) Marker() string { return Marker }

// Docs returns DocsBase or the default.
func (v Vars) Docs() string {
	if strings.TrimSpace(v.DocsBase) != "" {
		return strings.TrimRight(v.DocsBase, "/")
	}
	return DefaultDocsBase
}

// Hive is the hive's display name or "this hive".
func (v Vars) Hive() string {
	if strings.TrimSpace(v.HiveID) != "" {
		return v.HiveID
	}
	return "this hive"
}

func (v Vars) origin() string { return strings.TrimRight(strings.TrimSpace(v.Origin), "/") }

// ContributeURL is the hive's contribute landing page (pkg/dashboard
// /contribute), where a visitor signs in with GitHub and sets up a relay.
func (v Vars) ContributeURL() string { return v.origin() + "/contribute" }

// LeaderboardURL is the public contributor leaderboard
// (/contribute/leaderboard).
func (v Vars) LeaderboardURL() string { return v.origin() + "/contribute/leaderboard" }

// ProfileURL is the public dossier permalink the author would get once they
// register (/contribute/dossier/{username}).
func (v Vars) ProfileURL() string {
	return v.origin() + "/contribute/dossier/" + strings.TrimSpace(v.Author)
}

// DefaultTemplate is the welcome comment. It is a text/template over Vars;
// operators override it with governor.first_timer_welcome.template_path.
// The first line must stay the marker so the sweep can find it again.
const DefaultTemplate = `{{ .Marker }}
Hi @{{ .Author }}, thanks for your first pull request to {{ .Repo }} — welcome! This repository is maintained with [hive]({{ .Docs }}/README.md): CI and the review swarm run on their own, and a maintainer or the hive will follow up here.

**You can also let a hive do the contributing for you.** Run your own hive (we call them clankers): a small relay on your machine with the AI coding CLI of your choice, which {{ .Hive }} hands work to. It claims an issue, opens the PR, fixes review feedback and rebases — including while you are offline — and what you get back is:

- a contributor profile on this hive's dashboard ({{ .ProfileURL }}) and a place on the public leaderboard ({{ .LeaderboardURL }});
- trust tiers that advance as your PRs land (newcomer → contributor → trusted) and unlock auto-merge of your green PRs through the contributor lane;
- the hive handles failing checks, reviewer asks and rebases on your behalf, and can adopt a PR you had to step away from (your commits are preserved);
- faster turnaround: the review swarm and CI re-run the moment your relay pushes a fix.

Get started at {{ .ContributeURL }} — [relay setup]({{ .Docs }}/contributor-relay.md#basic-setup) · [trust tiers]({{ .Docs }}/contributor-trust-and-roles.md). Either way: sign your commits with DCO (` + "`git commit -s`" + `) and put ` + "`Fixes #<n>`" + ` in the description so the hive can link this PR to its issue. Thanks again for contributing!
`

// ParseTemplate parses an operator override, or DefaultTemplate when text is
// blank. A template that omits the marker is rejected: without it the sweep
// could not find its own comment and would post again every pass.
func ParseTemplate(text string) (*template.Template, error) {
	if strings.TrimSpace(text) == "" {
		text = DefaultTemplate
	}
	if !strings.Contains(text, ".Marker") && !strings.Contains(text, Marker) {
		return nil, fmt.Errorf("firsttimer: comment template must contain {{ .Marker }} or %q", Marker)
	}
	return template.New("first-timer-welcome").Option("missingkey=error").Parse(text)
}

// RenderComment executes tmpl over v.
func RenderComment(tmpl *template.Template, v Vars) (string, error) {
	if tmpl == nil {
		return "", fmt.Errorf("firsttimer: nil template")
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, v); err != nil {
		return "", err
	}
	out := strings.TrimSpace(buf.String()) + "\n"
	if !strings.Contains(out, Marker) {
		return "", fmt.Errorf("firsttimer: rendered comment lost the marker")
	}
	return out, nil
}
