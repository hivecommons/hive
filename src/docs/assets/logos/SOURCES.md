# Logo sources and licenses

Every file in this directory is listed here with where it came from and under
which license. Logos are trademarks of their owners and are used for
identification only. See [integrations.md](../../integrations.md).

## Simple Icons (CC0 1.0)

Fetched from [simple-icons/simple-icons](https://github.com/simple-icons/simple-icons) at commit `90646e4bf7f3b89df6c37e65baf3e7bc759a517c`. The SVG files are released under [CC0 1.0](https://github.com/simple-icons/simple-icons/blob/develop/LICENSE.md). The only edit is a neutral `fill="#6b7280"` on the root element so the icon reads on light and dark backgrounds.

| File | Brand | Source URL |
| --- | --- | --- |
| `github.svg` | GitHub | <https://github.com/simple-icons/simple-icons/blob/90646e4bf7f3b89df6c37e65baf3e7bc759a517c/icons/github.svg> |
| `githubcopilot.svg` | GitHub Copilot | <https://github.com/simple-icons/simple-icons/blob/90646e4bf7f3b89df6c37e65baf3e7bc759a517c/icons/githubcopilot.svg> |
| `gitlab.svg` | GitLab | <https://github.com/simple-icons/simple-icons/blob/90646e4bf7f3b89df6c37e65baf3e7bc759a517c/icons/gitlab.svg> |
| `gitea.svg` | Gitea | <https://github.com/simple-icons/simple-icons/blob/90646e4bf7f3b89df6c37e65baf3e7bc759a517c/icons/gitea.svg> |
| `forgejo.svg` | Forgejo | <https://github.com/simple-icons/simple-icons/blob/90646e4bf7f3b89df6c37e65baf3e7bc759a517c/icons/forgejo.svg> |
| `jira.svg` | Jira | <https://github.com/simple-icons/simple-icons/blob/90646e4bf7f3b89df6c37e65baf3e7bc759a517c/icons/jira.svg> |
| `linear.svg` | Linear | <https://github.com/simple-icons/simple-icons/blob/90646e4bf7f3b89df6c37e65baf3e7bc759a517c/icons/linear.svg> |
| `claude.svg` | Claude | <https://github.com/simple-icons/simple-icons/blob/90646e4bf7f3b89df6c37e65baf3e7bc759a517c/icons/claude.svg> |
| `kubernetes.svg` | Kubernetes | <https://github.com/simple-icons/simple-icons/blob/90646e4bf7f3b89df6c37e65baf3e7bc759a517c/icons/kubernetes.svg> |
| `podman.svg` | Podman | <https://github.com/simple-icons/simple-icons/blob/90646e4bf7f3b89df6c37e65baf3e7bc759a517c/icons/podman.svg> |
| `cncf.svg` | CNCF | <https://github.com/simple-icons/simple-icons/blob/90646e4bf7f3b89df6c37e65baf3e7bc759a517c/icons/cncf.svg> |

## Project artwork

Fetched from each project's own repository at the commit named in the URL.
`openshell.svg` and `openbao.svg` are unmodified. `agent-substrate.png` is the
project's `ate-logo.png` resized to 128 x 128 (the upstream SVG is about
95 KB, over the 20 KB limit below).

| File | Project | License | Source URL |
| --- | --- | --- | --- |
| `openshell.svg` | OpenShell (project mark, not the NVIDIA corporate logo) | [Apache-2.0](https://github.com/NVIDIA/OpenShell/blob/2e5589b2180dd432320adda124c4f7bcede119b5/LICENSE) | <https://github.com/NVIDIA/OpenShell/blob/2e5589b2180dd432320adda124c4f7bcede119b5/docs/brand/assets/openshell-mark.svg> |
| `agent-substrate.png` | Agent Substrate ("Yolo", designed and donated by Nan Hockin) | [Apache-2.0 OR CC-BY-4.0](https://github.com/agent-substrate/substrate/blob/cf5768a075b741511829a207bdfb4e296d4114c7/logo/LICENSE), used here under CC-BY-4.0 | <https://github.com/agent-substrate/substrate/blob/cf5768a075b741511829a207bdfb4e296d4114c7/logo/ate-logo.png> |
| `openbao.svg` | OpenBao | [CC-BY-4.0](https://github.com/openbao/artwork/blob/2387118edc846d2ec82ffd72409a3e0fe3be8519/LICENSE) | <https://github.com/openbao/artwork/blob/2387118edc846d2ec82ffd72409a3e0fe3be8519/color/openbao-color.svg> |

The same three files are served on the hub landing page from
`src/pkg/hub/static/integrations/`, next to copies of `podman.svg` and
`kubernetes.svg` from the Simple Icons table above.

## Text badges (generated, no third-party asset)

No clearly redistributable logo was found for the projects below, or the brand
guidelines do not permit redistribution. Each file is a small SVG generated in
this repository (rounded rectangle plus the project name). It contains no
third-party artwork and is covered by the repository license.

| File | Project | Note |
| --- | --- | --- |
| `goose-badge.svg` | Goose | no redistributable logo found |
| `antigravity-badge.svg` | Google Antigravity | no redistributable logo found |
| `codex-badge.svg` | OpenAI Codex | no redistributable logo found |
| `openai-agents-badge.svg` | OpenAI Agents API | no redistributable logo found |
| `omnigent-badge.svg` | Omnigent | no redistributable logo found |
| `opensandbox-badge.svg` | AAIF OpenSandbox | no redistributable logo found |
| `gateway-badge.svg` | OpenAI-compatible gateways (vLLM, llm-d, LiteLLM, watsonx, OpenRouter) | no redistributable logo found |
| `bob-badge.svg` | IBM Bob | no redistributable logo found |
| `pi-badge.svg` | Pi | no redistributable logo found |
| `aider-badge.svg` | Aider | no redistributable logo found |
| `kilo-badge.svg` | Kilo Code | no redistributable logo found |
| `muse-badge.svg` | Muse Code | no redistributable logo found |
| `opencode-badge.svg` | OpenCode | no redistributable logo found |
| `omp-badge.svg` | Oh My Pi | no redistributable logo found |
| `openhands-badge.svg` | OpenHands | no redistributable logo found |
| `gemini-badge.svg` | Gemini | no redistributable logo found |
| `gh-aw-badge.svg` | GitHub Agentic Workflows | no redistributable logo found |
| `keystore-badge.svg` | Hive built-in keystore | Hive feature, no third-party project |

## Rules for adding a logo

- Prefer the project's own brand page or a Simple Icons SVG (CC0).
- Record the exact source URL and license here in the same PR.
- Keep each SVG under 20 KB with no scripts, external references, or embedded images. Use a small PNG (128 px) when the upstream SVG is larger.
- If no redistributable asset exists, add a text badge instead of copying a logo.
