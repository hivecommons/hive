# Dashboard REST API reference — Knowledge

Part of the [Dashboard REST API reference](../api-reference.md); see that page for how auth levels and `pkg/<file>:<line>` source citations are read and checked. Edit this file for routes in this area and run `src/scripts/check-api-reference-citations.sh --fix` to resync citations.


| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/knowledge` | Dashboard auth/session | Paginated knowledge facts: `type` filter, `limit` (default 50, max 200; invalid/non-positive values use the default), `offset` (default 0). Returns `enabled`, `facts`, `count` (this page), `total`, `limit`, `offset`, `has_more`; ordered by layer then slug. Offsets are not snapshot cursors: concurrent edits may shift pages. | `pkg/dashboard/api.go:371` |
| `GET` | `/api/knowledge/export` | Dashboard auth/session | Knowledge Export | `pkg/dashboard/api.go:372` |
| `POST` | `/mcp/knowledge` | Public (read-only) when the dashboard setting or `HIVE_PUBLIC_KNOWLEDGE` is on; 404 otherwise | Public Knowledge MCP endpoint — `initialize`, `tools/list`, `tools/call` for `knowledge_search`/`knowledge_toc`/`knowledge_get`/`knowledge_export` ([docs](../public-knowledge-mcp.md)) | `pkg/dashboard/api.go:380` |
| `GET` | `/api/knowledge/search` | Dashboard auth/session | Knowledge Search | `pkg/dashboard/api.go:373` |
| `GET` | `/api/knowledge/toc` | Dashboard auth/session | Scoped knowledge table of contents: capped, body-free entries (`id`, `title`, `type`, `layer`, `repo`, `tags`, `status`, `confidence`, `updated`, `size_bytes`, `source`) plus `total`/`returned`/`truncated`. Query: `layers`, `repos`, `types`, `tags` (comma-separated), `limit` (default 50, max 200), `include_states` (operator opt-in for `draft`/`deprecated`/`superseded`/`all`), `format=prompt` with `max_chars` for a bounded markdown rendering, `agent` to apply that agent's `knowledge.agent_scopes` entry (the other parameters can only narrow it). Approved knowledge only by default ([docs](../knowledge-toc.md)) | `pkg/dashboard/api.go:374` |
| `GET` | `/api/knowledge/effective` | Dashboard auth/session | Effective knowledge for one agent (`agent` required): the agent's `knowledge.agent_scopes` entry (unrestricted when it has none) intersected with the request scope (approved only unless `include_states` is set). Returns `scope` (resolved scope applied, with `configured`), `included` TOC entries, `excluded` entries each with a `reason` (`lifecycle`, `layer`, `repo`, `type` or `tag` — first failing check, in that order), `included_total`/`excluded_total`/`included_returned`/`excluded_returned` and `truncated`; `limit` caps each list (default 50, max 200) ([docs](../knowledge-toc.md#effective-knowledge)) | `pkg/dashboard/api.go:375` |
| `GET` | `/api/knowledge/entry/{id}` | Dashboard auth/session | Full read of one knowledge entry: `markdown` (front-matter plus body) and the fact. Takes the same scope, `include_states` and `agent` query as the table of contents and returns 404 for an entry outside it ([docs](../knowledge-toc.md)) | `pkg/dashboard/api.go:376` |
| `GET` | `/api/knowledge/public` | Dashboard auth/session | Public Knowledge MCP sharing status (`enabled`, `tags`, `source`, endpoint URL) | `pkg/dashboard/api.go:381` |
| `PUT` | `/api/knowledge/public` | Owner only | Persist public Knowledge MCP sharing setting; overrides env vars until changed | `pkg/dashboard/api.go:382` |
| `GET` | `/api/knowledge/health` | Dashboard auth/session | Knowledge Health | `pkg/dashboard/api.go:383` |
| `GET` | `/api/knowledge/stats` | Dashboard auth/session | Knowledge Stats | `pkg/dashboard/api.go:384` |
| `GET` | `/api/knowledge/graph` | Dashboard auth/session | Knowledge Graph | `pkg/dashboard/api.go:385` |
| `GET` | `/api/knowledge/fact-history` | Dashboard auth/session | Fact History | `pkg/dashboard/api.go:386` |
| `POST` | `/api/knowledge/create` | Dashboard auth/session | Knowledge Create | `pkg/dashboard/api.go:387` |
| `POST` | `/api/knowledge/import` | Dashboard auth/session | Knowledge Import | `pkg/dashboard/api.go:388` |
| `POST` | `/api/knowledge/promote` | Dashboard auth/session | Knowledge Promote | `pkg/dashboard/api.go:394` |
| `GET` | `/api/knowledge/subscriptions` | Dashboard auth/session | Knowledge Subs List | `pkg/dashboard/api.go:395` |
| `POST` | `/api/knowledge/subscriptions` | Dashboard auth/session | Knowledge Subs Add | `pkg/dashboard/api.go:396` |
| `DELETE` | `/api/knowledge/subscriptions` | Dashboard auth/session | Knowledge Subs Remove | `pkg/dashboard/api.go:397` |
| `PUT` | `/api/knowledge/{layer}/{slug}` | Dashboard auth/session | Knowledge Update | `pkg/dashboard/api.go:398` |
| `DELETE` | `/api/knowledge/{layer}/{slug}` | Dashboard auth/session | Knowledge Delete | `pkg/dashboard/api.go:399` |
| `GET` | `/api/knowledge/{layer}` | Dashboard auth/session | Knowledge Layer | `pkg/dashboard/api.go:400` |
| `GET` | `/api/knowledge/{layer}/{slug}` | Dashboard auth/session | Knowledge Fact | `pkg/dashboard/api.go:401` |
| `PUT` | `/api/knowledge/enabled` | Owner only | Knowledge Toggle | `pkg/dashboard/api.go:402` |
| `GET` | `/api/knowledge/bead-synthesizer` | Dashboard auth/session | Bead Synth Status | `pkg/dashboard/api.go:403` |
| `PUT` | `/api/knowledge/bead-synthesizer/enabled` | Owner only | Bead Synth Toggle | `pkg/dashboard/api.go:404` |
| `GET` | `/api/knowledge/vaults` | Dashboard auth/session | Vaults List | `pkg/dashboard/api.go:405` |
| `POST` | `/api/knowledge/vaults` | Owner only | Vaults Connect | `pkg/dashboard/api.go:406` |
| `DELETE` | `/api/knowledge/vaults` | Dashboard auth/session | Vaults Disconnect | `pkg/dashboard/api.go:407` |
| `POST` | `/api/knowledge/vaults/reindex` | Dashboard auth/session | Vaults Reindex | `pkg/dashboard/api.go:408` |
| `GET` | `/api/knowledge/vaults/{name}/facts` | Dashboard auth/session | Vault Facts | `pkg/dashboard/api.go:409` |
| `GET` | `/api/config/knowledge/connectors` | Dashboard auth/session | Knowledge connectors config (name, type, layer, scope, env/file auth reference) and registered types | `pkg/dashboard/api.go` |
| `PUT` | `/api/config/knowledge/connectors` | Owner only | Replace `knowledge.connectors` after validation; inline secrets rejected | `pkg/dashboard/api.go` |
| `GET` | `/api/config/knowledge/connectors/status` | Dashboard auth/session | Per-connector status pill, last sync, pages/facts, last error, plus the `publish` mirror status | `pkg/dashboard/api.go` |
| `POST` | `/api/config/knowledge/connectors/validate` | Owner only | Dry-run validate one connector entry (no network) | `pkg/dashboard/api.go` |
| `POST` | `/api/config/knowledge/connectors/{name}/sync` | Owner only | Start an immediate background sync of one connector | `pkg/dashboard/api.go` |
| `GET` | `/api/config/knowledge/agent-scopes` | Dashboard auth/session | Per-agent knowledge scopes (`knowledge.agent_scopes`: layers, repos, types, tags, include_states) ([docs](../knowledge-toc.md#per-agent-scopes)) | `pkg/dashboard/api.go` |
| `PUT` | `/api/config/knowledge/agent-scopes` | Owner only | Replace `knowledge.agent_scopes` after validating layer and state names; persisted and audited as `config_knowledge_agent_scopes` | `pkg/dashboard/api.go` |
| `GET` | `/api/config/knowledge/pipeline` | Owner only | Scalar bead synthesizer, curator, code map and primer settings, plus the read-only engine, vault paths and primer merge strategy ([docs](../settings.md#knowledge)) | `pkg/dashboard/api.go` |
| `PUT` | `/api/config/knowledge/pipeline` | Owner only | Update those settings after `config.ValidateKnowledgePipeline`; 0 or empty restores the default; the bead synthesizer switch applies at once, the rest after a restart; audited as `config_knowledge_pipeline` | `pkg/dashboard/api.go` |
| `GET` | `/api/knowledge/git-sources` | Dashboard auth/session | Git Sources List | `pkg/dashboard/api.go:419` |
| `POST` | `/api/knowledge/git-sources` | Owner only | Git Sources Connect | `pkg/dashboard/api.go:420` |
| `DELETE` | `/api/knowledge/git-sources` | Owner only | Git Sources Disconnect | `pkg/dashboard/api.go:421` |
| `POST` | `/api/knowledge/obsidian/sync` | Dashboard auth/session | Obsidian Sync | `pkg/dashboard/api.go:422` |
| `GET` | `/api/knowledge/documents` | Dashboard auth/session | Documents List | `pkg/dashboard/api.go:423` |
| `POST` | `/api/knowledge/documents` | Dashboard auth/session | Documents Import | `pkg/dashboard/api.go:424` |
| `GET` | `/api/knowledge/documents/{slug}` | Dashboard auth/session | Document Get | `pkg/dashboard/api.go:425` |
| `DELETE` | `/api/knowledge/documents/{slug}` | Dashboard auth/session | Document Delete | `pkg/dashboard/api.go:426` |
| `POST` | `/api/knowledge/documents/{slug}/reimport` | Dashboard auth/session | Document Reimport | `pkg/dashboard/api.go:427` |
| `GET` | `/api/knowledge/context7/search` | Dashboard auth/session | Context7 Search | `pkg/dashboard/api.go:428` |
| `POST` | `/api/knowledge/cleanup-orphans` | Dashboard auth/session | Cleanup Orphans | `pkg/dashboard/api.go:429` |
| `GET` | `/api/knowledge/channels` | Dashboard auth/session | Knowledge Channels List | `pkg/dashboard/api.go:392` |
| `POST` | `/api/knowledge/channels` | Dashboard auth/session | Knowledge Channel Create | `pkg/dashboard/api.go:393` |
| `GET` | `/api/knowledge/connectors` | Dashboard auth/session | Knowledge connector statuses and publish mirror status (`{"connectors": [...], "publish": {...}\|null}`, see [knowledge-connectors.md](../knowledge-connectors.md)) | `pkg/dashboard/api_knowledge_connectors.go:23` |
| `POST` | `/api/knowledge/connectors/{name}/sync` | Owner only | Start one connector sync now (202; 404 unknown, 409 running) | `pkg/dashboard/api_knowledge_connectors.go:24` |
| `POST` | `/api/knowledge/publish/sync` | Owner only | Run a publish mirror batch now (202 with publish status; 503 mirror not running, 409 batch in progress) | `pkg/dashboard/api_knowledge_connectors.go:25` |
