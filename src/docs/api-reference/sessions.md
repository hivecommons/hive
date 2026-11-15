# Dashboard REST API reference — Sessions

Part of the [Dashboard REST API reference](../api-reference.md); see that page for how auth levels and `pkg/<file>:<line>` source citations are read and checked. Edit this file for routes in this area and run `src/scripts/check-api-reference-citations.sh --fix` to resync citations.

Sessions are interactive, named CLI-agent terminals. Each runs as its own unprivileged uid and is private to its creator and the hive owners unless `shared` is set (see [security model](../security-model.md)). The feature is off unless `dashboard.sessions.enabled` (or `HIVE_SESSIONS_ENABLED`) is true; while off, `GET /api/sessions` answers `{"enabled":false,"sessions":[],"vendors":[],"limits":{}}` and every other route answers 404. Errors are `{"ok":false,"error":"..."}`.

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/sessions` | Dashboard auth/session | Sessions, vendors and limits | `pkg/dashboard/api_sessions.go:139` |
| `POST` | `/api/sessions` | Owner or read-write | Create a session `{name, group, vendor, shared?}`; answers 202 at once with the session `preparing`, the start runs in the background | `pkg/dashboard/api_sessions.go:140` |
| `PATCH` | `/api/sessions/{id}` | Session creator or owner | Rename, regroup or (un)share `{name?, group?, shared?}` | `pkg/dashboard/api_sessions.go:141` |
| `DELETE` | `/api/sessions/{id}` | Session creator or owner | Close a running session (metadata kept, marked `ended`); on an ended session, remove its metadata and delete its sandbox files | `pkg/dashboard/api_sessions.go:142` |
| `GET` | `/api/sessions/{id}/log` | Dashboard auth/session | Redacted scrollback `?lines=N` (capped at 5000) | `pkg/dashboard/api_sessions.go:143` |
| `POST` | `/api/sessions/{id}/input` | Owner or read-write | Type `{text, submit}` into the pane (size-capped, audited without the text) | `pkg/dashboard/api_sessions.go:144` |
| `POST` | `/api/sessions/{id}/upload` | Owner or read-write | Multipart `file`; returns the absolute path inside the session directory | `pkg/dashboard/api_sessions.go:145` |
| `POST` | `/api/sessions/{id}/resume` | Owner or read-write | Reopen an ended session's last conversation where the vendor supports it (409 otherwise); answers 202 with the session `preparing` | `pkg/dashboard/api_sessions.go:146` |

## Session object

`{id, name, group, vendor, creator, created_at, last_activity, status, attached, tmux_name, resumable, shared, workspace_repo, workspace, message, error}` where `status` is `preparing`, `running`, `failed` or `ended`, `attached` is the number of attached tmux clients and `tmux_name` is `hive-sess-<id>`. `resumable`, `shared`, `workspace_repo`, `workspace`, `message` and `error` are additive. `GET /api/sessions` lists only sessions the caller created, owners (all) and `shared` sessions; the log, input, upload, resume and terminal routes answer 404 for a session the caller cannot see and 403 when it is visible but not usable.

## Lifecycle

A create (or resume) never blocks the request on the start. The record is saved and answered as `preparing` with a human `message` (`preparing workspace: cloning org/repo (first session on this hive)` when the workspace does not exist yet, otherwise the vendor being started), and the start runs in the background: the one-time clone of the workspace, its hand-over to the sessions group, `spektacular init` and the tmux server. Poll `GET /api/sessions` until the session is `running` (the terminal can then be attached) or `failed`, in which case `error` carries the reason (tokens redacted) and `DELETE` removes the record; a resume whose start fails goes back to `ended` with `error` set and stays resumable. Preparing sessions count against the limits, answer 409 to input, upload, terminal handoff and `DELETE`, and a second create that needs the same workspace waits for the clone in flight instead of starting another. A `preparing` record found after a hive restart, with no start behind it, is marked `failed` on the next list. The create is audited as `session_created` and its outcome as `session_started` (`outcome=running` or `outcome=failed` with the error).

## Workspace

Every session's CLI starts inside the hive's **session workspace**: one persistent clone per repository, `<HIVE_SESSIONS_WORKSPACE_DIR>/<org>/<repo>` (default `/data/session-sandbox/workspace/...`), shared by every session on the hive the way one checkout is shared by several people at one machine (two sessions editing the same file see each other's edits; `git fetch` is never run for you). The repository is `dashboard.sessions.workspace_repo` (`HIVE_SESSIONS_WORKSPACE_REPO`), defaulting to the hive's own `project.org/primary_repo`; with neither set, a session opens in its own empty directory. The hive clones once, with a short-lived App token in a one-shot credential helper that is deleted afterwards, so the clone's config and `origin` carry no credential: pushing from a session uses the person's own login. On every start the hive runs `spektacular init <vendor>` in the workspace once per vendor spek supports (`claude`, `bob`, `codex`, `copilot`, `omp`) when `/usr/local/bin/spektacular` (`runs.spektacular.binary`) is installed; a missing binary or a failed init is logged and the session still starts. `GET /api/sessions` reports the configured workspace as `workspace: {repo, path}`; each session carries the `workspace_repo` and `workspace` it was created in and a resume reopens that one. Uploads stay under the session's own directory (`/upload` returns their path).

## Attaching a terminal

`POST /api/terminal/handoff` accepts an optional body `{"session":"<id>"}`. The response is unchanged (`{code, expires_in}`); the UI then opens `/terminal/?arg=hive-sess-<id>&code=<code>`. The code is bound to that session id and caller, opens only `?arg=hive-sess-<id>`, and is refused (503) when the hive has no ttyd attach key. The terminal itself stays owner/read-write only, and only for the creator, owners or a `shared` session. Keystrokes typed in the terminal are not attributable to a user; only `/input` and `/upload` are audited (`session_input`, `session_upload`, with user, session id and byte count, never the text).

## Limits

`limits` in `GET /api/sessions` reports `max_sessions`, `max_per_user`, `idle_ttl_hours`, `max_input_bytes`, `max_upload_bytes` and `max_uploads_per_session`. Defaults are 12, 4, 72 hours, 64 KiB, 10 MiB and 25; each is overridable (`HIVE_SESSIONS_MAX`, `HIVE_SESSIONS_MAX_PER_USER`, `HIVE_SESSIONS_IDLE_TTL_HOURS`, `HIVE_SESSIONS_MAX_INPUT_BYTES`, `HIVE_SESSIONS_MAX_UPLOAD_BYTES`, `HIVE_SESSIONS_MAX_UPLOADS`).
