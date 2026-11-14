# Dashboard REST API reference — Sessions

Part of the [Dashboard REST API reference](../api-reference.md); see that page for how auth levels and `pkg/<file>:<line>` source citations are read and checked. Edit this file for routes in this area and run `src/scripts/check-api-reference-citations.sh --fix` to resync citations.

Sessions are interactive, named CLI-agent terminals. Each runs as its own unprivileged uid and is private to its creator and the hive owners unless `shared` is set (see [security model](../security-model.md)). The feature is off unless `dashboard.sessions.enabled` (or `HIVE_SESSIONS_ENABLED`) is true; while off, `GET /api/sessions` answers `{"enabled":false,"sessions":[],"vendors":[],"limits":{}}` and every other route answers 404. Errors are `{"ok":false,"error":"..."}`.

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/sessions` | Dashboard auth/session | Sessions, vendors and limits | `pkg/dashboard/api_sessions.go:120` |
| `POST` | `/api/sessions` | Owner or read-write | Create a session `{name, group, vendor, shared?}` (201) | `pkg/dashboard/api_sessions.go:121` |
| `PATCH` | `/api/sessions/{id}` | Session creator or owner | Rename, regroup or (un)share `{name?, group?, shared?}` | `pkg/dashboard/api_sessions.go:122` |
| `DELETE` | `/api/sessions/{id}` | Session creator or owner | Close a running session (metadata kept, marked `ended`); on an ended session, remove its metadata and delete its sandbox files | `pkg/dashboard/api_sessions.go:123` |
| `GET` | `/api/sessions/{id}/log` | Dashboard auth/session | Redacted scrollback `?lines=N` (capped at 5000) | `pkg/dashboard/api_sessions.go:124` |
| `POST` | `/api/sessions/{id}/input` | Owner or read-write | Type `{text, submit}` into the pane (size-capped, audited without the text) | `pkg/dashboard/api_sessions.go:125` |
| `POST` | `/api/sessions/{id}/upload` | Owner or read-write | Multipart `file`; returns the absolute path inside the session directory | `pkg/dashboard/api_sessions.go:126` |
| `POST` | `/api/sessions/{id}/resume` | Owner or read-write | Reopen an ended session's last conversation where the vendor supports it (409 otherwise) | `pkg/dashboard/api_sessions.go:127` |

## Session object

`{id, name, group, vendor, creator, created_at, last_activity, status, attached, tmux_name, resumable, shared}` where `status` is `running` or `ended`, `attached` is the number of attached tmux clients and `tmux_name` is `hive-sess-<id>`. `resumable` and `shared` are additive. `GET /api/sessions` lists only sessions the caller created, owners (all) and `shared` sessions; the log, input, upload, resume and terminal routes answer 404 for a session the caller cannot see and 403 when it is visible but not usable.

## Attaching a terminal

`POST /api/terminal/handoff` accepts an optional body `{"session":"<id>"}`. The response is unchanged (`{code, expires_in}`); the UI then opens `/terminal/?arg=hive-sess-<id>&code=<code>`. The code is bound to that session id and caller, opens only `?arg=hive-sess-<id>`, and is refused (503) when the hive has no ttyd attach key. The terminal itself stays owner/read-write only, and only for the creator, owners or a `shared` session. Keystrokes typed in the terminal are not attributable to a user; only `/input` and `/upload` are audited (`session_input`, `session_upload`, with user, session id and byte count, never the text).

## Limits

`limits` in `GET /api/sessions` reports `max_sessions`, `max_per_user`, `idle_ttl_hours`, `max_input_bytes`, `max_upload_bytes` and `max_uploads_per_session`. Defaults are 12, 4, 72 hours, 64 KiB, 10 MiB and 25; each is overridable (`HIVE_SESSIONS_MAX`, `HIVE_SESSIONS_MAX_PER_USER`, `HIVE_SESSIONS_IDLE_TTL_HOURS`, `HIVE_SESSIONS_MAX_INPUT_BYTES`, `HIVE_SESSIONS_MAX_UPLOAD_BYTES`, `HIVE_SESSIONS_MAX_UPLOADS`).
