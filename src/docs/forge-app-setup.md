# Forge setup: GitLab, Gitea, and Forgejo

> **Read this first.** On **GitHub.com and GitHub Enterprise** the Forge App is a
> GitHub App and everything works — see [GitHub App setup](github-app-setup.md).
> On **GitLab, Gitea, and Forgejo** it does not. The adapters exist in the source
> tree and are tested, but they are **still not on a path a non-GitHub hive can
> reach**. Two callers now construct an adapter from `project.forge` — the
> governor's escalation writes ([#5259](https://github.com/hivecommons/hive/issues/5259))
> and the hold guard's re-hold. A hive whose repositories are all on GitLab,
> Gitea or Forgejo now enumerates its work through the adapters
> ([#19669](https://github.com/hivecommons/hive/issues/19669)), so those writes
> are reached. Stage labels, merging, agent execution and mixed GitHub hives are
> still missing. The operation list per adapter is on
> [Forges: GitLab, Gitea and Forgejo](integrations/forges.md). There is no
> non-GitHub setup path to follow yet, and this page exists so you can establish
> that in one read instead of discovering it after an install.

## Terminology

The dashboard and docs call the app Hive installs on your source control system
the **Forge App**, and the docs call the system itself your **forge**. That
naming is deliberately forge-neutral because the code abstraction underneath it
is. The naming does not, on its own, imply the non-GitHub paths are finished —
this page is the difference.

## What is supported today

| Forge | Agents do work | Read API | Comment / label / hold | Open issues & PRs | Merge | Dashboard login |
| --- | --- | --- | --- | --- | --- | --- |
| GitHub.com | Yes | Yes | Yes | Yes | Yes | Yes |
| GitHub Enterprise | Yes | Yes | Yes | Yes | Yes | Yes |
| GitLab (SaaS or self-managed) | **No** | Adapter only, unwired | Wired but unreachable | **Not implemented** | Adapter only, unwired | **No** |
| Gitea | **No** | Adapter only, unwired | Wired but unreachable | **Not implemented** | **Not implemented** | **No** |
| Forgejo | **No** | Adapter only, unwired | Wired but unreachable | **Not implemented** | **Not implemented** | **No** |

"Adapter only, unwired" means the Go code is written and covered by tests, but no
running code path calls it. "Wired but unreachable" is the narrower state the
comment/label column is now in: two production callers — the governor's
escalation sweep and the hold guard — select their adapter from `project.forge`.
They would post the evidence comment and the `needs-human` label, or the drift
comment and a re-applied `hold` label, to your forge. Both are driven by the
governor's enumeration. On a forge-only hive that enumeration now runs through
the adapter, so they fire; in a hive with any repository on GitHub, a repository
on another forge is still enumerated through GitHub and finds nothing. Gitea and Forgejo are separate
kinds (`gitea` and `forgejo`) that share one REST client, so a call that differs
between them can change in one kind without touching the other. Forgejo has its
own kind, config block and adapter type. It is no further along than Gitea.

**A hive cannot presently run against GitLab, Gitea, or Forgejo.** If that is
your forge, there is nothing to install and no configuration that will make
agents work.

This is the state the [roadmap](roadmap.md) records for the abstraction: the
adapters are built, the first production caller is behind the seam, and what
remains is the read path — neutralizing enumeration, which today owns hold-label
filtering, issue filters and SLA tracking inside `pkg/github`. The design
rationale is [ADR-0005](adr/0005-forge-abstraction.md).

## Why: the agent execution path is GitHub-only

The forge abstraction and the thing agents actually run are two different
mechanisms, and only the second one moves work.

Agents do not call the forge abstraction. They shell out to the **`gh` CLI**,
through the wrapper Hive installs ahead of it on `PATH`
(`bin/gh-wrapper.sh`), which injects a **GitHub App installation token** and
enforces the per-agent restriction rules. The higher-level helpers agents use to
file work — `bin/hive-open-issue.sh`, `bin/hive-open-pr.sh`, `bin/hive-merge.sh`
— are all built on that same `gh` path. There is no `glab` equivalent, no Gitea
CLI, and no forge-neutral shell entry point.

The wrapper does not even let an agent call GitHub directly for writes: it
intercepts `gh pr create`, `gh issue create`, and `gh issue comment` and redirects
them to those helpers, which write request files that the hive daemon consumes
and executes with an App installation token. The GitHub MCP server's write tools
are explicitly denied for the same reason, so that route is closed too.

So the write path that matters — open an issue, push a branch, open a PR, merge
it — is GitHub-shaped end to end, independently of anything `project.forge` says.

Three further consequences follow, and each is load-bearing:

- **Authentication.** Every credential path is a GitHub App: App ID, private key,
  installation ID, per-tier minted installation tokens. GitLab and Gitea have no
  GitHub App, so none of that machinery has an analogue. The adapters authenticate
  with a plain personal access token instead, which is why they could never simply
  inherit the existing credential plumbing.
- **Dashboard login.** Signing in uses GitHub OAuth or a configured OIDC provider.
  There is no GitLab or Gitea login provider, including for a hive whose
  `project.forge` names one.
- **Webhooks.** Both forge webhook receivers verify `X-Hub-Signature-256` and read
  `X-GitHub-Event`. There is no GitLab (`X-Gitlab-Token`) or Gitea receiver.

## Why the adapters exist anyway

`src/pkg/forge` is a genuine, tested abstraction, written so Hive is not
permanently welded to GitHub. It defines a forge-neutral `Forge` interface plus
neutral types — note `ChangeRequest` rather than "pull request", since that is a
GitHub term — and three adapters: GitHub (wrapping `pkg/github`), GitLab (REST
v4), and Gitea/Forgejo (REST v1).

All three implement the same operation set:

| Operation | Meaning |
| --- | --- |
| `GetRepo` | Fetch one repo/project by its slug |
| `ListOpenIssues` | Open issues (GitHub's adapter filters out PRs, which its REST API models as issues) |
| `ListOpenChangeRequests` | Open pull requests / merge requests |
| `CreateIssueComment` | Comment on an issue or change request |
| `AddLabels` / `RemoveLabel` | Label maintenance |
| `SetHold` | Apply or clear the `hold` merge gate, via the label |

What is deliberately **absent** matters as much as what is present:

- **Merge is optional and partial.** `Merger` is an optional extension interface
  kept off `Forge`, because merge semantics diverge across forges — strategy
  names, gate checks, MR-versus-PR. Only the GitLab adapter implements it so far
  (accept the MR with `merge`, `squash` or `rebase`, a head-SHA guard and branch
  removal). No production caller uses it yet; routing the merge gate by forge
  kind is [#14113](https://github.com/hivecommons/hive/issues/14113).
- **Creation is partial.** Gitea and Forgejo implement `CreateIssue` and
  `CreateChangeRequest` through optional seams (`forge.IssueSeam`,
  `forge.ChangeRequestCreator`), and on a forge-only hive the forge request
  relay uses them for agents' `hive-open-pr`, issue, comment and label requests
  ([#19670](https://github.com/hivecommons/hive/issues/19670)). GitLab has
  neither seam, and the `Forge` interface itself still has no create methods.

That second point is the ceiling on this path: the abstraction was scoped to the
read path plus light write operations, not to the create-and-merge lifecycle an
agent needs.

## The configuration surface that exists

These keys parse and validate today. They are documented here because they are
real and you will find them in the source — **not** because setting them makes a
non-GitHub hive work. They have two consumers: the dashboard, whose Platform card
and Projects cards read them to display the forge name and instance host, and
the governor's escalation and hold-guard writes, which build an adapter from them
(see the table above for why those callers are not reached on a non-GitHub hive
today).

```yaml
project:
  forge: gitlab        # "github" (default) | "gitlab" | "gitea" | "forgejo"

gitlab:
  gitlab_url: https://gitlab.example.com   # default https://gitlab.com
  token_env: GITLAB_TOKEN                  # env var NAME, never the token

gitea:
  gitea_url: https://gitea.example.com     # no default; required for Gitea
  token_env: GITEA_TOKEN                   # env var NAME, never the token

forgejo:
  forgejo_url: https://codeberg.org        # no default; required for Forgejo
  token_env: FORGEJO_TOKEN                 # env var NAME, never the token
```

Notes that will save you a wrong guess:

- `gitlab:`, `gitea:` and `forgejo:` are **top-level** blocks, siblings of
  `github:` — not nested inside it.
- `token_env` names the **environment variable to read the token from**, and
  `token_file` names a file under `/secrets` or `/data/secrets` that holds it.
  When both are set, the file wins, and a bad file is an error with no
  fallback to the env var. The token value is never written to config,
  matching Hive's no-secrets-in-config rule. Use `GITLAB_TOKEN` /
  `GITEA_TOKEN` / `FORGEJO_TOKEN` unless you have a reason not to.
- Omitting `project.forge` means GitHub, so every existing config is unaffected.
- GitLab defaults to `https://gitlab.com`. Gitea and Forgejo have **no** default
  — there is no single public host, so selecting either forge without a URL is a
  configuration error at client construction.
- The instance URL is the **bare instance root**. The adapters append `/api/v4`
  (GitLab) and `/api/v1` (Gitea and Forgejo) themselves; adding the suffix
  yourself produces a doubled path.
- A Forgejo instance configured as `gitea` is detected. The spoke reads
  `/api/v1/version` from each instance a `gitea` repo resolves to, at startup
  and every ten minutes for instances it has not read yet (a failed request,
  or one added by a reload). When the answer is a Forgejo version
  (`X.Y.Z+gitea-A.B.C` from Forgejo 7.0 on, `A.B.C+N` before), it logs one
  warning per instance suggesting `forge: forgejo`, the only kind with the
  Forgejo automerge and close-on-merge sweeps. The request carries the
  `gitea.token_env` token only where that token may go (the `forge_url` host
  rule below).
- A single repo can live on a different forge from the rest of the project with
  `project.repo_policies[].forge` and `forge_url` (bare instance root; inherits
  `gitlab_url` / `gitea_url` / `forgejo_url` when omitted). Unlike
  `project.forge`, these keys are validated at load: an unknown kind, a
  non-http(s) URL, or a Gitea or Forgejo repo with no URL anywhere is a config
  error. A `forge_url` must also be on the same host and port as its forge
  block's URL, because the block's token is only sent to that host. A mismatch
  is a config error. The GitLab block URL defaults to `https://gitlab.com`. A
  Gitea or Forgejo block with no URL of its own is not checked. The governor's
  escalation writes and its repo-scoped kicks follow the per-repo value; the
  token still comes from the forge-wide `token_env` or `token_file`.

```yaml
project:
  repo_policies:
    - repo: mirror
      forge: gitea
      forge_url: https://codeberg.org
```

### Forgejo git credentials: one short-lived token per tier

Git clone and push on a Forgejo repo send a token only to that repo's forge
host. By default the token is the `forgejo.token_env` access token, the one
every agent tier shares, and it never expires. The read-only tiers (advisor
and shared-uid-read) can then push with it, and hive logs a warning at boot
and shows it in the dashboard's forge settings while this is the case. Set
`forgejo.integration` to
have hive sign a one-hour JWT for each push and clone instead. Forgejo's
authorized integrations accept these tokens, and each tier gets its own
integration, the Forgejo analogue of a GitHub App installation token:

```yaml
forgejo:
  forgejo_url: https://forgejo.example.com
  integration:
    issuer: https://keys.example.com      # https origin serving the key list
    key_file: /etc/hive/forgejo-jwt.pem   # RSA key, mode 0600/0400, or 0440 group hive-launch
    audiences:                            # token tier -> integration audience
      contributor: "<audience>"
      advisor: "<audience>"
```

1. Generate the RSA key (2048 bits or more), and publish the two files that
   `hive forge key-list --issuer https://keys.example.com --key /etc/hive/forgejo-jwt.pem --out DIR`
   writes at the issuer, over https (see
   [Host the key list](#host-the-key-list)), or set `serve_key_list: true`
   to have the spoke serve them (see
   [Let the spoke serve the key list](#let-the-spoke-serve-the-key-list)).
   The key host must be live before step 2.
2. For each tier, run

   ```sh
   forgejo admin user create-authorized-integration --username <hive user> --name hive-<tier> \
     --issuer https://keys.example.com --scope <scope> \
     --repo <owner/repo1> --repo <owner/repo2> --claim-eq sub=hive:<tier>
   ```

   `--username` is the Forgejo user the integration acts as, the hive's
   user, and `--name` is a label for the integration. Forgejo requires both.
   Pass `--scope` and `--repo` explicitly, because both default to `all`.
   Give one `--repo` flag per repository: Forgejo does not split a value at
   commas, so `--repo owner/app,owner/lib` fails with `invalid repo name`.
   The `--repo` flags replace the default `all`, so the integration refuses
   any repository you did not list.

   Forgejo fetches `<issuer>/.well-known/openid-configuration` and the key
   list it names when you create the integration, before it saves anything.
   If the command fails with `invalid issuer`, fix the key host before you
   continue. This is the only end-to-end check of the key host before the
   first clone.
3. Copy each integration's audience under `audiences`, keyed by tier.

If the issuer host resolves to a private or loopback address, Forgejo refuses
it by default, and step 2 fails with `authorized_integration can only call
allowed HTTP servers (check your authorized_integration.ALLOWED_DOMAINS
setting)`. Set the issuer host in Forgejo's `app.ini`:

```ini
[authorized_integration]
ALLOWED_DOMAINS = keys.example.com
```

Setting `ALLOWED_DOMAINS` replaces the default, which allows every external
host and no private one, so Forgejo then fetches only from the hosts you
list. `ALLOW_LOCALNETWORKS = true` instead allows every private and loopback
address.

A tier with no audience gets the closest less-privileged tier that has one,
and never a more-privileged one. A tier with none cannot clone or push. The
`sub` claim is `hive:<tier>`, so a token minted for one tier is refused by
every other tier's integration.

The key file must be owner-only (0600 or 0400), or group-readable only by
`hive-launch` (GID 1002, the pod `fsGroup`), which is how a Kubernetes Secret
mounted at `/secrets` with `defaultMode: 0440` presents it. hive refuses a
group-readable key under any other group, because agent UIDs share group
`node` (GID 1000), and refuses any key that others can read. A key kept under
`/data/secrets` is sealed in place by the keystore at boot and still loads
afterwards. `hive validate` reports a `key_file` that does not exist or
cannot be read. It never contacts the issuer and does not compare the
published key list with `key_file`, so `config OK` does not mean a clone will
work: a key host that is down or serves the wrong key shows up only as
`Authentication failed` on the first clone or push.

By default `forgejo.integration` covers git clone and push only. Work
discovery, the issue and pull request relays, merges and both sweeps call the
Forgejo REST API with the access token from `forgejo.token_env` or
`forgejo.token_file`. The hive writes labels, comments and merges with it, so
it needs `write:repository` and `write:issue`; the live end-to-end run in
[#19909](https://github.com/hivecommons/hive/issues/19909) used
`write:repository,write:issue,read:user,read:organization`. An unset
`token_env` variable fails with an error that names it.

The close-on-merge sweep first identifies the token's own user
through `GET /user`, which needs the `read:user` scope. A token limited to
specific repositories cannot carry `read:user`; Forgejo then answers that
call with 403 and the sweep skips the repo ([#19994](https://github.com/hivecommons/hive/issues/19994)).
The automerge sweep does not look up the token's user, so it is not affected
by this. There are two ways out: either keep `read:user` on an all-repositories
token, or name the hive's own Forgejo username in `forgejo.login` so the
close-on-merge sweep skips the lookup and uses it directly. The login is not a
secret:

```yaml
forgejo:
  forgejo_url: https://forgejo.example.com
  login: hive-bot
```

To move the REST calls to a short-lived token as well, create one more
authorized integration for the hive's own identity and put its audience under
`rest_audience`:

```sh
forgejo admin user create-authorized-integration --username <hive user> --name hive-rest \
  --issuer https://keys.example.com \
  --scope write:repository,write:issue,read:user,read:organization \
  --repo <owner/repo1> --repo <owner/repo2> --claim-eq sub=hive:rest
```

```yaml
forgejo:
  integration:
    rest_audience: "<audience>"   # REST integration, sub=hive:rest
```

With `rest_audience` set, every REST client the hive builds sends a one-hour
JWT with `sub` `hive:rest` for that integration, minted again 10 minutes
before it expires, and REST no longer reads the access token. Without it,
REST keeps using the access token, so existing configs are unchanged
([#19920](https://github.com/hivecommons/hive/issues/19920)). A failed mint,
for example an unreadable key file, builds no REST client and names
`rest_audience` in the error; it does not fall back to the access token.

- The REST integration needs the same scopes as the access token above, and
  it must cover every repository the hive works on, as each tier's
  integration does.
- Forgejo acts on a JWT as the user that owns the integration. The
  close-on-merge sweep reads that user through `GET /user`, or takes it from
  `forgejo.login`, and trusts closing keywords from it, and the hive's
  issues, comments and merges are made as it. Pass the hive's own Forgejo
  user, the same user the access token belonged to, as `--username`, and
  keep `read:user` in the scope unless `forgejo.login` is set.
- `rest_audience` is set beside at least one tier audience, and it must
  differ from every tier audience; `hive validate` refuses a repeated one.
- The REST token is reused until shortly before it expires, but a change to
  the key file's size or modification time drops it, so the rotation below
  applies to REST as well.
- The Forgejo automerge sweep reads the collaborator permission endpoint,
  the sweeps read the issue timeline, and the close-on-merge sweep reads
  `GET /user` ([#19914](https://github.com/hivecommons/hive/issues/19914)).
  The nightly Forgejo JWT lane (`.github/workflows/forgejo-jwt.yml`) calls
  each of them on Forgejo 17 with a JWT for a `sub=hive:rest` integration
  created as above, logs the status, and fails on an answer the hive does
  not handle ([#19920](https://github.com/hivecommons/hive/issues/19920)):
  - The issue timeline must answer 200; the sweeps have no fallback for it.
  - A 403 from the collaborator permission endpoint makes the automerge
    sweep read the repo's assignee list instead, as it does for an access
    token with write but not admin access. The lane checks that list
    answers 200.
  - A 403 from `GET /user` makes the close-on-merge sweep use
    `forgejo.login`. Without `forgejo.login` the sweep skips the repo with
    an error that names `forgejo.login`. If Forgejo refuses `read:user` on
    an integration limited to repositories, set `forgejo.login`.

#### Pod-run agents clone through the hive

An agent that runs in the hive pod (`sandbox.runtime` off) gets no
credential for the Forgejo host: the access token is stripped from its pane,
and the agents' proxy never attaches a token to a forge host. A plain
`git clone` of a private repo fails with HTTP 401. The agent asks the hive
instead ([#19997](https://github.com/hivecommons/hive/issues/19997)):

```sh
hive-clone --repo <owner/repo> [--dir <path>]          # new checkout
hive-clone --repo <owner/repo> --fetch [--dir <path>]  # update a checkout
```

For an AGit pull request without a head branch, add `--pr <number>` to
either command. The relay fetches only that PR's `refs/pull/<number>/head`,
and the checkout receives it as `refs/hive/pull/<number>/head`. Inspect it
with `git diff origin/main...refs/hive/pull/<number>/head` (substitute the
PR's base branch for `main`), or check it out detached. It is a pull ref,
not a writable source branch. If the forge cannot fetch the requested ref,
the request fails rather than returning a checkout without the PR.

The hive's clone relay serves the request on a hive whose repos are all off
GitHub, as the push relay does:

- The request file must be owned by the agent's own UID, mapped to the agent
  it names. There is no shared-UID fallback, so a hive without per-agent UIDs
  refuses every clone request.
- The repo must be one of `project.repos`, not paused, and in the agent's
  repo scope. A clone is a read, so the agent's ACMM mode and whether it is
  paused do not gate it; an advisory agent clones at the advisor tier. The URL is built from the configured name and
  `forgejo.forgejo_url`, never from the request.
- `forgejo.integration` is required. The hive mints a one-hour JWT for the
  agent's own token tier and fetches with it. It never uses the long-lived
  `token_env` token for this. If the mint fails, nothing is fetched.
- The forge URL must be https; the relay refuses a plain-http
  `forgejo_url`. The fetch runs as the hive, in a fresh hive-owned git
  directory, with the push relay's git hardening: system and global config
  off, no hooks or credential helpers, TLS verification on, https only, and
  no redirects followed. The token is in the environment of the hive's own
  git process and its https helper only, as a header scoped to the repo URL,
  and it passes through the hive's egress proxy as any of the hive's own
  forge calls do. It is never in an argument, a file, a log line or the
  agent's environment, and any git output is scrubbed of it.
- The agent receives a git bundle of the repo's branches and tags, mode 0600
  and owned by its UID, in `/var/run/hive-metrics/clone-requests`.
  `hive-clone` checks that the result and the bundle are the hive's and its
  own, clones from or fetches the bundle as the agent, checks out the
  default branch, points `origin` at the plain https URL, and deletes the
  bundle. An agent has at most one bundle at a time, and the relay removes
  one left behind after 30 minutes. Each scan serves at most one request per
  agent, oldest first. `HIVE_FORGE_CLONE_MAX_BUNDLE_BYTES` caps a bundle (2 GiB
  by default).
- The hive needs no `CAP_CHOWN` for this. Where it can give the bundle away
  with `chown` it does; where it cannot (the stock pod, whose hive process
  keeps only an ambient `CAP_NET_ADMIN`), it writes the bundle as the agent's
  UID through the image's `su-exec` helper, so the result is the same 0600
  agent-owned file.

A later `git fetch origin` has no credential either; run `hive-clone --fetch`.
Publish work with `hive-push-branch`, which pushes through the push relay.

#### Rotate the signing key

The key file is read at every mint, so replacing it rotates the key. Forgejo
caches the key list for `[authorized_integration] CACHE_TTL` in its
`app.ini`, 10 minutes by default, and does not fetch it again when a token
names a key ID it does not know. A token signed with a key that is not yet in
Forgejo's cached copy is refused, and the clone or push fails with
`Authentication failed`. Rotate in this order:

1. Publish a key list with both the old and the new key:
   `hive forge key-list --issuer https://keys.example.com --key old.pem --key new.pem --out DIR`.
2. Wait longer than Forgejo's `CACHE_TTL` (more than 10 minutes by default),
   so Forgejo fetches the new list.
3. Point `key_file` at the new key, or replace the file in place.
4. One hour after step 3, when the last token signed with the old key has
   expired, publish the key list with the new key only.

When the spoke serves the key list itself (`serve_key_list: true`, see
[Let the spoke serve the key list](#let-the-spoke-serve-the-key-list)), the
same order becomes:

1. Write the new key to its own path and add that path to
   `previous_key_files`, leaving `key_file` on the old key, and apply the
   config. `previous_key_files` only publishes a key and never signs with
   it, so the served list now holds both keys and tokens are still signed
   with the old one.
2. Wait longer than Forgejo's `CACHE_TTL`, plus the one-minute
   `Cache-Control` max-age of the served list if a cache sits in front of the
   spoke.
3. Point `key_file` at the new key and `previous_key_files` at the old one.
   New tokens are signed with the new key, and both still verify.
4. One hour after step 3, remove the old key from `previous_key_files`.

The served list re-reads a key file when its size, mode or modification
time changes, so replacing a file in place is picked up without a restart.

#### The key host is a git dependency

Forgejo keeps no stale copy of the key list. If the host that serves it at
`issuer` is unreachable for longer than the cache window (`CACHE_TTL`, 10
minutes by default), Forgejo refuses every JWT, and every clone and push on a
Forgejo repo fails until the host is back. Run the key host with the same
availability you expect from git.

#### Host the key list

Any https web server can serve the two files `hive forge key-list` writes.
Forgejo is strict about how it fetches them. Apart from the issuer form,
which Hive checks itself, and the private-address rule, which has its own
`ALLOWED_DOMAINS` error, a broken rule shows up only as `invalid issuer` when
you create the integration, or as `Authentication failed` on a clone:

- Forgejo refuses any scheme but https, and the certificate must be one
  Forgejo trusts.
- Hive accepts only `https://host[:port]`, with no path, query or fragment,
  as the issuer, and refuses any other `issuer` at config load and in
  `hive validate`. So the files sit at the root of the host:
  `/.well-known/openid-configuration` and `/jwks.json`.
- Forgejo follows no redirects. Serve both paths directly, with no login
  page, no `http` to `https` redirect and no trailing-slash redirect in front
  of them.
- `jwks_uri` in the discovery document must be on the issuer's host.
  `hive forge key-list` writes it that way; do not move `jwks.json` to
  another host.
- Forgejo reads at most 16 KiB of each response. The files `hive forge
  key-list` writes for two keys are under 2 KiB.
- A host on a private address needs `ALLOWED_DOMAINS`, as described above.

For example, with nginx, and `DIR` the directory you passed to `--out`:

```nginx
server {
    listen 443 ssl;
    server_name keys.example.com;
    ssl_certificate     /etc/ssl/keys.example.com.crt;
    ssl_certificate_key /etc/ssl/keys.example.com.key;
    root DIR;
    location = /.well-known/openid-configuration { default_type application/json; }
    location = /jwks.json { default_type application/json; }
    location / { return 404; }
}
```

Check the host from a machine on Forgejo's network before you create the
integration. Both requests must answer 200 with JSON, and no redirect:

```sh
curl -sS -o /dev/null -w '%{http_code} %{redirect_url}\n' https://keys.example.com/.well-known/openid-configuration
curl -sS -o /dev/null -w '%{http_code} %{redirect_url}\n' https://keys.example.com/jwks.json
```

#### Let the spoke serve the key list

Instead of a separate key host, the spoke's dashboard can serve both
documents itself, so the key host is up whenever the hive that mints the
tokens is up ([#19996](https://github.com/hivecommons/hive/issues/19996)).
It is off by default. Turn it on only when `issuer` is the dashboard's own
https origin:

```yaml
forgejo:
  integration:
    issuer: https://hive.example.com          # the dashboard's own origin
    key_file: /etc/hive/forgejo-jwt.pem
    serve_key_list: true
    previous_key_files:                       # optional: published, never signs
      - /etc/hive/forgejo-jwt.old.pem
    audiences:
      contributor: "<audience>"
```

- The dashboard then answers `GET /.well-known/openid-configuration` and
  `GET /jwks.json` with no login. They are the only anonymous paths this
  adds, and only while `serve_key_list` is on; with it off both answer 404
  like any unknown path. A hosted spoke's ingress passes the two paths
  through to the spoke the same way.
- Both documents are rendered from `key_file` and `previous_key_files` and
  hold public keys only: the modulus and exponent of each key, its key ID,
  the issuer and the `jwks_uri`. No private key material and no file path is
  served.
- The routes serve GET only, with `Content-Type: application/json` and
  `Cache-Control: public, max-age=60`, and are rate limited per client
  address. Behind a reverse proxy every request shares the proxy's address,
  so the limit is then shared too; Forgejo fetches each document only once
  per `CACHE_TTL`.
- If `key_file` cannot be read, both paths answer 503 and the dashboard logs
  why, and Forgejo refuses new tokens once its cached copy expires. A
  `previous_key_files` entry that cannot be read is left out of the list and
  logged; `hive validate` reports it.
- `previous_key_files` holds key files in the same form and mode as
  `key_file`, at most four of them. They are published and never used to
  sign, and only their public half is served.
  It is refused without `serve_key_list`: with a separate key host, pass the
  old key to `hive forge key-list --key` instead.
- The issuer rules above still apply and are checked at config load: an
  https origin with no path. Your reverse proxy must pass both paths through
  with no authentication and no redirect, and a spoke Forgejo reaches on a
  private address still needs `ALLOWED_DOMAINS`.

Check the two paths with the `curl` commands above, pointed at the
dashboard's origin, before you create the integration.

### Do not confuse `project.forge` with `github.forge`

Two unrelated settings share the word "forge", and mixing them up is the most
likely way to break a working hive:

| Key | Meaning | Values |
| --- | --- | --- |
| `project.forge` | Which forge **family** a spoke executes against — selects the `pkg/forge` adapter (read by the dashboard and by the governor's escalation and hold-guard writes; see above) | `github`, `gitlab`, `gitea`, `forgejo` |
| `github.forge` | Which GitHub **instance** this hive's App and repos live on — drives App ID, app slug, and API URL | a GitHub host label |

`github.forge` is part of the GitHub App identity system and is fully live. Do
not set it to `gitlab`, `gitea` or `forgejo`; those are not GitHub hosts and it
is not that kind of setting.

## If you evaluate the adapters anyway

Should you exercise `pkg/forge` directly — from a test or your own program, which
is still the only way to reach most of it — the tokens it expects are ordinary
access tokens, not apps:

| Forge | Credential | Sent as |
| --- | --- | --- |
| GitLab | Personal, project, or group access token | `PRIVATE-TOKEN` header |
| Gitea / Forgejo | Personal access token | `Authorization: token <TOKEN>` |

Scope them to the repositories the hive would work on, with read access to
issues and merge/pull requests, plus write access to issue comments and labels if
you intend to exercise the comment/label/hold operations. A token may be omitted
entirely for unauthenticated reads of public projects; private projects and all
writes require one. Use a throwaway token — this is an evaluation path, not a
supported deployment.

One further caveat if you do: Hive's egress proxy enforces ACMM policy only on
hosts registered for mode enforcement, which today are GitHub hosts and the
Linear API. A GitLab or Gitea host is not among them, so its traffic is tunneled
without request-level ACMM enforcement. An advisory-tier hive would not have its
writes to such a host gated the way it would on GitHub — another reason this path
is not deployment-ready.

## What full support would require

Listed so the size of the gap is legible, not as a plan of record:

1. Forge-neutral enumeration for mixed hives. A forge-only hive now enumerates
   through the adapters ([#19669](https://github.com/hivecommons/hive/issues/19669)),
   with the label gates and issue filter applied above the forge boundary. A
   hive with any repository on GitHub still starts at
   `github.Client.EnumerateActionable` for every repository.
2. Create seams on GitLab, and the create relay for repositories on another
   forge inside a hive that also has GitHub repositories. Gitea and Forgejo
   have `CreateIssue` and `CreateChangeRequest` (`forge.IssueSeam`,
   `forge.ChangeRequestCreator`), served to agents on a forge-only hive.
3. A forge-neutral agent execution path, since agents reach their forge through
   the `gh` CLI wrapper rather than through `pkg/forge`.
4. Credential plumbing for token-based forges alongside the GitHub App minting
   machinery, including per-tier scoping.
5. A GitLab/Gitea dashboard login provider.
6. Egress-proxy mode enforcement registration for the non-GitHub hosts.

## See also

- [GitHub App setup](github-app-setup.md) — the supported path: app creation,
  permissions, Setup URL, and `/gh-setup`.
- [Getting started](getting-started.md) — first-session setup, including Step 0.
- [Troubleshooting](troubleshooting.md) — Forge App and credential symptoms.
- [Operator reference](operator-reference.md) — the full configuration surface.
