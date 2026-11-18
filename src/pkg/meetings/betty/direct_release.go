package betty

import (
	"context"
	"net/url"
	"regexp"
	"strings"
)

// Latest releases from the GitHub releases API, unauthenticated. This is a
// public-web call like any other: it is made only for an explicit "latest
// release of <owner/repo or well-known project>" question and never
// carries the hive's GitHub token.

var (
	// releaseOf matches "the latest stable release of kubernetes".
	releaseOf = regexp.MustCompile(`(?i)\b(?:latest|newest|current|most recent|last)\s+(?:stable\s+)?(?:release|version)\s+of\s+(.+)$`)
	// releasePossessive matches "kubernetes' latest release".
	releasePossessive = regexp.MustCompile(`(?i)^(?:what(?:'s|s| is)\s+)?(.+?)(?:'s|')?\s+(?:latest|newest|current|most recent)\s+(?:stable\s+)?(?:release|version)$`)
	// releaseTail are closing words that are not part of the project.
	releaseTail = regexp.MustCompile(`(?i)\s+(?:right now|now|today|currently|at the moment|project|these days)$`)
	// ownerRepo is an explicit owner/repo.
	ownerRepo = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*/[a-z0-9._-]+$`)
)

// wellKnownRepos maps a project as people say it to its GitHub repository.
// Only projects that publish GitHub releases are listed.
var wellKnownRepos = map[string]string{
	"kubernetes": "kubernetes/kubernetes", "k8s": "kubernetes/kubernetes",
	"helm": "helm/helm", "prometheus": "prometheus/prometheus", "grafana": "grafana/grafana",
	"etcd": "etcd-io/etcd", "containerd": "containerd/containerd", "istio": "istio/istio",
	"argo cd": "argoproj/argo-cd", "argocd": "argoproj/argo-cd", "envoy": "envoyproxy/envoy",
	"cilium": "cilium/cilium", "node": "nodejs/node", "node.js": "nodejs/node", "nodejs": "nodejs/node",
	"terraform": "hashicorp/terraform", "docker compose": "docker/compose", "kind": "kubernetes-sigs/kind",
	"minikube": "kubernetes/minikube", "k3s": "k3s-io/k3s", "podman": "containers/podman",
	"rust": "rust-lang/rust", "deno": "denoland/deno", "bun": "oven-sh/bun",
	"flux": "fluxcd/flux2", "linkerd": "linkerd/linkerd2", "typescript": "microsoft/TypeScript",
	"react": "facebook/react", "next.js": "vercel/next.js", "nextjs": "vercel/next.js",
	"opentelemetry collector": "open-telemetry/opentelemetry-collector",
}

// releaseRepo reports whether q asks for a project's latest release, and
// the owner/repo to ask about. A project that is neither an explicit
// owner/repo nor well known is left to the general web search.
func releaseRepo(q string) (repo string, ok bool) {
	var target string
	if m := releaseOf.FindStringSubmatch(q); m != nil {
		target = m[1]
	} else if m := releasePossessive.FindStringSubmatch(q); m != nil {
		target = m[1]
	} else {
		return "", false
	}
	target = strings.ToLower(strings.TrimSpace(target))
	for {
		cut := releaseTail.FindStringIndex(target)
		if cut == nil {
			break
		}
		target = strings.TrimSpace(target[:cut[0]])
	}
	target = strings.TrimSpace(strings.TrimPrefix(target, "the "))
	// Speech-to-text writes the separator out.
	target = strings.ReplaceAll(target, " slash ", "/")
	if ownerRepo.MatchString(target) && !strings.Contains(target, "..") {
		return target, true
	}
	repo, ok = wellKnownRepos[target]
	return repo, ok
}

type releaseResponse struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	PublishedAt string `json:"published_at"`
	HTMLURL     string `json:"html_url"`
}

// dateLen is the length of the date in an RFC 3339 timestamp.
const dateLen = len("2006-01-02")

// release is the latest published release of an owner/repo.
func (d DirectRetriever) release(ctx context.Context, repo string) []Snippet {
	var r releaseResponse
	// GitHub answers this path without credentials; none are sent.
	u := base(d.GitHubAPIURL, defaultGitHubAPIURL) + "/repos/" + repo + "/releases/latest"
	if !d.getJSON(ctx, u, &r) || strings.TrimSpace(r.TagName) == "" {
		return nil
	}
	tag := clip(stripHTML(r.TagName), maxSnippetRunes)
	text := "The latest published release is " + tag
	if name := clip(stripHTML(r.Name), maxSnippetRunes); name != "" && name != tag {
		text += " (" + name + ")"
	}
	if len(r.PublishedAt) >= dateLen {
		text += ", published " + stripHTML(r.PublishedAt[:dateLen])
	}
	text += ". According to the " + SourceGitHubReleases + " API (github.com/" + repo + "/releases)."
	// The page of the release when GitHub gave one, else the repo's list.
	link := "https://github.com/" + repo + "/releases"
	if u, err := url.Parse(strings.TrimSpace(r.HTMLURL)); err == nil && u.Scheme == "https" && u.Host == "github.com" {
		link = u.String()
	}
	return []Snippet{{Source: SourceGitHubReleases, Title: "Latest release of " + repo, Text: text, URL: link}}
}
