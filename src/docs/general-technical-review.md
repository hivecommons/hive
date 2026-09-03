# General Technical Review — Hive

This document answers the CNCF [General Technical Review
questions](https://github.com/cncf/toc/blob/main/toc_subprojects/project-reviews-subproject/general-technical-questions.md)
(v1.0.1) for the `kubestellar/hive` project, following that template's exact
question wording and section order. It is written from, and cited against,
this repository at `v4` (`src/` prefix for source and most docs).

## Summary

- **Questions answered from the repository: 73**
- **Questions marked `[NEEDS OPERATOR INPUT]`: 0**

Every question below is reproduced verbatim as a heading. Substantive claims
cite a `file:line`, a doc, or a named CI workflow. Several answers are
deliberately plain "the project does not do this" statements rather than
padded ones — that is the accurate answer, not an omission.

### Known gaps

These are the real, standing gaps this review surfaced. They are stated
plainly in the relevant answers below rather than hidden behind a marker:

- **No SLOs/SLIs** — the project defines no project-wide Service Level
  Objectives or Indicators, has run no controlled load test, and publishes
  no recommended capacity limits ([SLO/SLI](#describe-how-the-project-defines-service-level-objectives-slos-and-service-level-indicators-slis), [load testing](#describe-the-load-testing-that-has-been-performed-on-the-project-and-the-results), [limits](#describe-the-recommended-limits-of-users-requests-system-resources-etc-and-how-they-were-obtained)).
- **No formal compliance certification** — no SOC 2, FedRAMP, or other
  formal certification is pursued or claimed ([compliance](#describe-any-compliance-requirements-addressed-by-the-project)).
- **No security-response on-call or diversity target** — the process is now
  documented in [`security-response.md`](security-response.md), including who
  responds and how membership changes. What it deliberately does not define:
  an on-call rotation, a CVSS rubric beyond the 60-day fix commitment, or a
  diversity target. Response depends on whichever of three part-time
  maintainers sees a report first
  ([diversity](#how-does-the-project-ensure-its-security-reporting-and-response-team-is-representative-of-its-community-diversity-organizational-and-individual), [rotation](#how-does-the-project-invite-and-rotate-security-reporting-team-members)).
- **No third-party audit** — no independent security audit of the project
  has been performed or published.

---

# General Technical Review - Hive / Sandbox

- **Project:** [kubestellar/hive](https://github.com/hivecommons/hive)
- **Project Version:** Continuously delivered from branch `v4` (`v4-latest` / `stable` / `candidate` / `edge` channel tags plus automated semver `vX.Y.Z` releases — see [Release processes](#describe-the-projects-release-processes-including-major-minor-and-patch-releases))
- **Website:** [hive.kubestellar.io](https://hive.kubestellar.io)
- **Date Updated:** 2026-08-28
- **Template Version:** v1.0.1
- **Description:** Hive orchestrates fleets of AI coding agents (Claude, GitHub Copilot, Gemini, Goose, Bob, Agy) that autonomously maintain software projects — filing issues, opening pull requests, reviewing code, and, at the highest operator-selected autonomy level, merging on green CI — under deterministic, technically-enforced guardrails rather than prompted behavior. It runs as a single-container Kubernetes/Compose/Podman workload, with an optional hub coordinating many self-hosted "spoke" hives.

## Day 0 - Planning Phase

### Scope

#### Describe the project's vision and goals.

The goal is to assist maintainers in keeping their projects relevant and current. Many projects have one to three maintainers; most have one. Hive keeps maintenance going strong while maintainers do the higher-order things needed to gain adoption — adding new features, speaking at conferences, onboarding new users.

#### Describe the primary use cases for the project. Describe any additional use cases the project supports. Describe any use cases specifically out of scope of the project.

Documented, factual use case: running a fleet of AI coding agents against one or more GitHub (or GitHub Enterprise/GitLab/Gitea) repositories to triage issues, open PRs, review code, and — at higher autonomy — merge, with a human-controlled ACMM autonomy dial and layered enforcement (`src/docs/architecture.md:1-12`). Two named production adopters are documented as end users today: the KubeStellar Console team (`kubestellar/console`) and Project Bluefin (`projectbluefin`), each running their own spoke hive (`src/docs/cncf-reference-architecture.md` "Describe your organisation" section).

Deliberately **out of scope**:

- **Writing net-new features.** Hive maintains existing code — triage, fixes, tests, docs, dependency updates. It is not intended for greenfield development from a spec.
- **Replacing human review.** The ACMM autonomy levels and merge gates exist so a human sets policy for how much an agent is trusted to do unsupervised; zero human oversight is not a goal of the project.

#### Describe the roadmap process, how scope is determined for mid to long term features, as well as how the roadmap maps back to current contributions and maintainer ladder?

The public roadmap (`src/docs/roadmap.md`) is organized as **Now / Next / Later** — near-term in-flight work, likely follow-ups, and longer-horizon bets, in planning order rather than priority rank (roadmap.md "Reading this roadmap"). It states explicitly it is "directional, not a promise," "maintained by pull request," and that closed umbrella issues (`#2812`, `#2811`) are cited as the origin of a line of work, not as live trackers, because a closed umbrella says nothing about whether any particular item under it shipped (roadmap.md header). Contribution flows into the roadmap through the same GitHub issue/PR process as any other change (`GOVERNANCE.md` "Decision process": proposals opened as issues/PRs, public discussion, lazy consensus). The [KubeStellar Contributor Ladder](https://github.com/kubestellar/community/blob/main/CONTRIBUTOR_LADDER.md) governs how contributors earn responsibility (`GOVERNANCE.md` "Contributor ladder"), but the roadmap itself does not name a formal scoring/prioritization rubric tying specific ladder rungs to specific roadmap decisions.

#### Describe the target persona or user(s) for the project?

Three target personas: **OSS maintainers** — individuals or small teams self-hosting Hive for their own repositories; **enterprise development organizations** — running Hive internally against closed-source repositories as well as open-source ones, where closed-source support matters; and **foundations / multi-project organizations** — running Hive across a portfolio of projects under a shared policy. The repository documents *actors* in the security sense (repository maintainer/hive operator, AI agents, public GitHub users, contributors, hub operators, dashboard users — `src/docs/security-self-assessment.md` "Actors"), which maps onto these personas but does not itself state them as a product decision.

#### Describe the intended types of organizations who would benefit from adopting this project. (i.e. financial services, any software manufacturer, organizations providing platform engineering services)?

Intended adopter organization types: **software manufacturers**; **platform engineering organizations**; and **foundations and distributions**. Small-team OSS projects are the motivating case behind the project's vision (see [vision and goals](#describe-the-projects-vision-and-goals) above — most projects have one to three maintainers), even though these three organization types are the ones the project targets for adoption. The two documented production users (KubeStellar Console, Project Bluefin) are both open-source infrastructure projects, consistent with this.

#### Please describe any completed end user research and link to any reports.

No formal study or structured survey/interview program has been conducted. Real, informal adopter feedback exists in the form of issues filed by adopters running their own hives, and it is the closest thing the project has to end-user research today: [#4918](https://github.com/hivecommons/hive/issues/4918) is a contributor's incident report, with journal evidence, of the default unconfined agent-launch path reaching their host's bootloader; [#4928](https://github.com/hivecommons/hive/issues/4928) and [#4929](https://github.com/hivecommons/hive/issues/4929) were filed by `ahmedadan` from operating `projectbluefin/dakota`; and [#4971](https://github.com/hivecommons/hive/issues/4971) and [#4973](https://github.com/hivecommons/hive/issues/4973) were filed by `Danathar` from running a hive. The CNCF adopter interviews conducted as part of the Incubation application will be the first structured end-user research the project has done.

### Usability

#### How should the target personas interact with your project?

Operators interact through: the web dashboard (`http://<host>:3001`, session/token-authenticated — `src/docs/architecture.md:329-346`), a CLI client (`hivectl`, `src/docs/hivectl.md`), a web terminal onto live agent tmux sessions (`ttyd` on `:7681`, reached authenticated via `:3001/terminal` — `src/docs/architecture.md:53-73`), and `hive.yaml` configuration edited directly or through the dashboard's Governor Config UI (`src/docs/config-layering.md`). Contributors interact via the ClankeR contributor relay, donating compute to a spoke's agent queue without their credentials ever leaving their machine (`src/docs/architecture.md:286-305`, `src/docs/contributor-relay.md`).

#### Describe the user experience (UX) and user interface (UI) of the project.

The dashboard is a server-rendered/inline-JS web UI (no separate SPA build — `src/docs/security-self-assessment.md:33`) showing live fleet status pushed via Server-Sent Events (`GET /api/events`, one push per governor eval cycle — `src/docs/architecture.md:340-341`), per-agent cards with self-healing watchdog conditions (`Ready`/`Authenticated`/`Producing` — `src/docs/agent-watchdog.md` "Conditions"), a cost/token view (`src/docs/token-tracking.md`), an audit log view, a Governor Config settings surface, and a public read-only `/snapshot` page for sharing status without dashboard access (`src/docs/snapshots.md`). A `hivectl` CLI and a full REST API (`src/docs/api-reference.md`, machine-readable OpenAPI at `dashboard/openapi.json`) provide non-UI access.

#### Describe how this project integrates with other projects in a production environment.

Hive integrates with: **GitHub / GitHub Enterprise / GitLab / Gitea** as the managed source-control system, via a GitHub App or PAT and, going forward, the forge abstraction (`pkg/forge`, `src/docs/adr/0005-forge-abstraction.md`); **AI CLI backends** — Claude Code, GitHub Copilot CLI, Gemini, Goose, Bob, Agy — run as subprocesses (`src/docs/architecture.md:9`, `src/docs/agent-configuration.md`); **self-hosted inference gateways** — vLLM, llm-d, LiteLLM, OpenRouter, watsonx — reached through an in-pod credential-translating proxy (`src/docs/security-model.md` Layer 3); **cert-manager** and **nginx-ingress** for Kubernetes TLS termination and routing (README.md "Kubernetes Deployment" prerequisites); **Prometheus**, via an opt-in `/metrics` scrape endpoint (`src/docs/security-model.md` "Metrics endpoint"); and **notification channels** — ntfy, Slack, Discord (`src/docs/notifications.md`). It is part of the KubeStellar CNCF Sandbox project family (`src/docs/security-self-assessment.md` "Related projects / vendors").

### Design

#### Explain the design principles and best practices the project is following.

The stated core design rule, quoted verbatim: *"if a human would give the same answer every time, it belongs in infrastructure, not in a prompt"* (`src/docs/security-model.md:5`). Architecturally this becomes a separation between **deterministic decisions** (filtering, classification, merge-gating, permission enforcement — run in Go/shell before any LLM sees a task) and **judgment calls** (reading code, reasoning about a fix, writing a PR), with agents only handling the latter (`src/docs/architecture.md:9-12`). Enforcement is explicitly layered defense-in-depth: three independent guardrail layers (CLI tool-deny, scoped credentials, MITM network proxy) share one mode signal so "a bug in one layer is caught by the next" (`src/docs/architecture.md:185-190`). The project also runs a lightweight Architecture Decision Record process (17 ADRs at time of writing, `src/docs/adr/README.md`) to keep durable decisions auditable rather than tribal knowledge.

#### Outline or link to the project's architecture requirements? Describe how they differ for Proof of Concept, Development, Test and Production environments, as applicable.

The full reference architecture is at `src/docs/architecture.md` (process model, governor loop, deterministic pipeline, layered guardrails, ACMM, beads ledger, hub/spoke, model backends, dashboard/observability). No formal PoC/Dev/Test/Prod environment tiers are named, but three deployment runtimes carry different guarantees:

- **Docker Compose** — the default standalone runtime (README.md "Quick Start").
- **Podman** — a "parallel supported choice, not an experiment," via systemd Quadlet units, in both rootful and rootless modes (README.md "Quick Start"; `src/docs/adr/0017-podman-quadlet-lifecycle.md`). Rootless Podman is explicitly **unsupported for enforcing deployments**, because it cannot meaningfully grant `CAP_NET_ADMIN` and the egress gate falls back to advisory-only (`src/docs/security-model.md` "Rootless podman").
- **Kubernetes** — the production/hosted path, single-replica Deployment with PVC, Service, and Ingress (`src/docs/cncf-reference-architecture.md`, `src/deploy/k8s/`).

Image-tag guidance further differentiates environments: `stable` or a pinned digest for production, `v4-latest` for tracking mainline, an immutable short-SHA tag for incident reproduction (`src/docs/operator-reference.md#image-provenance-and-tags`, `src/docs/release-channels.md`).

#### Define any specific service dependencies the project relies on in the cluster.

None required beyond the hive pod itself and its PVC. Hive is self-contained: no etcd, external database, or cache is required — all durable state lives on one PVC as flat files (JSON beads ledgers, config overlays, a SQLite-style knowledge graph at `/data/graph/knowledge.db`; full table in `src/docs/architecture.md` "Data stores at a glance"). The Kubernetes manifest set confirms this — only `namespace.yaml`, `deployment.yaml`, `service.yaml`, `pvc.yaml`, `configmap.yaml`, `secret.yaml`, `dashboard-route-rbac.yaml`, and a backup `CronJob` exist under `src/deploy/k8s/`; no Deployment/StatefulSet for a database is shipped. Optional in-cluster or external dependencies exist only if configured by the operator: an inference-gateway Service (vLLM/llm-d/LiteLLM), or OCI Object Storage for disaster-recovery backups (`src/docs/network-requirements.md`, `src/docs/backup-restore.md`).

#### Describe how the project implements Identity and Access Management.

Two IAM lanes depending on topology, both detailed in `src/docs/security-model.md`:

- **Hub-proxied hives** (Layer 1): multi-provider SSO at the hub (GitHub OAuth, Google, IBMid, Red Hat, Microsoft Entra ID, generic OIDC), `HttpOnly`/`Secure`/`SameSite=Lax` **Ed25519-signed** session cookies (the legacy HMAC lane was deleted on v4), per-user-per-hive authorization enforced on every request via an nginx `auth_request` check, and identity injected downstream as `X-Hive-User`/`X-Hive-Role` headers with a role of `read`/`read-write`/`merger`/`owner`.
- **Direct-route spokes** (Layer 2, for clusters the hub cannot proxy): GitHub device-flow login validated against a per-hive allowlist (`HIVE_AUTHORIZED_USERS`), opaque 256-bit per-user sessions that never store the user's GitHub token, and forged `X-Hive-User`/`X-Hive-Role` headers stripped by the outermost middleware since there is no trusted hub in front.

A separate coarse shared secret, `HIVE_DASHBOARD_TOKEN`, gates the dashboard/API as a whole on non-direct-route deployments and is not a per-user identity mechanism (`src/docs/env-vars.md` "Generating and rotating `HIVE_DASHBOARD_TOKEN`"). Hub↔spoke channel authentication uses per-hive derived keys with no HMAC fallback (`security-model.md` "Sessions and SSO are Ed25519-only").

#### Describe how the project has addressed sovereignty.

Hive is self-hosted and runs in the operator's own cluster or host — there is no hosted multi-tenant plane holding adopter data. Data residency and jurisdiction therefore follow the operator's own infrastructure placement by construction, as a design property of the deployment model rather than a policy statement the project makes on the operator's behalf. What does leave the operator's environment, by design: agent CLI traffic to configured model providers (Anthropic, GitHub Copilot, Gemini, or any OpenAI-compatible gateway), and forge API calls to GitHub/GitHub Enterprise/GitLab/Gitea — both catalogued in `src/docs/network-requirements.md` ("Outbound egress") and enforced/inspectable via the in-pod MITM policy proxy (`src/docs/security-model.md` Layer 3, Layer 5). The only region-shaped configuration in the repository is the OCI Object Storage region for the optional disaster-recovery backup CronJob (`src/docs/env-vars.md`, `src/docs/backup-restore.md`), which an operator chooses and controls like any other infrastructure placement decision.

#### Describe any compliance requirements addressed by the project.

`src/docs/security-self-assessment.md` "Project compliance" states plainly: Hive holds no formal certification (FIPS, Common Criteria, SOC 2) and makes no claim to one. What it does have: a weekly **OpenSSF Scorecard** run (`.github/workflows/scorecard.yml`, no score floor gated in CI), **DCO** enforcement for human contributors (`.github/workflows/copilot-dco.yml`) and as stated policy for agent-authored commits (`git commit -s`), and the **Apache License 2.0** (OSI-approved, CNCF-preferred). The **OpenSSF Best Practices Badge** has since been earned at the **Passing** level with 100% completion across all five categories (Basics 13/13, Change Control 9/9, Reporting 8/8, Quality 13/13, Security 16/16, Analysis 8/8) — [bestpractices.dev/projects/14261](https://www.bestpractices.dev/projects/14261) — which supersedes the self-assessment document's earlier "not yet pursued" note.

None. No SOC 2, no FedRAMP, no HIPAA, and no other formal compliance certification is pursued or claimed beyond what is already stated above (OpenSSF Scorecard, OpenSSF Best Practices Badge, DCO, Apache 2.0).

#### Describe the project's High Availability requirements.

Hive does **not** support HA/multi-replica operation today. `src/deploy/k8s/deployment.yaml` sets `replicas: 1` with `strategy: { type: Recreate }` (a restart window on rollout, not rolling update). `src/docs/security-model.md` Layer 7 states provisioning "creates a dedicated `hive-hosted-<id>` namespace with its own PVC and a **single-replica** Deployment," and `src/docs/cncf-reference-architecture.md` describes "a single-replica Kubernetes Deployment with a PVC, Service, and Ingress." One partial exception: hosted-hive image upgrades use `maxSurge=1`/`maxUnavailable=0` against a ReadWriteMany PVC for a zero-downtime *rollout* (security-model.md Layer 7), but this is an upgrade mechanism, not steady-state redundancy — there is still exactly one logical replica per hive at any given moment.

#### Describe the project's resource requirements, including CPU, Network and Memory.

`src/deploy/k8s/deployment.yaml` sets resource **requests**: `cpu: 500m`, `memory: 512Mi`; **limits**: `cpu: "2"`, `memory: 2Gi` (also stated in README.md "Kubernetes Deployment": "Resource defaults: 500m CPU / 512Mi memory (requests), 2 CPU / 2Gi memory (limits)"). Network port/egress requirements are catalogued in `src/docs/network-requirements.md` (inbound dashboard/API/terminal ports, outbound to GitHub/model backends). No documented network bandwidth (throughput) requirement exists.

#### Describe the project's storage requirements, including its use of ephemeral and/or persistent storage.

**Persistent**: one PVC, default `ReadWriteOnce` 10Gi (`src/deploy/k8s/pvc.yaml`), mounted at `/data`, holding all durable state — beads ledgers, config overlays, the knowledge graph, secrets/CA material (`src/docs/architecture.md` "Data stores at a glance"). For zero-downtime rolling upgrades an NFS-backed `ReadWriteMany` StorageClass is recommended instead (README.md "Kubernetes Deployment" step 4). **Ephemeral**: agent scratch state under `/tmp`, `/run`, and per-agent `$HOME` directories; the container deliberately does not set `readOnlyRootFilesystem`/`runAsNonRoot` because entrypoint privilege-drop operations and agent scratch writes need a writable root at boot (`src/deploy/k8s/deployment.yaml` securityContext comments).

#### Please outline the project's API Design:

##### Describe the project's API topology and conventions

REST + Server-Sent Events for the dashboard/hub surface — no GraphQL is exposed by Hive itself (GraphQL only appears as a classification target for *inbound* GitHub traffic the MITM proxy inspects, not as Hive's own API shape). Three distinct API surfaces exist: (1) the **dashboard/hub API** under `/api/*`, session/token-authenticated, documented endpoint-by-endpoint in `src/docs/api-reference.md` and machine-readably in `dashboard/openapi.json` (`info.title: "KubeStellar Hive API"`); (2) the **hub API** (`pkg/hub` routes: `/api/heartbeat`, `/api/saas/*` admin/SaaS endpoints); (3) outbound **GitHub REST + GraphQL** calls made through the forge abstraction (`src/docs/adr/0005-forge-abstraction.md`) and mediated by the MITM policy proxy (`src/docs/adr/0002-mitm-proxy-network-enforcement.md`). Internally, the Node front-door proxy rewrites `/foo` → `/api/foo` and injects an `X-Hive-Internal` header before forwarding to the Go API (`src/docs/architecture.md:83`). A separate `/api/v1/*` surface serves the contributor relay, authenticated with the contributor's own GitHub PAT plus an allowlist check (`src/docs/api-reference.md` "Auth and external accounts").

##### Describe the project defaults

Selected defaults from `src/pkg/config/config.go` `applyDefaults()` (`config.go:4320-4602`) and `src/docs/operator-reference.md`: dashboard port `3002` (`defaultDashboardPort`); governor eval interval `300s` (`defaultEvalIntervalS`); per-agent `replicas` default `1`, `enabled` default `true`, `clear_on_kick` default `true`; `hub.url` defaults to `https://hive.kubestellar.io` with `hub.is_public: true` if unset; token-budget period `7` days at a `90%` critical threshold; auto-merge label default `"lgtm"`. Minimum required config for the process to start at all (enforced by `Config.Validate`): `project.org`, at least one repo, one GitHub credential (`github.token`, `github.app_id`, or `github.forge`), and at least one agent (`src/docs/operator-reference.md` "Minimum required configuration").

##### Outline any additional configurations from default to make reasonable use of the project

For an internet-reachable, production-grade install beyond the documented minimum, operators should additionally set: `HIVE_DASHBOARD_TOKEN` (a CSPRNG-generated shared secret — without it the dashboard API is effectively unauthenticated on non-direct-route spokes, `src/docs/env-vars.md`); TLS termination at an ingress/proxy in front of Hive, since Hive itself terminates no TLS (`src/docs/tls-setup.md`); a `CAP_NET_ADMIN` capability grant so the MITM egress gate can install its iptables redirect (without it the deployment either refuses to start, or must be run with `HIVE_PROXY_ADVISORY_OK=true`, which downgrades enforcement to advisory-only — `src/docs/security-model.md` "Forced proxy egress and `CAP_NET_ADMIN`"); and a **GitHub App** rather than a PAT, for least-privilege, per-tier scoped tokens (`src/docs/security-model.md` Layer 5, `src/docs/github-app-setup.md`).

##### Describe any new or changed API types and calls - including to cloud providers - that will result from this project being enabled and used

GitHub REST + GraphQL calls: issue/PR reads, comment/label writes, PR creation, reviews, and merges, scoped by the operator-selected ACMM tier (`src/docs/operator-reference.md` GitHub App/PAT permission table: Contents, Pull requests, Issues, Checks, Commit statuses, Metadata). Model-backend calls to Anthropic, GitHub Copilot, or any OpenAI-compatible gateway (`src/docs/architecture.md:313-318`). Optional: Linear API calls if a Linear work source is configured (`src/docs/env-vars.md`, `src/docs/linear-agent.md`); OCI Object Storage API calls for disaster-recovery backups; outbound notification webhooks (ntfy/Slack/Discord). **No new Kubernetes CRDs are introduced** — Hive is a stock `Deployment`/`Service`/`PVC`/`ConfigMap`/`Secret`/`CronJob` workload plus RBAC to read Ingress/Route objects in its own namespace (`src/deploy/k8s/`).

##### Describe compatibility of any new or changed APIs with API servers, including the Kubernetes API server

Hive is a **workload**, not a Kubernetes API extension: it ships no CRDs, admission webhooks, or aggregated API server, and does not run a controller/operator reconciliation pattern against the cluster. Its own calls *to* the Kubernetes API server are narrow and self-scoped: reading its own Deployment to report its running image tag, listing Ingress/Route objects in its own namespace for dashboard health reporting (`src/docs/health-checks.md`, `src/deploy/k8s/dashboard-route-rbac.yaml`), and — hub-side only — provisioning spoke namespaces/PVCs/Deployments via `kubectl`/client-go against reachable clusters (`src/docs/architecture.md` "Hub & spoke"; `src/docs/security-model.md` "roughly 6 hours for a ~70-spoke fleet" — 3 Deployment patches per 15-minute reconcile cycle). RBAC granted to the hive's own ServiceAccount is least-privilege and namespace-scoped (`security-model.md` Layer 7).

##### Describe versioning of any new or changed APIs, including how breaking changes are handled

The dashboard/hub REST API itself carries **no formal version scheme** for its surface as a whole — `src/docs/api-reference.md` states it is a hand-maintained index with "no generator," kept current by updating the doc in the same PR that adds or renames a route. The one namespaced exception is the contributor relay surface at `/api/v1/*`. Product/image versioning instead runs through **release channels** (`stable`/`candidate`/`edge` moving tags — `src/docs/release-channels.md`) plus automated **semver tagged releases** (see below). Config-schema evolution is handled additively: `src/docs/migration-v2-v4.md` documents, verified by diffing branches and loading a v2 config through the v4 loader, that v4 adds keys but renames or removes none — a v2 `hive.yaml` loads on v4 unmodified.

#### Describe the project's release processes, including major, minor and patch releases.

Two layers, both automated with no human tagging step in the normal path (`src/docs/releases.md`):

1. **Continuous delivery** — every merge to `v4` publishes moving image tags (`v4-latest`, the three channel tags, and an immutable short-SHA tag) via `.github/workflows/docker.yml`.
2. **Tagged semver releases** — `.github/workflows/tagged-release.yml` runs after every successful `docker.yml` build on `v4` and reads `CHANGELOG.md`'s `## Unreleased` section: an empty section means no release; a non-empty section triggers a release, with the bump inferred from which subsections are present — `### Security` → **major**, else `### Added` → **minor**, else (`### Changed`/`### Fixed`/`### Deprecated`) → **patch** (`releases.md` "What triggers a release"). A `<!-- release: none|major|minor|patch -->` HTML-comment marker is the human escape hatch when inference would be wrong. `tagged-release.yml` never rebuilds — it retags the already-published, freshness-verified digest with `docker buildx imagetools create`, so the versioned image is byte-identical to the commit's `v4-latest`/short-SHA image, and generates a per-image SPDX JSON SBOM (Syft) attached to the GitHub Release (`releases.md` "How a release is actually built", "Software bill of materials (SBOM)").

### Installation

#### Describe how the project is installed and initialized, e.g. a minimal install with a few lines of code or does it require more complex integration and configuration?

Three supported installation paths, all documented in the root `README.md`:

1. **Docker Compose** (default): `git clone`, copy `src/hive.yaml.example` to `src/hive.yaml`, set `HIVE_GITHUB_TOKEN` and a CSPRNG-generated `HIVE_DASHBOARD_TOKEN` in `src/.env`, then `docker compose -f src/docker-compose.yaml up -d` — five commands (README.md "Quick Start (Docker Compose)").
2. **Podman**: a single script, `bin/hive-podman-setup.sh --rootless` (or `--rootful`), performs preflight checks, configuration, installs four systemd Quadlet units, wires boot persistence, and confirms the gateway answers on its published port before returning (README.md "Quick Start (Podman)").
3. **Kubernetes**: `kubectl apply` of manifests under `src/deploy/k8s/` (Namespace, Secret, ConfigMap from `hive.yaml`, PVC, Deployment, Service, Ingress), or the equivalent Kustomize base (README.md "Kubernetes Deployment").

The minimum working `hive.yaml` needs only four things — `project.org`, at least one repo, one GitHub credential, and one agent block — with every other setting (governor cadences, knowledge, notifications, gateways, hub) optional and defaulted (`src/docs/operator-reference.md` "Minimum required configuration").

#### How does an adopter test and validate the installation?

Health endpoints back every deployment path: `/api/health` (basic probe), `/api/health/deep` (deep probe), and `/api/livez` (Kubernetes liveness — deliberately checks the HTTP server *and* the eval/heartbeat loop, not just process-up, so a wedged-but-listening process is still caught — `src/docs/architecture.md` "Dashboard & observability"). The Compose quick start explicitly instructs verifying end-to-end rather than assuming the port answers: `curl -sf http://127.0.0.1:3001/api/health` → `{"status":"ok"}` (README.md). The Kubernetes manifest wires all three probe types (`startupProbe`, `livenessProbe`, `readinessProbe`) against those same endpoints on the dashboard port (`src/deploy/k8s/deployment.yaml`). The Podman path's Quadlet health check probes the same ports, and `bin/hive-podman-setup.sh` itself blocks until the gateway answers before returning success (README.md, `src/docs/podman-standalone-quadlet.md`). Live config correctness (which layer — seed, dashboard overlay, runtime — is winning per field, and whether an overlay was rejected as corrupt) is inspectable via `GET /api/config/provenance` (`src/docs/config-layering.md`).

### Security

#### Please provide a link to the project's cloud native security self assessment.

[`src/docs/security-self-assessment.md`](security-self-assessment.md) — a complete CNCF TAG-Security-format self-assessment submitted as part of Hive's CNCF Incubation application, covering metadata, actors/actions/goals, critical security components with file/line citations, project compliance, secure development practices, vulnerability response process, and the three most significant known weaknesses stated plainly (see excerpts throughout this Security section).

#### Please review the Cloud Native Security Tenets from TAG Security.

##### How are you satisfying the tenets of cloud native security projects?

Hive's posture is documented across three companion pages that together cover the tenets: [security-model.md](security-model.md) (operator/evaluator view — seven enforcement layers, each with file/line evidence), [security-threat-model.md](security-threat-model.md) (attacker view — assets, trust boundaries, threat actors, defense-layer mapping, residual risks), and [security.md](security.md) (log-scrubbing/secret-redaction implementation). Concretely: least-privilege by default (per-tier scoped GitHub App tokens, `security-model.md` Layer 5); defense-in-depth (three independent enforcement layers keyed to one mode signal, `architecture.md` §5); fail-safe defaults where it matters most (hard-denied REST PR-create/merge routes for every agent mode regardless of prompt, `security-self-assessment.md` "Critical security components" #1); supply-chain pinning (digest-pinned base images, SHA-pinned CI actions and tool downloads, `--ignore-scripts` on all AI-CLI npm installs, `security-model.md` "Supply chain"); and automated vulnerability scanning (`govulncheck`, `gosec`, `golangci-lint` in `.github/workflows/go-security-analysis.yml`, weekly plus on every PR).

##### How do you recommend users alter security defaults in order to "loosen" the security of the project? Please link to any documentation the project has written concerning these use cases.

Every documented "loosening" path is named explicitly rather than left implicit, and each carries a stated cost:

- **`HIVE_PROXY_ADVISORY_OK=true`** — starts a spoke without `CAP_NET_ADMIN`, with GitHub-egress enforcement advisory-only (agents can bypass the MITM proxy); logs a `WARN` stating exactly that (`src/docs/security-model.md` "Forced proxy egress and `CAP_NET_ADMIN`"; `src/docs/net-admin-requirement.md`).
- **`ioscan.fail_mode: closed`** vs. the **default `open`** — closed blocks Critical injection kicks and canary leaks instead of redacting and continuing (`src/docs/ioscan.md` "Configuration"); the default is the looser setting.
- **Raising the ACMM level** (L1→L6) — the single documented, intended "loosen autonomy" lever, always a human decision, with each level's effect on agent permissions tabulated (`src/docs/acmm-policy-matrix.md`, `architecture.md` §6).
- **Rootless Podman** — cannot enforce the egress gate at all; documented as effectively unsupported for enforcing deployments (`security-model.md` "Rootless podman").
- **`agent_sandbox` left unconfigured** (the default) — the tmux execution path runs with no per-run container/microVM isolation; `src/docs/sandbox-isolation.md` documents the opt-in Podman sandbox and states plainly that on the default path "there is no container: the backend CLI runs as the operator's own user, on the operator's own host, with nothing scoping its filesystem access to the workspace," citing incident `#4918` where a benign test run escaped its stubs and issued a real deployment command.

#### Security Hygiene

##### Please describe the frameworks, practices and procedures the project uses to maintain the basic health and security of the project.

CI runs three independent static/vulnerability jobs on every PR and push to `v4` plus a weekly cron (`.github/workflows/go-security-analysis.yml`): **govulncheck** (reachability-based known-vulnerability scanning against `golang.org/x/vuln`), **gosec** (insecure-pattern detection — command injection, weak randomness, unhandled security-relevant errors, file permissions), and **golangci-lint** (correctness/maintainability). `govulncheck` and `golangci-lint` block merge; `gosec` currently runs with `continue-on-error: true` — visible on every PR but non-blocking — explicitly pending resolution of a pre-existing HIGH-severity finding backlog under issue `#4903` before it is flipped to blocking (`go-security-analysis.yml` comment at the `gosec` step). Weekly **OpenSSF Scorecard** runs against `main`/`v4` (`.github/workflows/scorecard.yml`). **Dependabot** (`.github/dependabot.yml`) opens weekly (Monday) update PRs across four ecosystems: `gomod` (`/src`), `github-actions` (root), `docker` (`/src`), and `npm` (`/src/proxy`, target branch `v4`). CI enforces a `go test -short -race -count=1` sharded suite (`v2-tests.yml`, the workflow name is a historical artifact of the branch rename) plus an hourly full-suite coverage monitor that auto-files an issue on regression (`coverage-hourly.yml`). `CONTRIBUTING.md` "Test policy" requires a change to behavior ship with a test that fails without it — bug fixes must reproduce the bug, new functionality must cover its normal and failure paths, and "tests not practical" requires a stated verification method instead. The project holds a **Passing** OpenSSF Best Practices badge at 100% completion ([bestpractices.dev/projects/14261](https://www.bestpractices.dev/projects/14261)).

##### Describe how the project has evaluated which features will be a security risk to users if they are not maintained by the project?

The clearest documented instance is the **shared-container execution model** for agents: `security-threat-model.md` "Residual risks and known gaps" names it directly as a feature the project chose not to harden further yet — "Shared-container execution remains a material risk... it is not equivalent to per-run containers or microVMs" — tracked as an open issue (`#2804`) rather than silently accepted. Similarly, `ioscan`'s optional semantic (LLM-judge) classifier is documented as intentionally fail-open on model errors/timeouts, a stated availability-vs-security tradeoff rather than an oversight (`src/docs/ioscan.md`, `security-threat-model.md` "ioscan semantic classification is optional and fail-open"). The SBOM/provenance-attestation decision (`#3760`) is another example: attestation was deliberately disabled after it caused a real production crash-loop (an OCI-index manifest change broke `execve()` on the shipped binary via an overlayfs metacopy redirect), and a CI guard (`image-attestation-guard.yml`) now fails loudly if that decision is silently reverted (`src/docs/releases.md` "Why this is a release artifact, not an in-image attestation").

### Cloud Native Threat Modeling

#### Explain the least minimal privileges required by the project and reasons for additional privileges.

Baseline: the container runs as non-root user `dev` (UID 1001); each agent runs as its own dedicated UID from `HIVE_UID_BASE=2001` upward (`security-model.md` "In-container privilege model"). `su-exec` is the **only** setuid binary shipped, locked to mode `4750 root:hive-launch`, with eleven world-executable setuid/setgid binaries inherited from the `node:26-slim` base image (`chfn`, `chsh`, `mount`, `su`, `passwd`, etc.) stripped at build time and the strip enforced by a build-time contract script *and* a boot-time inventory check against the real built image (`security-self-assessment.md` "Critical security components" #6). The one **additional** privilege the deployment requires is `CAP_NET_ADMIN` (Kubernetes `securityContext.capabilities.add: ["NET_ADMIN"]`, or `--cap-add NET_ADMIN` for Docker/Podman), needed to install the iptables `REDIRECT` that forces all agent egress through the MITM policy proxy — without it the container either refuses to start (fail-closed, distinct exit code `77`/`EX_NOPERM`) or must run with enforcement explicitly downgraded to advisory (`HIVE_PROXY_ADVISORY_OK=true`) (`security-model.md` "Forced proxy egress and `CAP_NET_ADMIN`"). The v2→v4 Kubernetes capability set is `drop: [ALL]` with eight capabilities added back individually (`NET_ADMIN`, `SETUID`, `SETGID`, `SETPCAP`, `CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `FSETID`), each commented with the specific mechanism it exists for (`src/docs/migration-v2-v4.md` "Kubernetes"). `allowPrivilegeEscalation: false` is deliberately **not** set, because it would flip the kernel's `no_new_privs` bit and break `su-exec` itself — the project states this tradeoff explicitly rather than silently accepting weaker isolation (`security-model.md` "Why the image, and not `allowPrivilegeEscalation: false`").

#### Describe how the project is handling certificate rotation and mitigates any issues with certificates.

TLS termination in front of a hive is the operator's responsibility, not the project's: Hive itself implements no native TLS configuration and serves plain HTTP (`src/docs/tls-setup.md` — the dashboard/hub servers call Go `ListenAndServe`, and the bundled Docker gateway also listens with plain HTTP). The project ships no certificate-rotation policy for those operator-owned certificates; renewal, key rotation, HSTS, and CA policy belong to the terminating ingress controller, Route, or reverse proxy — cert-manager on the Kubernetes path (`src/docs/tls-setup.md` "Certificate handling").

Separate from that operator-owned ingress certificate, the project does rotate credentials it owns:

- **Hub master secret** — supports generational rotation with a bounded dual-generation acceptance window (default 7 days) for session, heartbeat, and SSO/session-public keys; terminal and invite keys have no dual lane and are invalidated immediately on rotation (`src/docs/security-model.md` "Master key rotation").
- **GitHub App private key** — a manual, documented rotation procedure: generate a new key in GitHub, mount it at the configured `key_file`, restart Hive, then delete the old key in GitHub once the new one is confirmed working (`src/docs/github-app-setup.md` "Rotation and recovery").
- **MITM proxy CA** — the proxy's own CA certificate/key pair is generated and stored by the entrypoint (`/data/proxy-ca.pem` public, `/data/.hive/proxy-ca-key.pem` private at `0600` in a `0700` directory — `src/docs/security-model.md` "In-container privilege model"), but no rotation schedule for that CA is documented.

None of these three is the ingress-facing TLS certificate the question asks about — that boundary is exactly the line described above.

#### Describe how the project is following and implementing secure software supply chain best practices

- **Base images and tools pinned by digest/SHA**: `golang`, both `node` stages, and the vLLM inference image are digest-pinned in `src/Dockerfile`; tmux, ttyd, `gh`, goose, and `su-exec` are checksum/commit-verified at build (`security-model.md` "Supply chain").
- **CI actions SHA-pinned**: `actions/checkout@<40-hex-sha>` throughout `.github/workflows/`; a dedicated `action-pins` CI job (`go-security-analysis.yml`) verifies every pinned SHA actually resolves, added after a fabricated SHA broke a release (`#4908`).
- **`npm install --ignore-scripts`** for every global AI-CLI install (Claude Code, Copilot, Goose-adjacent tooling).
- **Vulnerability/vetting**: `govulncheck` (reachability-based), `gosec`, `golangci-lint`, weekly OpenSSF Scorecard — all above.
- **SBOM**: every tagged release ships a per-image SPDX JSON SBOM (Syft, scanning the published `linux/amd64` manifest) attached to the GitHub Release — but this is deliberately a **release artifact, not an in-image attestation**: `docker.yml` sets `provenance: false`/`sbom: false` on every build step, guarded by `image-attestation-guard.yml` + `src/scripts/check-no-image-attestations.sh`, because enabling in-image attestation forces an OCI-index manifest, which previously caused a real shipped-binary crash-loop (`#3760`, `src/docs/releases.md` "Why this is a release artifact, not an in-image attestation"). The SBOM scans only the `linux/amd64` manifest of the multi-arch images, stated as a documented (not silent) coverage limitation (`releases.md` "Coverage limitation, stated plainly").
- **DCO**: enforced for human contributors via `copilot-dco.yml`; stated policy (not a hard repo-side gate from Hive itself) for agent-authored commits (`security-self-assessment.md` "Project compliance").

## Day 1 - Installation and Deployment Phase

### Project Installation and Configuration

#### Describe what project installation and configuration look like.

Covered fully under [Day 0 — Installation](#installation) above (three supported runtimes: Docker Compose, Podman/Quadlet, Kubernetes) and [Day 0 — API defaults](#describe-the-project-defaults) (minimum config, defaults). Configuration is a single `hive.yaml`, layered: a ConfigMap/file **seed** (`/etc/hive/hive.yaml`), a PVC-backed **dashboard overlay** (`/data/hive.yaml.dashboard`, written by the UI, secret-free by design — never persists a raw key value, only file paths or env-var names), and a **runtime** merge (`/data/hive.yaml.runtime`) — precedence and reload behavior documented field-by-field in `src/docs/config-layering.md`, inspectable live via `GET /api/config/provenance`.

### Project Enablement and Rollback

#### How can this project be enabled or disabled in a live cluster? Please describe any downtime required of the control plane or nodes.

Hive is a standalone workload, not a cluster-wide control-plane addon — "enabling" it is deploying the Deployment/Service/PVC/ConfigMap/Secret set (`src/deploy/k8s/`), and "disabling" it is `kubectl delete` of the same objects (or `docker compose down`, or the Podman teardown script `bin/hive-podman-teardown.sh`). Enabling or disabling Hive requires **no downtime of the Kubernetes control plane or of any other node workload** — it does not modify cluster-wide policy, install CRDs, or run admission webhooks (see [API-server compatibility](#describe-compatibility-of-any-new-or-changed-apis-with-api-servers-including-the-kubernetes-api-server) above). The single-replica Deployment does incur its **own** restart window on updates by design (`strategy: { type: Recreate }`, `src/deploy/k8s/deployment.yaml`) — see [High Availability](#describe-the-projects-high-availability-requirements) above.

#### Describe how enabling the project changes any default behavior of the cluster or running workloads.

None documented, and none expected by design: Hive only acts on the GitHub/GitLab/Gitea repositories explicitly listed in its own `project.repos` config, enforced twice (network-level proxy allowlist plus a prompt-level `AUTHORIZED REPOS` reinforcement — `security-model.md` Layer 5 "Repo allowlist, enforced twice"). It does not modify Kubernetes RBAC, admission policy, or other workloads' behavior; its own RBAC grant is limited to reading same-namespace Ingress/Route objects and its own Secret (`security-model.md` Layer 7).

#### Describe how the project tests enablement and disablement.

CI exercises multiple install/lifecycle paths directly rather than only unit-testing code: `podman-contract.yml`, `podman-rootful-lane.yml`, `podman-rootless-lane.yml`, `podman-arm64-lane.yml`, and `quadlet-gate.yml` drive real container starts/stops on hosted runners; `suid-contract.yml` boots the built image and reads back the SUID inventory (`src/deploy/test_image_suid_inventory.sh`) rather than trusting the static Dockerfile check alone. `src/docs/podman-quadlet-lifecycle.md` records measured stop/start/restart/recreate/boot-persistence behavior for the Podman path, including honestly-reported gaps (a clean `systemctl stop` left the unit `failed`; `systemctl enable` fails outright on a generated unit) rather than an assumed-clean result. One documented **enablement/disablement gap**: `src/docs/migration-v2-v4.md` "Limits" states plainly that "no production v2→v4 upgrade was performed to write it" and "no data-migration step exists anywhere in the v4 tree" — the guide is a reading of the manifests plus a config-load compatibility check, not a rehearsed upgrade→downgrade→upgrade runbook.

#### How does the project clean up any resources created, including CRDs?

Hive creates **no CRDs** (see [API-server compatibility](#describe-compatibility-of-any-new-or-changed-apis-with-api-servers-including-the-kubernetes-api-server)), so there is no CRD cleanup concern. Standard resource cleanup is `kubectl delete -f src/deploy/k8s/` (or namespace deletion) for Kubernetes, `docker compose down` (optionally `-v` to remove volumes) for Compose, and a dedicated `bin/hive-podman-teardown.sh` for Podman that is aware of Hive-owned resource labels so it does not reach the operator's other containers, Distroboxes, or images (`src/docs/README.md` "Podman ownership and cleanup contract"). PVC/volume data (`/data`) is not deleted by ordinary teardown unless the operator explicitly removes the PVC or passes `-v`, matching the backup/restore documentation's assumption that `/data` outlives a pod recreation.

### Rollout, Upgrade and Rollback Planning

#### How does the project intend to provide and maintain compatibility with infrastructure and orchestration management tools like Kubernetes and with what frequency?

Kubernetes 1.24+ is the stated floor (README.md "Kubernetes Deployment" prerequisites). No documented cadence for re-validating against new Kubernetes minor releases exists in the repository. Podman compatibility is actively, repeatedly re-measured rather than assumed: `src/docs/podman-support-matrix.md` states the support level (supported/experimental/deliberately-unenforced) for each of four rootful/rootless × enforcing/advisory combinations, each backed by a dated, reproducible measurement page (e.g. `podman-rootless-startup-spike.md`, `podman-rootful-egress-baseline.md`, `podman-ipv6-egress-bypass.md`), and `podman-selinux-release-qualification.md` documents a **per-release** manual qualification procedure for the one lane hosted CI cannot run (SELinux-enforcing Podman), with a results ledger and an explicit "UNEXECUTED" state rather than a false pass.

#### Describe how the project handles rollback procedures.

Two levels: **image rollback** and **config rollback**. Image rollback for Podman is a first-class, tested, scripted flow: `bin/hive-podman-update.sh` moves between two pinned digests, executed in both root modes against real `v4` builds, with a measured 11-second rollback and `hive-data` intact throughout (`src/docs/podman-quadlet-update-rollback.md`). For Docker Compose/Kubernetes, rollback is "redeploy the previous pinned digest" — the immutable short-SHA and semver tags exist specifically to make that possible (`src/docs/releases.md` "What is immutable vs. moving"). **Config rollback**: the dashboard overlay layer is a discrete, separately-writable file (`/data/hive.yaml.dashboard`) distinguishable from the ConfigMap seed, and a corrupt/rejected overlay is surfaced via `GET /api/config/provenance` (`overlay_rejected` — `src/docs/config-layering.md`) rather than silently falling back. **What is not covered**: `src/docs/migration-v2-v4.md` "Limits" states no v2→v4 production upgrade (and therefore no upgrade→downgrade cycle) was actually rehearsed to produce that guide.

#### How can a rollout or rollback fail? Describe any impact to already running workloads.

Documented, measured failure modes for the Podman update path: a failed update can hold the systemd unit in `activating` for the full `TimeoutStartSec` (301 seconds) and then loop without ever reaching `failed` state — a real, observed failure signature, not a hypothetical (`src/docs/podman-quadlet-update-rollback.md`). The auto-update lane (`podman-auto-update.md`) documents that `podman auto-update --rollback` *does* fire correctly against this failure shape because it reads the D-Bus start-job result rather than `ActiveState`, but costs one full `TimeoutStartSec` of downtime per bad update, repeated on every timer firing because Podman does not remember a prior rollback. On Kubernetes, the fail-closed egress gate exits with a distinct code (`77`/`EX_NOPERM`) specifically when `CAP_NET_ADMIN` is missing from the bounding set — the documented signature of a capability list that did not carry across during a manifest change (`security-model.md`, `migration-v2-v4.md` "Verifying the upgrade"). Because there is exactly one replica (see [HA](#describe-the-projects-high-availability-requirements)), a failed rollout on the standard `Recreate` strategy means the hive is unavailable for the duration of the failure — there is no surviving old replica to serve traffic in the interim outside the RWX-PVC hosted-fleet path.

#### Describe any specific metrics that should inform a rollback.

Not formalized as a named "rollback trigger" set, but the observable signals a rollback decision would use are documented: `systemctl is-failed` / journal state for the Podman path (`podman-quadlet-lifecycle.md`); `/api/livez` failing (process up but the eval/heartbeat loop stalled — `architecture.md`); the fail-closed exit code `77` on container start; and the agent watchdog's per-agent `Ready`/`Authenticated`/`Producing` conditions degrading fleet-wide after an upgrade (`src/docs/agent-watchdog.md`).

#### Explain how upgrades and rollbacks were tested and how the upgrade->downgrade->upgrade path was tested.

**Podman**: yes, tested and documented as executed, not merely designed. `src/docs/podman-quadlet-update-rollback.md` records an actual "11-second healthy update... a failed update that held the unit in `activating`... and an 11-second rollback out of it with `hive-data` intact throughout," executed in both root modes between two real `v4` builds. `src/docs/podman-auto-update.md` separately records the health-aware auto-update/auto-rollback path executed in both rootless (`#4411`) and rootful-under-system-manager (`#4447`) modes. **Kubernetes/Compose v2→v4**: explicitly **not** tested end-to-end — `src/docs/migration-v2-v4.md` "Limits" states this in as many words: "No production v2→v4 upgrade was performed to write it... `/data` compatibility was not tested end-to-end... 'no migration code' is weaker evidence than a tested restore. Back up first." The full **upgrade→downgrade→upgrade** cycle specifically is documented as tested only on the Podman digest-pin path described above; it is not documented as tested for the Kubernetes/Compose image-tag path.

#### Explain how the project informs users of deprecations and removals of features and APIs.

Through `CHANGELOG.md`'s `## Unreleased` section, which explicitly asks for entries covering "features, fixes, security changes, migrations, deprecations, and breaking changes" (`CHANGELOG.md` "How we maintain this file"), nudged by a GitHub Actions advisory comment (`changelog-reminder.yml`) when a PR touches code without touching the changelog — stated as "a reminder, not a merge gate" (`CONTRIBUTING.md`). A `### Security` or breaking entry drives a **major** version bump under the automated release-versioning scheme, which is itself the signal to downstream consumers (`src/docs/releases.md`). The v2→v4 transition is the concrete worked example: it is documented with an explicit compatibility table, diffed key-by-key, and a dedicated migration guide (`src/docs/migration-v2-v4.md`) rather than a bare changelog line.

#### Explain how the project permits utilization of alpha and beta capabilities as part of a rollout.

No formal alpha/beta feature-flag maturity system (like Kubernetes feature gates) exists. The closest analogs, both explicit and self-labeled: (1) **release channels** — `edge`/`candidate`/`stable`, all currently synced to the same `v4-latest` digest but designed as the promotion mechanism for future channel divergence (`src/docs/release-channels.md`); (2) **doc-level status labels** on features that are design-only, partly shipped, or shipped-but-unwired — e.g. the skill registry carried "loaded and counted on the dashboard, but not yet delivered to agents" until delivery shipped and the label was retired (`src/docs/README.md` skills.md entry), `AGENTS.md` parsing was labeled "parsed and tested, but not wired into kicks" until its checkout root was threaded in [#5227](https://github.com/hivecommons/hive/issues/5227), and the `design/` directory indexes longer-form records each carrying a status of shipped/partly-shipped/design-only/historical specifically so a proposal is never mistaken for current behavior (`src/docs/README.md` "Historical/design notes"). The agent self-healing watchdog is a concrete example of graduated rollout via an explicit mode ladder: it ships in `observe` mode (classifies and would-have-acted, but takes no action) and only promotes to `heal` (acts) on operator decision, with `HIVE_WATCHDOG_PAUSE=true` as a fleet-wide downgrade switch (`src/docs/agent-watchdog.md`).

## Day 2 - Day-to-Day Operations Phase

### Scalability/Reliability

#### Describe how the project increases the size or count of existing API objects.

Two independent scaling axes, both config-driven rather than automatic: **agent replica count** is bounded per agent, `1`–`5` (`MaxAgentReplicas = 5`, `src/pkg/config/config.go`, validated at load); **governor mode thresholds** (the idle/quiet/busy/surge ladder that drives kick cadence) scale automatically with the number of watched repositories — the default is a per-repo base value multiplied by `project.repos` count, with a selectable curve (`linear` default, `sqrt`, or `none` via `governor.threshold_scaling` — `src/docs/governor-thresholds.md`). This was added specifically because a fixed absolute threshold put a 39-repo hive permanently in SURGE mode (observed live: queue ~210 against a threshold of 70) while under-triggering for small hives (`governor-thresholds.md` "The problem the scaling solves"). Operator-set explicit thresholds are always used verbatim and never re-scaled. No mechanism scales the number of *hives* themselves beyond the one-pod-per-tenant model (see [HA](#describe-the-projects-high-availability-requirements)).

#### Describe how the project defines Service Level Objectives (SLOs) and Service Level Indicators (SLIs).

The project defines no SLOs or SLIs. "SLO"/"SLA" appear only as (a) a lane-keyword label for an optional "operations" agent role (`src/docs/agent-configuration.md`) and (b) a per-hive dashboard notification for stale actionable issues exceeding a configured age ("SLA breach" — `src/docs/notifications.md`), neither of which is a stated project-level commitment. What does exist, as raw material from which SLOs could later be defined: `/api/health` and `/api/livez` health endpoints, the agent watchdog's `Ready`/`Authenticated`/`Producing` conditions (`src/docs/agent-watchdog.md`), the append-only audit log (`src/docs/audit-log.md`), and the opt-in Prometheus `/metrics` endpoint (`src/docs/security-model.md` "Metrics endpoint") — all described in full under [Observability Requirements](#observability-requirements) below.

#### Describe any operations that will increase in time covered by existing SLIs/SLOs.

Not applicable — the project defines no SLOs or SLIs (see above), so there is nothing to describe an increase against.

#### Describe the increase in resource usage in any components as a result of enabling this project, to include CPU, Memory, Storage, Throughput.

None measured. No baseline-vs.-loaded resource-usage measurements (CPU/Memory/Storage/Throughput deltas) are documented, consistent with the absence of a load-testing program described below. The only numeric resource figures in the repository are the static Kubernetes `requests`/`limits` (500m/512Mi requests, 2 CPU/2Gi limits — `src/deploy/k8s/deployment.yaml`), which describe a configured ceiling, not a measured usage curve.

#### Describe which conditions enabling / using this project would result in resource exhaustion of some node resources (PIDs, sockets, inodes, etc.)

Partially documented as enforced ceilings rather than measured exhaustion behavior: agent replicas are capped at 5 per agent (`config.go`, `MaxAgentReplicas`); each agent runs one dedicated tmux server on a per-agent socket (`security-model.md` Layer 4), so fleet size bounds session/socket count, but no documented total-session or per-node PID/socket/inode ceiling exists. The sandboxed-kick path (opt-in) has a default `timeout_s: 2700` per Podman run (`src/docs/sandbox-isolation.md`). No specific numeric PID/socket/inode exhaustion thresholds are documented — the project defines none, consistent with the absence of a load-testing program or recommended-limits guidance described above.

#### Describe the load testing that has been performed on the project and the results.

None. No controlled load testing has been performed on the project. The 39-repo governor-threshold example (`governor-thresholds.md`, "Observed live: queue ~210 against a threshold of 70") is a single production field observation used to justify a scaling-curve design decision, not a controlled load test with methodology or results.

#### Describe the recommended limits of users, requests, system resources, etc. and how they were obtained.

None. The project defines no recommended limits of users, requests, or system resources, and none were obtained through measurement. The only enforced numeric ceilings found are code-level defaults, not user-facing capacity recommendations: agent replicas ≤ 5 per agent, and the in-memory audit-log ring capped at 500 entries before it relies on the on-disk JSONL file (`src/docs/audit-log.md`).

#### Describe which resilience pattern the project uses and how, including the circuit breaker pattern.

No component is named "circuit breaker" in the classic network sense, but Hive implements the equivalent pattern under a different name for its highest-risk automated loop — CI auto-fix — and a general restart/backoff ladder for agent liveness:

- **Escalation circuit breaker for CI fix loops** (`src/docs/adr/0010-escalation-circuit-breaker.md`): a persistent ledger tracks distinct failing head SHAs per PR; after a default threshold of **3** distinct red SHAs, the automated fix loop **trips** — it stops re-dispatching blind fixes, posts a "Fix loop escalated — human attention needed" comment with failing-check evidence, and applies a `needs-human` label so future fix dispatch skips the PR (functionally: open circuit, human reset required). This was added directly in response to an incident where fix PRs kept `main` red for days while missing a one-line root cause (ADR-0010 "Context").
- **Agent watchdog restart-with-backoff and crash-loop escalation** (`src/docs/agent-watchdog.md`): dead agent panes are restarted on exponential backoff (`1m, 2m, 4m, 8m, 16m`); after `crash_loop_after` (default 5) consecutive failed restarts the agent is **paused** — no further restarts until human/API intervention — the same trip-and-hold shape as a circuit breaker, gated behind an explicit `observe`→`heal` mode promotion so the reconciler earns fleet-wide restart authority on evidence rather than by default.
- **Fail-open reviewer layers** (`ioscan` semantic classifier, trajectory review) deliberately trip toward *continuing operation* rather than blocking the scheduler on their own outage — a documented availability-over-strict-enforcement tradeoff (`security-threat-model.md` "Residual risks and known gaps").
- **Governor mode ladder** (idle→quiet→busy→surge) functions as a backpressure mechanism, throttling kick cadence as queue depth rises rather than an unbounded dispatch rate (`governor-thresholds.md`).

### Observability Requirements

#### Describe the signals the project is using or producing, including logs, metrics, profiles and traces. Please include supported formats, recommended configurations and data storage.

- **Logs**: structured log output to `hive.log` under `/data/logs` (configurable via `governor.logging.dir`), teed to stdout; wrapped by `pkg/logscrub`, which redacts recognized GitHub token prefixes (`ghs_`, `ghp_`, `gho_`, `github_pat_`) and JWT-shaped strings before they reach any log sink (`src/docs/security.md`, `architecture.md` "Dashboard & observability").
- **Metrics**: Prometheus text-exposition format at `GET /metrics`, **off by default**, requiring `HIVE_METRICS_ENABLED` plus a mandatory `HIVE_METRICS_TOKEN` bearer (fails closed — enabled-but-tokenless returns 403 naming both variables; `security-model.md` "Metrics endpoint"). Exposed series include per-model/per-agent cumulative estimated cost (`hive_estimated_cost_usd*`) and token totals — explicitly called out as business-sensitive, hence the mandatory auth.
- **Traces**: optional OpenTelemetry OTLP/HTTP export, off by default, enabled via an `otel:` config block — spans for governor eval cycles, agent kicks, and lifecycle/PR events, with agent spans using GenAI semantic-convention attributes (`gen_ai.system`, `gen_ai.request.model`, token usage) plus Hive-specific attributes (`hive.agent`, `hive.lane`, `hive.acmm_level`, `hive.governor.mode`) — `architecture.md` "Dashboard & observability".
- **Profiles**: not documented — no pprof/continuous-profiling integration found in the repository.
- **Live push**: `GET /api/events` Server-Sent Events, a fresh fleet+governor snapshot pushed every eval cycle (`architecture.md`).

#### Describe how the project captures audit logging.

Append-only JSONL at `/data/audit.jsonl`, written by `pkg/dashboard/audit.go`. Each entry carries `ts` (RFC3339, always present), `user` (always, may be a pseudo-user for system-triggered actions), `action` (always), an optional flat `detail` string of `k=v` pairs, and an optional `agent` field (`src/docs/audit-log.md`). Covers dashboard config changes, logins, GitHub App setup changes, and agent lifecycle events (start/stop/launch-failure/pause/resume/kick/add/remove/backend-change/model-change — `architecture.md` "Dashboard & observability"). **Rotation is size-triggered, not calendar-triggered**: lumberjack rotation at 5 MB with 3 backups retained, gzip-compressed, with a 90-day max-age cap that only prunes files already past the size threshold — meaning the *effective* lookback window a responder can rely on varies with hive activity level rather than being a guaranteed 90 days, a limitation the project's own self-assessment states explicitly as a "secondary, narrower" forensic-capability gap (`security-self-assessment.md` "A secondary, narrower point"). An in-memory ring additionally caps at 500 most-recent entries for fast dashboard reads, reset on process restart (`audit-log.md`).

#### Describe any dashboards the project uses or implements as well as any dashboard requirements.

Hive ships its own web dashboard (the primary operator interface — see [Usability](#how-should-the-target-personas-interact-with-your-project) above); it does not bundle or ship pre-built Grafana dashboards. External Prometheus/Grafana integration is possible only via the opt-in, token-gated `/metrics` endpoint described above — the project provides the scrape target, not a pre-built dashboard-as-code artifact. `grafana`/`prometheus`/`opentelemetry` otherwise appear only as example tag values in agent-configuration documentation, not as shipped dashboard assets.

#### Describe how the project surfaces project resource requirements for adopters to monitor cloud and infrastructure costs, e.g. FinOps

Documented in full at `src/docs/token-tracking.md`. The token collector scans each backend's session JSONL files (plus a live proxy sniff of Copilot's usage block) and multiplies token counts by a dated per-model price table — explicitly labeled a **list-price estimate, not a billing feed** (`architecture.md` "Model backends & cost"; `token-tracking.md` "Cost estimates are not invoices"). `GET /api/cost` returns per-session and per-agent/model cost rows tagged `estimated`/`unpriced`/`native` (native for gateways that report their own spend, e.g. OpenRouter, LiteLLM); `GET /api/repo-activity` gives per-repo attribution of output counts (not dollars). At hub scale, each spoke's 24-hour cumulative token count rides the heartbeat payload and rolls up at `GET /api/saas/usage` for fleet-wide visibility.

#### Which parameters is the project covering to ensure the health of the application/service and its workloads?

The **agent self-healing watchdog** (`src/docs/agent-watchdog.md`) is the primary mechanism: on every governor eval tick it classifies each launched agent's live tmux pane into a state machine (`ready`, `shell-prompt`, `auth-required`, `stuck-overlay`, `no-output`, `no-session`, `unknown` — `unknown` is never treated as healthy), and publishes three Kubernetes-style **conditions** per agent in `/api/agents`: `Ready` (pane state), `Authenticated` (credential-probe result — deliberately does **not** trigger a restart, to avoid the documented "1042-restart loop" of restarting into a dead credential), and `Producing` (recent-activity evidence; degrades to a warning after `no_production_for`, default 6h, never a pause). Separately, [health-checks.md](health-checks.md) documents dashboard-route/URL health: a self-check probe (`public_url_self_check`) and Ingress/Route existence check (`route_exists`), with hub-side hysteresis (consecutive-failure counts, minimum hive age) before an alert reaches the operator's Attention panel, specifically to avoid false dead-link pages for private-network hives the public hub cannot reach.

#### How can an operator determine if the project is in use by workloads?

Via the dashboard's live agent cards and the watchdog's `Producing` condition (evidence = tmux pane activity plus newest state-file mtime under the agent's backend-specific directories); via `/api/audit` entries for kicks/lifecycle events; and via "last kick"/"next kick" timing fields surfaced in the dashboard and troubleshooting guidance (`src/docs/troubleshooting.md` "The dashboard says the next kick is later, but the agent is visibly working now").

#### How can someone using this project know that it is working for their instance?

The combination of health endpoints (`/api/health`, `/api/livez`), the three per-agent watchdog conditions, and — explicitly recommended in the troubleshooting guide — cross-checking real GitHub effects (actual issues/PRs created) rather than trusting only self-reported dashboard state, because a pane can misreport (`src/docs/troubleshooting.md` "An agent writes a heartbeat but its work is obviously broken").

#### Describe the SLOs (Service Level Objectives) for this project.

None — see [SLOs/SLIs](#describe-how-the-project-defines-service-level-objectives-slos-and-service-level-indicators-slis) above; the project does not currently define SLOs.

#### What are the SLIs (Service Level Indicators) an operator can use to determine the health of the service?

While the project does not formally label these as "SLIs," the de facto operator-usable health signals are the watchdog's `Ready`/`Authenticated`/`Producing` conditions with `status`/`reason`/`lastTransitionTime` (`agent-watchdog.md`), and the health-checks.md alert matrix (spoke self-check `fail` after three consecutive failures, `route_exists: missing`, hub-fronted URL failures gated by heartbeat freshness). These are real, queryable, structured signals; they are simply not published with a target/threshold framed as an SLI in the CNCF sense.

### Dependencies

#### Describe the specific running services the project depends on in the cluster.

None required. As established under [Day 0 — service dependencies](#define-any-specific-service-dependencies-the-project-relies-on-in-the-cluster), a hive is a single self-contained pod plus its own PVC, with no required etcd, database, or cache. Everything else it talks to — GitHub, model backends, optional Linear/notification/backup-storage endpoints — is an external service reached over the network, not a required in-cluster co-resident service.

#### Describe the project's dependency lifecycle policy.

No standalone prose "dependency lifecycle policy" document exists; the policy is expressed entirely as automation. `.github/dependabot.yml` opens weekly (Monday) update PRs across four ecosystems: `gomod` (`/src`, limit 10 open PRs), `github-actions` (root, limit 10), `docker` (`/src`, limit 5), and `npm` (`/src/proxy`, target branch `v4`, limit 10). Every such PR passes through the same CI gates as any other PR (`go-security-analysis.yml`, `v2-tests.yml`, `golangci-lint`) before merge.

#### How does the project incorporate and consider source composition analysis as part of its development and security hygiene? Describe how this source composition analysis (SCA) is tracked.

`govulncheck` provides SCA specifically for **known, reachable** vulnerabilities (not merely "a vulnerable version is present" — it is call-graph-reachability-based, so it only fires when the code actually calls a vulnerable symbol, keeping findings actionable — `go-security-analysis.yml` header comment). It runs on every push/PR touching `src/**` and on a weekly cron (`0 6 * * 1`) specifically so a newly-published advisory against *unchanged* code is still caught, since a push-only trigger cannot see that case. `gosec` covers insecure code patterns (a narrower, complementary class of finding to dependency vulnerabilities). Both tools' findings are visible in the CI log on every run; `govulncheck` blocks merge, `gosec` currently does not (see [Security Hygiene](#security-hygiene) above).

#### Describe how the project implements changes based on source composition analysis (SCA) and the timescale.

`govulncheck` and `golangci-lint` findings are hard CI gates — a PR cannot merge while either fails, so remediation is enforced at merge time with no separate timescale needed. `gosec` findings are currently visible-but-non-blocking, explicitly pending resolution of a pre-existing HIGH-severity backlog under issue `#4903` before the gate flips to blocking (`go-security-analysis.yml` step comment) — no calendar deadline is committed for that flip, only the completion condition ("do not flip it before the HIGH-severity findings are resolved"). `CONTRIBUTING.md` states the general policy: "Fix findings rather than suppressing them; when a suppression is genuinely right, comment why at the suppression site" — again a completion-condition policy, not a numeric SLA.

### Troubleshooting

#### How does this project recover if a key component or feature becomes unavailable? e.g Kubernetes API server, etcd, database, leader node, etc.

Hive has no etcd/database/leader-election dependency to fail (see [Dependencies](#describe-the-specific-running-services-the-project-depends-on-in-the-cluster) above), which removes an entire class of this question. For the dependencies it does have:

- **GitHub API/App misconfigured or unreachable**: Hive starts the dashboard but disables write-capable GitHub work rather than crash-looping — a documented "dashboard-only mode" (`src/docs/troubleshooting.md` "GitHub credentials are missing or invalid").
- **Inference/model backend or the LLM-judge reviewer unavailable**: both the `ioscan` semantic classifier and the trajectory-review reviewer are documented to **fail open** on outage/timeout — a reviewer outage does not become a scheduler outage, a deliberate availability tradeoff stated plainly rather than hidden (`security-threat-model.md` "Residual risks and known gaps").
- **Kubernetes API server briefly unreachable** (in-cluster self-inspection calls, e.g. reading its own Deployment or Ingress/Route list): treated as non-fatal/`unknown` rather than a hard failure — `route_exists: unknown` on an RBAC or transient error, not an alert (`src/docs/health-checks.md`).
- **`/api/livez` is deliberately scoped to the process's own health** (HTTP server + eval/heartbeat loop) rather than to hub reachability, so stale hub-heartbeat state cannot crash-loop an otherwise-healthy spoke pod (`troubleshooting.md` "Health endpoints").
- **Notification channels** (ntfy/Slack/Discord) fail silently as a no-op if misconfigured rather than blocking any agent or governor operation (`troubleshooting.md` "Notifications... never arrive").

#### Describe the known failure modes.

Extensively catalogued, not a short list — `src/docs/troubleshooting.md` documents, with specific symptom-to-cause mappings: config load/validation failures; missing/invalid GitHub credentials; a hosted hive disappearing or its URL timing out (reaping); agents stuck/paused/needing CLI re-login; a permission prompt silently blocking an agent; notifications never arriving; an agent heartbeating while its actual work is broken (self-report vs. reality mismatch); a confusing "please run `/login`" loop on inference-backend auth failures; a terminal that looks frozen but is just scrollback; kick-timing display confusion; model-switch gotchas; dashboard auth/access errors; and a dedicated Podman/Quadlet section covering startup hangs, the `activating`-not-`failed` misreport, SELinux denials on `/data` or the secrets directory, permission-denied errors restoring an archive as the wrong UID, and auto-update rollback behavior. The volume and specificity of this document is itself evidence the project treats real operator-reported failure modes as first-class documentation, not an afterthought.

### Compliance

#### What steps does the project take to ensure that all third-party code and components have correct and complete attribution and license notices?

The process and its enforcement are in place; the authoritative data is not yet generated.

A repo-root `NOTICE` lists every Go module dependency compiled into `hive`, `hive-hub`, and `hive-contributor`, generated by `src/scripts/generate-notice.sh` (pinned `google/go-licenses`) and kept current by the `notice-drift` CI job in `go-security-analysis.yml`, which regenerates on every change to `src/go.mod`, `src/go.sum`, or the script — and weekly, since an upstream dependency can relicense with our files untouched. Tagged releases attach `NOTICE` alongside the per-image SBOMs (`src/docs/releases.md`).

The guard is enforcing rather than advisory, and it has already caught a real licensing problem rather than mere drift. On its first working run it flagged `github.com/fumiama/go-docx` — a **direct** dependency compiled into the shipped binary — as **AGPL-3.0**, which `go-licenses` classifies `FORBIDDEN` and which is incompatible with the project's Apache-2.0 posture. It backed one narrow function, read-only text extraction in the knowledge vault's `.docx` parser, and was removed in favor of a standard-library implementation (`archive/zip` + `encoding/xml`) that preserves behavior and adds no replacement dependency; `go mod tidy` dropped its transitive `github.com/fumiama/imgsz` with it. An AGPL dependency had been shipping undetected, and the attribution check is what surfaced it.

Regenerating the committed `NOTICE` from the generator's verified output is ordinary maintenance carried by that CI job, not an open design question. The project does not infer or fabricate license identifiers: an attribution file asserting a wrong license is a false legal claim, and a field is left explicitly unverified rather than guessed. Note `go.sum` pinning is integrity and provenance tracking, not license attribution, and never produced an assembled notice by itself.

#### Describe how the project ensures alignment with CNCF recommendations for attribution notices.

`NOTICE` and its generator are the project's attribution mechanism; see the answer above. Notices for unmodified third-party Go modules are covered by the generated file rather than by vendoring their license texts into the tree, and build artifacts carry it via the release attachment. The project ships no third-party code copied directly into its own source files, so there is no per-file header convention to maintain.

##### How are notices managed for third-party code incorporated directly into the project's source files?

Not documented. No per-file attribution-header convention exists in the repository.

##### How are notices retained for unmodified third-party components included within the project's repository?

Not documented; no vendored-component notice-retention process exists. The project does not vendor third-party source trees — dependencies are resolved via `go.mod`/`go.sum` and `package.json`/`package-lock.json` rather than committed in-tree, so this question does not currently have a concrete case to apply to.

##### How are notices for all dependencies obtained at build time included in the project's distributed build artifacts (e.g. compiled binaries, container images)?

Not currently included. The per-release SBOM (Syft/SPDX, attached to each GitHub Release — `src/docs/releases.md`) records the *package versions* present in the shipped image, which is adjacent evidence of what a full license-notice aggregation would need, but the SBOM itself is not a license-notice file, and no shipped build artifact (image or release) currently includes a dependency-notices file derived from it.

### Security

#### Security Hygiene

##### How is the project executing access control?

Fully detailed in `src/docs/security-model.md`'s seven layers (reproduced with citations under [Day 0 — Identity and Access Management](#describe-how-the-project-implements-identity-and-access-management) above, plus): **Layer 3** — per-agent placeholder inference keys with server-side key-swap, so agents never hold the real gateway credential; **Layer 4** — per-agent Unix UIDs (`su-exec`, base 2001), per-agent tool denylists (`--disallowed-tools`/`--deny-tool`, backend-specific), and a default-on GitHub policy proxy enforcing `(method, path) → minimum ACMM mode` plus a hard repo allowlist at the network layer, independent of what the agent was prompted to do; **Layer 5** — GitHub App scoping with per-tier installation tokens (advisory tiers get read-only tokens that cannot even create issues; only trusted tiers get contents/PR write), ACMM L1–L6 gating merge permission at the token/proxy level (not just a UI toggle — "merge permission simply is not granted below L6... whatever its prompt says"), and policy-driven DCO sign-off; **Layer 6** — hub↔spoke heartbeat authentication via per-hive derived bearer keys; **Layer 7** — hosted-platform tenant isolation (one namespace/pod/PVC per hive, least-privilege namespace-scoped RBAC limited to the hive's own named Secret).

#### Cloud Native Threat Modeling

##### How does the project ensure its security reporting and response team is representative of its community diversity (organizational and individual)?

`OWNERS` lists three approvers/reviewers spanning three distinct affiliations: `clubanderson` (IBM), `hanthor` (Universal Blue), and `Danathar` (independent) — not a single-organization team (`OWNERS`, `GOVERNANCE.md`). [`src/docs/security-response.md`](security-response.md) now documents who responds, how reports are handled end to end, the 60-day fix commitment for publicly-known vulnerabilities, how responders are added and removed via the existing maintainer lifecycle, and the escalation path. It states plainly that the three-organization spread is a property of the current roster rather than a policy target, and that the project has set no formal diversity requirement.

[`src/docs/security-response.md`](security-response.md) defines the process. Responders are the maintainers in `OWNERS`, deliberately not a separate roster that could drift from it. Membership changes follow the maintainer lifecycle in `GOVERNANCE-HIVE.md` — nomination, lazy consensus, a PR against both `OWNERS` and the upstream table — and the Committee may add a non-maintainer as a security-only responder, though none exists today.

The document is explicit about what it does not do: there is no on-call rotation, no CVSS rubric beyond the 60-day commitment, and no diversity target. The current three-organization spread (IBM, Universal Blue, independent) is incidental to maintainer status, and the document says so rather than presenting it as policy.

##### How does the project invite and rotate security reporting team members?

No security-specific rotation process is documented. The general maintainer-lifecycle process in `OWNERS` applies: "nomination by an existing maintainer, lazy consensus of the committee, and a PR against BOTH this file and the upstream table [`GOVERNANCE-HIVE.md`]," with affiliation changes required to be reflected within 30 days. Because `OWNERS`' approvers are the same people who receive private security advisories (per `SECURITY.md`'s routing to "a repository maintainer"), this maintainer-lifecycle process is, in effect, also the security-team membership process — but no document names it as such or describes a rotation cadence specific to security-report handling.
