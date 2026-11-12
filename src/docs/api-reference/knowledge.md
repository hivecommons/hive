# Dashboard REST API reference — Knowledge

Part of the [Dashboard REST API reference](../api-reference.md); see that page for how auth levels and `pkg/<file>:<line>` source citations are read and checked. Edit this file for routes in this area and run `src/scripts/check-api-reference-citations.sh --fix` to resync citations.


| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/knowledge` | Dashboard auth/session | Paginated knowledge facts: `type` filter, `limit` (default 50, max 200; invalid/non-positive values use the default), `offset` (default 0). Returns `enabled`, `facts`, `count` (this page), `total`, `limit`, `offset`, `has_more`; ordered by layer then slug. Offsets are not snapshot cursors: concurrent edits may shift pages. | `pkg/dashboard/api.go:403` |
| `GET` | `/api/knowledge/export` | Dashboard auth/session | Knowledge Export | `pkg/dashboard/api.go:404` |
| `POST` | `/mcp/knowledge` | Public (read-only) when the dashboard setting or `HIVE_PUBLIC_KNOWLEDGE` is on; otherwise 404 to a signed-in caller and 401 to one who is not | Public Knowledge MCP endpoint — `initialize`, `tools/list`, `tools/call` for `knowledge_search`/`knowledge_toc`/`knowledge_get`/`knowledge_export` ([docs](../public-knowledge-mcp.md)) | `pkg/dashboard/api.go:413` |
| `GET` | `/api/knowledge/search` | Dashboard auth/session | Knowledge Search | `pkg/dashboard/api.go:405` |
| `GET` | `/api/knowledge/toc` | Dashboard auth/session | Scoped knowledge table of contents: capped, body-free entries (`id`, `title`, `type`, `layer`, `repo`, `tags`, `status`, `confidence`, `updated`, `size_bytes`, `source`) plus `total`/`returned`/`truncated`. Query: `layers`, `repos`, `types`, `tags` (comma-separated), `limit` (default 50, max 200), `include_states` (operator opt-in for `draft`/`deprecated`/`superseded`/`all`), `format=prompt` with `max_chars` for a bounded markdown rendering, `agent` to apply that agent's `knowledge.agent_scopes` entry (the other parameters can only narrow it). Approved knowledge only by default ([docs](../knowledge-toc.md)) | `pkg/dashboard/api.go:406` |
| `GET` | `/api/knowledge/effective` | Dashboard auth/session | Effective knowledge for one agent (`agent` required): the agent's `knowledge.agent_scopes` entry (unrestricted when it has none) intersected with the request scope (approved only unless `include_states` is set). Returns `scope` (resolved scope applied, with `configured`), `included` TOC entries, `excluded` entries each with a `reason` (`lifecycle`, `layer`, `repo`, `type` or `tag` — first failing check, in that order), `included_total`/`excluded_total`/`included_returned`/`excluded_returned` and `truncated`; `limit` caps each list (default 50, max 200) ([docs](../knowledge-toc.md#effective-knowledge)) | `pkg/dashboard/api.go:407` |
| `GET` | `/api/knowledge/entry/{id}` | Dashboard auth/session | Full read of one knowledge entry: `markdown` (front-matter plus body) and the fact. Takes the same scope, `include_states` and `agent` query as the table of contents and returns 404 for an entry outside it ([docs](../knowledge-toc.md)) | `pkg/dashboard/api.go:408` |
| `POST` | `/api/knowledge/cli-memory/drain-legacy` | Owner only | One-shot drain of the shared home's legacy Claude memory (`/data/home/.claude/projects/*/memory`) into `state: draft` entries in `bead-synth-wiki`, tagged `cli-memory` and `agent:shared`, deduplicated by content hash. Body `{"truncate": bool}` (default false) empties only files whose every fact is now stored. Returns `store`, `agent`, `sources`, `drained`, `dupes`, `skipped`, `truncate`, `truncated`, `kept`, `errors`; 409 when the vault is not connected. Audited as `knowledge_drain_legacy_memory` ([docs](../knowledge-lifecycle.md#auto-memory-drafts)) | `pkg/dashboard/api.go:410` |
| `GET` | `/api/knowledge/public` | Dashboard auth/session | Public Knowledge MCP sharing status (`enabled`, `tags`, `source`, endpoint URL) | `pkg/dashboard/api.go:414` |
| `PUT` | `/api/knowledge/public` | Owner only | Persist public Knowledge MCP sharing setting; overrides env vars until changed | `pkg/dashboard/api.go:415` |
| `GET` | `/api/knowledge/health` | Dashboard auth/session | Knowledge Health | `pkg/dashboard/api.go:416` |
| `GET` | `/api/knowledge/stats` | Dashboard auth/session | Knowledge Stats | `pkg/dashboard/api.go:417` |
| `GET` | `/api/knowledge/graph` | Dashboard auth/session | Knowledge Graph | `pkg/dashboard/api.go:418` |
| `GET` | `/api/knowledge/fact-history` | Dashboard auth/session | Fact History | `pkg/dashboard/api.go:419` |
| `POST` | `/api/knowledge/create` | Dashboard auth/session | Create one fact (`title`, `body`, `type`, `tags`, `layer`, `confidence`, `state`). `state` is `approved` (default) or `draft`; a draft is written with `state: draft` into the local channel named by `layer` (400 when there is none, 409 when its title already names an entry). A request tagged `cli-memory` must be a draft carrying only `cli-memory`, `agent:<name>`, `cli:<claude\|codex\|gemini>`, `memhash:<hex>` and `repo:<org>/<repo>` tags (400 otherwise) | `pkg/dashboard/api.go:420` |
| `POST` | `/api/knowledge/import` | Dashboard auth/session | Knowledge Import | `pkg/dashboard/api.go:421` |
| `POST` | `/api/knowledge/promote` | Dashboard auth/session | Knowledge Promote | `pkg/dashboard/api.go:427` |
| `GET` | `/api/knowledge/subscriptions` | Dashboard auth/session | Knowledge Subs List | `pkg/dashboard/api.go:428` |
| `POST` | `/api/knowledge/subscriptions` | Dashboard auth/session | Knowledge Subs Add | `pkg/dashboard/api.go:429` |
| `DELETE` | `/api/knowledge/subscriptions` | Dashboard auth/session | Knowledge Subs Remove | `pkg/dashboard/api.go:430` |
| `PUT` | `/api/knowledge/{layer}/{slug}` | Dashboard auth/session | Knowledge Update | `pkg/dashboard/api.go:431` |
| `DELETE` | `/api/knowledge/{layer}/{slug}` | Dashboard auth/session | Knowledge Delete | `pkg/dashboard/api.go:432` |
| `GET` | `/api/knowledge/{layer}` | Dashboard auth/session | Knowledge Layer | `pkg/dashboard/api.go:433` |
| `GET` | `/api/knowledge/{layer}/{slug}` | Dashboard auth/session | Knowledge Fact | `pkg/dashboard/api.go:434` |
| `PUT` | `/api/knowledge/enabled` | Owner only | Knowledge Toggle | `pkg/dashboard/api.go:435` |
| `GET` | `/api/knowledge/bead-synthesizer` | Dashboard auth/session | Bead Synth Status | `pkg/dashboard/api.go:436` |
| `PUT` | `/api/knowledge/bead-synthesizer/enabled` | Owner only | Bead Synth Toggle | `pkg/dashboard/api.go:437` |
| `GET` | `/api/knowledge/vaults` | Dashboard auth/session | Vaults List | `pkg/dashboard/api.go:438` |
| `POST` | `/api/knowledge/vaults` | Owner only | Vaults Connect | `pkg/dashboard/api.go:439` |
| `DELETE` | `/api/knowledge/vaults` | Dashboard auth/session | Vaults Disconnect | `pkg/dashboard/api.go:440` |
| `POST` | `/api/knowledge/vaults/reindex` | Dashboard auth/session | Vaults Reindex | `pkg/dashboard/api.go:441` |
| `GET` | `/api/knowledge/vaults/{name}/facts` | Dashboard auth/session | Vault Facts | `pkg/dashboard/api.go:442` |
| `GET` | `/api/config/knowledge/connectors` | Dashboard auth/session | Knowledge connectors config (name, type, layer, scope, env/file auth reference) and registered types | `pkg/dashboard/api.go` |
| `PUT` | `/api/config/knowledge/connectors` | Owner only | Replace `knowledge.connectors` after validation; inline secrets rejected | `pkg/dashboard/api.go` |
| `GET` | `/api/config/knowledge/connectors/status` | Dashboard auth/session | Per-connector status pill, last sync, pages/facts, last error, plus the `publish` mirror status | `pkg/dashboard/api.go` |
| `POST` | `/api/config/knowledge/connectors/validate` | Owner only | Dry-run validate one connector entry (no network) | `pkg/dashboard/api.go` |
| `POST` | `/api/config/knowledge/connectors/{name}/sync` | Owner only | Start an immediate background sync of one connector | `pkg/dashboard/api.go` |
| `GET` | `/api/config/knowledge/agent-scopes` | Dashboard auth/session | Per-agent knowledge scopes (`knowledge.agent_scopes`: layers, repos, types, tags, include_states) ([docs](../knowledge-toc.md#per-agent-scopes)) | `pkg/dashboard/api.go` |
| `PUT` | `/api/config/knowledge/agent-scopes` | Owner only | Replace `knowledge.agent_scopes` after validating layer and state names; persisted and audited as `config_knowledge_agent_scopes` | `pkg/dashboard/api.go` |
| `GET` | `/api/config/knowledge/pipeline` | Owner only | Scalar bead synthesizer, curator, code map and primer settings, plus the read-only engine, vault paths and primer merge strategy ([docs](../settings.md#knowledge)) | `pkg/dashboard/api.go` |
| `PUT` | `/api/config/knowledge/pipeline` | Owner only | Update those settings after `config.ValidateKnowledgePipeline`; 0 or empty restores the default; the bead synthesizer switch applies at once, the rest after a restart; audited as `config_knowledge_pipeline` | `pkg/dashboard/api.go` |
| `GET` | `/api/knowledge/git-sources` | Dashboard auth/session | Git Sources List | `pkg/dashboard/api.go:452` |
| `POST` | `/api/knowledge/git-sources` | Owner only | Git Sources Connect | `pkg/dashboard/api.go:453` |
| `DELETE` | `/api/knowledge/git-sources` | Owner only | Git Sources Disconnect | `pkg/dashboard/api.go:454` |
| `PUT` | `/api/knowledge/git-sources` | Owner only | Git Sources Update | `pkg/dashboard/api.go:455` |
| `POST` | `/api/knowledge/obsidian/sync` | Dashboard auth/session | Obsidian Sync | `pkg/dashboard/api.go:456` |
| `GET` | `/api/knowledge/documents` | Dashboard auth/session | Documents List | `pkg/dashboard/api.go:457` |
| `POST` | `/api/knowledge/documents` | Dashboard auth/session | Documents Import | `pkg/dashboard/api.go:458` |
| `GET` | `/api/knowledge/documents/{slug}` | Dashboard auth/session | Document Get | `pkg/dashboard/api.go:459` |
| `DELETE` | `/api/knowledge/documents/{slug}` | Dashboard auth/session | Document Delete | `pkg/dashboard/api.go:460` |
| `POST` | `/api/knowledge/documents/{slug}/reimport` | Dashboard auth/session | Document Reimport | `pkg/dashboard/api.go:461` |
| `GET` | `/api/knowledge/context7/search` | Dashboard auth/session | Context7 Search | `pkg/dashboard/api.go:462` |
| `POST` | `/api/knowledge/cleanup-orphans` | Dashboard auth/session | Cleanup Orphans | `pkg/dashboard/api.go:463` |
| `GET` | `/api/knowledge/channels` | Dashboard auth/session | Knowledge Channels List | `pkg/dashboard/api.go:425` |
| `POST` | `/api/knowledge/channels` | Dashboard auth/session | Knowledge Channel Create | `pkg/dashboard/api.go:426` |
| `GET` | `/api/knowledge/connectors` | Dashboard auth/session | Knowledge connector statuses and publish mirror status (`{"connectors": [...], "publish": {...}\|null}`, see [knowledge-connectors.md](../knowledge-connectors.md)) | `pkg/dashboard/api_knowledge_connectors.go:23` |
| `POST` | `/api/knowledge/connectors/{name}/sync` | Owner only | Start one connector sync now (202; 404 unknown, 409 running) | `pkg/dashboard/api_knowledge_connectors.go:24` |
| `POST` | `/api/knowledge/publish/sync` | Owner only | Run a publish mirror batch now (202 with publish status; 503 mirror not running, 409 batch in progress) | `pkg/dashboard/api_knowledge_connectors.go:25` |
| `GET` | `/api/knowledge/connectors/{name}/auth` | Dashboard auth/session | Interactive sign-in state for a connector (Plaud OAuth): connected, account, pending, auth_url; `?refresh=1` re-checks upstream (400 for connector types without interactive sign-in) | `pkg/dashboard/api_knowledge_connector_auth.go:25` |
| `POST` | `/api/knowledge/connectors/{name}/auth/connect` | Owner only | Start the connector's sign-in flow (202 with state incl. `auth_url`) | `pkg/dashboard/api_knowledge_connector_auth.go:26` |
| `POST` | `/api/knowledge/connectors/{name}/auth/disconnect` | Owner only | Sign out and revoke the connector's credential upstream | `pkg/dashboard/api_knowledge_connector_auth.go:27` |
| `POST` | `/api/knowledge/connectors/{name}/auth/complete` | Owner only | Finish a sign-in from pasted material on a hosted hive: `{redirect_url}` (the `localhost:8199/auth/callback?…` URL the browser landed on, forwarded in-pod while the login is pending) or `{tokens}` (contents of `~/.plaud/tokens-mcp.json`); see [inbox-voice-sources.md](../inbox-voice-sources.md) | `pkg/dashboard/api_inbox.go:29` |
| `GET` | `/api/inbox` | Dashboard auth/session | Inbox items (voice recordings/dictations keyed by `(source, id)`) with per-destination route state, source connection status, candidate repos and auto-routing settings; see [inbox-voice-sources.md](../inbox-voice-sources.md) | `pkg/dashboard/api_inbox.go:24` |
| `POST` | `/api/inbox/route` | Owner only | Route one item: `{source, item_id, destination: spek_rfc\|dibs\|dismiss, repo?}` (202 queued; 400 unconfigured destination, 404 unknown item, 409 already queued/running) | `pkg/dashboard/api_inbox.go:25` |
| `POST` | `/api/inbox/dismiss` | Owner only | Bulk "Knowledge only": `{items: [{source, item_id}], undo?}` | `pkg/dashboard/api_inbox.go:26` |
| `GET` | `/api/inbox/settings` | Dashboard auth/session | Auto-routing rules, default RFC repo, dibs URL, destination readiness | `pkg/dashboard/api_inbox.go:27` |
| `POST` | `/api/inbox/settings` | Owner only | Update `inbox.rules` / `inbox.spek.repo` / `inbox.dibs.url` in the runtime config (omitted fields unchanged) | `pkg/dashboard/api_inbox.go:28` |
