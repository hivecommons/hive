# Security model: how Hive keeps autonomous agents from doing damage

Last reviewed: 2026-10-23

Hive runs AI coding agents against real repositories. This page is the
inventory of the controls that limit what an agent can do, with the file or
setting behind each one. Hive's design rule is that anything a human would
decide the same way every time belongs in infrastructure, not in a prompt:
the controls below are Go, shell and kernel mechanisms, not instructions to a
model. Where a control is a proposal rather than code, the page says so.

An inventory is not a promise that every running hive has every control on.
Check the effective configuration before relying on a gate; a dashboard
toggle alone is not evidence of enforcement.

Companion pages: [security-threat-model.md](security-threat-model.md)
(attacker view), [security.md](security.md) (log scrubbing),
[securing-your-hive.md](securing-your-hive.md) (first-time operator guide).

## Choose a reading path

- **First-time operator:** use the guide above for trust choices and who can
  merge. This page is a technical inventory, not a deployment checklist.
- **Check a running hive:** start with [Human in the loop](#10-human-in-the-loop)
  and the inventory's “How to check” column. Dashboard owners can inspect
  effective settings; file, kernel and network checks need deployment-admin
  access. Hosted owners should ask their hub operator for those checks.
- **Audit the implementation:** use the control sections and
  [verification procedure](#12-how-to-audit-a-running-hive). Do not change production state
  or print secrets to prove a check.

**Terms used below:** a **UID** is a Unix user ID. A **cgroup** tracks and
limits a process group's resources. **Egress** means outgoing network traffic.
**MITM** means man-in-the-middle: Hive's local inspecting proxy. A **PAT** is
a personal access token. A **JWT** is a JSON Web Token; its **TTL** is its
time to live. **CSP** is Content Security Policy, a browser restriction on
page resources and scripts. These names describe mechanisms, not proof they
are enabled on a given hive.

## 1. Threats

* **Prompt injection.** Issue and pull-request text, labels, comments,
  cloned repositories and tool output are attacker-controlled and reach the
  agent's prompt.
* **Runaway or stuck agents.** Loops, token burn, endless re-dispatch, wedged
  terminals.
* **Credential theft.** An agent printing, pushing or sending a GitHub token,
  provider key or hub secret off host, or reading another agent's.
* **Bad merges.** Code landing without CI, review or human sign-off, or at a
  different commit than the one judged.
* **Over-trusted contributors.** Humans or bots steering agents, claiming work
  above their trust, adding themselves as owners, or weakening CI.
* **Cost abuse.** Token burn, API rate-limit exhaustion, label and comment
  storms.
* **Platform compromise.** Unauthorized dashboard or terminal access, forged
  hub-to-spoke messages, cross-tenant exposure on the hosted hub.
* **Supply chain.** Poisoned base image, npm install script, unpinned Action
  or vulnerable Go dependency.

## 2. Control inventory

| Control | Mitigates | Where it lives | How to check it is on |
|---|---|---|---|
| Fresh context per task (`clear_on_kick`) | injection persistence | `src/crux/agent/manager_kick.go` | `hive.yaml` agent block |
| Watchdog and crash-loop pause | stuck agents | `src/crux/agent/watchdog_fleet.go` | `conditions[]` in `GET /api/agents` |
| Per-agent Unix users, `su-exec` 4750 | cross-agent access, privilege escalation | `src/deploy/entrypoint.sh`, `src/Dockerfile` | `/var/run/hive/uid-map.json`; `suid-contract` CI job |
| Per-agent tool denials; GitHub MCP writes denied | GitHub writes through tools | `src/crux/agent/manager_launch.go` | agent launch command |
| Credential-free sandbox and push broker (opt-in) | credential theft | `src/crux/sandbox`, `src/crux/pushbroker` | `agentSandboxEnabled` in `GET /api/config/governor` |
| Work ledger and knowledge store instead of memory | injection persistence | `src/pkg/beads`, `src/pkg/knowledge` | `bd` CLI |
| `ioscan` scanning and canaries (default on) | prompt injection, secret leakage | `src/pkg/ioscan` | `ioscanEnabled`, `ioscanFailMode` in `GET /api/config/governor` |
| Trajectory review (default on) | goal drift | `src/crux/trajectory` | `governor.trajectory.enabled` |
| Policy proxy and repo allowlist | credential theft, out-of-scope writes | `src/crux/proxy/rules.go` | `403` with `X-Hive-Proxy-Blocked` |
| Authorization decision seam, shadow mode (opt-in, enforces nothing) | undetected drift between a gate and its declared policy | `src/crux/authz`, `HIVE_AUTHZ_SHADOW` | `authz shadow` log lines; no behaviour change either way |
| Forced egress through the proxy (iptables) | proxy bypass | `src/deploy/entrypoint.sh` | boot log; no `HIVE_PROXY_ADVISORY_OK` |
| Tiered GitHub tokens per agent mode | blast radius of a leaked token | `src/crux/agentmode`, `src/crux/agent/manager_tokens.go` | token tier on the agent card |
| Proxy-side credential injection (opt-in) | token theft | `HIVE_PROXY_INJECT_GH_AUTH` | `security.credentialInjection` in governor config |
| Sandbox Job GitHub read relay (opt-in) | anonymous 60/hr API cap; token in the Job | `agent_sandbox.job.github_read_proxy` | `sandbox GitHub read relay enabled` boot line |
| Inference-key placeholder swap | provider key theft | `src/pkg/apiproxy` | agent env shows `sk-hive-<agent>` |
| `gh` wrapper with per-agent restrictions | denied commands | `bin/gh-wrapper.sh` | `gh` resolves to `/usr/local/bin/gh` |
| `hive-open-pr` / `hive-merge` relays | forged authorship, stale-commit merges | `bin/hive-merge.sh`, `bin/hive-open-pr.sh` | `POST /pulls` returns 403 for agents |
| Automerge gates | bad merges | `src/pkg/github/automerge/automerge_sweep.go` | skip reasons in audit entries |
| Intent verification | scope creep, guardrail edits | `src/pkg/intent` | `intentEnforce` in governor config |
| Hold guard | drift after approval | `src/pkg/holdguard` | evidence comment on held PR |
| Escalation circuit breaker | fix loops | `src/pkg/escalation` | `needs-human` label |
| Sentinel (eight rules) | malicious PRs, CI weakening | `src/pkg/sentinel` | `sentinel-alert` label |
| Reporter trust (opt-in) | hostile issue reporters | `src/pkg/config/reporter_trust.go` | `reporter_trust.enabled` |
| Dashboard roles (`read`/`read-write`/`merger`/`owner`) | unauthorized writes | `src/pkg/dashboard/api_auth.go` | 403 on write as `read` |
| Audit and token-access logs | unattributed actions | `src/pkg/dashboard/audit.go`, `src/pkg/agentaudit` | `GET /api/audit` |
| Token budget and per-pass caps | cost abuse | `src/pkg/governor/governor.go` | `governor.budget.*` |
| Pause switches | emergency stop | `project.paused_repos`, `POST /api/pause/<agent>` | config |
| Webhook signature check | forged events | `src/pkg/dashboard/api_github_webhook.go` | unsigned request rejected |
| Hub auth (Ed25519 sessions, per-hive bearers) | platform compromise | `src/pkg/hub/saas_auth.go` | `GET /api/saas/admin/auth-rollout` |
| Content Security Policy and headers | dashboard XSS | `src/pkg/dashboard/server.go` | response headers |
| Log scrubbing | secrets in logs | `src/crux/logscrub` | redacted patterns in logs |
| Supply-chain guards | supply chain | `.github/workflows/*.yml`, `src/Dockerfile` | PR checks |
| DCO on every commit | unattributed code | `src/scripts/check-dco-trailers.sh` | DCO check on PR |

### GraphQL repository scope

The GitHub MITM proxy applies the **same per-agent repository scope predicate**
to attributable GraphQL mutations as to REST writes, independently of autonomy
mode. GraphQL queries are not scope-restricted: duplicate detection, linking,
search and cross-repository lookups continue to work. The selected operation
(`operationName`, when present), its arguments and variable defaults determine
which mutation inputs are inspected; comment bodies and unused variables are
not repository evidence.

Mutation targets can name a repository directly (including
`repositoryNameWithOwner` or `repository(owner:, name:)` arguments), or use a
repository/issue/PR/comment node ID. The proxy learns ID-to-repository mappings
from successful GitHub GraphQL responses (identity or gzip), following repository
selections and their arguments, aliases and fragments. The bounded in-memory
cache is isolated by agent, shared across that agent's connections and lost
on proxy restart; it holds at most 8,192 entries and resets when full. Responses
larger than 4 MiB on the wire or after decompression, and unsupported encodings,
are relayed normally but do not seed the cache. Requests alone
cannot seed it. Scope changes take effect on the next write, without invalidating
the ID cache. Every resolved target is checked, including batched mutations.
An out-of-scope target returns REST's `403`, `X-Hive-Proxy-Blocked: true`,
`X-Hive-Proxy-Reason: agent_repo_scope` and standard denial body/violation audit.
The internal control-plane caller retains its exemption.

**Unresolved mutation targets remain fail-open for repository scope only.**
This preserves ID-only `gh api graphql` calls, cold-cache/restarted sessions,
and document/response shapes the proxy cannot attribute. Normal `gh` lookup →
create/comment/edit/close/merge sequences are covered by document fixtures and
proxy integration tests; the preceding reads populate the cache even when the
repository is outside the agent's scope. Fail-closed is intentionally not chosen:
there is no guarantee every legitimate ID-only write has a preceding observable
lookup. Existing mode, write-surface and auto-merge gates still apply to unresolved
writes. Consequently this best-effort attribution is **not a complete security
boundary against an agent deliberately using previously unseen IDs**; token
repository permissions remain necessary to bound that case. This change does
not extend repository pause or the hive-wide REST allowlist to GraphQL.

## 3. Agent runtime isolation

**Bounded runs.** Work arrives as discrete tasks from the governor. By default
each task starts with a cleared model context, so the agent begins from the
task text and its instructions file, not from what a previous issue author
wrote. A resumed pull-request session is the deliberate exception. A turn
ceiling bounds how long the governor waits on a continuous turn. The watchdog
classifies every agent pane on each tick, restarts dead panes with backoff
from one to sixteen minutes, and after five failed restarts pauses the agent
and raises an alert until a human intervenes. Context reset is not process
reset: the CLI process and its on-disk session state persist until restart.

**Separation.** Each agent runs as its own Unix user (from UID 2001), in its
own `tmux` server on its own socket, with its own home directory and token
file. Token files are owned by the agent's group with mode 0640 or by the
agent with mode 0600. Agents cannot attach to each other's sessions, signal
each other's processes or read each other's tokens. Agents coordinate through
the work ledger and GitHub, never by reading transcripts.

**Tool denials.** Per-agent tool rules in `hive.yaml` become hard CLI flags
at launch. GitHub MCP write tools are denied for every agent in every mode.
Claude Code's Remote Control feature is pinned off in every agent's settings
at each launch.

**Privilege model.** The hive process runs as user `dev` (UID 1001).
`su-exec` is the only setuid binary in the image, mode 4750 `root:hive-launch`,
so no agent user can run it. The setuid bits on the eleven setuid or setgid binaries the Debian
base image ships are stripped at build, and the build fails if the
inventory is anything but `su-exec`. `allowPrivilegeEscalation: false` is not used
because it would disable `su-exec` and break agent launch. The proxy CA
private key is stored with mode 0600 in a 0700 directory.

**Credential-free sandbox (opt-in).** A task can run in a rootless Podman
container or a Kubernetes Job that holds no credentials. Both the global
`agent_sandbox.enabled` and the per-agent `sandbox.enabled` must be set. A
push broker outside the sandbox scans committed diffs for secrets and
protected-path edits, mints a short-lived token and pushes the branch. See
[sandbox-isolation.md](sandbox-isolation.md).

## 4. Context and memory

Hive replaces long-lived agent memory with two inspectable stores. The work
ledger holds typed work items, status and notes as git-backed JSON. The
knowledge store holds typed facts with sources and confidence. Neither stores
conversation text. Agents are primed with a bounded set of facts in the task
prompt; they cannot query the store, so a poisoned fact cannot be amplified.
Untrusted text is scanned by `ioscan` before it reaches a prompt
([ioscan.md](ioscan.md)). Inbox item text (voice and meeting transcripts,
summaries, messages) is fenced in a nonce-tagged `<untrusted-inbox-data>`
envelope before it reaches spektacular or an agent prompt
([inbox-voice-sources.md](inbox-voice-sources.md)); canaries are planted per task and agent output is
scanned for them. Session state is pruned and logs are scrubbed
([security.md](security.md)).

Customer profiles follow the same rule. An agent cannot read the customers
store. When a kick is about an issue whose author, assignee or claimer is a
customer, the hive adds one short block to the prompt: their GitHub login, the
tags that are on, and the goals a person confirmed. The block is capped at 1,024
bytes. It never holds an email address, an id, notes or the satisfaction
assessment. A goal is text a person typed, so it is scanned by `ioscan` first. A
goal that holds an address, an id or a recognised attack is dropped whole. The
rest sit inside a fence with a random nonce, marked as data and not as
instructions. The meeting persona gets the same slice for a customer a question
names: a name, tags and confirmed goals. It never says notes or the satisfaction
assessment aloud, because a room can hold people who are not the hive's users.
Goals the hive inferred stay out until a person confirms them.

Untrusted recordings transcribed by the built-in whisper provider are decoded
and transcribed outside the hive process, in the `hive-whisper` sandbox: a
separate `hive-transcribe` uid with no home, shell or supplementary groups,
reached through su-exec; an empty environment (no tokens or keys); the audio
on stdin and the model as an inherited read-only descriptor; a private 0700
work dir removed on exit; FFmpeg (an LGPL build with only audio demuxers and
decoders and no network protocols) and whisper.cpp under rlimits (address
space, CPU seconds, file size, open files, no core dumps), `nice` and a
wall-clock timeout scaled to the recording; and an empty network namespace
where the kernel allows unprivileged user namespaces. With whisper the audio
never leaves the spoke; with Gemini or Deepgram it is sent to that provider
under the hive's own account ([inbox-transcription.md](inbox-transcription.md)).

Files dropped onto the Inbox (owner and read-write only) are typed by content,
not by name or client MIME type: magic bytes refuse executables, archives,
PDFs, Office and media files whatever they are called, then the text must be
UTF-8 and parse as the format its extension asks for. Extraction runs with a
timeout and size caps; the original, the extracted text and the meta are
stored 0600 in a 0700 directory per upload, never executed or served raw, and
bounded by a per-hive quota. The extracted text is fenced in the same
`<untrusted-inbox-data>` envelope in its Knowledge fact and before any agent
prompt ([inbox-uploads.md](inbox-uploads.md)). Uploaded PDFs are never parsed in the hive process: the
`hive-pdftext` sandbox reads them as the `hive-transcribe` uid through
su-exec with an empty environment, under prlimit (address space, CPU, file
size, open files, no core dumps) and nice, in an empty network namespace
where the kernel allows it, from a private 0700 work dir, with a wall-clock
timeout that kills the process group. Word documents are read in
Go with zip-bomb caps (entry count, declared and actual inflated size,
compression ratio) and an XML decoder that refuses DTDs and unknown entities
(no XXE). Links dropped onto the Inbox are
fetched by the hive under an SSRF guard: http/https only, no credentials in
the URL, the hive's own DNS lookup with the connection pinned to the checked
address (any answer in a refused range, listed below, refuses the link), every
redirect re-checked (at most 3), the body size- and time-capped, and the
environment proxy never used.

The same address classifier, `pkg/netguard`, is the single source of truth for
every server-side fetch of a URL a user or remote party supplies: the dashboard
and hub `isPrivateURL` guards and the Inbox link fetcher. An address is judged
by what it is, not how it is written. Decimal, octal, hex and short IPv4
spellings are parsed by value, and IPv4-mapped, NAT64 (`64:ff9b::/96`) and 6to4
(`2002::/16`) addresses are judged by the IPv4 address they carry. A host made
only of dot-separated numbers that is not a valid IPv4 literal, such as
`1.2.3.4.5`, is refused before any DNS lookup. A name that resolves to even one
refused address is refused, and a missing or failing resolver fails closed. The
refused ranges are:

| Family | Ranges |
|---|---|
| IPv4 | `0.0.0.0/8`, `10.0.0.0/8`, `100.64.0.0/10` (carrier-grade NAT, which includes the `100.100.100.200` metadata address), `127.0.0.0/8`, `169.254.0.0/16` (link-local, cloud metadata), `172.16.0.0/12`, `192.0.0.0/24`, `192.0.2.0/24`, `192.88.99.0/24`, `192.168.0.0/16`, `198.18.0.0/15`, `198.51.100.0/24`, `203.0.113.0/24`, `224.0.0.0/4` (multicast), `240.0.0.0/4` (reserved, includes broadcast) |
| IPv6 | `::/128`, `::1/128`, `100::/64` (discard-only), `2001::/32` (Teredo), `2001:db8::/32` (documentation), `fc00::/7` (unique local), `fe80::/10` (link-local), `fec0::/10` (site-local), `ff00::/8` (multicast) |

Names ending in `localhost` are refused as well. There is no opt-in to allow
`100.64.0.0/10`: no registered hive or hub URL uses that range. The
authoritative list is the switch and the `specialPurposePrefixes` table in
`src/pkg/netguard/netguard.go`.

### Customers: people the hive has seen

The Customers section keeps one profile per person. It lives in two files on the
hive's own volume and nothing in it leaves the hive. It is not sent to the hub,
written to GitHub or put in a heartbeat. The design is in
[customers](design/customers.md).

Some of these people never signed up. They are issue authors, meeting guests and
mail senders. The hive keeps only a key, a few counts and up to five references
for them, and only until someone accepts them. A suggestion ends when the thing
it came from is deleted. Forgetting a person deletes them and leaves a salted
hash, so they are not rebuilt.

Links between identities are made only on exact keys. A display name never links
anyone. One link rests on an assumption you should know about. When the access
list holds a name such as `github:alice`, or the bare login `alice` that a
GitHub device-flow sign-in produces (the access list treats the two as one
user), the hive treats `alice` as that person's GitHub login. A name with
another provider prefix, an address, a dot or an underscore is not read as a
login. The hub put that entry there after it signed the person in with GitHub,
so the hive trusts it. If your access list is edited by hand, an entry like
that gives the named person's profile the matching GitHub activity.

One part of this section is a request your browser makes, not the hive. The list
shows a GitHub picture for each person with a confirmed GitHub login. Your
browser asks `github.com/<login>.png` over https for the people on screen. It does
this late, only for rows you can see, and it sends no referrer. GitHub learns
your IP address and which logins were asked for. So it can tell you have those
GitHub users in your customer list. It learns nothing else about them. The hive
does not fetch or keep the pictures. A person known only by an email address is
never looked up, and neither is any hash of the address. The dashboard already
allows `https:` images, so no policy changes, and a test pins it. To turn the
pictures off, set `customers.avatars.enabled: false` or
`HIVE_CUSTOMERS_AVATARS_ENABLED=false`, or use the switch in Settings. The list
then shows initials and asks GitHub for nothing.

A read-only user sees only their own profile. An identity they add is shown as
unconfirmed. It links nothing until the owner or a read-write user confirms it.

Goal and satisfaction suggestions are off by default, because they send text to
a model. When the owner turns them on, the hive sends a customer's own issues,
pull requests and approved mail to the model set up for meetings. That text can
be about people who never signed up, so it is cleaned first. A line that holds an
address, an id or a recognised attack is dropped, and the rest sits in a fenced
block marked as data. The reply is checked too: at most three short goals, a
level from a short list and no number, each citing the evidence it was given. A
suggestion is never applied. A person confirms, edits or rejects it, and a value
a person set is never overwritten. The model has a daily token budget, and
nothing it suggests leaves the hive.

## 5. Network and credentials

**Inference keys.** An agent using a self-hosted inference gateway is
launched with a placeholder key, `sk-hive-<agent>`, and a base URL pointing
at a translator proxy on loopback inside the pod. The translator recognises
the agent from the placeholder and attaches the real key server-side. The
real key is read from a key file, a Kubernetes Secret mount, or an owner-only
file on the volume, never from `hive.yaml`, which records only paths and
variable names. The dashboard never returns a key value. Agents using cloud
CLI backends (Claude, Copilot, Codex, Goose) run with the provider credential
you connected, because the CLI needs it.

**Secrets at rest.** The secret values a spoke stores on its data volume
(the owner-only key files under `/data/secrets`, the Copilot device-flow
token, and the `github.token` value in the persisted runtime config) are
sealed through a keystore provider interface
([#18214](https://github.com/hivecommons/hive/issues/18214)). Each value is
written as a single-line envelope, `hive-enc:v1:<provider>:<key
generation>:<ciphertext>`, bound with additional authenticated data to the
slot it belongs in, so a ciphertext moved from one file to another does not
open. The default provider derives an AES-256-GCM key with HKDF-SHA256 from
`HIVE_SECRETS_KEY` (or a key file), falling back to the per-hive
`HIVE_HEARTBEAT_KEY` the hub already injects into the Deployment; a distinct
info string keeps the encryption key separate from the signing key it is
derived from. The threat this closes is the data volume on its own: a PVC
snapshot, a backup archive or a copied disk no longer yields the credentials.
It does not protect against an attacker who also holds the Deployment
environment or the hub master key. Files written before the keystore existed
are re-sealed on their first read, and a boot sweep converges the ones
nothing reads, so an existing hive needs no migration step; a retired key
generation is accepted for decryption through `HIVE_SECRETS_KEY_PREVIOUS`
while that happens. A hive with no key material keeps plaintext storage and
says so once at `WARN`; it never seals with a key stored beside the data.
OpenBao Transit is available as an opt-in provider
([#18216](https://github.com/hivecommons/hive/issues/18216)). Agent-facing
credential files and the read-only `/secrets` Kubernetes mount are not
touched. Variables: [env-vars.md](env-vars.md#secrets-at-rest-keystore).

**GitHub blast radius.** The recommended authentication is a GitHub App,
whose reach is limited to the repositories it is installed on. Hive mints a
token per agent matching its mode: advisory agents get read-only tokens,
measured agents can write issues, hold-gated and full agents can write code.
The repo allowlist is enforced at the proxy and repeated in every task
prompt. Below L6, no agent token can merge. Agent policy requires DCO
sign-off; pair it with a DCO check on your repos for a hard gate.

**The `gh` wrapper and relays.** `/usr/local/bin/gh` is a wrapper that
applies per-agent restriction rules and injects the agent's token; agents
never pass tokens themselves. A scrub script runs at the start of every shell
in the agent's process tree and removes inherited credentials. `POST /pulls`
and `PUT /pulls/{n}/merge` are denied at the proxy for every mode; agents call
`hive-open-pr`, `hive-open-issue`, `hive-merge` and `hive-review`, which drop
a request file that Hive processes as the App. The merge relay requires the
expected head commit and checks the pull request is on the governor's
merge-eligible list at that commit.

**Proxy-side credential injection (opt-in).** With
`HIVE_PROXY_INJECT_GH_AUTH=true`, the agent's token file holds only a
placeholder and the proxy swaps in the real token on GitHub API requests. It
requires GitHub App authentication and is refused if advisory proxy mode is
also set, because advisory mode trusts a self-asserted agent name. A
default-on version with a signing sidecar was proposed in
[#9586](https://github.com/hivecommons/hive/issues/9586) and declined in
September 2026; injection stays opt-in. Rotation of contributor registration
tokens is a separate open item
([#13804](https://github.com/hivecommons/hive/issues/13804)).

**Forced egress.** The entrypoint installs an iptables rule redirecting all
outbound port-443 traffic through the proxy, so agents cannot bypass it with
a raw token. This needs `CAP_NET_ADMIN`; without it the spoke refuses to
start unless `HIVE_PROXY_ADVISORY_OK=true` is set, which makes the gate
advisory only. Rootless Podman cannot grant `NET_ADMIN`, so rootless
deployments are unsupported for enforcement. The proxy identifies the calling
agent by the user ID that owns the socket, read from `/proc/net/tcp`, which a
process cannot forge. Only `api.github.com` is inspected; other hosts are
tunnelled without inspection. See
[net-admin-requirement.md](net-admin-requirement.md).

**Plain-HTTP forwards and uninspected tunnels.** The redirect covers port 443
only, so plain HTTP reaches the proxy through its explicit proxy port. Those
forwards are identified like any other request and dialled only when the
resolved address is globally routable: loopback services beside the proxy, the
link-local metadata endpoint, RFC 1918/CGNAT cluster addresses and multicast
are refused with `403` and counted as violations (loopback excepted — agents
are handed `NO_PROXY` for it, so it is logged as the client misconfiguration it
is). The same policy applies to the tunnels the proxy does not inspect: a
CONNECT to a host other than the inspected API and the transparent-TLS
passthrough for such a host are dialled through the same guarded dialer, so
`CONNECT 169.254.169.254:80` or an SNI naming a cluster address is refused on
the same terms rather than tunnelled from the proxy's own uid. The check runs
at dial time on the resolved address, so a hostname pointed inward is refused
on the same evidence as a literal IP. Set
`HIVE_PROXY_ALLOW_PRIVATE_FORWARD=true` only where a private-network
destination is intended; it lifts the guard for all three legs at once, and the
proxy warns at boot when it is set.

**Server-side fetches of user- or spoke-chosen URLs.** The dashboard's agent
import and workflow spec fetches, the hub's hive config proxy and webhook
relay to a spoke's self-reported dashboard URL, and knowledge document imports
check the URL before fetching and then dial through a pinned transport
(`netguard.PinnedDialer`): the name is resolved once per connection, every
answer must pass the same classifier, and the connection goes to an address
that was checked, never to the name. A name that rebinds between the check and
the dial is refused, and because the check runs per connection it covers
every redirect hop as well. Two limits are deliberate. When an
`HTTP(S)_PROXY` applies, the connection is to the operator's proxy and the
proxy resolves the target, the same limit as the agent proxy's chained
upstream leg, so the pre-fetch check and the redirect policy are the controls
there. Operator-configured gateway endpoints and the fixed-host `/api/style`
fetch are not pinned: a gateway may legitimately sit on a private address, and
redirects off either are still checked.

**Proxy decisions fail closed.** The egress audit
([#18986](https://github.com/hivecommons/hive/issues/18986), item 5) walked
every branch of the proxy's per-request decision path that can fail and
checked what each does. The rule is that a check the proxy cannot evaluate
refuses the request. Each branch below has a test that injects the failure.

| Failure | Outcome |
|---|---|
| Body read error or body over the scan limit, canary scan | Refused, `403`. |
| Body read error or body over the limit, Linear | Refused, `403`. |
| Body read error, GitHub GraphQL | The connection is closed and nothing is forwarded. |
| GraphQL or Linear document that does not parse | Refused by the mutation classifier. |
| REST method and path with no matching rule | Refused at every mode. |
| Repo pause, agent repo scope, auto-merge or `write_surface.enforce` predicate panics or hangs | That gate refuses, `403` naming the failed check ([#18994](https://github.com/hivecommons/hive/issues/18994)). |
| Review-thread ownership seams (which agent opened a PR, the hive's own logins) panic or hang | Refused, `403` naming the failed check ([#19224](https://github.com/hivecommons/hive/issues/19224)). |
| GitHub lookup behind the reference-repo and review-thread gates errors, panics or hangs | Refused, naming the lookup failure. |
| Canary scanner panics or hangs | Refused, also in advisory canary mode ([#19224](https://github.com/hivecommons/hive/issues/19224)). |
| No review-thread guard installed | Every guarded reply and delete is refused. |
| Caller cannot be identified (`/proc/net/tcp` read fails) | Treated as advisory: writes refused, reads allowed. |
| Plain-HTTP or uninspected tunnel address that cannot be parsed | Refused at dial time. |

The panic and timeout cases share one guard. A panic is recovered and a hang
is cut off at a deadline (two seconds for a config predicate, ten for a
canary scan, fifteen for a GitHub lookup), and either answer is that gate's
deny. Before the guard, a panic in any of these took down the whole proxy and
with it every agent's egress, and a hang held the request open with no
answer.

Accepted risks, written down rather than fixed:

- **Unidentified reads are allowed.** An agent the proxy cannot attribute is
  given advisory mode, which allows GitHub reads. Denying them would break
  every agent on a host where `/proc/net/tcp` cannot be read, and reads carry
  the agent's own scoped token. Writes from such a caller are refused and
  logged as a UID attribution failure.
- **Budget, credential and audit seams are not guarded.** The GitHub budget
  cap and reserve floor are rate limits: they fall back to config values, and
  a wrong answer costs quota, not a policy bypass. The agent token source,
  the Linear credential and the reference-repo token supply credentials; a
  missing one makes GitHub refuse the request. The refused-write audit, the
  canary leak handler and the review-reply handoff run after the decision
  is made and cannot turn a deny into an allow.
- **A timed-out check keeps running.** Go cannot cancel a goroutine, so a
  hung predicate finishes in the background after the request is refused.
  It holds no lock the request path needs.
- **The canary scan is off when not configured.** With `ioscan` or canaries
  disabled, no scan runs and nothing is refused on its account. That is the
  operator's choice, not a failure of the check.

**Hub-to-spoke channel.** Heartbeats carry a per-hive bearer key derived from
the hub master secret and are compared in constant time. The payload is
telemetry only: health, version, agent and governor summaries and 24-hour
token counts. No credential leaves a spoke in a heartbeat. The hub delivers
configuration, including App credentials for hosted spokes, in the response.

**Authorization decision point (shadow mode).** Several gates decide whether
a write may happen. They sit in different places, and the
[design record](design/authorization-decision-point.md) proposes one shared
point for them. The first slice is a measurement, and it is off by default.
Set `HIVE_AUTHZ_SHADOW=true` to turn it on.

With it on, a small checker re-derives one existing decision. That decision is
the push broker's check for an empty push or a protected path. The checker
runs after the broker has already decided. Hive compares the two answers and
keeps one short event for each check. It logs any difference.

The shadow enforces nothing. The broker stays the only authority, so a push
goes through or is refused exactly as before. Nothing acts on the checker's
answer. An event holds names, counts and codes. It never holds a request
body, a diff, a file, a list of paths or a credential. Free text in an event is
scrubbed and cut to a fixed length.

The seam is meant to fail closed once it enforces, and that rule is already
written down and tested. An error or crash in the checker counts as a deny. A
request with a missing required field also counts as a deny. In this phase
those are only log lines, not refusals.

## 6. Code admission

A green check is necessary but not sufficient. Between green CI and a merge,
Hive runs deterministic gates that do not depend on the agent's own claims:

| Gate | Refuses |
|---|---|
| Relay authorization | A merge request without an expected commit, or for a pull request not on the merge-eligible list at that commit, or from a user whose mode cannot merge. |
| Automerge preconditions | Draft, closed, not mergeable, `hold`, `needs-human`, `needs-rebase`, `do-not-merge`, `blocked`, `hive-pause/<hive-id>`; CI not green; a current `CHANGES_REQUESTED` review; a missing reviewer verdict when required. |
| Fresh fetch | Merging on cached state: the head commit is re-fetched and re-verified immediately before the merge call. |
| No-CI repositories | Merging where no checks exist unless `auto_merge.no_ci_ok` is explicit. |
| DCO | Merging a head whose DCO check is not passing. |
| Trusted-author automerge | Auto-merging a human or bot pull request unless the author is a trusted author with a `merger` or `owner` role and GitHub write access; permission lookup failures fail closed. |
| Intent verification | A pull request with no linked issue; a feature with no approved plan; any change to workflows, the `gh` wrapper, proxy rules, policies, `hive.yaml`, `OWNERS` or `CODEOWNERS` without maintainer approval. |
| Hold guard | Lifting a hold onto a head that moved since the human saw it; drift re-applies `hold` and posts one comment naming the new commits. |
| Review swarm verdicts | A self-asserted approval; verdicts are written by the review relay into a hive-owned directory and aggregated deterministically. |
| Escalation circuit breaker | A fourth fix attempt after three distinct red commits; the item is labelled and routed to a person. |
| Stage engine | Any writer of `stage/*` labels other than `src/pkg/stageengine`; an admission test enforces this. |
| Sentinel label | Hive approval and every automerge lane while `sentinel-alert` is present. |
| Repo pause | Agent writes, relays and automerge on a paused repo. |
| GitHub | Hive never force-merges; branch protection and rulesets apply to the App token as to a human. |

Every merge leaves a review evidence bundle: canonical JSON of what was
reviewed and which checks and verdicts were seen, hashed and optionally
Ed25519-signed. See [review-evidence.md](review-evidence.md). Check the
enforcing mode: `intent.enforce: true` blocks; shadow and report-only modes
do not.

## 7. Contributor trust

* **Reporter trust (opt-in)** withholds issues from authors outside the
  trusted set until a maintainer adds a triage label, and holds pull requests
  that trace to unaccepted issues. See
  [securing-your-hive.md](securing-your-hive.md).
* **Dashboard roles.** `authorized_users` carries `read`, `read-write`,
  `merger` or `owner`. Security settings, sandbox toggles and merge-lane
  releases are owner-only.
* **Contributor tiers.** Relay contributors move from `newcomer` to
  `contributor` automatically after five completed pull-request tasks, to
  `trusted` by operator grant, and to `merger` or `advisor` by maintainer
  decision. Tiers do not decay. See
  [contributor-trust-and-roles.md](contributor-trust-and-roles.md).
* **Claims and leases** are fenced by task generation, so no issue is held
  by two contributors at once. The lease protocol is formally modelled.
* **Author kind, not author claim.** Pull-request authors are classified
  from GitHub identity (Hive App, trusted bot, human, unknown bot).
* **Issue commands (045).** A human with repo write/maintain/admin access
  can use `/hive approve` or `/hive decision <direction>` to release decision
  parking. The command lifts `needs-human`, whoever applied it, but never
  `hold`. Label-driven repos use their configured acceptance labels instead.
* **PR commands (085/070).** Bare `/hive approve` is acknowledged only.
  On an eligible Hive-authored PR, `/hive decision <instruction>` routes
  the full text to the author/fixer. `/hive decision approve <rationale>`
  records approval for the current head and can release `requires_human`.
  This requires repo write access or higher **and** configured merger trust.
  Other authors use GitHub reviews/manual merging. Commands do not waive
  CI, holds, author admission or path fences. See
  [the exact gate contract](running-at-level-6.md#does-the-hive-reviewer-stop-a-merge).
* **Owner self-nomination** is a sentinel rule: a pull request adding its
  author to `OWNERS`, `CODEOWNERS` or `MAINTAINERS` is flagged and blocked.

## 8. Detection and response

**Sentinel** (default on) checks every open pull request against eight rules:
sensitive paths, owner self-nomination, permission escalation in workflows,
secret exposure, CI gate weakening, test removal, security policy edits, and
remote code execution patterns. A match applies `sentinel-alert`, posts one
comment and writes an audit entry. The label blocks Hive approval and every
automerge lane until a maintainer removes it.
Whether that label and comment reach GitHub is decided by the **write-back
containment gate** (`pkg/writeback`): on a PR Hive did not open, sentinel
writes need ACMM L5 plus the review opt-in label (L6 without it), the repo
must not be in `issues-only`/`off` mode, and the PR must not predate Hive's
write access to the repo (backfill guard). Below the gate the finding is
recorded, audited as `sentinel-finding-suppressed`, and shown in the
Security tab and `/api/sentinel` — the scan never stops, only the
unsolicited write does. The same gate fronts hold-gate notices, stage
labels, supersession and duplicate-fix notices and reporter-trust holds; a
ratchet test fails on any new direct GitHub write outside the package.

**Trajectory review** runs every 600 seconds by default. A second model
compares each agent's assigned intent with the last 120 lines of its rendered
terminal transcript and pauses the agent on divergence. It fails open if the
reviewer is unavailable.

**Session taint (observe only, off by default).** `session_taint.enabled`
turns on a recorder that remembers which coarse labels an agent session has
accrued (today: a canary hit sets `secret`) and, when that session later asks
the issue, comment, PR or review relay to write, records which rule would have
held the write. It writes a log line and an audit entry (`session_taint_label`,
`session_taint_would_act`) and keeps bounded in-memory counters; it never
blocks, holds, restricts or alters a request or an action, and it stores
labels, counts and ids only, never a credential or any content. A restart
starts every session clean. It is a measurement step toward the design in
[design/session-taint.md](design/session-taint.md); no enforcement exists, so
it adds no protection yet and must not be read as a control. Reads Hive cannot
see (local files, shell output, direct provider traffic) produce no label.

**Audit trails.** The dashboard audit log records every dashboard and API
mutation, merge decision, sentinel finding and watchdog action to
`/data/audit.jsonl`, rotated at 5 MB with three backups and a 90-day
retention. The token-access log records which agent used a credential for
which command. Proxy decisions, review evidence bundles and scrubbed agent
transcripts complete the set. See [audit-log.md](audit-log.md) and
[token-access-log.md](token-access-log.md).

**Budgets and caps.** A seven-day token budget warns at a configurable
percentage and suppresses tasks on exhaustion. Fair-share scheduling spreads
tasks across repositories; every sweep that writes labels or comments has a
per-pass cap. Fix cycles share a re-engagement limit.

**Alerts.** Sentinel findings, trajectory pauses, crash loops and
`needs-human` routing appear as dashboard banners and, when configured, as
notifications.

## 9. Platform and supply chain

**Hub-routed dashboards.** Each hosted hive has its own hostname. The hub
authenticates every request (GitHub, Google or a configured OIDC provider)
and returns 403 unless that user has a grant on that hive. On success it
injects the verified identity as headers that the spoke re-verifies. Grants
carry `read`, `read-write`, `merger` or `owner`.

**Direct-route spokes** enforce their own authentication: GitHub device-flow
login against a per-hive allowlist, per-user opaque sessions in `HttpOnly`
cookies, forged identity headers stripped, and the shared bearer token
disabled for user login.

**Hosted isolation.** One namespace, one pod and one volume per hive. The
per-hive Role is namespace-scoped and limited to the hive's own Secret.
Internal server-to-server calls carry an auth token compared in constant
time.

**Keys.** Sessions and single sign-on are verified with Ed25519 signatures
only; there is no symmetric fallback, and a missing key fails closed. Every
spoke uses keys derived per hive from the hub master secret. The master
secret supports two generations, current and previous, with a seven-day
verify-only window; rotation is triggered by an admin endpoint and spokes
converge through a 15-minute reconcile sweep. Setting a fresh master secret
directly strands every spoke.

**Webhooks and terminals.** GitHub webhooks are verified with HMAC-SHA256
against `X-Hub-Signature-256`. Terminal access uses short-lived signed
assertions. Systemd units refuse to run code from a directory an
unprivileged user could have created.

**Sessions.** With `dashboard.sessions.enabled` (default on; Settings →
Dashboard → Inception (spek) sessions, overridden by `HIVE_SESSIONS_ENABLED`,
`false` turns it off) the dashboard
offers interactive CLI terminals (`/api/sessions`, [API
reference](api-reference/sessions.md)). A session is a plain vendor CLI, never
a shell, launched with none of the permission-bypass flags agents run with, so
the CLI's own approval prompts stay on. The isolation model, in order of
importance:

- **Its own unprivileged uid.** Each session's tmux server and CLI run as a uid
  from a reserved range (`HIVE_SESSION_UID_BASE`, default 20001, with
  `HIVE_SESSION_UID_SLOTS` slots) that cannot collide with agent uids
  (`HIVE_UID_BASE`, 2001 upward) or the hive's own, through the same `su-exec`
  hop agents use. The slot is allocated by the dashboard store, persisted in
  the session record and reused on resume; the runtime refuses a range that
  overlaps the agent uid map and recovers a session's uid from the owner of
  its home directory, never from the id. The uid has its own gid, is not in
  group `node` (all agents) and not in `hive-launch` (`su-exec`), so it cannot
  read `/secrets/*.pem`, `/data/secrets`, `/data/.hive`, the shared vendor
  logins in `/data/home` (group-only, 2770, re-asserted by the entrypoint), the
  hive's `/proc/<pid>/environ`, or another session's sandbox, and it cannot
  reach another session's tmux server.
- **A private sandbox.** `HIVE_SESSIONS_SANDBOX_DIR` (default
  `/data/session-sandbox`) holds one `root:hive-launch 0751` directory per
  session with session-owned `home`, `work` (uploads) and `tmp` below it, so
  the session cannot replace its own top-level directories and the hive reads
  transcripts for resume only through the launcher group. Deleting or pruning a
  session removes the sandbox and the uid's tmux socket directory before the
  slot can be reused; if removal fails the record and slot are kept and retried.
- **A shared clone, a worktree per session, no shared credential.** The hive
  keeps one per-repository clone (`HIVE_SESSIONS_WORKSPACE_DIR`, default
  `<sandbox root>/workspace/<org>/<repo>`) that it clones once as itself,
  with a short-lived App token in a one-shot `git -c credential.helper` file
  removed right after, so neither the clone's config nor its remote holds a
  credential; root then hands the tree to the sessions-only group
  (`HIVE_SESSIONS_GID`, default 20000) as `root:<gid> 2770`, group read-write,
  nothing for "other". That group is every session's single group (su-exec
  gives a numeric uid exactly one), which is what lets sessions share the
  clone's objects and branches; it is refused when it is the hive's own group,
  the reader group or inside the session uid range, and session sandboxes stay
  `hive-launch` group-only, so sessions still cannot read each other's homes.
  The hive itself runs as `dev`, which is never in that group, so it cannot
  look inside the clone or a worktree: every check of what is in them runs in
  a script as the session's own uid.
  Each session opens in its own git worktree of that clone,
  `<workspace>/.sessions/<id>/<org>/<repo>` on branch `hive-session/<id>`: the
  hive makes the directory under hive-owned `0711` parents, root only hands
  that one empty directory (never one another session already owns) to
  `<session uid>:<gid> 2770`, and the session uid itself runs
  `git worktree add`. Git never runs as root in the clone. `spektacular init`
  runs in the session's worktree as the session uid.
- **The shared clone's git config and hooks are locked.** Before any session
  gets a worktree of a clone, and once per clone at every boot (which fixes a
  clone made by an earlier release), root rewrites the clone's `.git/config`
  from scratch, without running git, as `root:<gid> 0644`, and empties
  `.git/hooks`, `.git/info` and `.git/modules` into `root 0755` directories.
  `extensions.worktreeConfig` is on, so a person's own settings go in
  `git config --worktree`, the worktree's own `config.worktree`, which only
  that session's uid can write. Root-owned placeholders take the names a
  session could otherwise create to redirect or configure git
  (`.git/config.worktree`, `.git/commondir`). `objects/`, `refs/` and `logs/`
  stay setgid group-writable, so commits, branches and fetches work.
  `.git/worktrees/`, `.git` and the clone directory are `3770`: a session can
  add its own entry, but the sticky bit stops it from renaming or replacing
  anything root or another session owns there. A session's entry under
  `.git/worktrees/` is created with no group write, even while
  `git worktree add` is still checking it out. A session's pane gets no
  `safe.directory`, because its worktree is its own; git's ownership check
  therefore refuses a `.git` that another uid swapped into the
  group-writable worktree. As a result, **one session can no longer plant a
  config entry, hook, filter driver, `core.fsmonitor` or alias that runs as
  another session's uid**, whether in that person's own git or in the git the
  hive runs for them. The hive's own git always passes
  `-c core.hooksPath=/dev/null -c core.fsmonitor=false`. Before the
  add and remove scripts run git in a worktree, they check (as the session
  uid) that its `.git` file, its entry under `.git/worktrees/` and that
  entry's `commondir` are the session's own and lead back to the clone; if
  they don't, the scripts refuse. What stays shared is data, not code: any
  session can still read, write or delete the shared objects and refs, and a
  branch another session made. A worktree is group-writable, so another
  session can edit or delete its files. The worktree keeps one person's
  working tree, checked-out branch and spek workflow state (`.spektacular/`)
  apart from another's; it is not a defence against a session that sets out
  to corrupt shared data. The lock also costs these, by design: writes to the
  shared config fail (`git push -u` and `git branch --set-upstream-to` warn;
  use `git config --worktree`), deleting a ref that is still in the clone's
  `packed-refs` fails (for example a remote-tracking branch that
  `git fetch --prune` drops; session branches are never packed), gc and
  maintenance are off, submodules can't be initialised, and hooks only run
  from a path the session names with `git config --worktree core.hooksPath`.
- **Deleting a session never silently drops its work.** The removal script
  runs as the worktree's owner uid. It auto-saves anything uncommitted
  (untracked files included, ignored ones not) as a commit on the worktree's
  branch, keeps a detached `HEAD` as `hive-session/<id>-detached`, then
  deletes the worktree's entry, prunes, and keeps any branch the clone's
  `HEAD` lacks. If git fails, the removal fails, and the record and uid slot
  are kept for a retry. The periodic sweep removes the worktrees of sessions
  whose record is gone. It does not run while the sessions store has failed
  to load, or while an unreadable copy is set aside for recovery, so an
  empty-looking store can't remove every worktree. Pushing from a session
  uses the person's own login; the hive's token never enters a session.
- **Environment.** Built from an allowlist: no dashboard token, terminal, hub,
  heartbeat, ttyd or attach-key secret and no GitHub token reaches it.
- **Logins are read-only copies, not links.** A fixed allowlist of bare login
  files (`.claude/.credentials.json`, `.copilot/config.json`,
  `.gemini/oauth_creds.json`, `.gemini/google_accounts.json`,
  `.codex/auth.json`, `opencode/auth.json`) is copied into the session home
  mode 0400, session-owned. `settings.json`, hooks, MCP or extension config,
  `gh`'s `hosts.yml` and `~/.config` are never bridged, so a session can neither
  rewrite shared configuration nor plant a hook an agent would run.
- **One refreshable Claude login, and it is not in a session.** A copy of an
  OAuth login that can refresh is a second holder of the grant: when the CLI
  in a session refreshed its copy, the refresh rotated the grant, the shared
  file the agents read was left with a dead refresh token, and every agent on
  the hive fell to the login prompt until a person signed in again
  ([#18724](https://github.com/hivecommons/hive/issues/18724)). A session
  therefore gets the Claude login **without its refresh token**, and only the
  login's own token block, nothing else stored in that file. It can use the
  fleet's current access token and cannot rotate or carry away the long-lived
  grant. Who refreshes it: the hive itself, once, ahead of expiry
  ([#18731](https://github.com/hivecommons/hive/issues/18731); it redeems the
  shared refresh token and holds the new grant on disk and in memory until the
  shared file has it, `HIVE_CLAUDE_SHARED_REFRESH=off` disables it), the
  agents' credential bridge when a CLI refreshed first anyway, and the
  dashboard sign-in. A session never refreshes. All writers of the shared file
  go through one function under one lock that accepts only a usable credential
  expiring strictly later than the current one. The hive keeps
  the copies current (it replaces them, atomically and as the session's uid,
  when the shared login changes), so a session depends on the fleet's login
  being refreshed by an agent or a sign-in; a session cannot do that itself.
  The session uid still has no access to `/data/home`: nothing is linked or
  bound into the session, and the hive reads a session's login file as that
  session's uid, through a size-bounded helper with the minimal session
  environment.
  As a safety net, a complete Claude login that is in a session home anyway (a
  copy made before this rule, or a person signing in from inside a session)
  is **adopted** into the shared file when it is usable and strictly newer
  than the shared login: a later access-token expiry and a file written after
  the shared one, or a shared login that is not usable at all. Only expiry
  and modification time decide; token values are never logged. The replaced
  shared file is kept beside it as `.credentials.json.stale-<UTC time>`, the
  session is put back on a refresh-less copy, and a
  `user_session_login_adopted` audit entry names the session and the trigger.
  This runs at boot, on every credential-bridge poll
  (`HIVE_CLAUDE_CREDENTIAL_BRIDGE_INTERVAL`), after a dashboard sign-in, when
  a session starts or resumes, and before a session's sandbox is deleted: if
  the login cannot be adopted the deletion fails and is retried, so removing
  a session never removes the only valid login. It runs under the lock the
  agents' bridge repair holds, so two newer logins settle on the later one.
  Note what this means: anyone who may use a session's keyboard can sign that
  session in with an account of their own, and that login becomes the hive's
  when it is newer. The audit entry is the record of it, and a dashboard
  sign-in puts the hive back on the operator's account.
  Codex's ChatGPT login (`.codex/auth.json`) and an OpenCode OAuth entry
  (`opencode/auth.json`) rotate too, and follow the same three rules
  ([#18740](https://github.com/hivecommons/hive/issues/18740)). A session
  gets them **without a refresh token**: the key stays (Codex:
  `tokens.refresh_token`; OpenCode: `refresh` of an `oauth` entry), because
  both CLIs require it in their login file, but its value is empty. Both CLIs
  accept an empty value (from their sources, Codex 0.170.0 and OpenCode v1;
  the live check below confirms it on a real install): the CLI keeps using
  the access token, and a refresh it attempts fails without rotating
  anything. Every other field is copied as it is. So the shared file, which
  each agent links to, is the only refreshable holder of the grant, and only
  the agents' CLIs refresh it; a session refresh and an agent refresh can no
  longer race. The hive replaces a session's copy, atomically and as the
  session's uid, when the shared file was written after it. A session
  therefore depends on an agent or a sign-in refreshing that login before
  its access token expires. As a safety net, a complete login that is in a
  session home anyway (a full copy from an earlier release, refreshed there,
  or a person signing in inside the session) is **adopted** into the shared
  file when it is strictly newer. Codex: the session's file was written
  after the shared one, holds a ChatGPT login with a different refresh token
  and records a later `last_refresh`, and the shared file itself holds a
  ChatGPT login (an API-key login does not rotate and is never replaced).
  OpenCode, per provider entry: both entries are `oauth`, the session's has a
  different refresh token and a later `expires`, and the session's file was
  written after the shared one; only those entries are replaced, every other
  entry of the shared file is kept, and a provider the shared file does not
  hold is not added. Only those fields and modification times decide; token
  values are never logged. The replaced file is kept as
  `auth.json.stale-<UTC time>`, the new one keeps the old one's mode (and
  owner, where the hive may set it), `user_session_login_adopted` names the
  session, trigger, vendor and file, and the session is put back on a
  refresh-less copy. A full copy that carries the shared file's own refresh
  token is put on a refresh-less copy too; a refresh token the shared file
  does not hold that is not newer is the session's own login and is left
  alone. It runs when the Claude check runs, deletion included: a newer login
  that cannot be adopted keeps the session. The hive never replaces a shared
  file that changed after it was compared, so an agent refresh that lands
  during a check is never overwritten. Gemini's Google refresh token and the
  Copilot CLI token do not rotate and are still plain copies.
  Manual check, needs a person and a real install (not run in CI, so the
  assumed file formats stay unconfirmed until it is done,
  [#18740](https://github.com/hivecommons/hive/issues/18740)): on a hive with
  a signed-in Codex and OpenCode, (1) start a session, confirm
  `.codex/auth.json` and `opencode/auth.json` in its home match the shared
  files' shape (compare key names only, never print values); (2) confirm
  the refresh keys in the session's copies are present and empty, that the
  CLI in the session still works, and that once its access token expires
  (Codex: a request past the token's expiry; OpenCode: past `expires`) its
  refresh attempt fails without changing the shared file and without an
  `auth.json.stale-*` or a `user_session_login_adopted` entry;
  (3) confirm an agent of that vendor is still logged in; (4) remove the
  session and confirm the shared login still works; (5) grep the hive log
  for the access and refresh token values: there must be none. Attach a
  redacted real file, with token values replaced, as the test fixture. For Gemini and Copilot a resume no
  longer overwrites a file the session's CLI rewrote after the shared one
  was written.
- **Terminal access is bound.** ttyd listens on loopback without a secret a
  session uid could not read (its `-c` credential is visible in `/proc`), so
  session attaches are gated by a per-session token, HMAC-SHA256 of the tmux
  session name under a per-boot `HIVE_TTYD_ATTACH_KEY` that only the hive and
  ttyd hold. The dashboard proxy appends it after authorising the request;
  `ttyd-tmux.sh` refuses any `hive-sess-*` attach without a valid token,
  refuses a socket not owned by a reserved session uid and runs the attach
  with a scrubbed environment. No key means no session terminal. A handoff code
  minted for a session is bound to that session id and its caller (anyone who
  may see the session) and opens only `?arg=hive-sess-<id>`; an agent-terminal
  code never opens a session and still needs owner or read-write.
- **One keyboard at a time.** Every attach is read-only (`tmux attach-session
  -r`) unless the proxy also appends the session's *write token*, a second
  HMAC under the same key (`hive-ttyd-attach-rw-v1`), which it does only for
  the one user holding the session's keyboard lease. The browser's request
  carries a single arg, the session name; an extra arg is rejected as
  malformed before it reaches ttyd, and `ttyd-tmux.sh` treats a missing or
  wrong write token as read-only, so a viewer cannot promote itself. The
  creator holds the keyboard from the start; owners and the creator may take
  it at any time, another read-write user only after it is released or the
  holder has been idle for `HIVE_SESSIONS_KEYBOARD_GRACE_SEC` (default 60).
  Taking it detaches the previous holder's writable tmux client immediately.
- **Ownership.** A session is private by default: only its creator and owners
  can list, read, watch, type into or upload to it. The creator or an owner may
  set `shared`, which lists it for every role (the read role gets the
  token-redacted scrollback and a read-only live terminal, never the keyboard,
  Send or upload) and lets read-write users Send, upload and take the
  keyboard. Renaming, closing, resuming, deleting and the `shared` flag stay
  with the creator and owners. Ids of sessions a caller may not see are never
  disclosed.
- **Startup screens only.** When a session starts the hive takes the CLI to
  its input prompt by answering its startup screens (first-run notice, trust
  for the session's own working directory, onboarding, an update menu), with
  the detector agents use restricted to those screens; a Claude Code HOME is
  also seeded so they do not appear. This does not weaken the posture above:
  a tool or command permission prompt is classified as refused and is never
  answered (the refusal is checked before anything else and across every
  vendor's patterns, and a test fails when a prompt added for agents is not
  classified), a login or OAuth screen is never answered (the tab says
  "needs sign-in"), the seed carries no account or credential and never
  replaces an existing file, and nothing is sent once a person has typed
  into the session, after the input prompt is reached, or after a bounded
  window. Trusting the working directory is the one decision made for the
  person: it is the session's own worktree of the hive's project repository,
  the directory they asked to work in. Each outcome is audited as
  `session_autostart`. `dashboard.sessions.autostart: false` or
  `HIVE_SESSIONS_AUTOSTART=false` turns it off. See
  [Startup auto-answer](api-reference/sessions.md#startup-auto-answer).
- **Resume ids** never start with `-` and a resume command may carry only the
  vendor binary, a fixed set of resume words and a flag-free id, so a planted
  transcript name cannot add `--dangerously-skip-permissions`.
- **Voice notes stay in the browser.** The input bar's Voice button (write
  roles only) uses the browser's Web Speech API, so transcription is done by
  the browser vendor's speech service (Chrome sends the audio to Google), not
  by Hive; the audio never reaches the Hive backend. The transcript only fills
  the Send textarea and goes through the same queued `/input` path as typed
  text when the user presses Send. To allow it, the gateway's
  `Permissions-Policy` for the dashboard document is
  `geolocation=(), camera=(), microphone=(self)` (`src/deploy/nginx.conf`,
  `map $uri $hive_document_permissions_policy`); the ttyd terminal iframe
  (`/terminal/`) and `/api/` responses keep `microphone=()`, and no other origin
  is ever granted the microphone. The same document policy carries
  `display-capture=(self)` for Inbox ● Record's tab audio
  ([inbox-recording.md](inbox-recording.md)); `/terminal/` and `/api/` keep
  `display-capture=()`.

Audit: `user_session_login_adopted` (a newer Claude, Codex or OpenCode login
found in a session home was adopted into the shared login; session id,
trigger, the preserved file's path and, for Codex and OpenCode, the vendor and
file, never token material), `session_created`, `session_started` (the
outcome of the background start), `session_autostart` (how the startup auto-answer ended and which
screens it answered), `session_closed`, `session_renamed`, `session_input` and
`session_upload` record the user and session id (never input text).
`session_input` is written when a queued Send is actually typed into the pane,
as the user who queued it, so attribution follows the sender, not the moment
of delivery. `session_keyboard` records every keyboard hand-over (`from`,
`to`); since only the holder's attach is writable, keystrokes typed inside the
terminal are attributable to the holder of record at that time. Keystrokes are
still not logged individually.

Operator requirements the code cannot enforce: the container needs the
`su-exec` helper with `CAP_SETUID`, `CAP_SETGID` and `CAP_CHOWN` (the standard
agent layout; a rootless or single-uid runtime cannot run sessions and they fail
closed). `/proc` should be mounted `hidepid=2` (or the pod run with a
non-dumpable, PID-namespaced layout) where possible: `ttyd`'s command line
exposes its basic-auth credential to every uid, and agent terminals on the same
loopback ttyd are not covered by the session attach token, so a session user on
a hive that also runs agents can still reach them there until ttyd moves to a
credential only the hive can read. Reserve `HIVE_SESSION_UID_BASE` above every
agent uid and keep `/data` and `/data/home` closed to "other".

**Dashboard headers.** Content Security Policy, `X-Frame-Options` and related
headers are always set. The metrics endpoint is off unless
`HIVE_METRICS_ENABLED` is set.

**Supply chain.** Global npm installs run with `--ignore-scripts`. Base
images are pinned by digest. Tools such as tmux, ttyd, `gh`, goose and
`su-exec` are checksum-verified at build. Third-party Actions are pinned to
commit SHAs. CI runs `govulncheck`, `gosec` and `golangci-lint` as separate
jobs; the agent image ships the same pinned `govulncheck` and `gosec` so the
sec-check lane runs them on Go targets inside the pod without downloading
anything (`GOVULNCHECK_VERSION`, `GOSEC_VERSION` in `src/Dockerfile`).
CI also runs OpenSSF Scorecard, Dependabot, an Action-pin check, a setuid inventory
check, DCO trailer checks, a documentation link check and formal-model
checks. Image attestations are turned off because they broke
multi-architecture manifests, and a guard workflow keeps them off. An SPDX
software bill of materials per image is attached to each release
([releases.md](releases.md)). Container images are not signed.
`.github/CODEOWNERS` covers the security-sensitive paths but is advisory
until code-owner review is required in branch protection.

## 10. Human in the loop

* **ACMM** is the autonomy dial. Merge permission is not minted below L6.
  L5 adds a `hold` label on every agent pull request. See
  [acmm-policy-matrix.md](acmm-policy-matrix.md).
* **Issue decision parking is 045.** A person with repository write access or
  higher answers with `/hive approve` or `/hive decision <text>`, unless the
  repo uses label-driven acceptance. This releases decision parking and
  `needs-human`, not `hold`.
* **PR decision parking is 085.** A trusted merger's GitHub **Approve**
  review must name the current head to clear the enforcing sweep gate.
  It can override any swarm outcome, including a request for changes;
  the audit records who overrode what. On eligible Hive-authored PRs,
  `/hive decision approve` can answer `requires_human`, while other decision
  text sends work to the fixer. Bare `/hive approve` never releases a PR.
  The detailed [command boundaries](#7-contributor-trust) above still apply.
* **Owner-only security settings.** Changes to `ioscan`, intent enforcement,
  review requirements and the agent sandbox require the `owner` role.
* **Emergency stops are not interchangeable.** To stop merging, disable
  automerge per repo, apply an item hold, or drop below L6. Pausing an agent
  or freezing the watchdog does not stop the App from merging existing PRs.
  Use the [L6 emergency-stop table](running-at-level-6.md#stops-smallest-to-largest)
  to choose the right boundary.
* **Hooks** run only vetted actions from a fixed set ([hooks.md](hooks.md)).
* **Advisory digests** are pinned to one analysed commit; findings whose
  file no longer exists are dropped.

## 11. What Hive does not do yet

* **Rootless containers cannot enforce the egress gate.**
* **Nothing remembers what a session has read.** Pattern scanning checks each
  outgoing message on its own, so a reworded copy of something an agent read
  earlier passes. Session taint is designed for this
  ([design/session-taint.md](design/session-taint.md)); only its observe-only
  recorder exists, and it does not block anything.
* **The proxy inspects GitHub API traffic only.** Other destinations are
  tunnelled. Use Kubernetes NetworkPolicies for full egress control.
* **The L5 `hold` label is applied by agent policy.** The hard guarantee at
  L5 is the withheld merge permission, not the label.
* **Cloud CLI credentials are shared with the CLI.** The placeholder-key
  isolation applies to inference-gateway keys only.
* **DCO is policy-driven inside Hive.** Enforce it repo-side for a hard
  guarantee.
* **Agents share one container.** Per-user separation is not per-task
  containers. The sandbox is opt-in and two-gated.
* **Container images are not signed.**
* **No CodeQL or container CVE scan** runs in this repo's CI.
* **Branch protection is GitHub's.** Hive does not configure or verify it.
* **Gate-integrity invariants are a proposal.** Refusing agent force-pushes
  and history rewrites is designed
  ([design/gate-integrity-invariants.md](design/gate-integrity-invariants.md)),
  not enforced.
* **Hub registration has no review step.** Any spoke with the derived keys
  can register.
* **Trust tiers do not decay.**
* **Trajectory review fails open; the `ioscan` classifier fails open only
  at L1–L4.** At L5–L6 text is withheld when the classifier cannot answer.
  The deterministic rules, canaries, proxy and token tiers hold when a model
  is down.

## 12. How to audit a running hive

Use an owner-authorized session. Keep production inspection read-only. Do not
paste tokens, environments or Secret contents into reports.

| Question | Check |
|---|---|
| Is the egress gate enforcing? | Boot log has no `HIVE_PROXY_ADVISORY_OK` warning; `iptables -t nat -L HIVE_PROXY` in the pod lists the port-443 redirect. |
| Are agents separated? | `cat /var/run/hive/uid-map.json`; `ls -l /usr/local/bin/su-exec` shows `-rwsr-x--- root hive-launch`; `find / -perm /6000 -type f` lists only `su-exec`. |
| Does an agent hold a usable token? | `GET /api/config/governor` → `security.credentialInjection`. |
| Is `ioscan` on, and in which mode? | `GET /api/config/governor` → `security.ioscanEnabled`, `ioscanFailMode`, `ioscanCanaries`. |
| Is trajectory review on? | `GET /api/config/governor` → `trajectory`. |
| Is the sentinel on? | `sentinel` config and recent findings with the alert label. |
| Who merged what, and why? | `GET /api/audit`; `hivectl evidence export --repo <o/r> --since <date>`. |
| Who used a token? | `/var/run/hive-metrics/token-access.jsonl`. |
| What is the merge posture per repo? | `hive.yaml` → `acmm_level`, `project.repo_policies[].auto_merge`, `auto_merge.trusted_authors`, `project.paused_repos`. |
| Are webhooks authenticated? | `GITHUB_WEBHOOK_SECRET` is configured; test unsigned-request rejection only in a disposable hive. |
| Is the image what CI built? | GHCR digest from the `docker.yml` run equals the running `imageID`. |

To test denials, use a disposable evaluation hive and an operator-owned test
repository. A write to a repository outside the allowlist must return `403`
with `X-Hive-Proxy-Blocked`; a request authenticated with a placeholder token
must fail; an unsigned webhook must be rejected. Never probe production by
relaxing gates.

## Compliance

Hive holds no SOC 2, FedRAMP, ISO 27001 or other certification.
[compliance.md](compliance.md) maps the controls above to SOC 2 requirements
and lists the requirements Hive cannot cover.

## Reporting vulnerabilities

Follow [SECURITY.md](../../SECURITY.md): use GitHub private vulnerability
reporting from the repository's Security tab, or contact a maintainer
directly. Do not open a public issue.
